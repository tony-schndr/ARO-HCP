# Velero Sharding

## Overview

Each management cluster runs N Velero instances, one per shard, each in its own namespace (`velero-0`, `velero-1`, ...). Each shard is responsible for a subset of HCPs. All shards share the same Azure Blob Storage account but write to separate path prefixes, enabling parallel backup execution. The shard count is configurable per environment: 2 for dev, 4 for INT/STG/PROD.

## Storage Layout

All shards write to the same storage account. Each shard's BackupStorageLocation uses a distinct blob prefix:

```
backups/                          # Azure Blob container
  velero/0/                       # Shard 0 BSL prefix
    backups/
      cluster-A-daily-20260530/
      cluster-C-daily-20260530/
  velero/1/                       # Shard 1 BSL prefix
    backups/
      cluster-B-daily-20260530/
      cluster-D-daily-20260530/
```

Every shard UAMI is granted Storage Blob Data Contributor, Storage Account Key Operator, and Reader on the entire storage account. RBAC is not prefix-scoped.

## Velero Deployment Layout

A single Helm release is installed in the `velero-release` namespace. This namespace holds no workloads -- it is an anchor for the release only. The chart loops over `shardCount` to generate per-shard resources in namespaces `velero-0` through `velero-N`.

### Per-shard resources (created inside the loop)

| Resource | Naming |
|----------|--------|
| Namespace | `velero-{i}` |
| ServiceAccount | `velero` in `velero-{i}` |
| ServiceAccount | `velero-installer` in `velero-{i}` |
| Secret | `cloud-credentials-azure` in `velero-{i}` |
| Install Job | `velero-install-{i}` in `velero-{i}` (post-install/post-upgrade hook) |
| ClusterRoleBinding | `velero-cluster-role-binding-{i}` (binds SA `velero` in `velero-{i}`) |
| ClusterRoleBinding | `velero-installer-{i}` (binds SA `velero-installer` in `velero-{i}`) |
| ConfigMap | `velero-kustomize-patch-{i}` in `velero-{i}` |
| PodMonitor | per-shard in `velero-{i}` |
| AcrPullBinding | per-shard in `velero-{i}` |

### Shared resources (created once, outside the loop)

| Resource | Note |
|----------|------|
| ClusterRole `velero-cluster-role` | Bound per-shard via unique ClusterRoleBindings |
| ClusterRole `velero-installer` | Bound per-shard via unique ClusterRoleBindings |
| VolumeSnapshotClass `azure-disk-snapclass` | Cluster-scoped, created by shard 0 only |

ClusterRoleBinding names must be unique per shard. They are cluster-scoped -- a shared name means the last `kubectl apply` overwrites the subject, breaking RBAC for all other shards.

### Install job

Each shard's install job runs two containers:

1. **Init container** (`generate-manifest`): runs `velero install --dry-run -o yaml` with the shard's namespace and prefix (`--prefix velero/{i}`), writing the manifest to a shared volume.
2. **Main container** (`apply-manifest`): applies the manifest via kustomize overlay (infra node scheduling), then waits for rollout with a 300s timeout.

All shards apply the same Velero CRDs. This is idempotent via `kubectl apply`, though concurrent applies may produce transient conflict errors that retry.

## Node-Agent Redundancy

Each shard deploys its own node-agent DaemonSet via `--use-node-agent`. This means every node runs `shardCount` node-agent pods. With 4 shards and 3 worker nodes, that is 12 node-agent pods when 3 would suffice -- any node-agent can serve data-mover pods from any Velero namespace, and all shards share the same MSI/credentials.

Deduplicating to a single shared node-agent DaemonSet is tracked as follow-up work.

## Shard Assignment

Each HCP is assigned to a shard using FNV-32a hash of the Cluster Service ID modulo the shard count:

```go
func AssignShard(clusterID string, numShards int) int {
    if numShards <= 1 {
        return 0
    }
    h := fnv.New32a()
    h.Write([]byte(clusterID))
    return int(h.Sum32() % uint32(numShards))
}
```

Assignment is deterministic and stateless -- the same cluster always maps to the same shard with no external state. The shard index derives the target namespace (`velero-{shardIndex}`) and BSL prefix (`velero/{shardIndex}`).

## Backend

The backup controller reads `veleroShardCount` from its config (defaults to 1). During reconciliation for each cluster:

1. Computes `shardIndex = AssignShard(clusterID, shardCount)`.
2. Derives `veleroNamespace = velero-{shardIndex}`.
3. Creates Velero Schedule resources targeting that namespace.
4. Wraps schedules in a ManifestWork sent to Maestro.
5. Maestro syncs the ManifestWork, creating the Schedule in the correct shard namespace on the management cluster.
6. Status feedback flows back through the ManifestWork (shard-agnostic).

All schedules for a given cluster (hourly, daily, weekly) go to the same shard.

The admin API uses the same `AssignShard` function to resolve which shard namespace to query when listing, getting, or creating backups for a cluster.

Orphan cleanup operates at the Maestro ManifestWork level using labels and is shard-agnostic.

## Identity

Each shard namespace has its own UAMI and federated credential. The federated credential subject binds to the shard's namespace and service account (`system:serviceaccount:velero-{i}:velero`), enabling workload identity authentication from pods in that namespace.

## Re-sharding

When `veleroShardCount` changes:

- Clusters are reassigned via `fnv32a(id) % newCount`.
- The backup controller patches ManifestWorks to target new shard namespaces within a few sync cycles.
- Old Velero Schedules in previous namespaces become orphaned (expire via TTL until orphan cleanup is extended to cover them).
- Backup data under old prefixes remains intact and accessible for restore.
- Scaling down (e.g., 4 to 2): `helm upgrade` removes namespaces `velero-2` and `velero-3`. Backup data in blob storage is unaffected.
