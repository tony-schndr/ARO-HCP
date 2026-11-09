// Copyright 2026 Microsoft Corporation
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package backups

import (
	"encoding/json"
	"fmt"
	"strings"

	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	azcorearm "github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"

	hyperv1beta1 "github.com/openshift/hypershift/api/hypershift/v1beta1"

	"github.com/Azure/ARO-HCP/internal/api/coreapi"
	"github.com/Azure/ARO-HCP/internal/api/kubeapplierapi"
	"github.com/Azure/ARO-HCP/internal/apihelpers/kubeapplierapihelpers"
	"github.com/Azure/ARO-HCP/internal/backup"
	"github.com/Azure/ARO-HCP/internal/utils"
)

func backupApplyDesireName(scheduleName string) string {
	return backup.BackupScheduleDesireNamePrefix + scheduleName
}

// backupScheduleTarget is the management-cluster object each backup-schedule
// desire points at: the Velero Schedule with the schedule's name.
func backupScheduleTarget(scheduleName string) kubeapplierapi.ResourceReference {
	return kubeapplierapi.ResourceReference{
		Group:     backup.VeleroGroup,
		Version:   backup.VeleroVersion,
		Resource:  backup.VeleroScheduleResource,
		Namespace: backup.VeleroNamespace,
		Name:      scheduleName,
	}
}

func buildApplyDesireFromSchedule(
	subscriptionID, resourceGroupName, clusterName, desireName string,
	managementClusterResourceID *azcorearm.ResourceID,
	schedule *velerov1.Schedule,
) (*kubeapplierapi.ApplyDesire, error) {
	resourceIDStr := kubeapplierapihelpers.ToClusterScopedApplyDesireResourceIDString(
		subscriptionID, resourceGroupName, clusterName, desireName,
	)
	resourceID, err := azcorearm.ParseResourceID(resourceIDStr)
	if err != nil {
		return nil, utils.TrackError(fmt.Errorf("failed to parse ApplyDesire resource ID for schedule %s: %w", schedule.Name, err))
	}

	raw, err := json.Marshal(schedule)
	if err != nil {
		return nil, utils.TrackError(fmt.Errorf("failed to marshal schedule %s: %w", schedule.Name, err))
	}

	return &kubeapplierapi.ApplyDesire{
		CosmosMetadata: coreapi.CosmosMetadata{ResourceID: resourceID, PartitionKey: strings.ToLower(managementClusterResourceID.String())},
		Spec: kubeapplierapi.ApplyDesireSpec{
			ManagementCluster: managementClusterResourceID,
			Type:              kubeapplierapi.ApplyDesireTypeServerSideApply,
			TargetItem:        backupScheduleTarget(schedule.Name),
			ServerSideApply: &kubeapplierapi.ServerSideApplyConfig{
				KubeContent: &runtime.RawExtension{Raw: raw},
			},
		},
		Tags: map[string]string{
			backup.DesireTagKeySchedule:      "",
			kubeapplierapi.TagControllerName: BackupScheduleControllerName,
		},
	}, nil
}

// The helpers below are thin package-local aliases for the shared
// implementations in internal/backup. The construction and gating logic is
// shared with the admin API, which creates admin-requested backup desires
// directly and therefore cannot import this package.

// buildReadDesireFromApplyDesire builds the cluster-scoped ReadDesire that
// observes the same management-cluster object a backup ApplyDesire lands.
func buildReadDesireFromApplyDesire(
	subscriptionID, resourceGroupName, clusterName string,
	applyDesire *kubeapplierapi.ApplyDesire,
) (*kubeapplierapi.ReadDesire, error) {
	return backup.BuildReadDesireFromApplyDesire(subscriptionID, resourceGroupName, clusterName, applyDesire)
}

// isDesireSuccessful reports whether the given desire conditions include a
// successful reconciliation.
func isDesireSuccessful(conditions []metav1.Condition) bool {
	return backup.IsDesireSuccessful(conditions)
}

// observedVeleroBackupPhase returns the phase of the Backup observed by kube-applier.
func observedVeleroBackupPhase(kubeContent *runtime.RawExtension) (velerov1.BackupPhase, error) {
	return backup.ObservedVeleroBackupPhase(kubeContent)
}

// rotationComplete reports whether KMS key rotation has fully completed and the
// cluster is safe to snapshot.
func rotationComplete(hostedCluster *hyperv1beta1.HostedCluster) bool {
	return backup.RotationComplete(hostedCluster)
}

// keyRotationDesireName returns the desire name for an on-demand Velero Backup.
func keyRotationDesireName(backupName string) string {
	return backup.OndemandBackupDesireName(backupName)
}
