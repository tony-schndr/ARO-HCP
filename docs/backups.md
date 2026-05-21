# HCP Backups

## Overview

ARO-HCP uses [Velero](https://velero.io/) to perform automated backups of Hosted Control Plane (HCP) resources. The backup system is composed of:

- A **backup schedule controller** in the backend service that creates and manages Velero Schedule resources on management clusters via Maestro.
- An **admin API** that exposes endpoints for on-demand backup creation, listing, and pause/resume of scheduled backups.
- **Velero** deployed on each management cluster with the Azure and HyperShift plugins.
- **Azure Blob Storage** as the backup storage backend.

Backups capture the Kubernetes resources that define a hosted control plane — not volume data. This allows disaster recovery by recreating the control plane from backed-up manifests.

## Architecture

```mermaid
flowchart LR
    subgraph SVC["Service Cluster"]
        BC["Backup Schedule\nController\n(backend)"]
        Admin["Admin API\n(admin server)"]
        Cosmos["Cosmos DB\n(ServiceProviderCluster)"]
    end

    subgraph Maestro
        MW["ManifestWork"]
    end

    subgraph MGMT["Management Cluster"]
        VS["Velero Schedule"]
        VB["Velero Backups"]
    end

    Blob["Azure Blob Storage"]

    BC -- "create / patch" --> MW
    MW -- "synced by Maestro" --> VS
    VS -- "creates on cron" --> VB
    VB -- "uploads to" --> Blob
    VS -- "status feedback" --> MW
    MW -- "feedback propagated" --> BC
    BC -- "updates SPC status" --> Cosmos
    Admin -- "reads/writes backup state" --> Cosmos
    Admin -- "creates on-demand backups,\nlists/gets backups" --> MGMT
```

### Data Flow

1. The backup schedule controller watches clusters in the Cosmos DB. When a cluster reaches an operational state (Ready, Error, or Updating), the controller creates a Maestro ManifestWork containing a Velero Schedule.
2. Maestro syncs the ManifestWork to the appropriate management cluster, which creates the Velero Schedule in the `velero` namespace.
3. Velero executes backups according to the cron schedule, uploading backup data to Azure Blob Storage.
4. The Velero Schedule status (last backup time, phase) propagates back through ManifestWork feedback rules.
5. The controller extracts this feedback and persists it in the ServiceProviderCluster record in Cosmos DB.
6. The admin API reads from Cosmos DB for profile state and connects directly to management clusters for backup listing and on-demand creation.

## What Gets Backed Up

Each backup targets the two namespaces associated with a hosted control plane: the hosted cluster namespace and the hosted control plane namespace.

The list of included Kubernetes resource types is defined in `internal/recovery/backup.go` (the `backupIncludedResources` variable). This is the authoritative source for what resources are captured. At a high level, it includes HyperShift resources (HostedCluster, HostedControlPlane, NodePool), Cluster API resources (Cluster, Machine, MachineDeployment, etc.), Azure-specific resources (AzureCluster, AzureMachine, etc.), and standard workload resources (Deployments, StatefulSets, ConfigMaps, Secrets, Services, etc.).

Volume snapshots are disabled. Backups capture only Kubernetes resource manifests, not persistent volume data.

## Backup Schedule Controller

The backup schedule controller runs in the backend service. Its source is in `backend/pkg/controllers/backupcontroller/`.

### Cluster Selection

The controller watches all HCP clusters stored in Cosmos DB. Only clusters whose Cluster Service state is Ready, Error, or Updating are eligible for backup scheduling. Clusters that are still provisioning or being deleted are skipped.

### Reconciliation

Each sync cycle follows a step-chain pattern where at most one mutating action is performed:

1. **Ensure ManifestWork created** — If no ManifestWork exists in Maestro for this cluster, create one containing the desired Velero Schedule.
2. **Ensure ManifestWork patched** — If the ManifestWork exists but its spec has drifted from the desired state (e.g., schedule changed, pause state toggled), patch it.
3. **Record ManifestWork in status** — If the ServiceProviderCluster record does not yet reference the ManifestWork name, update it.
4. **Extract feedback** — Read the ManifestWork status feedback from Maestro and update the ServiceProviderCluster with the last backup time and phase.

The controller stops after the first step that produces an action. On the next sync cycle, it picks up where it left off. This keeps each reconciliation simple and predictable.

### ManifestWork Structure

The ManifestWork wraps a single Velero Schedule resource. It uses the ServerSideApply update strategy and includes FeedbackRules that extract the `.status` field from the Velero Schedule, enabling backup status to propagate back through Maestro without direct management cluster access.

Naming conventions for schedules and ManifestWorks are defined in `backend/pkg/controllers/backupcontroller/manifestwork.go`.

## Schedule Configuration

Three configuration knobs control backup scheduling behavior:

| Setting | Description |
|---------|-------------|
| Backup schedule | Cron expression controlling how often Velero creates backups. |
| Backup TTL | How long backups are retained before Velero garbage-collects them. |
| Global pause | Pauses all backup schedules across all clusters. |

These are exposed as command-line flags on the backend. See `backend/cmd/root.go` for flag definitions and default values. The values are injected at deployment time through the backend's Helm chart ConfigMap (`backend/deploy/templates/backend.configmap.yaml`).

## Pause and Resume

Backup schedules can be paused at two levels:

- **Global pause** — Configured via the backend's global pause setting. When set to Paused, all Velero Schedules managed by the controller have their `paused` field set to true.
- **Per-cluster pause** — Set via the admin API by patching the backup profile for a specific cluster. The BackupState field on the ServiceProviderCluster is set to Paused.

If either the global pause or the per-cluster state is Paused, the resulting Velero Schedule is paused. The controller evaluates both on every sync cycle and updates the ManifestWork if the pause state changes.

## Admin API Endpoints

All backup admin API endpoints are scoped to a specific HCP cluster, identified by its full ARM resource ID in the URL path. The endpoints are registered in `admin/server/server/admin.go` and implemented in `admin/server/handlers/hcp/backups.go` and `admin/server/handlers/hcp/backup_profile.go`.

| Method | Path Suffix | Description |
|--------|------------|-------------|
| GET | `/backups` | Lists all Velero Backups for the cluster on its management cluster, filtered by cluster label. |
| GET | `/backups/{backupName}` | Returns a single backup by name. Validates that the backup belongs to the requesting cluster. Returns 404 if the backup does not exist or belongs to a different cluster. |
| POST | `/backups` | Creates an on-demand Velero Backup on the management cluster. The backup name includes a timestamp. Uses the same resource list and configuration as scheduled backups. |
| GET | `/backupprofile` | Returns the backup profile for the cluster, including the current schedule state (Active or Paused) and the last backup time and status from Velero feedback. |
| PATCH | `/backupprofile` | Updates the backup schedule state for the cluster. Accepts a state of Active or Paused. Returns 400 for invalid state values. |

The backup and backup list endpoints connect directly to the management cluster hosting the HCP's control plane. They resolve the management cluster by looking up the cluster's provision shard in Cluster Service, then create a controller-runtime client to query Velero resources in the `velero` namespace.

The backup profile endpoints read from and write to the ServiceProviderCluster record in Cosmos DB. Changes to the backup state are picked up by the backup schedule controller on its next sync cycle.

## Orphaned ManifestWork Cleanup

A separate controller (`delete_orphaned_backup_manifestworks.go`) runs every 10 minutes (with jitter) to clean up backup ManifestWorks that are no longer associated with any cluster. This handles cases like cluster deletion or controller recovery after failures.

The cleanup process:

1. Lists all ServiceProviderCluster records from Cosmos DB.
2. Groups them by provision shard (management cluster).
3. For each shard, lists all ManifestWorks in Maestro that carry the backup-managed-by label.
4. Deletes any ManifestWork whose name is not referenced by any ServiceProviderCluster's backup schedule ManifestWork name field.

## Infrastructure

### Storage

Backup data is stored in Azure Blob Storage. The storage account is provisioned via Bicep templates in `dev-infrastructure/modules/hcp-backups/`. The storage account uses Cool access tier for cost optimization and zone-redundant storage (ZRS) where available, falling back to locally-redundant storage (LRS).

### Velero Deployment

Velero is deployed to each management cluster via the Helm chart in `velero/deploy/`. The deployment uses Velero's CLI-based installation (not the upstream Helm chart) wrapped in a Kubernetes Job. Two plugins are included:

- **Azure plugin** — Provides the Azure Blob Storage backend for backup data.
- **HyperShift plugin** — Handles HyperShift-specific backup and restore operations.

### Authentication

Velero authenticates to Azure Blob Storage using workload identity. The Velero service account is annotated with the managed identity's client ID. The identity is granted Storage Blob Data Contributor, Storage Account Key Operator, and Reader roles on the backup storage account. Role assignments are managed in `dev-infrastructure/modules/hcp-backups/storage-rbac.bicep`.
