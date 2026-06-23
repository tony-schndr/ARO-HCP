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
package backupcontroller

import (
	"encoding/json"
	"fmt"
	"strings"

	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"

	"k8s.io/apimachinery/pkg/runtime"

	azcorearm "github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"

	"github.com/Azure/ARO-HCP/internal/api"
	"github.com/Azure/ARO-HCP/internal/api/kubeapplier"
	"github.com/Azure/ARO-HCP/internal/backup"
)

const (
	veleroScheduleGroup    = "velero.io"
	veleroScheduleVersion  = "v1"
	veleroScheduleResource = "schedules"
	veleroNamespace        = "velero"
)

func backupApplyDesireName(scheduleName string) string {
	return backup.BackupDesireNamePrefix + scheduleName
}

func buildApplyDesiresFromSchedules(
	subscriptionID, resourceGroupName, clusterName string,
	mcResourceID *azcorearm.ResourceID,
	schedules []*velerov1.Schedule,
) ([]*kubeapplier.ApplyDesire, error) {
	desires := make([]*kubeapplier.ApplyDesire, 0, len(schedules))
	for _, schedule := range schedules {
		desireName := backupApplyDesireName(schedule.Name)
		resourceIDStr := kubeapplier.ToClusterScopedApplyDesireResourceIDString(
			subscriptionID, resourceGroupName, clusterName, desireName,
		)
		resourceID, err := azcorearm.ParseResourceID(resourceIDStr)
		if err != nil {
			return nil, fmt.Errorf("failed to parse ApplyDesire resource ID for schedule %s: %w", schedule.Name, err)
		}

		raw, err := json.Marshal(schedule)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal schedule %s: %w", schedule.Name, err)
		}

		desires = append(desires, &kubeapplier.ApplyDesire{
			CosmosMetadata: api.CosmosMetadata{ResourceID: resourceID, PartitionKey: strings.ToLower(mcResourceID.String())},
			Spec: kubeapplier.ApplyDesireSpec{
				ManagementCluster: mcResourceID,
				TargetItem: kubeapplier.ResourceReference{
					Group:     veleroScheduleGroup,
					Version:   veleroScheduleVersion,
					Resource:  veleroScheduleResource,
					Namespace: veleroNamespace,
					Name:      schedule.Name,
				},
				KubeContent: &runtime.RawExtension{Raw: raw},
			},
		})
	}
	return desires, nil
}

func buildDeleteDesireFromApplyDesire(
	ad *kubeapplier.ApplyDesire,
	mcResourceID *azcorearm.ResourceID,
) (*kubeapplier.DeleteDesire, error) {
	clusterResourceID := ad.ResourceID.Parent
	resourceIDStr := kubeapplier.ToClusterScopedDeleteDesireResourceIDString(
		clusterResourceID.SubscriptionID,
		clusterResourceID.ResourceGroupName,
		clusterResourceID.Name,
		ad.ResourceID.Name,
	)
	resourceID, err := azcorearm.ParseResourceID(resourceIDStr)
	if err != nil {
		return nil, fmt.Errorf("failed to parse DeleteDesire resource ID %q: %w", resourceIDStr, err)
	}

	return &kubeapplier.DeleteDesire{
		CosmosMetadata: api.CosmosMetadata{
			ResourceID:   resourceID,
			PartitionKey: strings.ToLower(mcResourceID.String()),
		},
		Spec: kubeapplier.DeleteDesireSpec{
			ManagementCluster: mcResourceID,
			TargetItem:        ad.Spec.TargetItem,
		},
	}, nil
}

func buildReadDesiresFromSchedules(
	subscriptionID, resourceGroupName, clusterName string,
	mcResourceID *azcorearm.ResourceID,
	schedules []*velerov1.Schedule,
) ([]*kubeapplier.ReadDesire, error) {
	desires := make([]*kubeapplier.ReadDesire, 0, len(schedules))
	for _, schedule := range schedules {
		desireName := backupApplyDesireName(schedule.Name)
		resourceIDStr := kubeapplier.ToClusterScopedReadDesireResourceIDString(
			subscriptionID, resourceGroupName, clusterName, desireName,
		)
		resourceID, err := azcorearm.ParseResourceID(resourceIDStr)
		if err != nil {
			return nil, fmt.Errorf("failed to parse ReadDesire resource ID for schedule %s: %w", schedule.Name, err)
		}

		desires = append(desires, &kubeapplier.ReadDesire{
			CosmosMetadata: api.CosmosMetadata{ResourceID: resourceID, PartitionKey: strings.ToLower(mcResourceID.String())},
			Spec: kubeapplier.ReadDesireSpec{
				ManagementCluster: mcResourceID,
				TargetItem: kubeapplier.ResourceReference{
					Group:     veleroScheduleGroup,
					Version:   veleroScheduleVersion,
					Resource:  veleroScheduleResource,
					Namespace: veleroNamespace,
					Name:      schedule.Name,
				},
			},
		})
	}
	return desires, nil
}
