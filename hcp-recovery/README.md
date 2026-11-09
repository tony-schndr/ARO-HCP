# HCP Recovery

`hcp-recovery` runs on the management cluster. It restores **etcd in place**
for an existing three-member hosted control plane from a selected Velero backup.
It replaces the etcd volumes; it does not recreate the HostedCluster, namespaces,
worker nodes, or Azure resources.

Start a recovery through the admin API's `POST /restore` on the target HCP
resource. The backend pauses backup schedules before it creates the `HCPRecovery`.
Read progress with `GET /restore?recoveryID=<id>` on the same resource. Creating
the CR directly bypasses that coordination. [`test.sh`](test.sh) applies a live
CR to the current Kubernetes context; it is **not** a unit test.

- [Recovery flow and diagnosis](docs/hosted-cluster-recovery-flow.md): request lifecycle, phase gates, snapshot handling, and what to inspect if progress stops.
- [Manual restore SOP](../docs/etcd-restore.md): the separate operator procedure, not steps to run alongside an automated recovery.
