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

package recovery

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilsclock "k8s.io/utils/clock"

	azcorearm "github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"

	"github.com/Azure/ARO-HCP/backend/pkg/controllers/cluster/backups"
	"github.com/Azure/ARO-HCP/backend/pkg/kubeapplierhelpers"
	"github.com/Azure/ARO-HCP/backend/pkg/utils/controllerutils"
	hcprecoveryv1alpha1 "github.com/Azure/ARO-HCP/hcp-recovery/pkg/apis/hcprecovery/v1alpha1"
	"github.com/Azure/ARO-HCP/internal/api/coreapi"
	"github.com/Azure/ARO-HCP/internal/api/kubeapplierapi"
	"github.com/Azure/ARO-HCP/internal/apihelpers/kubeapplierapihelpers"
	"github.com/Azure/ARO-HCP/internal/backup"
	"github.com/Azure/ARO-HCP/internal/database/cosmosstorage/corecosmosstorage"
	"github.com/Azure/ARO-HCP/internal/database/cosmosstorage/cosmosstorageutils"
	"github.com/Azure/ARO-HCP/internal/database/cosmosstorage/kubeappliercosmosstorage"
	"github.com/Azure/ARO-HCP/internal/database/informers/coreinformers"
	"github.com/Azure/ARO-HCP/internal/database/listers/corelisters"
	"github.com/Azure/ARO-HCP/internal/database/listers/kubeapplierlisters"
	unionkubeapplier "github.com/Azure/ARO-HCP/internal/database/unioninformers/kubeapplier"
	"github.com/Azure/ARO-HCP/internal/utils"
)

const RecoveryControllerName = "Recovery"

type recoverySyncer struct {
	cosmosClient                 corecosmosstorage.ResourcesDBClient
	kubeApplierDBClients         kubeappliercosmosstorage.KubeApplierDBClients
	clusterLister                corelisters.ClusterLister
	serviceProviderClusterLister corelisters.ServiceProviderClusterLister
	applyDesireLister            kubeapplierlisters.ApplyDesireLister
	readDesireLister             kubeapplierlisters.ReadDesireLister
	backupConfig                 *backups.BackupConfig
	clock                        utilsclock.PassiveClock
}

func NewRecoveryController(
	cosmosClient corecosmosstorage.ResourcesDBClient,
	kubeApplierDBClients kubeappliercosmosstorage.KubeApplierDBClients,
	informers coreinformers.BackendInformers,
	kubeApplierInformers *unionkubeapplier.UnionKubeApplierInformers,
	backupConfig *backups.BackupConfig,
	clock utilsclock.PassiveClock,
) controllerutils.Controller {
	_, clusterLister := informers.Clusters()
	_, serviceProviderClusterLister := informers.ServiceProviderClusters()
	_, applyDesireLister := kubeApplierInformers.ApplyDesires()
	_, readDesireLister := kubeApplierInformers.ReadDesires()
	syncer := &recoverySyncer{
		cosmosClient: cosmosClient, kubeApplierDBClients: kubeApplierDBClients,
		clusterLister: clusterLister, serviceProviderClusterLister: serviceProviderClusterLister,
		applyDesireLister: applyDesireLister, readDesireLister: readDesireLister,
		backupConfig: backupConfig, clock: clock,
	}
	controller := controllerutils.NewClusterWatchingController(
		RecoveryControllerName, cosmosClient, informers, kubeApplierInformers, 30*time.Second, syncer,
	)
	controller.AddInterestingResource(coreapi.ClusterResourceType, "*")
	controller.AddInterestingResource(coreapi.ServiceProviderClusterResourceType, coreapi.ServiceProviderClusterResourceName)
	controller.AddInterestingResource(kubeapplierapi.ClusterScopedApplyDesireResourceType, "*")
	controller.AddInterestingResource(kubeapplierapi.ClusterScopedReadDesireResourceType, "*")
	return controller
}

func needsWork(cluster *coreapi.Cluster, spc *coreapi.ServiceProviderCluster) bool {
	return cluster.ResourceID != nil &&
		cluster.ServiceProviderProperties.DeletionTimestamp == nil &&
		spc.Status.ManagementClusterResourceID != nil &&
		spc.Status.HostedClusterNamespace != "" &&
		spc.Status.ControlPlaneNamespace != "" &&
		cluster.CustomerProperties.DNS.BaseDomainPrefix != "" &&
		len(spc.Spec.RecoveryRequests) > 0
}

func (c *recoverySyncer) SyncOnce(ctx context.Context, key controllerutils.HCPClusterKey) error {
	cluster, err := c.clusterLister.Get(ctx, key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName)
	if cosmosstorageutils.IsNotFoundError(err) {
		return nil
	}
	if err != nil {
		return utils.TrackError(fmt.Errorf("get cluster: %w", err))
	}
	existing, err := c.serviceProviderClusterLister.Get(ctx, key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName)
	if cosmosstorageutils.IsNotFoundError(err) {
		return nil
	}
	if err != nil {
		return utils.TrackError(fmt.Errorf("get ServiceProviderCluster: %w", err))
	}
	if !needsWork(cluster, existing) {
		return nil
	}
	spc := existing.DeepCopy()
	request, status, err := findActiveRecovery(spc)
	if err != nil {
		return utils.TrackError(err)
	}
	if request == nil {
		return nil
	}
	client, err := c.kubeApplierDBClients.For(ctx, spc.Status.ManagementClusterResourceID)
	if err != nil {
		return utils.TrackError(err)
	}
	if client == nil {
		return nil // The management-cluster registry will retrigger when available.
	}
	applyCRUD, err := client.ApplyDesiresForCluster(key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName)
	if err != nil {
		return utils.TrackError(err)
	}
	readCRUD, err := client.ReadDesiresForCluster(key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName)
	if err != nil {
		return utils.TrackError(err)
	}
	if status.StartedAt == nil {
		started := metav1.NewTime(c.clock.Now())
		status.StartedAt = &started
		return c.updateServiceProviderCluster(ctx, key, spc, existing)
	}
	applyDesire, readDesire, err := buildRecoveryDesires(key, cluster, spc, request)
	if err != nil {
		return utils.TrackError(err)
	}
	observed, err := c.readDesireLister.GetByResourceID(ctx, readDesire.ResourceID.String())
	if err != nil && !cosmosstorageutils.IsNotFoundError(err) {
		return utils.TrackError(fmt.Errorf("get recovery ReadDesire: %w", err))
	}
	if observed != nil && observed.Status.KubeContent != nil && len(observed.Status.KubeContent.Raw) > 0 {
		var recovery hcprecoveryv1alpha1.HCPRecovery
		if err := json.Unmarshal(observed.Status.KubeContent.Raw, &recovery); err != nil {
			return utils.TrackError(fmt.Errorf("decode observed HCPRecovery: %w", err))
		}
		switch recovery.Status.Phase {
		case hcprecoveryv1alpha1.RestoreStateCompleted:
			status.State = coreapi.RecoveryStateCompleted
			status.CompletedAt = recovery.Status.CompletedAt
			spc.Spec.BackupScheduleState = coreapi.BackupScheduleStateEnabled
			return c.updateServiceProviderCluster(ctx, key, spc, existing)
		case hcprecoveryv1alpha1.RestoreStateFailed:
			status.State = coreapi.RecoveryStateFailed
			status.CompletedAt = recovery.Status.CompletedAt
			spc.Spec.BackupScheduleState = coreapi.BackupScheduleStateDisabled
			return c.updateServiceProviderCluster(ctx, key, spc, existing)
		default:
			status.State = coreapi.RecoveryStateInProgress
		}
	}

	if spc.Spec.BackupScheduleState != coreapi.BackupScheduleStateDisabled {
		spc.Spec.BackupScheduleState = coreapi.BackupScheduleStateDisabled
		return c.updateServiceProviderCluster(ctx, key, spc, existing)
	}
	paused, err := c.schedulesPaused(ctx, key, cluster, spc)
	if err != nil {
		return utils.TrackError(err)
	}
	if !paused {
		return utils.TrackError(fmt.Errorf("waiting for backup schedules to be observed paused"))
	}
	if err := kubeapplierhelpers.EnsureReadDesire(ctx, readCRUD, c.readDesireLister, readDesire); err != nil {
		return err
	}
	if err := kubeapplierhelpers.EnsureApplyDesire(ctx, applyCRUD, c.applyDesireLister, applyDesire); err != nil {
		return err
	}
	if status.State == coreapi.RecoveryStatePending {
		status.State = coreapi.RecoveryStateRecoveryCRCreated
	}
	return c.updateServiceProviderCluster(ctx, key, spc, existing)
}

func (c *recoverySyncer) schedulesPaused(ctx context.Context, key controllerutils.HCPClusterKey, cluster *coreapi.Cluster, spc *coreapi.ServiceProviderCluster) (bool, error) {
	if c.backupConfig == nil {
		return false, fmt.Errorf("backup configuration is required to verify schedule pause")
	}
	for _, config := range c.backupConfig.Schedules() {
		schedule := backups.NewScheduledBackup(cluster.ResourceID.String(), "", spc.Status.HostedClusterNamespace, spc.Status.ControlPlaneNamespace, config, true)
		resourceID := kubeapplierapihelpers.ToClusterScopedReadDesireResourceIDString(key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName, backup.BackupScheduleDesireNamePrefix+config.Name)
		desire, err := c.readDesireLister.GetByResourceID(ctx, resourceID)
		if cosmosstorageutils.IsNotFoundError(err) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("get schedule ReadDesire: %w", err)
		}
		if desire == nil || desire.Status.KubeContent == nil || !meta.IsStatusConditionTrue(desire.Status.Conditions, kubeapplierapi.ConditionTypeSuccessful) {
			return false, nil
		}
		var observed velerov1.Schedule
		if err := json.Unmarshal(desire.Status.KubeContent.Raw, &observed); err != nil {
			return false, fmt.Errorf("decode schedule %s: %w", schedule.Name, err)
		}
		if observed.Name != schedule.Name || observed.Namespace != backup.VeleroNamespace || !observed.Spec.Paused {
			return false, nil
		}
	}
	return true, nil
}

func findActiveRecovery(spc *coreapi.ServiceProviderCluster) (*coreapi.RecoveryRequest, *coreapi.RecoveryStatus, error) {
	requestIndex, statusIndex := -1, -1
	for i, request := range spc.Spec.RecoveryRequests {
		foundStatus := -1
		for j, status := range spc.Status.Recoveries {
			if status.RecoveryID == request.RecoveryID {
				foundStatus = j
				break
			}
		}
		if foundStatus >= 0 && spc.Status.Recoveries[foundStatus].State.IsTerminal() {
			continue
		}
		if requestIndex >= 0 {
			return nil, nil, fmt.Errorf("found multiple active recoveries, expected at most 1")
		}
		requestIndex, statusIndex = i, foundStatus
	}
	if requestIndex < 0 {
		return nil, nil, nil
	}
	request := &spc.Spec.RecoveryRequests[requestIndex]
	if statusIndex < 0 {
		spc.Status.Recoveries = append(spc.Status.Recoveries, coreapi.RecoveryStatus{RecoveryID: request.RecoveryID, State: coreapi.RecoveryStatePending})
		statusIndex = len(spc.Status.Recoveries) - 1
	}
	return request, &spc.Status.Recoveries[statusIndex], nil
}

func buildRecoveryDesires(key controllerutils.HCPClusterKey, cluster *coreapi.Cluster, spc *coreapi.ServiceProviderCluster, request *coreapi.RecoveryRequest) (*kubeapplierapi.ApplyDesire, *kubeapplierapi.ReadDesire, error) {
	name := backup.RecoveryDesireName(request.RecoveryID)
	applyID, err := azcorearm.ParseResourceID(kubeapplierapihelpers.ToClusterScopedApplyDesireResourceIDString(key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName, name))
	if err != nil {
		return nil, nil, err
	}
	readID, err := azcorearm.ParseResourceID(kubeapplierapihelpers.ToClusterScopedReadDesireResourceIDString(key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName, name))
	if err != nil {
		return nil, nil, err
	}
	recovery := hcprecoveryv1alpha1.HCPRecovery{
		TypeMeta:   metav1.TypeMeta{APIVersion: hcprecoveryv1alpha1.SchemeGroupVersion.String(), Kind: "HCPRecovery"},
		ObjectMeta: metav1.ObjectMeta{Name: request.RecoveryID, Namespace: hcprecoveryv1alpha1.HCPRecoveryNamespace},
		Spec: hcprecoveryv1alpha1.HCPRecoverySpec{
			HcpClusterAzureResourceID: cluster.ResourceID.String(),
			HostedClusterName:         cluster.CustomerProperties.DNS.BaseDomainPrefix,
			HostedClusterNamespace:    spc.Status.HostedClusterNamespace,
			ControlPlaneNamespace:     spc.Status.ControlPlaneNamespace,
			VeleroBackupName:          request.VeleroBackupName,
		},
	}
	raw, err := json.Marshal(recovery)
	if err != nil {
		return nil, nil, err
	}
	target := kubeapplierapi.ResourceReference{
		Group: hcprecoveryv1alpha1.SchemeGroupVersion.Group, Version: hcprecoveryv1alpha1.SchemeGroupVersion.Version,
		Resource: hcprecoveryv1alpha1.HCPRecoveryResource, Namespace: recovery.Namespace, Name: recovery.Name,
	}
	tags := map[string]string{kubeapplierapi.TagControllerName: RecoveryControllerName}
	partition := strings.ToLower(spc.Status.ManagementClusterResourceID.String())
	return &kubeapplierapi.ApplyDesire{
			CosmosMetadata: coreapi.CosmosMetadata{ResourceID: applyID, PartitionKey: partition},
			Spec: kubeapplierapi.ApplyDesireSpec{
				ManagementCluster: spc.Status.ManagementClusterResourceID, TargetItem: target,
				Type:            kubeapplierapi.ApplyDesireTypeServerSideApply,
				ServerSideApply: &kubeapplierapi.ServerSideApplyConfig{KubeContent: &runtime.RawExtension{Raw: raw}},
			}, Tags: tags,
		}, &kubeapplierapi.ReadDesire{
			CosmosMetadata: coreapi.CosmosMetadata{ResourceID: readID, PartitionKey: partition},
			Spec:           kubeapplierapi.ReadDesireSpec{ManagementCluster: spc.Status.ManagementClusterResourceID, TargetItem: target},
			Tags:           tags,
		}, nil
}

func (c *recoverySyncer) updateServiceProviderCluster(ctx context.Context, key controllerutils.HCPClusterKey, spc, existing *coreapi.ServiceProviderCluster) error {
	if equality.Semantic.DeepEqual(spc, existing) {
		return nil
	}
	_, err := c.cosmosClient.ServiceProviderClusters(key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName).Replace(ctx, spc, nil)
	if cosmosstorageutils.IsPreconditionFailedError(err) {
		return nil
	}
	return utils.TrackError(err)
}
