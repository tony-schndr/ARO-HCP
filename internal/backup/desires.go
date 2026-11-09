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
	"encoding/json"
	"fmt"
	"strings"
	"time"

	velerov1api "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	azcorearm "github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"

	hyperv1beta1 "github.com/openshift/hypershift/api/hypershift/v1beta1"

	"github.com/Azure/ARO-HCP/internal/api/coreapi"
	"github.com/Azure/ARO-HCP/internal/api/kubeapplierapi"
	"github.com/Azure/ARO-HCP/internal/apihelpers/kubeapplierapihelpers"
	"github.com/Azure/ARO-HCP/internal/utils"
)

// OnDemandBackupControllerName and KeyRotationBackupControllerName name the two
// backend controllers that own on-demand backup desires: respectively the one
// reaping admin-requested backups and the one driving KMS key-rotation backups.
// Whoever creates a desire stamps its owner into kubeapplierapi.TagControllerName,
// which is the only thing distinguishing the two flows — they share a tag key, a
// desire name prefix, and a document shape.
//
// Each backend controller re-declares its own as a ControllerName constant; the
// values live here because the admin server cannot import backend packages.
const (
	OnDemandBackupControllerName    = "OnDemandBackup"
	KeyRotationBackupControllerName = "KeyRotationBackup"
)

// OnDemandBackupTTL is the Velero TTL applied to an admin-requested on-demand
// backup. It matches the shortest production schedule's TTL, which is the value
// the on-demand path resolved to before the TTL moved here.
//
// This is deliberately independent of the backup cadence profile: a
// testing-cadence environment runs a 1h schedule TTL, and expiring an SRE's
// explicitly requested backup after an hour would make it useless as a restore
// point in exactly the environments where recovery is being exercised.
const OnDemandBackupTTL = 48 * time.Hour

// OndemandBackupDesireName returns the ApplyDesire/ReadDesire name for an
// on-demand Velero Backup of the given name.
func OndemandBackupDesireName(backupName string) string {
	return OndemandBackupDesireNamePrefix + backupName
}

// IsBackupDesire reports whether tags belong to an on-demand backup desire,
// whichever flow created it. Use this only where both flows are genuinely in
// scope — listing backups for an operator, say. Anything that mutates or
// deletes must use OwnsBackupDesire instead.
func IsBackupDesire(tags map[string]string) bool {
	_, ok := tags[DesireTagKeyOndemandBackup]
	return ok
}

// OwnsBackupDesire reports whether an on-demand backup desire belongs to
// controllerName. Both flows tag their desires DesireTagKeyOndemandBackup and
// name them alike, so the owner recorded in kubeapplierapi.TagControllerName is
// what keeps one flow's cleanup sweep off the other's live desires.
//
// The match is strict: BuildBackupApplyDesire always stamps the owner (and
// kubeapplierhelpers rejects any desire persisted without it), so a backup
// desire with no owner tag does not exist. Treating a missing owner as "mine"
// would make it mine for both controllers at once, which is the double-reap
// this function exists to prevent.
func OwnsBackupDesire(tags map[string]string, controllerName string) bool {
	return IsBackupDesire(tags) && tags[kubeapplierapi.TagControllerName] == controllerName
}

// IsTerminalBackupPhase reports whether a Velero Backup has stopped progressing.
// Once terminal, Velero owns the Backup for the remainder of its TTL and the
// ApplyDesire that created it can be retired.
func IsTerminalBackupPhase(phase velerov1api.BackupPhase) bool {
	switch phase {
	case velerov1api.BackupPhaseCompleted,
		velerov1api.BackupPhaseFailed,
		velerov1api.BackupPhasePartiallyFailed,
		velerov1api.BackupPhaseFailedValidation:
		return true
	}
	return false
}

// ObservedVeleroBackupPhase returns the phase of the Backup observed by
// kube-applier. An empty phase means no Backup has been observed yet: a
// Successful=True condition only confirms kube-applier is alive, not that it
// found the object.
func ObservedVeleroBackupPhase(kubeContent *runtime.RawExtension) (velerov1api.BackupPhase, error) {
	if kubeContent == nil || kubeContent.Raw == nil {
		return "", nil
	}
	var veleroBackup velerov1api.Backup
	if err := json.Unmarshal(kubeContent.Raw, &veleroBackup); err != nil {
		return "", fmt.Errorf("failed to unmarshal observed Backup: %w", err)
	}
	return veleroBackup.Status.Phase, nil
}

// IsDesireSuccessful reports whether the given desire conditions include a
// successful reconciliation.
func IsDesireSuccessful(conditions []metav1.Condition) bool {
	for _, condition := range conditions {
		if condition.Type == kubeapplierapi.ConditionTypeSuccessful && condition.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}

// RotationComplete returns true only when KMS key rotation has fully completed and the
// cluster is safe to snapshot. The HostedClusterConfigOperator derives the rotation phase
// from the live SecretEncryption state and clears TargetKey once the final rotation update
// has completed; History[0].State is recorded for observability, not as an input to the
// decision. We therefore require both TargetKey=="" and History[0].State==Completed as
// a conservative fail-safe: if those fields are ever patched separately, we skip the
// backup instead of snapshotting mid-rotation. This also covers an existing AESCBC
// cluster's spec-driven migration to KMS after the first KMS key is active; clusters
// created directly with KMS never populate History and therefore never trigger this path.
//
// This function does not deduplicate; callers gate on their own completion record.
func RotationComplete(hostedCluster *hyperv1beta1.HostedCluster) bool {
	status := hostedCluster.Status.SecretEncryption
	if len(status.History) == 0 {
		return false
	}
	return status.TargetKey.Azure.KeyVersion == "" &&
		status.History[0].State == hyperv1beta1.EncryptionMigrationStateCompleted
}

// SnapshotSafe reports whether the observed HostedCluster is in a state where
// taking an etcd snapshot produces a restorable backup. It returns a non-nil
// error naming the reason when it is not: the HostedCluster has not been
// observed yet, or a KMS key rotation is still in flight (snapshotting
// mid-rotation can capture etcd encrypted under a key the restore will not
// have).
func SnapshotSafe(hostedCluster *hyperv1beta1.HostedCluster, observed bool) error {
	if hostedCluster == nil || !observed {
		return fmt.Errorf("waiting for an observed HostedCluster")
	}
	if hostedCluster.Status.SecretEncryption.TargetKey.Azure.KeyVersion != "" {
		return fmt.Errorf("waiting for in-progress KMS key rotation to complete")
	}
	if len(hostedCluster.Status.SecretEncryption.History) > 0 && !RotationComplete(hostedCluster) {
		return fmt.Errorf("waiting for in-progress KMS key rotation to complete")
	}
	return nil
}

// BuildBackupApplyDesire constructs the cluster-scoped ApplyDesire that lands a
// Velero Backup on the management cluster. Every such desire is tagged
// DesireTagKeyOndemandBackup; controllerName is stamped into
// kubeapplierapi.TagControllerName and is what tells the KMS-rotation flow from
// the admin-requested one, so each flow's cleanup sweep only reaps its own.
func BuildBackupApplyDesire(
	subscriptionID, resourceGroupName, clusterName string,
	managementClusterResourceID *azcorearm.ResourceID,
	veleroBackup *velerov1api.Backup,
	controllerName string,
) (*kubeapplierapi.ApplyDesire, error) {
	desireName := OndemandBackupDesireName(veleroBackup.Name)
	resourceIDStr := kubeapplierapihelpers.ToClusterScopedApplyDesireResourceIDString(
		subscriptionID, resourceGroupName, clusterName, desireName,
	)
	resourceID, err := azcorearm.ParseResourceID(resourceIDStr)
	if err != nil {
		return nil, fmt.Errorf("failed to parse ApplyDesire resource ID for backup %s: %w", veleroBackup.Name, err)
	}

	raw, err := json.Marshal(veleroBackup)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal backup %s: %w", veleroBackup.Name, err)
	}

	return &kubeapplierapi.ApplyDesire{
		CosmosMetadata: coreapi.CosmosMetadata{
			ResourceID:   resourceID,
			PartitionKey: strings.ToLower(managementClusterResourceID.String()),
		},
		Spec: kubeapplierapi.ApplyDesireSpec{
			ManagementCluster: managementClusterResourceID,
			Type:              kubeapplierapi.ApplyDesireTypeServerSideApply,
			TargetItem: kubeapplierapi.ResourceReference{
				Group:     VeleroGroup,
				Version:   VeleroVersion,
				Resource:  VeleroBackupResource,
				Namespace: VeleroNamespace,
				Name:      veleroBackup.Name,
			},
			ServerSideApply: &kubeapplierapi.ServerSideApplyConfig{
				KubeContent: &runtime.RawExtension{Raw: raw},
			},
		},
		Tags: map[string]string{
			DesireTagKeyOndemandBackup:       "",
			kubeapplierapi.TagControllerName: controllerName,
		},
	}, nil
}

// BuildReadDesireFromApplyDesire builds the cluster-scoped ReadDesire that
// observes the same management-cluster object a backup ApplyDesire lands. It
// reuses the ApplyDesire's name, target, management cluster, partition key, and
// tags so the apply/read pair stays in lockstep.
func BuildReadDesireFromApplyDesire(
	subscriptionID, resourceGroupName, clusterName string,
	applyDesire *kubeapplierapi.ApplyDesire,
) (*kubeapplierapi.ReadDesire, error) {
	desireName := applyDesire.ResourceID.Name
	resourceIDStr := kubeapplierapihelpers.ToClusterScopedReadDesireResourceIDString(
		subscriptionID, resourceGroupName, clusterName, desireName,
	)
	resourceID, err := azcorearm.ParseResourceID(resourceIDStr)
	if err != nil {
		return nil, utils.TrackError(fmt.Errorf("failed to parse ReadDesire resource ID for %s: %w", desireName, err))
	}

	return &kubeapplierapi.ReadDesire{
		CosmosMetadata: coreapi.CosmosMetadata{ResourceID: resourceID, PartitionKey: applyDesire.PartitionKey},
		Spec: kubeapplierapi.ReadDesireSpec{
			ManagementCluster: applyDesire.Spec.ManagementCluster,
			TargetItem:        applyDesire.Spec.TargetItem,
		},
		Tags: applyDesire.Tags,
	}, nil
}
