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
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"

	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiservervalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	clocktesting "k8s.io/utils/clock/testing"

	"sigs.k8s.io/yaml"

	azcorearm "github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"

	"github.com/Azure/ARO-HCP/backend/pkg/controllers/cluster/backups"
	"github.com/Azure/ARO-HCP/backend/pkg/utils/controllerutils"
	hcprecoveryv1alpha1 "github.com/Azure/ARO-HCP/hcp-recovery/pkg/apis/hcprecovery/v1alpha1"
	"github.com/Azure/ARO-HCP/internal/api/coreapi"
	"github.com/Azure/ARO-HCP/internal/api/fleetapi"
	"github.com/Azure/ARO-HCP/internal/api/kubeapplierapi"
	"github.com/Azure/ARO-HCP/internal/apihelpers/fleetapihelpers"
	"github.com/Azure/ARO-HCP/internal/apihelpers/kubeapplierapihelpers"
	"github.com/Azure/ARO-HCP/internal/backup"
	"github.com/Azure/ARO-HCP/internal/database/cosmosstorage/corecosmosstorage"
	"github.com/Azure/ARO-HCP/internal/database/cosmosstoragetesting/corecosmosstoragetesting"
	"github.com/Azure/ARO-HCP/internal/database/cosmosstoragetesting/kubeappliercosmosstoragetesting"
	"github.com/Azure/ARO-HCP/internal/database/listertesting/corelistertesting"
	"github.com/Azure/ARO-HCP/internal/database/listertesting/fleetlistertesting"
	"github.com/Azure/ARO-HCP/internal/database/listertesting/kubeapplierlistertesting"
)

type recoveryTest struct {
	syncer  *recoverySyncer
	key     controllerutils.HCPClusterKey
	cluster *coreapi.Cluster
	spc     *coreapi.ServiceProviderCluster
}

func newRecoveryTest(t *testing.T) *recoveryTest {
	t.Helper()
	ctx := t.Context()
	key := controllerutils.HCPClusterKey{SubscriptionID: "sub", ResourceGroupName: "rg", HCPClusterName: "cluster"}
	cluster := &coreapi.Cluster{CosmosMetadata: coreapi.CosmosMetadata{ResourceID: key.GetResourceID(), PartitionKey: key.SubscriptionID}}
	cluster.CustomerProperties.DNS.BaseDomainPrefix = "hosted-cluster"
	db := corecosmosstoragetesting.NewMockResourcesDBClient()
	var err error
	cluster, err = db.HCPClusters(key.SubscriptionID, key.ResourceGroupName).Create(ctx, cluster, nil)
	require.NoError(t, err)
	mc, err := fleetapihelpers.ToManagementClusterResourceID("mc1")
	require.NoError(t, err)
	spc, err := corecosmosstorage.GetOrCreateServiceProviderCluster(ctx, db, key.GetResourceID())
	require.NoError(t, err)
	spc.Status.ManagementClusterResourceID = mc
	spc.Status.HostedClusterNamespace = "hc-ns"
	spc.Status.ControlPlaneNamespace = "hc-ns-hosted-cluster"
	spc.Spec.RecoveryRequests = []coreapi.RecoveryRequest{{RecoveryID: "r1", VeleroBackupName: "b1"}}
	spc, err = db.ServiceProviderClusters(key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName).Replace(ctx, spc, nil)
	require.NoError(t, err)
	clients := kubeappliercosmosstoragetesting.NewMockKubeApplierDBClients()
	clients.Register(mc, kubeappliercosmosstoragetesting.NewMockKubeApplierDBClient())
	lister := &fleetlistertesting.SliceManagementClusterLister{ManagementClusters: []*fleetapi.ManagementCluster{{CosmosMetadata: coreapi.CosmosMetadata{ResourceID: mc}}}}
	return &recoveryTest{
		key: key, cluster: cluster, spc: spc,
		syncer: &recoverySyncer{
			cosmosClient: db, kubeApplierDBClients: clients,
			clusterLister:                &corelistertesting.DBClusterLister{ResourcesDBClient: db},
			serviceProviderClusterLister: &corelistertesting.DBServiceProviderClusterLister{ResourcesDBClient: db},
			applyDesireLister:            &kubeapplierlistertesting.DBApplyDesireLister{Clients: clients, Lister: lister},
			readDesireLister:             &kubeapplierlistertesting.DBReadDesireLister{Clients: clients, Lister: lister},
			backupConfig:                 &backups.BackupConfig{BackupCadenceProfile: backups.BackupCadenceTesting},
			clock:                        clocktesting.NewFakePassiveClock(time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)),
		},
	}
}

func (r *recoveryTest) provider(t *testing.T) *coreapi.ServiceProviderCluster {
	t.Helper()
	spc, err := r.syncer.cosmosClient.ServiceProviderClusters(r.key.SubscriptionID, r.key.ResourceGroupName, r.key.HCPClusterName).Get(t.Context(), coreapi.ServiceProviderClusterResourceName)
	require.NoError(t, err)
	return spc
}

func (r *recoveryTest) seedSchedule(t *testing.T, paused, successful bool, raw []byte) {
	t.Helper()
	name := "hc-ns-10min"
	if raw == nil {
		schedule := &velerov1.Schedule{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: backup.VeleroNamespace}}
		schedule.Spec.Paused = paused
		var err error
		raw, err = json.Marshal(schedule)
		require.NoError(t, err)
	}
	rid, err := azcorearm.ParseResourceID(kubeapplierapihelpers.ToClusterScopedReadDesireResourceIDString(r.key.SubscriptionID, r.key.ResourceGroupName, r.key.HCPClusterName, backup.BackupScheduleDesireNamePrefix+"10min"))
	require.NoError(t, err)
	desire := &kubeapplierapi.ReadDesire{
		CosmosMetadata: coreapi.CosmosMetadata{ResourceID: rid, PartitionKey: strings.ToLower(r.spc.Status.ManagementClusterResourceID.String())},
		Spec:           kubeapplierapi.ReadDesireSpec{ManagementCluster: r.spc.Status.ManagementClusterResourceID},
		Status:         kubeapplierapi.ReadDesireStatus{KubeContent: &runtime.RawExtension{Raw: raw}},
	}
	if successful {
		desire.Status.Conditions = []metav1.Condition{{Type: kubeapplierapi.ConditionTypeSuccessful, Status: metav1.ConditionTrue}}
	}
	client, err := r.syncer.kubeApplierDBClients.For(t.Context(), r.spc.Status.ManagementClusterResourceID)
	require.NoError(t, err)
	crud, err := client.ReadDesiresForCluster(r.key.SubscriptionID, r.key.ResourceGroupName, r.key.HCPClusterName)
	require.NoError(t, err)
	_, err = crud.Create(t.Context(), desire, nil)
	require.NoError(t, err)
}

func TestRecoveryPauseBeforeDelivery(t *testing.T) {
	r := newRecoveryTest(t)
	ctx := t.Context()
	require.Nil(t, r.cluster.ServiceProviderProperties.ClusterServiceID, "recovery must work without Cluster Service")
	require.NoError(t, r.syncer.SyncOnce(ctx, r.key))
	require.NotNil(t, r.provider(t).Status.Recoveries[0].StartedAt)
	require.NoError(t, r.syncer.SyncOnce(ctx, r.key))
	require.Equal(t, coreapi.BackupScheduleStateDisabled, r.provider(t).Spec.BackupScheduleState)
	require.ErrorContains(t, r.syncer.SyncOnce(ctx, r.key), "waiting for backup schedules")
	desires, err := r.syncer.applyDesireLister.ListForCluster(ctx, r.key.SubscriptionID, r.key.ResourceGroupName, r.key.HCPClusterName)
	require.NoError(t, err)
	require.Empty(t, desires, "no recovery CR may be submitted before pause is observed")
	r.seedSchedule(t, true, true, nil)
	require.NoError(t, r.syncer.SyncOnce(ctx, r.key))
	desires, err = r.syncer.applyDesireLister.ListForCluster(ctx, r.key.SubscriptionID, r.key.ResourceGroupName, r.key.HCPClusterName)
	require.NoError(t, err)
	require.Len(t, desires, 1)
	require.Equal(t, coreapi.RecoveryStateRecoveryCRCreated, r.provider(t).Status.Recoveries[0].State)
	require.Equal(t, "recovery-r1", desires[0].ResourceID.Name)
	var recovery hcprecoveryv1alpha1.HCPRecovery
	require.NoError(t, json.Unmarshal(desires[0].Spec.ServerSideApply.KubeContent.Raw, &recovery))
	require.Equal(t, "hosted-cluster", recovery.Spec.HostedClusterName)
	require.Equal(t, r.cluster.ResourceID.String(), recovery.Spec.HcpClusterAzureResourceID)
	require.Equal(t, "b1", recovery.Spec.VeleroBackupName)
	require.Equal(t, "hc-ns", recovery.Spec.HostedClusterNamespace)
	require.Equal(t, "hc-ns-hosted-cluster", recovery.Spec.ControlPlaneNamespace)
	require.Equal(t, "hcp-recovery", recovery.Namespace)
	require.Equal(t, recovery.Name, desires[0].Spec.TargetItem.Name)

	rawCRD, err := os.ReadFile("../../../../../hcp-recovery/deploy/templates/hcprecovery-crd.yaml")
	require.NoError(t, err)
	var crd apiextensionsv1.CustomResourceDefinition
	require.NoError(t, yaml.Unmarshal(rawCRD, &crd))
	var schema apiextensions.JSONSchemaProps
	require.NoError(t, apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(crd.Spec.Versions[0].Schema.OpenAPIV3Schema, &schema, nil))
	validator, _, err := apiservervalidation.NewSchemaValidator(&schema)
	require.NoError(t, err)
	var content map[string]interface{}
	require.NoError(t, json.Unmarshal(desires[0].Spec.ServerSideApply.KubeContent.Raw, &content))
	require.Empty(t, apiservervalidation.ValidateCustomResource(field.NewPath(""), content, validator), "the submitted payload must satisfy the current CRD")

	providerETag, desireETag := r.provider(t).CosmosETag, desires[0].CosmosETag
	require.NoError(t, r.syncer.SyncOnce(ctx, r.key))
	require.Equal(t, providerETag, r.provider(t).CosmosETag, "unchanged recovery must not rewrite Cosmos")
	after, err := r.syncer.applyDesireLister.GetByResourceID(ctx, desires[0].ResourceID.String())
	require.NoError(t, err)
	require.Equal(t, desireETag, after.CosmosETag, "retries must not recreate or rewrite the apply desire")
}

func TestRecoveryPauseObservations(t *testing.T) {
	for _, tc := range []struct {
		name               string
		seed, paused, sync bool
		raw                []byte
		wantPaused         bool
		wantErr            bool
	}{
		{name: "missing"},
		{name: "unpaused", seed: true, sync: true},
		{name: "unobserved success", seed: true, paused: true},
		{name: "malformed", seed: true, sync: true, raw: []byte(`"not a schedule"`), wantErr: true},
		{name: "wrong schedule name", seed: true, sync: true, raw: []byte(`{"metadata":{"name":"other-10min","namespace":"velero"},"spec":{"paused":true}}`)},
		{name: "wrong schedule namespace", seed: true, sync: true, raw: []byte(`{"metadata":{"name":"hc-ns-10min","namespace":"other"},"spec":{"paused":true}}`)},
		{name: "paused", seed: true, paused: true, sync: true, wantPaused: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRecoveryTest(t)
			if tc.seed {
				r.seedSchedule(t, tc.paused, tc.sync, tc.raw)
			}
			paused, err := r.syncer.schedulesPaused(t.Context(), r.key, r.cluster, r.spc)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tc.wantPaused, paused)
		})
	}
}

func TestRecoveryEligibility(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*coreapi.Cluster, *coreapi.ServiceProviderCluster)
	}{
		{name: "no resource ID", change: func(c *coreapi.Cluster, _ *coreapi.ServiceProviderCluster) { c.ResourceID = nil }},
		{name: "deleting", change: func(c *coreapi.Cluster, _ *coreapi.ServiceProviderCluster) {
			now := metav1.Now()
			c.ServiceProviderProperties.DeletionTimestamp = &now
		}},
		{name: "no placement", change: func(_ *coreapi.Cluster, s *coreapi.ServiceProviderCluster) {
			s.Status.ManagementClusterResourceID = nil
		}},
		{name: "no hosted namespace", change: func(_ *coreapi.Cluster, s *coreapi.ServiceProviderCluster) { s.Status.HostedClusterNamespace = "" }},
		{name: "no control plane namespace", change: func(_ *coreapi.Cluster, s *coreapi.ServiceProviderCluster) { s.Status.ControlPlaneNamespace = "" }},
		{name: "no hosted name", change: func(c *coreapi.Cluster, _ *coreapi.ServiceProviderCluster) {
			c.CustomerProperties.DNS.BaseDomainPrefix = ""
		}},
		{name: "no request", change: func(_ *coreapi.Cluster, s *coreapi.ServiceProviderCluster) { s.Spec.RecoveryRequests = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRecoveryTest(t)
			require.True(t, needsWork(r.cluster, r.spc))
			tc.change(r.cluster, r.spc)
			require.False(t, needsWork(r.cluster, r.spc))
		})
	}
}

func TestRecoveryObservedPhases(t *testing.T) {
	for _, phase := range []hcprecoveryv1alpha1.RestoreState{
		hcprecoveryv1alpha1.RestoreStatePreparing, hcprecoveryv1alpha1.RestoreStateRestoring,
		hcprecoveryv1alpha1.RestoreStateValidating,
		hcprecoveryv1alpha1.RestoreStateCompleted, hcprecoveryv1alpha1.RestoreStateFailed,
	} {
		t.Run(string(phase), func(t *testing.T) {
			r := newRecoveryTest(t)
			ctx := t.Context()
			require.NoError(t, r.syncer.SyncOnce(ctx, r.key))
			require.NoError(t, r.syncer.SyncOnce(ctx, r.key))
			r.seedSchedule(t, true, true, nil)
			require.NoError(t, r.syncer.SyncOnce(ctx, r.key))
			_, desired, err := buildRecoveryDesires(r.key, r.cluster, r.spc, &r.spc.Spec.RecoveryRequests[0])
			require.NoError(t, err)
			client, err := r.syncer.kubeApplierDBClients.For(ctx, r.spc.Status.ManagementClusterResourceID)
			require.NoError(t, err)
			crud, err := client.ReadDesiresForCluster(r.key.SubscriptionID, r.key.ResourceGroupName, r.key.HCPClusterName)
			require.NoError(t, err)
			observed, err := crud.Get(ctx, desired.ResourceID.Name)
			require.NoError(t, err)
			completed := metav1.Now()
			recovery := hcprecoveryv1alpha1.HCPRecovery{Status: hcprecoveryv1alpha1.HCPRecoveryStatus{Phase: phase, CompletedAt: &completed}}
			raw, err := json.Marshal(recovery)
			require.NoError(t, err)
			observed.Status.KubeContent = &runtime.RawExtension{Raw: raw}
			_, err = crud.Replace(ctx, observed, nil)
			require.NoError(t, err)
			require.NoError(t, r.syncer.SyncOnce(ctx, r.key))
			spc := r.provider(t)
			switch phase {
			case hcprecoveryv1alpha1.RestoreStateCompleted:
				require.Equal(t, coreapi.RecoveryStateCompleted, spc.Status.Recoveries[0].State)
				require.Equal(t, coreapi.BackupScheduleStateEnabled, spc.Spec.BackupScheduleState)
				require.NotNil(t, spc.Status.Recoveries[0].CompletedAt)
			case hcprecoveryv1alpha1.RestoreStateFailed:
				require.Equal(t, coreapi.RecoveryStateFailed, spc.Status.Recoveries[0].State)
				require.Equal(t, coreapi.BackupScheduleStateDisabled, spc.Spec.BackupScheduleState)
				require.NotNil(t, spc.Status.Recoveries[0].CompletedAt)
			default:
				require.Equal(t, coreapi.RecoveryStateInProgress, spc.Status.Recoveries[0].State)
				require.Nil(t, spc.Status.Recoveries[0].CompletedAt)
			}
			require.NoError(t, r.syncer.SyncOnce(ctx, r.key))
			require.Equal(t, spc.CosmosETag, r.provider(t).CosmosETag, "terminal and unchanged monitoring states must not be rewritten")
		})
	}
}

func TestFindActiveRecovery(t *testing.T) {
	spc := &coreapi.ServiceProviderCluster{}
	request, _, err := findActiveRecovery(spc)
	require.NoError(t, err)
	require.Nil(t, request)
	spc.Spec.RecoveryRequests = []coreapi.RecoveryRequest{{RecoveryID: "r1"}, {RecoveryID: "r2"}}
	_, _, err = findActiveRecovery(spc)
	require.ErrorContains(t, err, "multiple active")
	spc.Status.Recoveries = []coreapi.RecoveryStatus{{RecoveryID: "r1", State: coreapi.RecoveryStateCompleted}}
	request, status, err := findActiveRecovery(spc)
	require.NoError(t, err)
	require.Equal(t, "r2", request.RecoveryID)
	require.Equal(t, coreapi.RecoveryStatePending, status.State)
	status.State = coreapi.RecoveryStateFailed
	request, _, err = findActiveRecovery(spc)
	require.NoError(t, err)
	require.Nil(t, request)
}

func TestRecoveryETagConflict(t *testing.T) {
	r := newRecoveryTest(t)
	stale := r.spc.DeepCopy()
	latest := r.spc.DeepCopy()
	latest.Spec.BackupScheduleState = coreapi.BackupScheduleStateDisabled
	crud := r.syncer.cosmosClient.ServiceProviderClusters(r.key.SubscriptionID, r.key.ResourceGroupName, r.key.HCPClusterName)
	latest, err := crud.Replace(t.Context(), latest, nil)
	require.NoError(t, err)
	stale.Status.Recoveries = []coreapi.RecoveryStatus{{RecoveryID: "r1", State: coreapi.RecoveryStatePending}}
	require.NoError(t, r.syncer.updateServiceProviderCluster(t.Context(), r.key, stale, r.spc))
	require.Equal(t, latest, r.provider(t), "a stale reconcile must not overwrite another controller's changes")
}
