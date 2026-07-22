# ARO-HCP Hosted Cluster Recovery Flow

This document describes the end-to-end backup and recovery flow for a Hosted Control Plane (HCP) in ARO-HCP,
covering every component from the Admin API through the management cluster.

From a high level hosted control planes and node pools are completely torn down and restored from a backup.  Since ARO-HCP
is using CSI snapshots by nature this process does not clear informer caches or kubelet caches that exist in hosted control
plane or node pools.  etcd revisions are rolled back and controllers essentially ignore what happened.  This can cause mismatch
between etcd, kubelet, and CRI-O containers which causes cluster operators to be stuck (and any other customer components).
Due to this the recovery process deletes the hosted control plane to clear control plane caches, however the data plane
must also be be torn down which is not desirable but necessary.

There are 3 known solutions to leave the data plane online during etcd recovery, two are not viable to be automated so they are disregarded.

- Delete node pool and recreate on restore.
- Restart kubelet on all nodes.  Doesn't solve the problem of other controller caches.
- Add a noop config map to nodepool.spec.config to recreate nodes. This gets stuck draining / cordoning nodes due to the etcd/kubelet/CRI-O mismatch.

In the future ARO-HCP will move to etcdctl snapshots which has flags that bump object revision and compacts old objects
which effectively clear caches on informers and kubelets as described in https://etcd.io/docs/v3.5/op-guide/recovery/
etcdctl restore is not available until ocp 4.22 and ARO-HCP is required to support backups for 4.20 & 4.21.

## System Overview

ARO-HCP recovery spans four deployment planes:

| Plane | Components                                                        |
|-------|-------------------------------------------------------------------|
| **Service Cluster** | Admin API (`admin/server`), Backend controllers, Clusters Service |
| **Cosmos DB** | `ServiceProviderCluster` (SPC), `ApplyDesire`, `ReadDesire`       |
| **Management Cluster** | kube-applier, hcp-recovery controller, Velero + OADP plugin       |
| **Azure Blob Storage** | Velero backup archives, etcd snapshots                            |


## Restore Flow

### Overview

```mermaid
sequenceDiagram
    participant SRE as SRE
    participant Admin as Admin API
    participant Cosmos as Cosmos DB
    participant Backend as Backend\nRecovery Controller
    participant ClustersService as Clusters Service
    participant KA as kube-applier
    participant HCPR as hcp-recovery\ncontroller
    participant Velero as Velero + OADP Plugin
    participant Blob as Azure Blob Storage

    SRE->>Admin: POST /hcps/{id}/restore {backupID}
    Admin->>Cosmos: Append RecoveryRequest to SPC.Spec.RecoveryRequests[]
    Admin-->>SRE: {recoveryID, state: Pending}

    loop Backend reconcile
        Backend->>Cosmos: Read SPC
        Backend->>Cosmos: Set Status.Recoveries(recoveryId).StartedAt
        Backend->>Cosmos: Set SPC.Spec.BackupState = Paused
        Backend->>Cosmos: Poll ReadDesires — wait for all Velero Schedules to show Paused
        Note over Backend,ClustersService: Once migrated to ApplyDesires this call is dropped and we directly pause ApplyDesire's.
        Backend->>ClustersService: GET /clusters/{id}
        alt restore mode not active
            Backend->>ClustersService: PUT /clusters/{id}/restore (set MFWs to ReadOnly)
        end
        Backend->>Cosmos: Create ApplyDesire (HCPRecovery CR)
        Backend->>Cosmos: Create ReadDesire (watch HCPRecovery)
    end

    KA->>Cosmos: Poll ApplyDesire
    KA->>HCPR: Apply HCPRecovery CR to mgmt cluster

    Note over HCPR: Sequential step pipeline (see below)

    HCPR->>Velero: Create Velero Restore CR
    Velero->>Blob: Download backup archive
    Note over Velero: OADP Plugin RIA runs per resource

    KA->>HCPR: Read HCPRecovery status
    KA->>Cosmos: Write status to ReadDesire

    loop Backend polls ReadDesire
        Backend->>Cosmos: Read ReadDesire status
        alt Phase = Completed
            Backend->>ClustersService: GET /clusters/{id}
            alt restore mode active
                Backend->>ClustersService: PUT /clusters/{id}/restore/complete (set MFWs to original strategy)
            end
            Backend->>Cosmos: Set Status.Recoveries(recoveryId).State = Completed
            Backend->>Cosmos: Set Status.Recoveries(recoveryId).CompletedAt
            Backend->>Cosmos: Set SPC.Spec.BackupState = Enabled
        else Phase = Failed
            Backend->>Cosmos: Set Status.Recoveries(recoveryId).State = Failed
            Backend->>Cosmos: Set Status.Recoveries(recoveryId).CompletedAt
        end
    end

    SRE->>Admin: GET /hcps/{id}/restore?recoveryID={id}
    Admin-->>SRE: {state: Completed}
```

---

## Admin API Layer

**Files:** `admin/server/handlers/hcp/restore.go`, `admin/server/handlers/hcp/backups.go`

**POST /restore** (`PostRestore`):
1. Decodes `{backupID}` from the request body.
2. Checks no recovery is already in progress on the SPC (returns `409 Conflict` if so).
3. Generates a new `recoveryID` (UUID) and appends a `RecoveryRequest` to `SPC.Spec.RecoveryRequests`.
4. Replaces the SPC document in Cosmos.
5. Returns `{recoveryID, state: Pending}`.

**GET /restore** (`GetRestoreStatus`):
- Reads the SPC from Cosmos and looks up the `RecoveryStatus` matching the requested `recoveryID`.
- Returns `{state, startedAt, completedAt, backupID}`.

---

## Backend Recovery Controller

**File:** `backend/pkg/controllers/recoverycontroller/recovery_controller.go`

The backend controller (`recoverySyncer`) watches SPC changes. When it finds a pending `RecoveryRequest` it runs this state machine:

```mermaid
flowchart TD
    A([SPC with pending RecoveryRequest]) --> B{Recovery StartedAt set?}
    B -->|No| C["Set Status.Recoveries(recoveryId).StartedAt"]
    B -->|Yes| D{BackupState == Paused?}
    D -->|No| E[Set SPC.Spec.BackupState = Paused]
    D -->|Yes| F{All Velero Schedules\npaused in ReadDesires?}
    F -->|No| G[Wait and retry]
    F -->|Yes| H{CS cluster in\nrestore mode?}
    classDef note fill:#fffde7,stroke:#f9a825,stroke-dasharray:5 5,color:#555
    INOTE["Sets ManifestWorks to ReadOnly"]:::note
    I -.- INOTE
    H -->|Yes| J{HcpRecovery ApplyDesire exists?}
    H -->|No| I[Set CS cluster\nto restore mode]
    I --> J
    J -->|No| K[Create ApplyDesire:\nHCPRecovery CR]
    J -->|Yes| L{HcpRecovery ReadDesire exists?}
    L -->|No| M[Create ReadDesire:\nwatch HCPRecovery]
    L -->|Yes| N{HCPRecovery.Status.Phase?}
    N -->|empty| O[Wait and retry]
    N -->|Failed| P["Set Status.Recoveries(recoveryId).State = Failed\nSet CompletedAt"]
    N -->|Completed| R[Set CS cluster\nto disable restore mode]
    RNOTE["Sets ManifestWorks to original Strategy"]:::note
    R -.- RNOTE
    R --> Q["Set Status.Recoveries(recoveryId).State = Completed\nSet CompletedAt + BackupState = Enabled"]
```

**Key data flow:** The `ApplyDesire` carries the full `HCPRecovery` CR (marshalled JSON) for kube-applier to server-side apply on the management cluster. The `ReadDesire` points at the same `HCPRecovery` resource so kube-applier reflects its status back to Cosmos.

---

## kube-applier

kube-applier is a management-cluster agent that bridges Cosmos DB (`ApplyDesire` / `ReadDesire` documents) with actual Kubernetes resources on the management cluster.

For recovery:
- Reads the `ApplyDesire` containing the `HCPRecovery` CR.
- Applies it to the management cluster via **server-side apply** in the `hcp-recovery` namespace.
- Periodically reads the `HCPRecovery` status and writes it back to the corresponding `ReadDesire` in Cosmos.

---

## HCP Recovery Controller

**Files:** `hcp-recovery/pkg/controller/`
**CRD:** `HCPRecovery` (`hcprecovery.aro-hcp.azure.com`)

The controller reconciles `HCPRecovery` objects on the management cluster using a **sequential step pipeline**. Each reconcile executes at most one action; completed steps are idempotent (gated by status conditions).

```mermaid
flowchart TD
    Start([HCPRecovery created]) --> S1

    S1["Step 1: Validate Backup\n─────────────────────\nGet Velero Backup by spec.backupId\nVerify label api.openshift.com/id matches\nVerify phase == Completed\n→ Condition: BackupValidated"]

    S1 --> S3

    S3["Step 3: Delete HCP Namespace\n──────────────────────────────\nDelete {HC.namespace}-{HC.name}\n→ Condition: HCPNamespaceDeleted"]

    S3 --> S6

    S6["Step 6: Wait for HCP Namespace Deletion\n──────────────────────────────────────────\nPoll until namespace is gone\n→ Condition: NamespaceFullyRemoved"]

    S6 --> S7

    S7["Step 7: Delete HC Namespace\n─────────────────────────────\nDelete HC.namespace\n→ Condition: HCNamespaceDeleted"]

    S7 --> S8

    S8["Step 8: Wait for HC Namespace Deletion\n──────────────────────────────────────────\nPoll until namespace is gone\n→ Condition: HCNamespaceFullyRemoved"]

    S8 --> S9

    S9["Step 9: Create Velero Restore\n──────────────────────────────\nCreate Restore CR (restore-{name})\nfrom spec.backupId\nRestorePVs=true, existing=update\nPoll until Completed\n→ Condition: VeleroRestoreCompleted"]

    S9 --> S10

    S10["Step 10: Validate HostedCluster\n────────────────────────────────\nWait for HostedClusterAvailable = True\n→ Condition: HealthChecked"]

    S10 --> S11

    S11["Step 11: Unpause HostedCluster\n───────────────────────────────\nClear spec.pausedUntil\nRemove hcp-recovery annotations\n→ Condition: HostedClusterUnpaused"]

    S11 --> Done([Phase: Completed])
```

## Velero Restore Configuration

The `Velero Restore` CR created by the hcp-recovery controller (`hcp-recovery/pkg/recovery/velero.go`):

```yaml
apiVersion: velero.io/v1
kind: Restore
metadata:
  name: restore-{recoveryName}-{uid[:8]}
  namespace: velero
spec:
  backupName: {spec.backupId}
  restorePVs: true
  existingResourcePolicy: update
  itemOperationTimeout: 4h
  excludedResources:
  - nodes
  - events
  - events.events.k8s.io
  - backups.velero.io
  - restores.velero.io
  - resticrepositories.velero.io
```

`existingResourcePolicy: update` ensures that if any backed-up resources survived partial deletion, they are updated in place rather than skipped.

---

## Cosmos DB Data Model

### `ServiceProviderCluster` *(one per HCP cluster)*

**Spec**
- `backupState` — `Enabled | Paused`
- `recoveryRequests[]` — list; Admin API appends one entry per `POST /restore`
  - `recoveryId` — uuid
  - `backupId` — Velero backup name

**Status**
- `managementClusterResourceID` — ARM resource ID of the management cluster
- `recoveries[]` — list; correlated to `recoveryRequests[]` by `recoveryId`
  - `recoveryId` — uuid *(join key)*
  - `state` — `Pending | Completed | Failed`
    > `RecoveryCRCreated | Monitoring | Restoring` are defined but not yet set — TODO
  - `startedAt`
  - `completedAt`

---

### `ApplyDesire` *(one per recovery — name: `RecoveryDesireNamePrefix` + `recoveryId`)*

- `Spec.TargetItem` → `HCPRecovery` CR in namespace `hcp-recovery`

---

### `ReadDesire` *(one per recovery — name: `RecoveryDesireNamePrefix` + `recoveryId`)*

- `Spec.TargetItem` → `HCPRecovery` CR
- `Status.KubeContent` → Full `HCPRecovery` JSON (includes `.status.phase` and `.status.conditions`)

**List correlation (`findActiveRecovery`):** On each reconcile the backend controller iterates `Spec.RecoveryRequests[]` and matches each entry against `Status.Recoveries[]` by `recoveryId`. If no `Status.Recoveries` entry exists for a given request it appends `RecoveryStatus{RecoveryId: ..., State: Pending}`. The function enforces at most one non-terminal entry across both lists — any count > 1 returns an error.

---

## Concurrency and Safety

- **One recovery at a time:** `PostRestore` rejects the request with `409 Conflict` if any non-terminal recovery is already in progress (checked against both `SPC.Spec.RecoveryRequests` and `SPC.Status.Recoveries`).
- **Idempotent controller:** Every step in the hcp-recovery controller is re-entrant. If the controller restarts mid-recovery, it reads existing conditions from the `HCPRecovery` status and skips already-completed steps.
- **Duplicate restore prevention:** The Velero Restore name is persisted to `HCPRecovery.Status.RestoreName` before the `Restore` CR is created — if the controller crashes after creating the Restore but before updating status, a subsequent reconcile will find the existing Restore by name and monitor it rather than creating a second one.
- **Backup schedule pause:** Backup schedules are paused before any destructive action begins and re-enabled only after the hcp-recovery controller reports `Completed`. This prevents a new backup from capturing a partially-deleted cluster state.

---

## Key Component Locations

| Component | Path |
|-----------|------|
| Admin restore endpoint | `admin/server/handlers/hcp/restore.go` |
| Admin backup endpoint | `admin/server/handlers/hcp/backups.go` |
| Backend recovery controller | `backend/pkg/controllers/recoverycontroller/recovery_controller.go` |
| HCPRecovery CRD types | `hcp-recovery/pkg/apis/hcprecovery/v1alpha1/types.go` |
| Recovery controller main loop | `hcp-recovery/pkg/controller/controller.go` |
| Step: validate backup | `hcp-recovery/pkg/controller/step_validate_backup.go` |
| Step: pause/unpause HC | `hcp-recovery/pkg/controller/step_pause.go`, `step_unpause.go` |
| Step: namespace deletion | `hcp-recovery/pkg/controller/step_delete_namespace.go` |
| Step: finalizer removal | `hcp-recovery/pkg/controller/step_finalizers.go` |
| Step: Velero restore | `hcp-recovery/pkg/controller/step_restore.go` |
| Step: health check | `hcp-recovery/pkg/controller/step_validate_cluster.go` |
| Velero Restore builder | `hcp-recovery/pkg/recovery/velero.go` |
| Backup object builder | `internal/backup/backup.go` |
| ServiceProviderCluster types | `internal/api/types_serviceprovider_cluster.go` |
| OADP plugin architecture | `openshift/hypershift-oadp-plugin/ARCHITECTURE.md` |
| HCPEtcdBackup integration | `openshift/hypershift-oadp-plugin/docs/references/HCPEtcdBackup/HCPEtcdBackup-implementation.md` |
