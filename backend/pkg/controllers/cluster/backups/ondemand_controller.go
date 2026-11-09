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

package backups

import (
	"context"
	"fmt"
	"time"

	"github.com/Azure/ARO-HCP/backend/pkg/kubeapplierhelpers"
	"github.com/Azure/ARO-HCP/backend/pkg/utils/controllerutils"
	"github.com/Azure/ARO-HCP/internal/api/coreapi"
	"github.com/Azure/ARO-HCP/internal/api/kubeapplierapi"
	"github.com/Azure/ARO-HCP/internal/backup"
	"github.com/Azure/ARO-HCP/internal/database/cosmosstorage/corecosmosstorage"
	"github.com/Azure/ARO-HCP/internal/database/cosmosstorage/cosmosstorageutils"
	"github.com/Azure/ARO-HCP/internal/database/cosmosstorage/kubeappliercosmosstorage"
	"github.com/Azure/ARO-HCP/internal/database/informers/coreinformers"
	"github.com/Azure/ARO-HCP/internal/database/listers/corelisters"
	"github.com/Azure/ARO-HCP/internal/database/listers/kubeapplierlisters"
	unionkubeapplierinformers "github.com/Azure/ARO-HCP/internal/database/unioninformers/kubeapplier"
	"github.com/Azure/ARO-HCP/internal/utils"
)

// OnDemandBackupControllerName names the controller that reaps the desires
// behind admin-requested on-demand backups. The value is shared with the admin
// API, which stamps it into the desires' ControllerName tag when it creates
// them; see internal/backup for why it lives there.
const OnDemandBackupControllerName = backup.OnDemandBackupControllerName

// onDemandBackupSyncer cleans up after admin-requested on-demand backups.
//
// The admin API creates the Velero Backup ApplyDesire and its paired ReadDesire
// directly (admin/server/handlers/hcp/backups.go) because every input it needs
// is available synchronously. What it cannot do is observe the Backup reaching
// a terminal phase, which is this controller's only job:
//
//   - once the Backup is terminal, the ApplyDesire is purged. That hands
//     ownership of the Backup to Velero for the remainder of its TTL, so the
//     backup stays a valid restore point and stays listable via the admin API.
//   - the ReadDesire outlives the ApplyDesire so the Backup remains observable,
//     and is deleted only after Velero has garbage-collected the Backup at its
//     TTL.
//
// Desires are discovered by backup.OwnsBackupDesire: the DesireTagKeyOndemandBackup
// tag plus an owner of OnDemandBackupControllerName. No state is recorded on the
// ServiceProviderCluster. Rotation-triggered backups carry the identical tag and
// name prefix and differ only in owner — they belong to the KeyRotationBackup
// controller, which this sweep must never touch.
type onDemandBackupSyncer struct {
	clusterLister                corelisters.ClusterLister
	serviceProviderClusterLister corelisters.ServiceProviderClusterLister
	kubeApplierDBClients         kubeappliercosmosstorage.KubeApplierDBClients
	applyDesireLister            kubeapplierlisters.ApplyDesireLister
	readDesireLister             kubeapplierlisters.ReadDesireLister
}

var _ controllerutils.ClusterSyncer = (*onDemandBackupSyncer)(nil)

// NewOnDemandBackupController builds the cleanup controller. backupConfig is
// accepted for symmetry with the other backup controllers' registrations but is
// unused: cleanup is driven entirely by observed Velero state, and the TTL of an
// admin-requested backup is fixed at backup.OnDemandBackupTTL.
func NewOnDemandBackupController(
	cosmosClient corecosmosstorage.ResourcesDBClient,
	kubeApplierDBClients kubeappliercosmosstorage.KubeApplierDBClients,
	informers coreinformers.BackendInformers,
	kubeApplierInformers *unionkubeapplierinformers.UnionKubeApplierInformers,
	_ *BackupConfig,
) controllerutils.Controller {
	_, clusterLister := informers.Clusters()
	_, serviceProviderClusterLister := informers.ServiceProviderClusters()
	applyInformer, applyDesireLister := kubeApplierInformers.ApplyDesires()
	_, readDesireLister := kubeApplierInformers.ReadDesires()
	syncer := &onDemandBackupSyncer{
		clusterLister:                clusterLister,
		serviceProviderClusterLister: serviceProviderClusterLister,
		kubeApplierDBClients:         kubeApplierDBClients,
		applyDesireLister:            applyDesireLister,
		readDesireLister:             readDesireLister,
	}
	controller := controllerutils.NewClusterWatchingController(OnDemandBackupControllerName, cosmosClient, informers, kubeApplierInformers, 5*time.Minute, syncer)
	controller.AddInterestingResource(coreapi.ClusterResourceType, "*")
	controller.AddInterestingResource(coreapi.ServiceProviderClusterResourceType, coreapi.ServiceProviderClusterResourceName)
	controller.AddInterestingResource(kubeapplierapi.ClusterScopedApplyDesireResourceType, "*")
	controller.AddInterestingResource(kubeapplierapi.ClusterScopedReadDesireResourceType, "*")
	if err := controller.QueueForInformers(5*time.Minute, applyInformer); err != nil {
		panic(err)
	}
	return controller
}

func (c *onDemandBackupSyncer) SyncOnce(ctx context.Context, key controllerutils.HCPClusterKey) error {
	cluster, err := c.clusterLister.Get(ctx, key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName)
	if cosmosstorageutils.IsNotFoundError(err) {
		return nil
	}
	if err != nil {
		return utils.TrackError(err)
	}
	spc, err := c.serviceProviderClusterLister.Get(ctx, key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName)
	if cosmosstorageutils.IsNotFoundError(err) {
		return nil
	}
	if err != nil {
		return utils.TrackError(err)
	}
	if spc.Status.ManagementClusterResourceID == nil {
		return nil
	}

	client, err := c.kubeApplierDBClients.For(ctx, spc.Status.ManagementClusterResourceID)
	if err != nil {
		return utils.TrackError(err)
	}
	if client == nil {
		return utils.TrackError(fmt.Errorf("kube-applier client unavailable for on-demand backups"))
	}
	applyCRUD, err := client.ApplyDesiresForCluster(key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName)
	if err != nil {
		return utils.TrackError(err)
	}
	readCRUD, err := client.ReadDesiresForCluster(key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName)
	if err != nil {
		return utils.TrackError(err)
	}

	// A deleting cluster takes everything down regardless of Backup phase. The
	// Velero Backup itself is left intact for Velero to expire, matching the
	// rotation controller's deletion path.
	deleting := needsDeletionWork(*cluster, *spc)

	liveApplyDesireNames, err := c.sweepApplyDesires(ctx, key, applyCRUD, deleting)
	if err != nil {
		return err
	}
	return c.sweepReadDesires(ctx, key, readCRUD, applyCRUD, liveApplyDesireNames, deleting)
}

// sweepApplyDesires purges the ApplyDesire behind every requested backup that
// has reached a terminal phase. It returns the names of the ApplyDesires left
// standing, which sweepReadDesires uses to tell an in-flight backup from a
// finished one.
func (c *onDemandBackupSyncer) sweepApplyDesires(
	ctx context.Context,
	key controllerutils.HCPClusterKey,
	applyCRUD cosmosstorageutils.ResourceCRUD[kubeapplierapi.ApplyDesire, *kubeapplierapi.ApplyDesire],
	deleting bool,
) (map[string]bool, error) {
	logger := utils.LoggerFromContext(ctx)

	applyDesires, err := c.applyDesireLister.ListForCluster(ctx, key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName)
	if err != nil {
		return nil, utils.TrackError(err)
	}

	stillLive := map[string]bool{}
	for _, applyDesire := range applyDesires {
		if !backup.OwnsBackupDesire(applyDesire.Tags, OnDemandBackupControllerName) {
			continue
		}
		name := applyDesire.ResourceID.Name

		if deleting {
			logger.Info("purging requested backup ApplyDesire for deleted cluster", "desire", name)
			if err := kubeapplierhelpers.PurgeApplyDesire(ctx, name, applyCRUD); err != nil {
				return nil, err
			}
			continue
		}

		readDesire, err := c.readDesireLister.GetForCluster(ctx, key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName, name)
		if err != nil && !cosmosstorageutils.IsNotFoundError(err) {
			return nil, utils.TrackError(err)
		}
		if readDesire == nil {
			// Nothing is observing this backup yet; leave the ApplyDesire alone.
			stillLive[name] = true
			continue
		}

		phase, err := backup.ObservedVeleroBackupPhase(readDesire.Status.KubeContent)
		if err != nil {
			return nil, utils.TrackError(fmt.Errorf("failed to determine observed Backup phase for %s: %w", name, err))
		}
		if !backup.IsTerminalBackupPhase(phase) {
			stillLive[name] = true
			continue
		}

		logger.Info("purging requested backup ApplyDesire", "desire", name, "phase", phase,
			"reason", "backup reached a terminal phase; Velero Backup retained until its TTL")
		if err := kubeapplierhelpers.PurgeApplyDesire(ctx, name, applyCRUD); err != nil {
			return nil, err
		}
	}
	return stillLive, nil
}

// sweepReadDesires deletes the ReadDesire behind every requested backup whose
// Velero Backup has been garbage-collected.
//
// The "its ApplyDesire is gone" precondition is what makes this safe without
// any recorded state: a freshly created ReadDesire can legitimately report nil
// KubeContent with Successful=True in the window before kube-applier lands the
// Backup, and deleting it then would leave the backup unobservable forever.
// Because sweepApplyDesires purges an ApplyDesire only on a terminal phase, the
// ApplyDesire's absence is the durable signal that the backup is finished — but
// only when read from Cosmos. The cached absence in liveApplyDesireNames is also
// what a just-created ApplyDesire looks like before it reaches the informer, and
// reaping on that permanently blinds the admin API to a backup that is still
// being created: nothing recreates the ReadDesire. Candidates are therefore
// confirmed against Cosmos, as the key-rotation sweep does for the same reason.
func (c *onDemandBackupSyncer) sweepReadDesires(
	ctx context.Context,
	key controllerutils.HCPClusterKey,
	readCRUD cosmosstorageutils.ResourceCRUD[kubeapplierapi.ReadDesire, *kubeapplierapi.ReadDesire],
	applyCRUD cosmosstorageutils.ResourceCRUD[kubeapplierapi.ApplyDesire, *kubeapplierapi.ApplyDesire],
	liveApplyDesireNames map[string]bool,
	deleting bool,
) error {
	logger := utils.LoggerFromContext(ctx)

	readDesires, err := c.readDesireLister.ListForCluster(ctx, key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName)
	if err != nil {
		return utils.TrackError(err)
	}

	for _, readDesire := range readDesires {
		if !backup.OwnsBackupDesire(readDesire.Tags, OnDemandBackupControllerName) {
			continue
		}
		name := readDesire.ResourceID.Name
		reason := "cluster is being deleted"

		if !deleting {
			if liveApplyDesireNames[name] {
				// Backup still in flight.
				continue
			}
			backupGarbageCollected := readDesire.Status.KubeContent == nil && backup.IsDesireSuccessful(readDesire.Status.Conditions)
			if !backupGarbageCollected {
				continue
			}
			// Read Cosmos directly (not the informer lister) so a just-created
			// ApplyDesire that hasn't reached the cache yet is never mistaken
			// for "backup finished and Velero GC'd it" and its ReadDesire
			// reaped. Only needed once the cached view already says the desire
			// is gone, to disambiguate "not started" from "GC'd"; skip the
			// extra read otherwise.
			existingApplyDesire, err := applyCRUD.Get(ctx, name)
			if err != nil && !cosmosstorageutils.IsNotFoundError(err) {
				return utils.TrackError(fmt.Errorf("failed to get requested backup ApplyDesire: %w", err))
			}
			if existingApplyDesire != nil {
				continue
			}
			reason = "its ApplyDesire is gone and kube-applier observes no Backup"
		}

		logger.Info("deleting requested backup ReadDesire", "desire", name, "reason", reason)
		if err := readCRUD.Delete(ctx, name); err != nil && !cosmosstorageutils.IsNotFoundError(err) {
			return utils.TrackError(fmt.Errorf("failed to delete ReadDesire %s: %w", name, err))
		}
	}
	return nil
}
