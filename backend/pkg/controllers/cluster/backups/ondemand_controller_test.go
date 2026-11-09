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
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	azcorearm "github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"

	"github.com/Azure/ARO-HCP/backend/pkg/utils/controllerutils"
	"github.com/Azure/ARO-HCP/internal/api/coreapi"
	"github.com/Azure/ARO-HCP/internal/api/fleetapi"
	"github.com/Azure/ARO-HCP/internal/api/kubeapplierapi"
	"github.com/Azure/ARO-HCP/internal/apihelpers/fleetapihelpers"
	"github.com/Azure/ARO-HCP/internal/backup"
	"github.com/Azure/ARO-HCP/internal/database/cosmosstorage/corecosmosstorage"
	"github.com/Azure/ARO-HCP/internal/database/cosmosstorage/cosmosstorageutils"
	"github.com/Azure/ARO-HCP/internal/database/cosmosstoragetesting/corecosmosstoragetesting"
	"github.com/Azure/ARO-HCP/internal/database/cosmosstoragetesting/kubeappliercosmosstoragetesting"
	"github.com/Azure/ARO-HCP/internal/database/listers/kubeapplierlisters"
	"github.com/Azure/ARO-HCP/internal/database/listertesting/corelistertesting"
	"github.com/Azure/ARO-HCP/internal/database/listertesting/fleetlistertesting"
	"github.com/Azure/ARO-HCP/internal/database/listertesting/kubeapplierlistertesting"
)

type onDemandBackupFixture struct {
	syncer   *onDemandBackupSyncer
	key      controllerutils.HCPClusterKey
	db       corecosmosstorage.ResourcesDBClient
	mc       *azcorearm.ResourceID
	applyCRU cosmosstorageutils.ResourceCRUD[kubeapplierapi.ApplyDesire, *kubeapplierapi.ApplyDesire]
	readCRUD cosmosstorageutils.ResourceCRUD[kubeapplierapi.ReadDesire, *kubeapplierapi.ReadDesire]
}

func newOnDemandBackupTest(t *testing.T) *onDemandBackupFixture {
	t.Helper()
	key := controllerutils.HCPClusterKey{SubscriptionID: "sub", ResourceGroupName: "rg", HCPClusterName: "cluster"}
	db := corecosmosstoragetesting.NewMockResourcesDBClient()
	cluster := &coreapi.Cluster{CosmosMetadata: coreapi.CosmosMetadata{ResourceID: key.GetResourceID(), PartitionKey: key.SubscriptionID}}
	cluster.ServiceProviderProperties.BillingDocumentCosmosID = "billed"
	_, err := db.HCPClusters(key.SubscriptionID, key.ResourceGroupName).Create(t.Context(), cluster, nil)
	require.NoError(t, err)

	mc, err := fleetapihelpers.ToManagementClusterResourceID("mc1")
	require.NoError(t, err)
	spc, err := corecosmosstorage.GetOrCreateServiceProviderCluster(t.Context(), db, key.GetResourceID())
	require.NoError(t, err)
	spc.Status.ManagementClusterResourceID = mc
	spc.Status.HostedClusterNamespace = "hc-ns"
	spc.Status.ControlPlaneNamespace = "cp-ns"
	_, err = db.ServiceProviderClusters(key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName).Replace(t.Context(), spc, nil)
	require.NoError(t, err)

	clients := kubeappliercosmosstoragetesting.NewMockKubeApplierDBClients()
	client := kubeappliercosmosstoragetesting.NewMockKubeApplierDBClient()
	clients.Register(mc, client)
	mcLister := &fleetlistertesting.SliceManagementClusterLister{
		ManagementClusters: []*fleetapi.ManagementCluster{{CosmosMetadata: coreapi.CosmosMetadata{ResourceID: mc}}},
	}
	applyCRUD, err := client.ApplyDesiresForCluster(key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName)
	require.NoError(t, err)
	readCRUD, err := client.ReadDesiresForCluster(key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName)
	require.NoError(t, err)

	return &onDemandBackupFixture{
		syncer: &onDemandBackupSyncer{
			kubeApplierDBClients:         clients,
			clusterLister:                &corelistertesting.DBClusterLister{ResourcesDBClient: db},
			serviceProviderClusterLister: &corelistertesting.DBServiceProviderClusterLister{ResourcesDBClient: db},
			applyDesireLister:            &kubeapplierlistertesting.DBApplyDesireLister{Clients: clients, Lister: mcLister},
			readDesireLister:             &kubeapplierlistertesting.DBReadDesireLister{Clients: clients, Lister: mcLister},
		},
		key:      key,
		db:       db,
		mc:       mc,
		applyCRU: applyCRUD,
		readCRUD: readCRUD,
	}
}

// seedBackup creates an on-demand backup's ApplyDesire/ReadDesire pair owned by
// controllerName, and stamps the observed Velero Backup phase onto the
// ReadDesire. An empty phase means the Backup has not been observed at all (nil
// KubeContent).
//
// controllerName is the only thing that distinguishes an admin-requested backup
// from a key-rotation one: both carry DesireTagKeyOndemandBackup and the same
// desire name prefix.
func (f *onDemandBackupFixture) seedBackup(t *testing.T, backupName, controllerName string, phase velerov1.BackupPhase, successful bool) string {
	t.Helper()
	veleroBackup := backup.NewBackup(backupName, f.key.GetResourceID().String(), "", "hc-ns", "cp-ns", backup.OnDemandBackupTTL)
	applyDesire, err := backup.BuildBackupApplyDesire(
		f.key.SubscriptionID, f.key.ResourceGroupName, f.key.HCPClusterName,
		f.mc, veleroBackup, controllerName,
	)
	require.NoError(t, err)
	readDesire, err := backup.BuildReadDesireFromApplyDesire(f.key.SubscriptionID, f.key.ResourceGroupName, f.key.HCPClusterName, applyDesire)
	require.NoError(t, err)

	if phase != "" {
		observed := &velerov1.Backup{}
		observed.Name = backupName
		observed.Status.Phase = phase
		raw, err := json.Marshal(observed)
		require.NoError(t, err)
		readDesire.Status.KubeContent = &runtime.RawExtension{Raw: raw}
	}
	if successful {
		readDesire.Status.Conditions = []metav1.Condition{{
			Type:               kubeapplierapi.ConditionTypeSuccessful,
			Status:             metav1.ConditionTrue,
			Reason:             "AsExpected",
			LastTransitionTime: metav1.NewTime(time.Now()),
		}}
	}

	_, err = f.applyCRU.Create(t.Context(), applyDesire, nil)
	require.NoError(t, err)
	_, err = f.readCRUD.Create(t.Context(), readDesire, nil)
	require.NoError(t, err)
	return applyDesire.ResourceID.Name
}

func (f *onDemandBackupFixture) applyDesireExists(t *testing.T, name string) bool {
	t.Helper()
	_, err := f.applyCRU.Get(t.Context(), name)
	if cosmosstorageutils.IsNotFoundError(err) {
		return false
	}
	require.NoError(t, err)
	return true
}

func (f *onDemandBackupFixture) readDesireExists(t *testing.T, name string) bool {
	t.Helper()
	_, err := f.readCRUD.Get(t.Context(), name)
	if cosmosstorageutils.IsNotFoundError(err) {
		return false
	}
	require.NoError(t, err)
	return true
}

func (f *onDemandBackupFixture) markCluster(t *testing.T, mutate func(*coreapi.Cluster)) {
	t.Helper()
	crud := f.db.HCPClusters(f.key.SubscriptionID, f.key.ResourceGroupName)
	cluster, err := crud.Get(t.Context(), f.key.HCPClusterName)
	require.NoError(t, err)
	mutate(cluster)
	_, err = crud.Replace(t.Context(), cluster, nil)
	require.NoError(t, err)
}

// A terminal Backup means Velero now owns it for the rest of its TTL, so the
// ApplyDesire is retired while the ReadDesire stays to observe the Backup.
func TestOnDemandBackupPurgesApplyDesireOnTerminalPhase(t *testing.T) {
	for _, phase := range []velerov1.BackupPhase{
		velerov1.BackupPhaseCompleted,
		velerov1.BackupPhaseFailed,
		velerov1.BackupPhasePartiallyFailed,
		velerov1.BackupPhaseFailedValidation,
	} {
		t.Run(string(phase), func(t *testing.T) {
			f := newOnDemandBackupTest(t)
			name := f.seedBackup(t, "backup-1", backup.OnDemandBackupControllerName, phase, true)

			require.NoError(t, f.syncer.SyncOnce(t.Context(), f.key))

			require.False(t, f.applyDesireExists(t, name), "terminal backup must retire its ApplyDesire")
			require.True(t, f.readDesireExists(t, name), "ReadDesire must outlive the ApplyDesire so the Backup stays observable")
		})
	}
}

// Anything short of terminal means the backup is still running; both desires stay.
func TestOnDemandBackupLeavesInFlightBackupAlone(t *testing.T) {
	for _, tc := range []struct {
		name  string
		phase velerov1.BackupPhase
	}{
		{name: "not yet observed", phase: ""},
		{name: "new", phase: velerov1.BackupPhaseNew},
		{name: "in progress", phase: velerov1.BackupPhaseInProgress},
		{name: "finalizing", phase: velerov1.BackupPhaseFinalizing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newOnDemandBackupTest(t)
			name := f.seedBackup(t, "backup-1", backup.OnDemandBackupControllerName, tc.phase, true)

			require.NoError(t, f.syncer.SyncOnce(t.Context(), f.key))

			require.True(t, f.applyDesireExists(t, name), "in-flight backup must keep its ApplyDesire")
			require.True(t, f.readDesireExists(t, name), "in-flight backup must keep its ReadDesire")
		})
	}
}

// A freshly created ReadDesire can report nil KubeContent with Successful=True
// before kube-applier lands the Backup. Deleting it then would leave the backup
// unobservable, so the surviving ApplyDesire must hold it in place.
func TestOnDemandBackupKeepsReadDesireWhileApplyDesireLives(t *testing.T) {
	f := newOnDemandBackupTest(t)
	name := f.seedBackup(t, "backup-1", backup.OnDemandBackupControllerName, "", true)

	require.NoError(t, f.syncer.SyncOnce(t.Context(), f.key))

	require.True(t, f.applyDesireExists(t, name), "ApplyDesire must survive an unobserved Backup")
	require.True(t, f.readDesireExists(t, name), "ReadDesire must not be reaped while its ApplyDesire still exists")
}

// Once the backup is done and Velero has GC'd the Backup at its TTL, a
// successful empty read is the signal to retire the ReadDesire.
func TestOnDemandBackupDeletesReadDesireAfterVeleroGC(t *testing.T) {
	f := newOnDemandBackupTest(t)
	name := f.seedBackup(t, "backup-1", backup.OnDemandBackupControllerName, velerov1.BackupPhaseCompleted, true)

	// First sync retires the ApplyDesire; the Backup is still live.
	require.NoError(t, f.syncer.SyncOnce(t.Context(), f.key))
	require.False(t, f.applyDesireExists(t, name), "completed backup must retire its ApplyDesire")
	require.True(t, f.readDesireExists(t, name), "ReadDesire must survive until Velero GCs the Backup")

	// Velero expires the Backup; kube-applier records a successful empty read.
	readDesire, err := f.readCRUD.Get(t.Context(), name)
	require.NoError(t, err)
	readDesire.Status.KubeContent = nil
	_, err = f.readCRUD.Replace(t.Context(), readDesire, nil)
	require.NoError(t, err)

	require.NoError(t, f.syncer.SyncOnce(t.Context(), f.key))
	require.False(t, f.readDesireExists(t, name), "ReadDesire must be deleted once the Backup is GC'd")
}

// staleApplyDesireLister hides one ApplyDesire from the lister while leaving it
// in Cosmos, reproducing the change-feed lag between the admin server creating
// the desire and the backend's informer receiving it.
type staleApplyDesireLister struct {
	kubeapplierlisters.ApplyDesireLister
	hidden string
}

func (l *staleApplyDesireLister) ListForCluster(ctx context.Context, subscriptionID, resourceGroupName, clusterName string) ([]*kubeapplierapi.ApplyDesire, error) {
	applyDesires, err := l.ApplyDesireLister.ListForCluster(ctx, subscriptionID, resourceGroupName, clusterName)
	if err != nil {
		return nil, err
	}
	kept := make([]*kubeapplierapi.ApplyDesire, 0, len(applyDesires))
	for _, applyDesire := range applyDesires {
		if applyDesire.ResourceID.Name == l.hidden {
			continue
		}
		kept = append(kept, applyDesire)
	}
	return kept, nil
}

func (l *staleApplyDesireLister) GetForCluster(ctx context.Context, subscriptionID, resourceGroupName, clusterName, name string) (*kubeapplierapi.ApplyDesire, error) {
	if name == l.hidden {
		return nil, cosmosstorageutils.NewNotFoundError()
	}
	return l.ApplyDesireLister.GetForCluster(ctx, subscriptionID, resourceGroupName, clusterName, name)
}

// A just-requested backup looks exactly like a GC'd one through the lister: the
// ReadDesire reports Successful=True with no KubeContent (kube-applier synced
// before Velero created the Backup), and the ApplyDesire the admin server wrote
// has not replicated into the informer yet.
//
// Reaping the ReadDesire there is unrecoverable — nothing recreates it, so the
// admin API can never report that backup again, which is what stalled the
// recovery e2e until its 660s timeout while the Backup itself completed fine.
func TestOnDemandBackupKeepsReadDesireWhenApplyDesireIsOnlyMissingFromTheLister(t *testing.T) {
	f := newOnDemandBackupTest(t)
	name := f.seedBackup(t, "backup-1", backup.OnDemandBackupControllerName, "", true)
	f.syncer.applyDesireLister = &staleApplyDesireLister{ApplyDesireLister: f.syncer.applyDesireLister, hidden: name}

	require.NoError(t, f.syncer.SyncOnce(t.Context(), f.key))

	require.True(t, f.applyDesireExists(t, name), "the ApplyDesire was never gone, only unreplicated")
	require.True(t, f.readDesireExists(t, name),
		"ReadDesire must survive: the ApplyDesire is absent from the lister but present in Cosmos, so the backup is still being created")
}

// Rotation-triggered backups belong to the KeyRotationBackup controller. This
// sweep must never reap them, or the two controllers would race.
//
// The two seeds below are byte-for-byte identical in tag and shape and differ
// only in Tags[ControllerName], which is the whole point: ownership is the only
// thing keeping this sweep off the rotation controller's desires.
func TestOnDemandBackupIgnoresRotationDesires(t *testing.T) {
	f := newOnDemandBackupTest(t)
	rotation := f.seedBackup(t, "hc-ns-keyrotation-abc", backup.KeyRotationBackupControllerName, velerov1.BackupPhaseCompleted, true)
	requested := f.seedBackup(t, "backup-1", backup.OnDemandBackupControllerName, velerov1.BackupPhaseCompleted, true)

	require.NoError(t, f.syncer.SyncOnce(t.Context(), f.key))

	require.True(t, f.applyDesireExists(t, rotation), "rotation ApplyDesire must be left to the KeyRotationBackup controller")
	require.True(t, f.readDesireExists(t, rotation), "rotation ReadDesire must be left to the KeyRotationBackup controller")
	require.False(t, f.applyDesireExists(t, requested), "requested backup ApplyDesire must still be retired")

	// The ReadDesire sweep is the more dangerous half: a rotation ReadDesire
	// whose Backup has been GC'd looks exactly like one this controller owns and
	// should delete, and its ApplyDesire is absent from the live set (we skipped
	// it above) so the "still in flight" guard does not save it either. Only the
	// owner check does.
	rotationReadDesire, err := f.readCRUD.Get(t.Context(), rotation)
	require.NoError(t, err, "failed to read back the seeded rotation ReadDesire")
	rotationReadDesire.Status.KubeContent = nil
	_, err = f.readCRUD.Replace(t.Context(), rotationReadDesire, nil)
	require.NoError(t, err, "failed to mark the rotation Backup as GC'd")

	require.NoError(t, f.syncer.SyncOnce(t.Context(), f.key))
	require.True(t, f.readDesireExists(t, rotation), "a GC'd rotation ReadDesire is still the KeyRotationBackup controller's to delete")
}

// A deleting cluster takes both desires down regardless of phase. The Velero
// Backup itself is left for Velero to expire.
func TestOnDemandBackupPurgesEverythingOnClusterDeletion(t *testing.T) {
	f := newOnDemandBackupTest(t)
	inFlight := f.seedBackup(t, "backup-1", backup.OnDemandBackupControllerName, velerov1.BackupPhaseInProgress, true)
	f.markCluster(t, func(cluster *coreapi.Cluster) {
		now := metav1.NewTime(time.Now())
		cluster.ServiceProviderProperties.DeletionTimestamp = &now
	})

	require.NoError(t, f.syncer.SyncOnce(t.Context(), f.key))

	require.False(t, f.applyDesireExists(t, inFlight), "deletion must purge the ApplyDesire even mid-backup")
	require.False(t, f.readDesireExists(t, inFlight), "deletion must delete the ReadDesire even mid-backup")
}

// Without a resolved management cluster there is nothing to reach, and no error
// to report: placement simply has not happened yet.
func TestOnDemandBackupNoopWithoutPlacement(t *testing.T) {
	f := newOnDemandBackupTest(t)
	crud := f.db.ServiceProviderClusters(f.key.SubscriptionID, f.key.ResourceGroupName, f.key.HCPClusterName)
	spc, err := crud.Get(t.Context(), coreapi.ServiceProviderClusterResourceName)
	require.NoError(t, err)
	spc.Status.ManagementClusterResourceID = nil
	_, err = crud.Replace(t.Context(), spc, nil)
	require.NoError(t, err)

	require.NoError(t, f.syncer.SyncOnce(t.Context(), f.key))
}

// Repeated syncs over an already-clean cluster must not error or thrash.
func TestOnDemandBackupIsIdempotent(t *testing.T) {
	f := newOnDemandBackupTest(t)
	name := f.seedBackup(t, "backup-1", backup.OnDemandBackupControllerName, velerov1.BackupPhaseCompleted, true)

	for range 3 {
		require.NoError(t, f.syncer.SyncOnce(t.Context(), f.key))
	}
	require.False(t, f.applyDesireExists(t, name), "ApplyDesire stays retired across repeated syncs")
	require.True(t, f.readDesireExists(t, name), "ReadDesire stays until Velero GCs the Backup")
}
