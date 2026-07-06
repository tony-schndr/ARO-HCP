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
package recoverycontroller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	azcorearm "github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"

	"github.com/Azure/ARO-HCP/backend/pkg/controllers/controllerutils"
	"github.com/Azure/ARO-HCP/backend/pkg/informers"
	"github.com/Azure/ARO-HCP/backend/pkg/listers"
	hcprecoveryv1alpha1 "github.com/Azure/ARO-HCP/hcp-recovery/pkg/apis/hcprecovery/v1alpha1"
	"github.com/Azure/ARO-HCP/internal/api"
	"github.com/Azure/ARO-HCP/internal/api/kubeapplier"
	"github.com/Azure/ARO-HCP/internal/backup"
	controllerutil "github.com/Azure/ARO-HCP/internal/controllerutils"
	"github.com/Azure/ARO-HCP/internal/database"
	"github.com/Azure/ARO-HCP/internal/utils"
)

type recoverySyncState struct {
	key                            controllerutils.HCPClusterKey
	spc                            *api.ServiceProviderCluster
	clusterID                      string
	mcResourceID                   *azcorearm.ResourceID
	spcCrud                        database.ResourceCRUD[api.ServiceProviderCluster, *api.ServiceProviderCluster]
	rdCrud                         database.ResourceCRUD[kubeapplier.ReadDesire, *kubeapplier.ReadDesire]
	adCrud                         database.ResourceCRUD[kubeapplier.ApplyDesire, *kubeapplier.ApplyDesire]
	recoveryRequestToProcess       *api.RecoveryRequest
	recoveryRequestStatusToProcess *api.RecoveryStatus
}

type recoverySyncer struct {
	cooldownChecker controllerutil.CooldownChecker

	cosmosClient database.ResourcesDBClient

	kubeApplierDBClients database.KubeApplierDBClients
}

var _ controllerutils.ClusterSyncer = (*recoverySyncer)(nil)

func NewRecoveryController(
	activeOperationLister listers.ActiveOperationLister,
	cosmosClient database.ResourcesDBClient,
	kubeApplierDBClients database.KubeApplierDBClients,
	informers informers.BackendInformers,
) controllerutils.Controller {

	syncer := &recoverySyncer{
		cooldownChecker:      controllerutils.DefaultActiveOperationPrioritizingCooldown(activeOperationLister),
		cosmosClient:         cosmosClient,
		kubeApplierDBClients: kubeApplierDBClients,
	}

	controller := controllerutils.NewClusterWatchingController(
		"Recovery",
		cosmosClient,
		informers,
		nil,
		30*time.Second,
		syncer,
	)

	return controller
}

func (c *recoverySyncer) SyncOnce(ctx context.Context, key controllerutils.HCPClusterKey) error {
	existingCluster, err := c.cosmosClient.HCPClusters(key.SubscriptionID, key.ResourceGroupName).Get(ctx, key.HCPClusterName)
	if database.IsNotFoundError(err) {
		return nil
	}
	if err != nil {
		return utils.TrackError(fmt.Errorf("failed to get Cluster: %w", err))
	}

	spc, err := database.GetOrCreateServiceProviderCluster(ctx, c.cosmosClient, key.GetResourceID())
	if err != nil {
		return utils.TrackError(fmt.Errorf("failed to get or create ServiceProviderCluster: %w", err))
	}

	if len(spc.Spec.RecoveryRequests) == 0 {
		return nil
	}

	mcResourceID := spc.Status.ManagementClusterResourceID
	if mcResourceID == nil {
		return nil
	}

	kaClient := c.kubeApplierDBClients.For(ctx, mcResourceID)
	if kaClient == nil {
		return nil
	}
	adCrud, err := kaClient.ApplyDesiresForCluster(key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName)
	if err != nil {
		return utils.TrackError(fmt.Errorf("failed to get apply desires for cluster: %w", err))
	}
	rdCrud, err := kaClient.ReadDesiresForCluster(key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName)
	if err != nil {
		return utils.TrackError(fmt.Errorf("failed to get read desires for cluster: %w", err))
	}

	clusterID := existingCluster.ServiceProviderProperties.ClusterServiceID.ID()

	recoveryRequestToProcess, recoveryRequestStatusToProcess, err := findActiveRecovery(spc)
	if err != nil {
		return err
	}

	if recoveryRequestToProcess == nil {
		return nil
	}

	state := recoverySyncState{
		key:                            key,
		spc:                            spc,
		clusterID:                      clusterID,
		mcResourceID:                   mcResourceID,
		rdCrud:                         rdCrud,
		adCrud:                         adCrud,
		recoveryRequestToProcess:       recoveryRequestToProcess,
		recoveryRequestStatusToProcess: recoveryRequestStatusToProcess,
	}

	err = c.process(ctx, state)
	if err != nil {
		return err
	}

	return nil
}

func findActiveRecovery(spc *api.ServiceProviderCluster) (*api.RecoveryRequest, *api.RecoveryStatus, error) {
	if spc.Status.Recoveries == nil {
		spc.Status.Recoveries = make([]api.RecoveryStatus, 0)
	}

	var activeCount int
	var activeRequest *api.RecoveryRequest
	var activeStatus *api.RecoveryStatus

	for i, request := range spc.Spec.RecoveryRequests {
		matchFound := false
		for j, status := range spc.Status.Recoveries {
			if request.RecoveryId != status.RecoveryId {
				continue
			}
			matchFound = true
			if !isTerminal(status.State) {
				activeCount++
				activeRequest = &spc.Spec.RecoveryRequests[i]
				activeStatus = &spc.Status.Recoveries[j]
			}
		}

		if !matchFound {
			spc.Status.Recoveries = append(spc.Status.Recoveries, api.RecoveryStatus{
				RecoveryId: request.RecoveryId,
				State:      api.RecoveryStatePending,
			})
			activeCount++
			activeRequest = &spc.Spec.RecoveryRequests[i]
			activeStatus = &spc.Status.Recoveries[len(spc.Status.Recoveries)-1]
		}
	}

	if activeCount > 1 {
		return nil, nil, fmt.Errorf("found %d active recoveries, expected at most 1", activeCount)
	}

	return activeRequest, activeStatus, nil
}

func (c *recoverySyncer) updateSpcStatus(ctx context.Context, state recoverySyncState) error {
	return nil
}

func (c *recoverySyncer) process(ctx context.Context, state recoverySyncState) error {
	if state.recoveryRequestStatusToProcess.CompletedAt != nil {
		// Request completed, no further processing needed
		return nil
	}
	if state.recoveryRequestStatusToProcess.StartedAt == nil {
		state.recoveryRequestStatusToProcess.StartedAt = &metav1.Time{Time: time.Now()}
	}
	if state.spc.Spec.BackupState != api.BackupScheduleStatePaused {
		state.spc.Spec.BackupState = api.BackupScheduleStatePaused
		return nil
	}
	// Fetch Schedule Read Desires for cluster and make sure pause was applied
	schedules, err := fetchSchedules(ctx, state)
	if err != nil {
		return fmt.Errorf("failed to fetch schedules: %w", err)
	}
	if !areAllSchedulesPaused(schedules) {
		return fmt.Errorf("waiting for all schedules to be paused")
	}

	// Schedules are paused, check for active hcprecovery
	_, err = state.adCrud.Get(ctx, backup.RecoveryDesireNamePrefix+state.recoveryRequestToProcess.RecoveryId)
	if err != nil {
		if database.IsNotFoundError(err) {
			err = createRecoveryApplyDesire(ctx, state)
			if err != nil {
				return fmt.Errorf("failed to create recovery apply desire: %w", err)
			}
			return nil
		}
		return fmt.Errorf("failed to fetch hcprecovery: %w", err)
	}

	// Fetch hcpRecoveryReadDesire and check status, if its terminal set recoveryRequestStatusToProcess completedAt
	_, err = state.rdCrud.Get(ctx, backup.RecoveryDesireNamePrefix+state.recoveryRequestToProcess.RecoveryId)
	if err != nil {
		if database.IsNotFoundError(err) {
			err = createRecoveryReadDesire(ctx, state)
			if err != nil {
				return fmt.Errorf("failed to create recovery read desire: %w", err)
			}
			return nil
		}
		return fmt.Errorf("failed to fetch recovery read desire: %w", err)
	}

	return nil
}

func createRecoveryReadDesire(ctx context.Context, state recoverySyncState) error {
	desireName := backup.RecoveryDesireNamePrefix + state.recoveryRequestToProcess.RecoveryId
	resourceIDStr := kubeapplier.ToClusterScopedReadDesireResourceIDString(
		state.key.SubscriptionID,
		state.key.ResourceGroupName,
		state.key.HCPClusterName,
		desireName,
	)
	resourceID, err := azcorearm.ParseResourceID(resourceIDStr)
	if err != nil {
		return fmt.Errorf("failed to parse read desire resource ID: %w", err)
	}

	readDesire := &kubeapplier.ReadDesire{
		CosmosMetadata: api.CosmosMetadata{
			ResourceID:   resourceID,
			PartitionKey: strings.ToLower(state.mcResourceID.String()),
		},
		Spec: kubeapplier.ReadDesireSpec{
			ManagementCluster: state.mcResourceID,
			TargetItem: kubeapplier.ResourceReference{
				Group:     "hcprecovery.aro-hcp.azure.com",
				Version:   "v1alpha1",
				Resource:  "hcprecoveries",
				Namespace: "hcp-recovery",
				Name:      state.recoveryRequestToProcess.RecoveryId,
			},
		},
	}

	if _, err := state.rdCrud.Create(ctx, readDesire, nil); err != nil {
		return fmt.Errorf("failed to create recovery read desire: %w", err)
	}
	return nil
}


func createRecoveryApplyDesire(ctx context.Context, state recoverySyncState) error {
	recoveryCr := hcprecoveryv1alpha1.HCPRecovery{
		ObjectMeta: metav1.ObjectMeta{
			Name: state.recoveryRequestToProcess.RecoveryId,
		},
		Spec: hcprecoveryv1alpha1.HCPRecoverySpec{
			ClusterId: state.clusterID,
			BackupId:  state.recoveryRequestToProcess.BackupId,
		},
		Status: hcprecoveryv1alpha1.HCPRecoveryStatus{},
	}
	desireName := backup.RecoveryDesireNamePrefix + state.recoveryRequestToProcess.RecoveryId
	resourceIDStr := kubeapplier.ToClusterScopedApplyDesireResourceIDString(
		state.key.SubscriptionID,
		state.key.ResourceGroupName,
		state.key.HCPClusterName,
		desireName,
	)
	resourceID, err := azcorearm.ParseResourceID(resourceIDStr)
	if err != nil {
		return fmt.Errorf("failed to parse apply desire resource ID: %w", err)
	}

	raw, err := json.Marshal(recoveryCr)
	if err != nil {
		return fmt.Errorf("failed to marshal HCPRecovery: %w", err)
	}

	applyDesire := &kubeapplier.ApplyDesire{
		CosmosMetadata: api.CosmosMetadata{
			ResourceID:   resourceID,
			PartitionKey: strings.ToLower(state.mcResourceID.String()),
		},
		Spec: kubeapplier.ApplyDesireSpec{
			ManagementCluster: state.mcResourceID,
			Type:              kubeapplier.ApplyDesireTypeServerSideApply,
			TargetItem: kubeapplier.ResourceReference{
				Group:     "hcprecovery.aro-hcp.azure.com",
				Version:   "v1alpha1",
				Resource:  "hcprecoveries",
				Namespace: "hcp-recovery",
				Name:      state.recoveryRequestToProcess.RecoveryId,
			},
			ServerSideApply: &kubeapplier.ServerSideApplyConfig{
				KubeContent: &runtime.RawExtension{Raw: raw},
			},
		},
	}

	if _, err := state.adCrud.Create(ctx, applyDesire, nil); err != nil {
		return fmt.Errorf("failed to create recovery apply desire: %w", err)
	}
	return nil
}

func fetchSchedules(ctx context.Context, state recoverySyncState) ([]*kubeapplier.ReadDesire, error) {
	iterator, err := state.rdCrud.List(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to list ReadDesires: %w", err)
	}
	var scheduleDesires []*kubeapplier.ReadDesire

	for _, rd := range iterator.Items(ctx) {
		if strings.HasPrefix(rd.ResourceID.Name, backup.BackupScheduleDesireNamePrefix) {
			scheduleDesires = append(scheduleDesires, rd)
		}
	}

	if err := iterator.GetError(); err != nil {
		return nil, fmt.Errorf("failed to iterate ReadDesires: %w", err)
	}

	return scheduleDesires, nil
}

func areAllSchedulesPaused(schedulesReadDesires []*kubeapplier.ReadDesire) bool {
	for _, sd := range schedulesReadDesires {
		var schedule velerov1.Schedule
		if err := json.Unmarshal(sd.Status.KubeContent.Raw, &schedule); err == nil {
			if schedule.Spec.Paused == false {
				return false
			}
		}
	}
	return true
}

func isTerminal(restoreState api.RecoveryState) bool {
	return restoreState == api.RecoveryStateCompleted || restoreState == api.RecoveryStateFailed
}
func (c *recoverySyncer) CooldownChecker() controllerutil.CooldownChecker {
	return c.cooldownChecker
}
