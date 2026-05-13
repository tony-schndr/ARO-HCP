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
	"errors"
	"fmt"
	"strings"
	"time"

	workv1 "open-cluster-management.io/api/work/v1"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	arohcpv1alpha1 "github.com/openshift-online/ocm-sdk-go/arohcp/v1alpha1"

	"github.com/Azure/ARO-HCP/backend/pkg/controllers/controllerutils"
	controllerutil "github.com/Azure/ARO-HCP/internal/controllerutils"
	"github.com/Azure/ARO-HCP/backend/pkg/informers"
	"github.com/Azure/ARO-HCP/backend/pkg/listers"
	"github.com/Azure/ARO-HCP/backend/pkg/maestro"
	"github.com/Azure/ARO-HCP/internal/api"
	"github.com/Azure/ARO-HCP/internal/database"
	"github.com/Azure/ARO-HCP/internal/ocm"
	"github.com/Azure/ARO-HCP/internal/utils"
)

type recoveryAction struct {
	patchManifestWorks []manifestWorkPatch
	createManifestWork *workv1.ManifestWork
	updateSPC          *api.ServiceProviderCluster
}

func (a *recoveryAction) validate() error {
	var set int
	if len(a.patchManifestWorks) > 0 {
		set++
	}
	if a.createManifestWork != nil {
		set++
	}
	if a.updateSPC != nil {
		set++
	}
	if set > 1 {
		return errors.New("programmer error: more than one action set")
	}
	return nil
}

type recoverySyncState struct {
	key                  controllerutils.HCPClusterKey
	spc                  *api.ServiceProviderCluster
	maestroClient        maestro.Client
	clusterID            string
	consumerName         string
	clusterManifestWorks []*workv1.ManifestWork
	backupID             string
}

type recoveryStep func(ctx context.Context, state *recoverySyncState) (bool, *recoveryAction, error)

type recoverySyncer struct {
	cooldownChecker controllerutil.CooldownChecker

	cosmosClient database.ResourcesDBClient

	clusterServiceClient ocm.ClusterServiceClientSpec

	maestroSourceEnvironmentIdentifier string

	maestroClientBuilder maestro.MaestroClientBuilder
}

var _ controllerutils.ClusterSyncer = (*recoverySyncer)(nil)

func NewRecoveryController(
	activeOperationLister listers.ActiveOperationLister,
	cosmosClient database.ResourcesDBClient,
	clusterServiceClient ocm.ClusterServiceClientSpec,
	informers informers.BackendInformers,
	maestroSourceEnvironmentIdentifier string,
	maestroClientBuilder maestro.MaestroClientBuilder,
) controllerutils.Controller {

	syncer := &recoverySyncer{
		cooldownChecker:                    controllerutils.DefaultActiveOperationPrioritizingCooldown(activeOperationLister),
		cosmosClient:                       cosmosClient,
		clusterServiceClient:               clusterServiceClient,
		maestroSourceEnvironmentIdentifier: maestroSourceEnvironmentIdentifier,
		maestroClientBuilder:               maestroClientBuilder,
	}

	controller := controllerutils.NewClusterWatchingController(
		"Recovery",
		cosmosClient,
		informers,
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

	switch spc.Status.RecoveryState {
	case "", api.RecoveryStateCompleted, api.RecoveryStateFailed:
		return nil
	}

	clusterProvisionShard, err := c.clusterServiceClient.GetClusterProvisionShard(ctx, *existingCluster.ServiceProviderProperties.ClusterServiceID)
	if err != nil {
		return utils.TrackError(fmt.Errorf("failed to get Cluster Provision Shard from Cluster Service: %w", err))
	}

	maestroClient, err := createMaestroClientFromProvisionShard(ctx, clusterProvisionShard, c.maestroSourceEnvironmentIdentifier, c.maestroClientBuilder)
	if err != nil {
		return utils.TrackError(fmt.Errorf("failed to create Maestro client: %w", err))
	}

	clusterID := existingCluster.ServiceProviderProperties.ClusterServiceID.ID()

	state := recoverySyncState{
		key:           key,
		spc:           spc,
		maestroClient: maestroClient,
		clusterID:     clusterID,
		consumerName:  clusterProvisionShard.MaestroConfig().ConsumerName(),
		backupID:      spc.Status.RecoveryBackupID,
	}

	action, err := c.processRecovery(ctx, &state)
	if err != nil {
		return err
	}

	if action == nil {
		return nil
	}

	if err := action.validate(); err != nil {
		return utils.TrackError(fmt.Errorf("invalid recovery action: %w", err))
	}

	switch {
	case len(action.patchManifestWorks) > 0:
		var patchErrors []error
		for _, patch := range action.patchManifestWorks {
			_, patchErr := maestroClient.Patch(ctx, patch.Name, types.MergePatchType, patch.Data, metav1.PatchOptions{})
			if patchErr != nil {
				patchErrors = append(patchErrors, fmt.Errorf("failed to patch ManifestWork %s: %w", patch.Name, patchErr))
			}
		}
		if len(patchErrors) > 0 {
			return utils.TrackError(errors.Join(patchErrors...))
		}

	case action.createManifestWork != nil:
		_, err = maestroClient.Create(ctx, action.createManifestWork, metav1.CreateOptions{})
		if err != nil && !k8serrors.IsAlreadyExists(err) {
			return utils.TrackError(fmt.Errorf("failed to create ManifestWork: %w", err))
		}

	case action.updateSPC != nil:
		spcCRUD := c.cosmosClient.ServiceProviderClusters(key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName)
		_, err = spcCRUD.Replace(ctx, action.updateSPC, nil)
		if err != nil {
			return utils.TrackError(fmt.Errorf("failed to update ServiceProviderCluster status: %w", err))
		}
	}

	return nil
}

func (c *recoverySyncer) processRecovery(ctx context.Context, state *recoverySyncState) (*recoveryAction, error) {
	for _, step := range []recoveryStep{
		c.ensureRecoveryStarted,
		c.discoverClusterManifestWorks,
		c.ensureManifestWorksReadOnly,
		c.confirmAllReadOnly,
		c.ensureRecoveryCRCreated,
		c.monitorRecoveryStatus,
		c.ensureManifestWorksRestored,
	} {
		done, action, err := step(ctx, state)
		if done {
			return action, err
		}
	}
	return nil, nil
}

// ensureRecoveryStarted transitions from Pending to ReadOnlyPending and pauses backups.
func (c *recoverySyncer) ensureRecoveryStarted(_ context.Context, state *recoverySyncState) (bool, *recoveryAction, error) {
	if state.spc.Status.RecoveryState != api.RecoveryStatePending {
		return false, nil, nil
	}

	now := metav1.Now()
	state.spc.Status.RecoveryState = api.RecoveryStateReadOnlyPending
	state.spc.Status.RecoveryStartedAt = &now
	state.spc.Status.BackupState = api.BackupScheduleStatePaused
	return true, &recoveryAction{updateSPC: state.spc}, nil
}

// discoverClusterManifestWorks lists all ManifestWorks for the cluster from Maestro.
func (c *recoverySyncer) discoverClusterManifestWorks(ctx context.Context, state *recoverySyncState) (bool, *recoveryAction, error) {
	var clusterMWs []*workv1.ManifestWork
	err := maestro.ForEachMaestroBundle(ctx, state.maestroClient, metav1.ListOptions{Limit: 100}, func(mw *workv1.ManifestWork) error {
		if strings.HasPrefix(mw.Name, state.clusterID) {
			clusterMWs = append(clusterMWs, mw)
		}
		return nil
	})
	if err != nil {
		return true, nil, utils.TrackError(fmt.Errorf("failed to list ManifestWorks: %w", err))
	}

	state.clusterManifestWorks = clusterMWs
	return false, nil, nil
}

// ensureManifestWorksReadOnly patches all non-ReadOnly ManifestWorks to ReadOnly.
func (c *recoverySyncer) ensureManifestWorksReadOnly(_ context.Context, state *recoverySyncState) (bool, *recoveryAction, error) {
	if state.spc.Status.RecoveryState != api.RecoveryStateReadOnlyPending {
		return false, nil, nil
	}

	var patches []manifestWorkPatch
	for _, mw := range state.clusterManifestWorks {
		if isAllReadOnly(mw) {
			continue
		}
		data, err := buildReadOnlyPatch(mw)
		if err != nil {
			return true, nil, utils.TrackError(fmt.Errorf("failed to build ReadOnly patch for %s: %w", mw.Name, err))
		}
		patches = append(patches, manifestWorkPatch{Name: mw.Name, Data: data})
	}

	if len(patches) == 0 {
		return false, nil, nil
	}

	return true, &recoveryAction{patchManifestWorks: patches}, nil
}

// confirmAllReadOnly verifies all ManifestWorks are ReadOnly and transitions state.
func (c *recoverySyncer) confirmAllReadOnly(ctx context.Context, state *recoverySyncState) (bool, *recoveryAction, error) {
	if state.spc.Status.RecoveryState != api.RecoveryStateReadOnlyPending {
		return false, nil, nil
	}

	// Re-read MWs from Maestro to confirm the patches took effect
	var clusterMWs []*workv1.ManifestWork
	err := maestro.ForEachMaestroBundle(ctx, state.maestroClient, metav1.ListOptions{Limit: 100}, func(mw *workv1.ManifestWork) error {
		if strings.HasPrefix(mw.Name, state.clusterID) {
			clusterMWs = append(clusterMWs, mw)
		}
		return nil
	})
	if err != nil {
		return true, nil, utils.TrackError(fmt.Errorf("failed to list ManifestWorks for confirmation: %w", err))
	}

	for _, mw := range clusterMWs {
		if !isAllReadOnly(mw) {
			return true, nil, nil
		}
	}

	state.spc.Status.RecoveryState = api.RecoveryStateRecoveryCRCreated
	return true, &recoveryAction{updateSPC: state.spc}, nil
}

// ensureRecoveryCRCreated creates the HCPRecovery ManifestWork via Maestro.
// This step first records the MW name in SPC (one sync), then creates the MW (next sync),
// then transitions to Monitoring (following sync) — each sync performs at most one mutation.
func (c *recoverySyncer) ensureRecoveryCRCreated(ctx context.Context, state *recoverySyncState) (bool, *recoveryAction, error) {
	if state.spc.Status.RecoveryState != api.RecoveryStateRecoveryCRCreated {
		return false, nil, nil
	}

	manifestWorkName := fmt.Sprintf("%s-recovery", state.clusterID)

	// Step A: Record the MW name in SPC before creating it
	if state.spc.Status.RecoveryManifestWorkName == "" {
		state.spc.Status.RecoveryManifestWorkName = manifestWorkName
		return true, &recoveryAction{updateSPC: state.spc}, nil
	}

	// Step B: Check if MW already exists
	_, err := state.maestroClient.Get(ctx, state.spc.Status.RecoveryManifestWorkName, metav1.GetOptions{})
	if err == nil {
		// MW exists, transition to monitoring
		state.spc.Status.RecoveryState = api.RecoveryStateMonitoring
		return true, &recoveryAction{updateSPC: state.spc}, nil
	}
	if !k8serrors.IsNotFound(err) {
		return true, nil, utils.TrackError(fmt.Errorf("failed to get recovery ManifestWork: %w", err))
	}

	// Step C: Create the MW
	maestroBundleNamespacedName := types.NamespacedName{
		Name:      state.spc.Status.RecoveryManifestWorkName,
		Namespace: state.consumerName,
	}
	desiredMW, err := buildRecoveryManifestWork(maestroBundleNamespacedName, state.clusterID, state.backupID)
	if err != nil {
		return true, nil, utils.TrackError(fmt.Errorf("failed to build recovery ManifestWork: %w", err))
	}

	return true, &recoveryAction{createManifestWork: desiredMW}, nil
}

// monitorRecoveryStatus reads HCPRecovery feedback and updates SPC.
func (c *recoverySyncer) monitorRecoveryStatus(ctx context.Context, state *recoverySyncState) (bool, *recoveryAction, error) {
	if state.spc.Status.RecoveryState != api.RecoveryStateMonitoring {
		return false, nil, nil
	}

	if state.spc.Status.RecoveryManifestWorkName == "" {
		return false, nil, nil
	}

	mfw, err := state.maestroClient.Get(ctx, state.spc.Status.RecoveryManifestWorkName, metav1.GetOptions{})
	if err != nil {
		return true, nil, utils.TrackError(fmt.Errorf("failed to get recovery ManifestWork for feedback: %w", err))
	}

	phase, condType, condMsg := extractRecoveryFeedback(mfw)

	needsUpdate := false
	if phase != "" && phase != state.spc.Status.RecoveryPhase {
		state.spc.Status.RecoveryPhase = phase
		needsUpdate = true
	}
	if condType != "" && condType != state.spc.Status.RecoveryLastConditionType {
		state.spc.Status.RecoveryLastConditionType = condType
		state.spc.Status.RecoveryLastConditionMessage = condMsg
		needsUpdate = true
	}

	switch phase {
	case "Completed":
		state.spc.Status.RecoveryState = api.RecoveryStateRestoring
		now := metav1.Now()
		state.spc.Status.RecoveryCompletedAt = &now
		needsUpdate = true
	case "Failed":
		state.spc.Status.RecoveryState = api.RecoveryStateFailed
		now := metav1.Now()
		state.spc.Status.RecoveryCompletedAt = &now
		needsUpdate = true
	}

	if !needsUpdate {
		return true, nil, nil
	}

	return true, &recoveryAction{updateSPC: state.spc}, nil
}

// ensureManifestWorksRestored patches all cluster ManifestWorks back to ServerSideApply after recovery.
func (c *recoverySyncer) ensureManifestWorksRestored(ctx context.Context, state *recoverySyncState) (bool, *recoveryAction, error) {
	if state.spc.Status.RecoveryState != api.RecoveryStateRestoring {
		return false, nil, nil
	}

	// Re-discover MWs since earlier state may be stale
	var clusterMWs []*workv1.ManifestWork
	err := maestro.ForEachMaestroBundle(ctx, state.maestroClient, metav1.ListOptions{Limit: 100}, func(mw *workv1.ManifestWork) error {
		if strings.HasPrefix(mw.Name, state.clusterID) {
			// Skip the recovery ManifestWork itself — it should stay ServerSideApply
			if mw.Labels[recoveryManagedByK8sLabelKey] == recoveryManagedByK8sLabelValue {
				return nil
			}
			clusterMWs = append(clusterMWs, mw)
		}
		return nil
	})
	if err != nil {
		return true, nil, utils.TrackError(fmt.Errorf("failed to list ManifestWorks for restore: %w", err))
	}

	var patches []manifestWorkPatch
	for _, mw := range clusterMWs {
		if !isAllReadOnly(mw) {
			continue
		}
		data, err := buildServerSideApplyPatch(mw)
		if err != nil {
			return true, nil, utils.TrackError(fmt.Errorf("failed to build ServerSideApply patch for %s: %w", mw.Name, err))
		}
		patches = append(patches, manifestWorkPatch{Name: mw.Name, Data: data})
	}

	if len(patches) > 0 {
		return true, &recoveryAction{patchManifestWorks: patches}, nil
	}

	state.spc.Status.RecoveryState = api.RecoveryStateCompleted
	state.spc.Status.BackupState = api.BackupScheduleStateActive
	return true, &recoveryAction{updateSPC: state.spc}, nil
}

func (c *recoverySyncer) CooldownChecker() controllerutil.CooldownChecker {
	return c.cooldownChecker
}

func createMaestroClientFromProvisionShard(
	ctx context.Context,
	provisionShard *arohcpv1alpha1.ProvisionShard,
	maestroSourceEnvironmentIdentifier string,
	maestroClientBuilder maestro.MaestroClientBuilder,
) (maestro.Client, error) {
	provisionShardMaestroConsumerName := provisionShard.MaestroConfig().ConsumerName()
	provisionShardMaestroRESTAPIEndpoint := provisionShard.MaestroConfig().RestApiConfig().Url()
	provisionShardMaestroGRPCAPIEndpoint := provisionShard.MaestroConfig().GrpcApiConfig().Url()
	maestroSourceID := maestro.GenerateMaestroSourceID(maestroSourceEnvironmentIdentifier, provisionShard.ID())

	return maestroClientBuilder.NewClient(ctx, provisionShardMaestroRESTAPIEndpoint, provisionShardMaestroGRPCAPIEndpoint, provisionShardMaestroConsumerName, maestroSourceID)
}
