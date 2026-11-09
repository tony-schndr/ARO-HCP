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
	"context"
	"fmt"
	"strings"
	"time"

	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"

	azcorearm "github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"

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

const keyRotationBackupNameSeparator = "-keyrotation-"

// KeyRotationBackupControllerName names the controller that takes a backup after
// a KMS key rotation. The value is shared with the admin API, which must be able
// to tell this flow's desires from its own; see internal/backup for why it lives
// there.
const KeyRotationBackupControllerName = backup.KeyRotationBackupControllerName

type keyRotationBackupSyncer struct {
	cosmosClient                 corecosmosstorage.ResourcesDBClient
	clusterLister                corelisters.ClusterLister
	serviceProviderClusterLister corelisters.ServiceProviderClusterLister

	kubeApplierDBClients kubeappliercosmosstorage.KubeApplierDBClients
	applyDesireLister    kubeapplierlisters.ApplyDesireLister
	readDesireLister     kubeapplierlisters.ReadDesireLister

	backupConfig *BackupConfig
}

var _ controllerutils.ClusterSyncer = (*keyRotationBackupSyncer)(nil)

func NewKeyRotationBackupController(
	cosmosClient corecosmosstorage.ResourcesDBClient,
	kubeApplierDBClients kubeappliercosmosstorage.KubeApplierDBClients,
	informers coreinformers.BackendInformers,
	kubeApplierInformers *unionkubeapplierinformers.UnionKubeApplierInformers,
	backupConfig *BackupConfig,
) controllerutils.Controller {

	_, clusterLister := informers.Clusters()
	_, serviceProviderClusterLister := informers.ServiceProviderClusters()
	_, applyDesireLister := kubeApplierInformers.ApplyDesires()
	_, readDesireLister := kubeApplierInformers.ReadDesires()

	syncer := &keyRotationBackupSyncer{
		cosmosClient:                 cosmosClient,
		clusterLister:                clusterLister,
		serviceProviderClusterLister: serviceProviderClusterLister,
		kubeApplierDBClients:         kubeApplierDBClients,
		applyDesireLister:            applyDesireLister,
		readDesireLister:             readDesireLister,
		backupConfig:                 backupConfig,
	}

	controller := controllerutils.NewClusterWatchingController(
		KeyRotationBackupControllerName,
		cosmosClient,
		informers,
		kubeApplierInformers,
		5*time.Minute,
		syncer,
	)
	// Backup desire names include runtime namespace/configuration or key fingerprints.
	controller.AddInterestingResource(coreapi.ClusterResourceType, "*")
	controller.AddInterestingResource(coreapi.ServiceProviderClusterResourceType, coreapi.ServiceProviderClusterResourceName)
	controller.AddInterestingResource(kubeapplierapi.ClusterScopedApplyDesireResourceType, "*")
	controller.AddInterestingResource(kubeapplierapi.ClusterScopedReadDesireResourceType, "*")

	// NewClusterWatchingController already watches ApplyDesire changes with no
	// resync. Resync cluster keys once, not once for every input.

	return controller
}

func (c *keyRotationBackupSyncer) SyncOnce(ctx context.Context, key controllerutils.HCPClusterKey) error {
	cachedCluster, err := c.clusterLister.Get(ctx, key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName)
	if cosmosstorageutils.IsNotFoundError(err) {
		return nil
	}
	if err != nil {
		return utils.TrackError(fmt.Errorf("failed to get cached Cluster: %w", err))
	}
	cachedServiceProviderCluster, err := c.serviceProviderClusterLister.Get(ctx, key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName)
	if cosmosstorageutils.IsNotFoundError(err) {
		return nil
	}
	if err != nil {
		return utils.TrackError(fmt.Errorf("failed to get cached ServiceProviderCluster: %w", err))
	}

	if needsDeletionWork(*cachedCluster, *cachedServiceProviderCluster) {
		return c.syncDeletion(ctx, key, cachedServiceProviderCluster)
	}

	if !needsWork(*cachedCluster, *cachedServiceProviderCluster) {
		return nil
	}

	cm := cachedCluster.CustomerProperties.Etcd.DataEncryption.CustomerManaged
	if cm == nil || cm.Kms == nil || cm.Kms.ActiveKey.Version == "" {
		return nil
	}

	hostedCluster, _, err := kubeapplierhelpers.GetCachedHostedClusterForCluster(
		ctx, c.readDesireLister, key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName,
	)
	if err != nil {
		return utils.TrackError(fmt.Errorf("failed to get cached HostedCluster: %w", err))
	}
	if hostedCluster == nil {
		return nil
	}

	if !rotationComplete(hostedCluster) {
		return nil
	}

	managementClusterResourceID := cachedServiceProviderCluster.Status.ManagementClusterResourceID
	kubeApplierClient, err := c.kubeApplierDBClients.For(ctx, managementClusterResourceID)
	if err != nil {
		return err
	}
	if kubeApplierClient == nil {
		return nil
	}

	applyDesireCRUD, err := kubeApplierClient.ApplyDesiresForCluster(key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName)
	if err != nil {
		return utils.TrackError(fmt.Errorf("get ApplyDesire CRUD: %w", err))
	}
	readDesireCRUD, err := kubeApplierClient.ReadDesiresForCluster(key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName)
	if err != nil {
		return utils.TrackError(fmt.Errorf("get ReadDesire CRUD: %w", err))
	}

	activeKey := hostedCluster.Status.SecretEncryption.ActiveKey.Azure
	kmsKeyFingerprint := backup.AzureKMSKeyFingerprint(activeKey.KeyVaultName, activeKey.KeyName, activeKey.KeyVersion)
	resourceID := cachedCluster.ResourceID.String()
	hostedClusterNamespace := cachedServiceProviderCluster.Status.HostedClusterNamespace
	controlPlaneNamespace := cachedServiceProviderCluster.Status.ControlPlaneNamespace

	currentDesireName := keyRotationDesireName(keyRotationBackupName(hostedClusterNamespace, kmsKeyFingerprint))

	// A completed or skipped rotation needs nothing further: fall through to cleanup.
	alreadyHandled := cachedServiceProviderCluster.Status.KeyRotationBackupFingerprint == kmsKeyFingerprint

	if !alreadyHandled {
		// Use the same effective pause decision as the schedule controller.
		// A backup already in flight (ApplyDesire exists) is never abandoned
		// once pause takes effect.
		backupsPaused := cachedServiceProviderCluster.Spec.BackupScheduleState == coreapi.BackupScheduleStateDisabled
		if c.backupConfig != nil {
			backupsPaused = c.backupConfig.SchedulePaused(
				cachedServiceProviderCluster.Spec.BackupScheduleState,
				cachedCluster.ServiceProviderProperties.ExperimentalFeatures.BackupScheduleOverride,
			)
		}

		inFlight := false
		if backupsPaused {
			// Read Cosmos directly (not the informer lister) so a just-created ApplyDesire
			// that hasn't reached the cache yet is never mistaken for "not started" and
			// abandoned by the cleanup sweep below. Only needed while paused, to
			// disambiguate "not started" from "in flight"; skip the extra read otherwise.
			existingApplyDesire, err := applyDesireCRUD.Get(ctx, strings.ToLower(currentDesireName))
			if err != nil && !cosmosstorageutils.IsNotFoundError(err) {
				return utils.TrackError(fmt.Errorf("failed to get current on-demand backup ApplyDesire: %w", err))
			}

			if existingApplyDesire == nil {
				// Nothing started yet and paused: record as skipped so it's never
				// retried after resume. Only a later rotation is eligible.
				if _, err := c.recordKeyRotationBackupFingerprint(ctx, key, cachedServiceProviderCluster, kmsKeyFingerprint); err != nil {
					return err
				}
				// Return now; the cached ServiceProviderCluster is stale so the cleanup sweep below
				// couldn't act on this yet. The write above retriggers the informer.
				return nil
			}

			// Already in flight: leave the existing ApplyDesire untouched, but still
			// ensure its companion ReadDesire exists (e.g. crash recovery may have left
			// the ApplyDesire without one) so the cleanup sweep below can observe the
			// Velero phase and eventually record completion.
			desiredReadDesire, err := buildReadDesireFromApplyDesire(key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName, existingApplyDesire)
			if err != nil {
				return utils.TrackError(fmt.Errorf("failed to build key rotation read desire: %w", err))
			}
			if err := kubeapplierhelpers.EnsureReadDesire(ctx, readDesireCRUD, c.readDesireLister, desiredReadDesire); err != nil {
				return err
			}
			inFlight = true
		}

		if !inFlight {
			// On-demand backups use the first (shortest-lived) schedule's TTL, just long
			// enough to survive until the next scheduled backup captures the post-rotation state.
			if c.backupConfig == nil || len(c.backupConfig.Schedules()) == 0 {
				return utils.TrackError(fmt.Errorf("no backup schedules configured to reuse TTL for on-demand backup (backupConfig=%v)", c.backupConfig))
			}
			ttl := c.backupConfig.Schedules()[0].TTL
			veleroBackup := backup.NewBackup(keyRotationBackupName(hostedClusterNamespace, kmsKeyFingerprint), resourceID, kmsKeyFingerprint, hostedClusterNamespace, controlPlaneNamespace, ttl)

			// No explicit in-flight check: Velero queues backups natively, ensuring
			// on-demand backups run after scheduled backups and provide a valid backup
			// as soon as possible, avoiding longer waits for the next scheduled backup.
			desiredApplyDesire, err := buildOnDemandBackupApplyDesire(key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName, managementClusterResourceID, veleroBackup)
			if err != nil {
				return utils.TrackError(fmt.Errorf("failed to build key rotation backup desire: %w", err))
			}

			// Build the paired ReadDesire, then create-or-update both through the
			// shared kubeapplierhelpers ensure helpers. The ReadDesire is ensured
			// first so the on-demand backup's status is observed as soon as its
			// ApplyDesire lands.
			desiredReadDesire, err := buildReadDesireFromApplyDesire(key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName, desiredApplyDesire)
			if err != nil {
				return utils.TrackError(fmt.Errorf("failed to build key rotation read desire: %w", err))
			}

			if err := kubeapplierhelpers.EnsureReadDesire(ctx, readDesireCRUD, c.readDesireLister, desiredReadDesire); err != nil {
				return err
			}

			if err := kubeapplierhelpers.EnsureApplyDesire(ctx, applyDesireCRUD, c.applyDesireLister, desiredApplyDesire); err != nil {
				return err
			}
		}
	}

	// Retire on-demand desires once their work is done. The completed backup itself is left
	// alone to expire at its own Velero TTL, so it stays a valid restore point (and stays
	// listable via the admin API) until then; scheduled backups provide ongoing coverage.
	if requeue, err := c.purgeCompletedOnDemandApplyDesires(ctx, key, applyDesireCRUD, cachedServiceProviderCluster, kmsKeyFingerprint, currentDesireName); requeue || err != nil {
		return err
	}
	if requeue, err := c.deleteStaleOnDemandReadDesires(ctx, key, readDesireCRUD, cachedServiceProviderCluster, kmsKeyFingerprint, currentDesireName); requeue || err != nil {
		return err
	}

	return nil
}

// syncDeletion purges on-demand key-rotation desires for a deleted cluster directly
// from Cosmos. Unlike Schedule desires, these target Backup CRs and have no in-flight
// check, so setting Type=Delete could race a running Velero backup. Direct purging
// leaves the Backup CR and its data intact for Velero cleanup; mgmt-agent may
// request deletion before TTL expiry after the HostedCluster disappears.
//
// Admin-requested backups carry the same tag and name prefix but a different owner,
// and the OnDemandBackup controller tears those down; OwnsBackupDesire keeps this
// sweep off them.
func (c *keyRotationBackupSyncer) syncDeletion(
	ctx context.Context,
	key controllerutils.HCPClusterKey,
	cachedServiceProviderCluster *coreapi.ServiceProviderCluster,
) error {
	managementClusterResourceID := cachedServiceProviderCluster.Status.ManagementClusterResourceID
	kubeApplierClient, err := c.kubeApplierDBClients.For(ctx, managementClusterResourceID)
	if err != nil {
		return err
	}
	if kubeApplierClient == nil {
		return nil
	}

	applyDesireCRUD, err := kubeApplierClient.ApplyDesiresForCluster(key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName)
	if err != nil {
		return utils.TrackError(fmt.Errorf("get ApplyDesire CRUD: %w", err))
	}
	readDesireCRUD, err := kubeApplierClient.ReadDesiresForCluster(key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName)
	if err != nil {
		return utils.TrackError(fmt.Errorf("get ReadDesire CRUD: %w", err))
	}

	logger := utils.LoggerFromContext(ctx)

	applyDesires, err := c.applyDesireLister.ListForCluster(ctx, key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName)
	if err != nil {
		return err
	}
	for _, applyDesire := range applyDesires {
		if !backup.OwnsBackupDesire(applyDesire.Tags, KeyRotationBackupControllerName) {
			continue
		}
		logger.Info("purging on-demand backup ApplyDesire for deleted cluster", "desire", applyDesire.ResourceID.Name)
		if err := kubeapplierhelpers.PurgeApplyDesire(ctx, applyDesire.ResourceID.Name, applyDesireCRUD); err != nil {
			return err
		}
	}

	readDesires, err := c.readDesireLister.ListForCluster(ctx, key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName)
	if err != nil {
		return utils.TrackError(fmt.Errorf("failed to list ReadDesires: %w", err))
	}
	for _, readDesire := range readDesires {
		if !backup.OwnsBackupDesire(readDesire.Tags, KeyRotationBackupControllerName) {
			continue
		}
		name := readDesire.ResourceID.Name
		logger.Info("deleting on-demand backup ReadDesire for deleted cluster", "desire", name)
		if err := readDesireCRUD.Delete(ctx, name); err != nil && !cosmosstorageutils.IsNotFoundError(err) {
			return utils.TrackError(fmt.Errorf("failed to delete ReadDesire %s: %w", name, err))
		}
	}

	return nil
}

// purgeCompletedOnDemandApplyDesires retires superseded or completed on-demand desires
// owned by this controller. Admin-requested backups share the tag and are skipped.
func (c *keyRotationBackupSyncer) purgeCompletedOnDemandApplyDesires(
	ctx context.Context,
	key controllerutils.HCPClusterKey,
	applyDesireCRUD cosmosstorageutils.ResourceCRUD[kubeapplierapi.ApplyDesire, *kubeapplierapi.ApplyDesire],
	cachedServiceProviderCluster *coreapi.ServiceProviderCluster,
	kmsKeyFingerprint string,
	currentDesireName string,
) (bool, error) {
	logger := utils.LoggerFromContext(ctx)

	applyDesires, err := c.applyDesireLister.ListForCluster(ctx, key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName)
	if err != nil {
		return false, err
	}
	for _, applyDesire := range applyDesires {
		if !backup.OwnsBackupDesire(applyDesire.Tags, KeyRotationBackupControllerName) {
			continue
		}

		isCurrent := applyDesire.ResourceID.Name == currentDesireName
		// Desires from older rotations can be retired regardless of backup status.
		reason := "superseded by newer rotation"
		if isCurrent {
			if cachedServiceProviderCluster.Status.KeyRotationBackupFingerprint == kmsKeyFingerprint {
				// Completion was already durably recorded on a prior sync: purge
				// unconditionally. Do not re-derive phase from the ReadDesire here,
				// since its KubeContent may have already gone nil (Velero TTL GC
				// observed by kube-applier) or the ReadDesire may already be gone
				// (deleteStaleOnDemandReadDesires runs after this); either would
				// otherwise orphan this ApplyDesire forever.
				reason = "backup completed successfully; Velero Backup retained until its TTL"
			} else {
				// Fingerprint not yet recorded: only retire the current rotation's
				// ApplyDesire once its backup has succeeded.
				readDesire, err := c.readDesireLister.GetForCluster(ctx, key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName, applyDesire.ResourceID.Name)
				if err != nil {
					if cosmosstorageutils.IsNotFoundError(err) {
						continue
					}
					return false, err
				}
				phase, err := observedVeleroBackupPhase(readDesire.Status.KubeContent)
				if err != nil {
					return false, utils.TrackError(fmt.Errorf("failed to determine observed Backup phase for %s: %w", applyDesire.ResourceID.Name, err))
				}
				if phase != velerov1.BackupPhaseCompleted {
					// Keep unsuccessful backups until a future rotation supersedes them.
					continue
				}
				// Record completion before purging so a crash cannot cause the desire to be recreated.
				logger.Info("recording key rotation backup fingerprint",
					"desire", applyDesire.ResourceID.Name, "fingerprint", kmsKeyFingerprint, "phase", phase)
				return c.recordKeyRotationBackupFingerprint(ctx, key, cachedServiceProviderCluster, kmsKeyFingerprint)
			}
		}

		logger.Info("purging on-demand backup ApplyDesire", "desire", applyDesire.ResourceID.Name, "reason", reason)
		// Purge the Cosmos document directly, leaving the Velero Backup to expire at its TTL.
		if err := kubeapplierhelpers.PurgeApplyDesire(ctx, applyDesire.ResourceID.Name, applyDesireCRUD); err != nil {
			return false, err
		}
		return true, nil
	}

	return false, nil
}

// recordKeyRotationBackupFingerprint durably marks kmsKeyFingerprint as handled,
// whether the backup completed or was skipped due to pause. Either way this
// rotation is never acted on again until a new fingerprint appears.
func (c *keyRotationBackupSyncer) recordKeyRotationBackupFingerprint(
	ctx context.Context,
	key controllerutils.HCPClusterKey,
	cachedServiceProviderCluster *coreapi.ServiceProviderCluster,
	kmsKeyFingerprint string,
) (bool, error) {
	replacement := cachedServiceProviderCluster.DeepCopy()
	replacement.Status.KeyRotationBackupFingerprint = kmsKeyFingerprint

	serviceProviderClusterCRUD := c.cosmosClient.ServiceProviderClusters(key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName)
	if _, err := serviceProviderClusterCRUD.Replace(ctx, replacement, nil); err != nil {
		// another writer already updated the document; the informer will retrigger a sync with the fresh cache.
		if cosmosstorageutils.IsPreconditionFailedError(err) {
			return false, nil
		}
		return false, utils.TrackError(fmt.Errorf("failed to record key rotation backup fingerprint: %w", err))
	}
	return true, nil
}

// deleteStaleOnDemandReadDesires removes this controller's on-demand key-rotation
// ReadDesires after their Velero Backups have been garbage-collected.
// Admin-requested backups share the tag and are skipped.
func (c *keyRotationBackupSyncer) deleteStaleOnDemandReadDesires(
	ctx context.Context,
	key controllerutils.HCPClusterKey,
	readDesireCRUD cosmosstorageutils.ResourceCRUD[kubeapplierapi.ReadDesire, *kubeapplierapi.ReadDesire],
	cachedServiceProviderCluster *coreapi.ServiceProviderCluster,
	kmsKeyFingerprint string,
	currentDesireName string,
) (bool, error) {
	logger := utils.LoggerFromContext(ctx)

	readDesires, err := c.readDesireLister.ListForCluster(ctx, key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName)
	if err != nil {
		return false, utils.TrackError(fmt.Errorf("failed to list ReadDesires: %w", err))
	}
	for _, readDesire := range readDesires {
		if !backup.OwnsBackupDesire(readDesire.Tags, KeyRotationBackupControllerName) {
			continue
		}

		// ApplyDesires are removed when backups complete, before Velero's TTL cleanup.
		// A successful empty read confirms that the Backup is now gone.
		backupGarbageCollected := readDesire.Status.KubeContent == nil && isDesireSuccessful(readDesire.Status.Conditions)

		isCurrent := readDesire.ResourceID.Name == currentDesireName
		reason := "stale: superseded rotation, Backup GC'd by Velero at TTL"
		if isCurrent {
			reason = "current: Backup GC'd by Velero at TTL"
			// Keep until the outcome (completed or skipped) is durably recorded;
			// a skipped rotation's ReadDesire is an orphan from a pause race.
			outcomeRecorded := cachedServiceProviderCluster.Status.KeyRotationBackupFingerprint == kmsKeyFingerprint
			if !outcomeRecorded || !backupGarbageCollected {
				continue
			}
		} else if !backupGarbageCollected {
			continue
		}

		name := readDesire.ResourceID.Name
		logger.Info("deleting stale on-demand backup ReadDesire", "desire", name, "reason", reason)
		if err := readDesireCRUD.Delete(ctx, name); err != nil && !cosmosstorageutils.IsNotFoundError(err) {
			return false, utils.TrackError(fmt.Errorf("failed to delete stale ReadDesire %s: %w", name, err))
		}
		return true, nil
	}

	return false, nil
}

func keyRotationBackupName(hostedClusterNamespace, kmsKeyFingerprint string) string {
	return hostedClusterNamespace + keyRotationBackupNameSeparator + kmsKeyFingerprint
}

func buildOnDemandBackupApplyDesire(
	subscriptionID, resourceGroupName, clusterName string,
	managementClusterResourceID *azcorearm.ResourceID,
	veleroBackup *velerov1.Backup,
) (*kubeapplierapi.ApplyDesire, error) {
	return backup.BuildBackupApplyDesire(subscriptionID, resourceGroupName, clusterName, managementClusterResourceID, veleroBackup, KeyRotationBackupControllerName)
}
