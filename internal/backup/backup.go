// Copyright 2026 Microsoft Corporation
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package backup

import (
	"crypto/sha256"
	"fmt"
	"time"

	velerov1api "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	"github.com/vmware-tanzu/velero/pkg/builder"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	hyperv1beta1 "github.com/openshift/hypershift/api/hypershift/v1beta1"

	"github.com/Azure/ARO-HCP/internal/controllerutils"
)

const (
	BackupScheduleDesireNamePrefix = "backupschedule-"
	OndemandBackupDesireNamePrefix = "ondemandbackup-"
	DesireTagKeySchedule           = "backupschedule"
	DesireTagKeyOndemandBackup     = "ondemandbackup"
	VeleroGroup                    = "velero.io"
	VeleroVersion                  = "v1"
	VeleroScheduleResource         = "schedules"
	VeleroBackupResource           = "backups"
	VeleroNamespace                = "velero"
	etcdHookName                   = "etcd-snapshot"
	etcdAppLabel                   = "app"
	etcdAppLabelValue              = "etcd"

	// These describe the on-disk and on-network layout of a HyperShift etcd member.
	// The backup hook below writes the snapshot, and hcp-recovery reads it back out of
	// the restored volume, so both sides must agree on every one of these values.

	// EtcdContainerName is the name of the etcd container in the etcd StatefulSet's pod.
	EtcdContainerName = "etcd"
	// EtcdSnapshotPath is where the backup hook writes the etcd snapshot on the member's
	// data volume, and therefore where a restored volume carries it.
	EtcdSnapshotPath = "/var/lib/backup.db"
	// EtcdCACertPath, EtcdClientCert and EtcdClientKey are the mTLS materials mounted into
	// the etcd container, used by etcdctl.
	EtcdCACertPath = "/etc/etcd/tls/etcd-ca/ca.crt"
	EtcdClientCert = "/etc/etcd/tls/client/etcd-client.crt"
	EtcdClientKey  = "/etc/etcd/tls/client/etcd-client.key"
	// EtcdClientService and EtcdClientPort address the etcd client endpoint in a
	// control-plane namespace.
	EtcdClientService = "etcd-client"
	EtcdClientPort    = "2379"
)

// etcdBackupHooks defines velero hooks that are run pre and post backup with the etcd pods via exec.
// The etcdctl snapshot is taken and will be captured by the CSI snapshot of the PVC.  After the backup is
// complete a post hook deletes the backup from the pvc to free up space.
func etcdBackupHooks(controlPlaneNamespace string) velerov1api.BackupHooks {
	endpoint := fmt.Sprintf("https://%s.%s.svc:%s", EtcdClientService, controlPlaneNamespace, EtcdClientPort)
	return velerov1api.BackupHooks{
		Resources: []velerov1api.BackupResourceHookSpec{
			{
				Name:               etcdHookName,
				IncludedNamespaces: []string{controlPlaneNamespace},
				IncludedResources:  []string{"pods"},
				LabelSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						etcdAppLabel: etcdAppLabelValue,
					},
				},
				PreHooks: []velerov1api.BackupResourceHook{
					{
						Exec: &velerov1api.ExecHook{
							Container: EtcdContainerName,
							Command: []string{
								"/bin/sh", "-c",
								// `etcdctl snapshot save` writes to <path>.part, fsyncs that file, then atomically
								// renames it to <path> — but it does NOT fsync the parent directory. The CSI snapshot
								// captures block-device state, so the rename (a directory metadata op still sitting in
								// page cache/journal) can be missing from the snapshot, leaving only backup.db.part on
								// the restored PVC. `sync -f <path>` issues syncfs() on the data PVC, committing the
								// file data AND the rename to the block device before Velero snapshots the volume.
								// Velero runs this pre-hook to completion before snapshotting the pod's volumes,
								// so the flush is guaranteed to land first.
								fmt.Sprintf(
									"ETCDCTL_API=3 etcdctl --endpoints=%s --cacert %s --cert %s --key %s snapshot save %s && sync -f %s",
									endpoint, EtcdCACertPath, EtcdClientCert, EtcdClientKey, EtcdSnapshotPath, EtcdSnapshotPath,
								),
							},
							OnError: velerov1api.HookErrorModeFail,
							Timeout: metav1.Duration{Duration: 10 * time.Minute},
						},
					},
				},
				PostHooks: []velerov1api.BackupResourceHook{
					{
						Exec: &velerov1api.ExecHook{
							Container: EtcdContainerName,
							Command: []string{
								"/bin/sh", "-c",
								fmt.Sprintf("rm -f %s %s.part", EtcdSnapshotPath, EtcdSnapshotPath),
							},
							OnError: velerov1api.HookErrorModeContinue,
							Timeout: metav1.Duration{Duration: 2 * time.Minute},
						},
					},
				},
			},
		},
	}
}

var backupIncludedResources = []string{
	"multitenantpodnetworkconfigs",
	"podnetworks",
	"sa",
	"role",
	"rolebinding",
	"pvc", // Not required if using HcpEtcdBackup
	"pv",  // Not required if using HcpEtcdBackup
	"configmap",
	"priorityclasses",
	"pods",
	"pdb",
	"hostedcluster",
	"nodepool",
	"secrets",
	"secretproviderclass",
	"services",
	"deployments",
	"statefulsets",
	"hostedcontrolplane",
	"cluster",
	"azurecluster",
	"azuremachinetemplate",
	"azuremachine",
	"machinedeployment",
	"machineset",
	"machine",
	"route",
	"clusterdeployment",
}

// ActiveKMSKeyFingerprint returns the fingerprint of the HostedCluster's active
// Azure KMS key, or "" when the cluster is not using a customer-managed key.
// The fingerprint is recorded on the Velero Backup so a restore can tell which
// key the snapshot's etcd data is encrypted under.
func ActiveKMSKeyFingerprint(hostedCluster *hyperv1beta1.HostedCluster) string {
	activeKey := hostedCluster.Status.SecretEncryption.ActiveKey.Azure
	if activeKey.KeyVersion == "" {
		return ""
	}
	return AzureKMSKeyFingerprint(activeKey.KeyVaultName, activeKey.KeyName, activeKey.KeyVersion)
}

// AzureKMSKeyFingerprint mirrors HyperShift's FingerprintAzureKMSKey.
func AzureKMSKeyFingerprint(keyVaultName, keyName, keyVersion string) string {
	h := sha256.Sum256([]byte(keyVaultName + "/" + keyName + "/" + keyVersion))
	return fmt.Sprintf("%x", h)
}

func NewBackup(backupName, resourceID, kmsKeyFingerprint, hostedClusterNamespace, controlPlaneNamespace string, ttl time.Duration) *velerov1api.Backup {
	annotations := map[string]string{
		controllerutils.HcpClusterAzureResourceIdAnnotation: resourceID,
	}
	if kmsKeyFingerprint != "" {
		annotations[controllerutils.HcpClusterKmsKeyFingerprintAnnotation] = kmsKeyFingerprint
	}
	backup := builder.ForBackup("velero", backupName).
		StorageLocation("default").
		ObjectMeta(func(object metav1.Object) {
			object.SetAnnotations(annotations)
		}).
		IncludedNamespaces(hostedClusterNamespace, controlPlaneNamespace).
		IncludedResources(backupIncludedResources...).
		TTL(ttl).
		SnapshotVolumes(true). // Set to false if using HcpEtcdBackup
		DefaultVolumesToFsBackup(false).
		DataMover("velero").
		SnapshotMoveData(true). // Set to false if using HcpEtcdBackup
		CSISnapshotTimeout(10 * time.Minute).
		ItemOperationTimeout(15 * time.Minute).
		Hooks(etcdBackupHooks(controlPlaneNamespace))
	return backup.Result()
}
