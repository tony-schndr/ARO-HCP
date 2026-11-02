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

package hcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	azcorearm "github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"

	hcprecoveryv1alpha1 "github.com/Azure/ARO-HCP/hcp-recovery/pkg/apis/hcprecovery/v1alpha1"
	"github.com/Azure/ARO-HCP/internal/api/coreapi"
	"github.com/Azure/ARO-HCP/internal/api/kubeapplierapi"
	"github.com/Azure/ARO-HCP/internal/api/metadataapi"
	"github.com/Azure/ARO-HCP/internal/apihelpers/kubeapplierapihelpers"
	"github.com/Azure/ARO-HCP/internal/apitesting/coreapitesting"
	"github.com/Azure/ARO-HCP/internal/backup"
	"github.com/Azure/ARO-HCP/internal/database/cosmosstorage/corecosmosstorage"
	"github.com/Azure/ARO-HCP/internal/database/cosmosstorage/cosmosstorageutils"
	"github.com/Azure/ARO-HCP/internal/database/cosmosstoragetesting/corecosmosstoragetesting"
	"github.com/Azure/ARO-HCP/internal/database/cosmosstoragetesting/kubeappliercosmosstoragetesting"
	"github.com/Azure/ARO-HCP/internal/utils"
)

func TestRestoreHandlers(t *testing.T) {
	ctx := t.Context()
	rid, err := azcorearm.ParseResourceID(coreapitesting.TestClusterResourceID)
	require.NoError(t, err)
	ctx = utils.ContextWithResourceID(ctx, rid)
	db := corecosmosstoragetesting.NewMockResourcesDBClient()
	seedHCPCluster(ctx, t, db, rid, "4.20", "")
	spc, err := corecosmosstorage.GetOrCreateServiceProviderCluster(ctx, db, rid)
	require.NoError(t, err)
	spc.Spec.BackupScheduleState = coreapi.BackupScheduleStateDisabled
	storage := db.ServiceProviderClusters(rid.SubscriptionID, rid.ResourceGroupName, rid.Name)
	spc, err = storage.Replace(ctx, spc, nil)
	require.NoError(t, err)

	post := NewPostRestoreHandler(db)
	recorder := httptest.NewRecorder()
	require.NoError(t, post.ServeHTTP(recorder, httptest.NewRequest("POST", "/restore", strings.NewReader(`{"backupID":"backup-1"}`)).WithContext(ctx)))
	var response RestoreResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	require.Equal(t, coreapi.RecoveryStatePending, response.RecoveryState)
	require.NotEmpty(t, response.RecoveryID)
	persisted, err := storage.Get(ctx, coreapi.ServiceProviderClusterResourceName)
	require.NoError(t, err)
	require.Equal(t, spc.Spec.BackupScheduleState, persisted.Spec.BackupScheduleState)
	require.Empty(t, persisted.Status.Recoveries, "the endpoint must only write intent")
	require.Equal(t, []coreapi.RecoveryRequest{{RecoveryID: response.RecoveryID, VeleroBackupName: "backup-1"}}, persisted.Spec.RecoveryRequests)

	err = post.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/restore", strings.NewReader(`{"backupID":"backup-2"}`)).WithContext(ctx))
	var cloudErr *coreapi.CloudError
	require.ErrorAs(t, err, &cloudErr)
	require.Equal(t, http.StatusConflict, cloudErr.StatusCode)

	completed := metav1.NewTime(time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC))
	persisted.Status.Recoveries = []coreapi.RecoveryStatus{{RecoveryID: response.RecoveryID, State: coreapi.RecoveryStateCompleted, StartedAt: &completed, CompletedAt: &completed}}
	_, err = storage.Replace(ctx, persisted, nil)
	require.NoError(t, err)
	getRecorder := httptest.NewRecorder()
	getHandler := NewGetRestoreStatusHandler(db, kubeappliercosmosstoragetesting.NewMockKubeApplierDBClients())
	require.NoError(t, getHandler.ServeHTTP(getRecorder, httptest.NewRequest("GET", "/restore?recoveryID="+response.RecoveryID, nil).WithContext(ctx)))
	require.NoError(t, json.Unmarshal(getRecorder.Body.Bytes(), &response))
	require.Equal(t, coreapi.RecoveryStateCompleted, response.RecoveryState)
	require.Equal(t, "backup-1", response.BackupID)
	require.Equal(t, "2026-11-02T12:00:00Z", response.CompletedAt)
	require.NoError(t, post.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/restore", strings.NewReader(`{"backupID":"backup-2"}`)).WithContext(ctx)), "a completed recovery must not prevent a new request")
}

func TestRestoreHandlerErrors(t *testing.T) {
	for _, tc := range []struct {
		name, method, url, body string
		cluster, provider       bool
		active, deleting        bool
		status                  int
	}{
		{name: "invalid JSON", method: "POST", body: "{", status: http.StatusBadRequest},
		{name: "missing backup", method: "POST", body: "{}", status: http.StatusBadRequest},
		{name: "missing cluster", method: "POST", body: `{"backupID":"b"}`, status: http.StatusNotFound},
		{name: "missing provider", method: "POST", body: `{"backupID":"b"}`, cluster: true, status: http.StatusNotFound},
		{name: "deleting", method: "POST", body: `{"backupID":"b"}`, cluster: true, provider: true, deleting: true, status: http.StatusConflict},
		{name: "active recovery", method: "POST", body: `{"backupID":"b"}`, cluster: true, provider: true, active: true, status: http.StatusConflict},
		// recoveryID is optional (omitting it lists every recovery), so a GET
		// without it fails only on the cluster lookup.
		{name: "missing cluster on list", method: "GET", status: http.StatusNotFound},
		{name: "unknown recovery", method: "GET", url: "/restore?recoveryID=absent", cluster: true, provider: true, status: http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			rid, err := azcorearm.ParseResourceID(coreapitesting.TestClusterResourceID)
			require.NoError(t, err)
			ctx = utils.ContextWithResourceID(ctx, rid)
			db := corecosmosstoragetesting.NewMockResourcesDBClient()
			if tc.cluster {
				seedHCPCluster(ctx, t, db, rid, "4.20", "")
			}
			if tc.deleting {
				crud := db.HCPClusters(rid.SubscriptionID, rid.ResourceGroupName)
				cluster, err := crud.Get(ctx, rid.Name)
				require.NoError(t, err)
				now := metav1.Now()
				cluster.ServiceProviderProperties.DeletionTimestamp = &now
				_, err = crud.Replace(ctx, cluster, nil)
				require.NoError(t, err)
			}
			if tc.provider {
				spc, err := corecosmosstorage.GetOrCreateServiceProviderCluster(ctx, db, rid)
				require.NoError(t, err)
				if tc.active {
					spc.Spec.RecoveryRequests = []coreapi.RecoveryRequest{{RecoveryID: "r1", VeleroBackupName: "b1"}}
					spc.Status.Recoveries = []coreapi.RecoveryStatus{{RecoveryID: "r1", State: coreapi.RecoveryStateInProgress}}
					_, err = db.ServiceProviderClusters(rid.SubscriptionID, rid.ResourceGroupName, rid.Name).Replace(ctx, spc, nil)
					require.NoError(t, err)
				}
			}
			url := tc.url
			if url == "" {
				url = "/restore"
			}
			req := httptest.NewRequest(tc.method, url, strings.NewReader(tc.body)).WithContext(ctx)
			if tc.method == "POST" {
				err = NewPostRestoreHandler(db).ServeHTTP(httptest.NewRecorder(), req)
			} else {
				err = NewGetRestoreStatusHandler(db, kubeappliercosmosstoragetesting.NewMockKubeApplierDBClients()).ServeHTTP(httptest.NewRecorder(), req)
			}
			var cloudErr *coreapi.CloudError
			require.ErrorAs(t, err, &cloudErr)
			require.Equal(t, tc.status, cloudErr.StatusCode)
		})
	}
}

type restoreConcurrentClient struct {
	corecosmosstorage.ResourcesDBClient
	crud *restoreConcurrentCRUD
}

func (c *restoreConcurrentClient) ServiceProviderClusters(_, _, _ string) cosmosstorageutils.ResourceCRUD[coreapi.ServiceProviderCluster, *coreapi.ServiceProviderCluster] {
	return c.crud
}

type restoreConcurrentCRUD struct {
	cosmosstorageutils.ResourceCRUD[coreapi.ServiceProviderCluster, *coreapi.ServiceProviderCluster]
}

func (c *restoreConcurrentCRUD) Get(ctx context.Context, name string) (*coreapi.ServiceProviderCluster, error) {
	old, err := c.ResourceCRUD.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	concurrent := old.DeepCopy()
	concurrent.Spec.RecoveryRequests = append(concurrent.Spec.RecoveryRequests, coreapi.RecoveryRequest{RecoveryID: "other", VeleroBackupName: "other-backup"})
	if _, err = c.Replace(ctx, concurrent, nil); err != nil {
		return nil, err
	}
	return old, nil
}

func TestRestoreConcurrentEnqueue(t *testing.T) {
	ctx := t.Context()
	rid, err := azcorearm.ParseResourceID(coreapitesting.TestClusterResourceID)
	require.NoError(t, err)
	ctx = utils.ContextWithResourceID(ctx, rid)
	db := corecosmosstoragetesting.NewMockResourcesDBClient()
	seedHCPCluster(ctx, t, db, rid, "4.20", "")
	_, err = corecosmosstorage.GetOrCreateServiceProviderCluster(ctx, db, rid)
	require.NoError(t, err)
	storage := db.ServiceProviderClusters(rid.SubscriptionID, rid.ResourceGroupName, rid.Name)
	client := &restoreConcurrentClient{ResourcesDBClient: db, crud: &restoreConcurrentCRUD{ResourceCRUD: storage}}
	err = NewPostRestoreHandler(client).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/restore", strings.NewReader(`{"backupID":"b"}`)).WithContext(ctx))
	var cloudErr *coreapi.CloudError
	require.ErrorAs(t, err, &cloudErr)
	require.Equal(t, http.StatusConflict, cloudErr.StatusCode)
	after, err := storage.Get(ctx, coreapi.ServiceProviderClusterResourceName)
	require.NoError(t, err)
	require.Equal(t, []coreapi.RecoveryRequest{{RecoveryID: "other", VeleroBackupName: "other-backup"}}, after.Spec.RecoveryRequests)
}

// restoreManagementClusterResourceID is the placement every observed-status test
// registers its kube-applier client under.
var restoreManagementClusterResourceID = metadataapi.Must(azcorearm.ParseResourceID(
	"/subscriptions/" + coreapitesting.TestSubscriptionID + "/resourceGroups/mgmt-rg/providers/Microsoft.ContainerService/managedClusters/mgmt-cluster",
))

// newRecoveryReadDesire builds the ReadDesire the backend Recovery controller
// creates for recoveryID, carrying status as the mirrored HCPRecovery. A nil
// status leaves kubeContent empty, which is what an unobserved CR looks like.
func newRecoveryReadDesire(t *testing.T, recoveryID string, status *hcprecoveryv1alpha1.HCPRecoveryStatus) *kubeapplierapi.ReadDesire {
	t.Helper()
	resourceID := metadataapi.Must(azcorearm.ParseResourceID(
		kubeapplierapihelpers.ToClusterScopedReadDesireResourceIDString(
			coreapitesting.TestSubscriptionID, coreapitesting.TestResourceGroupName, coreapitesting.TestClusterName,
			backup.RecoveryDesireName(recoveryID),
		),
	))
	readDesire := &kubeapplierapi.ReadDesire{
		CosmosMetadata: coreapi.CosmosMetadata{
			ResourceID:   resourceID,
			PartitionKey: strings.ToLower(restoreManagementClusterResourceID.String()),
		},
		Spec: kubeapplierapi.ReadDesireSpec{
			ManagementCluster: restoreManagementClusterResourceID,
			TargetItem:        kubeapplierapi.ResourceReference{Name: recoveryID},
		},
		Tags: map[string]string{kubeapplierapi.TagControllerName: "Recovery"},
	}
	if status != nil {
		raw, err := json.Marshal(&hcprecoveryv1alpha1.HCPRecovery{
			ObjectMeta: metav1.ObjectMeta{Name: recoveryID, Namespace: hcprecoveryv1alpha1.HCPRecoveryNamespace},
			Status:     *status,
		})
		require.NoError(t, err)
		readDesire.Status.KubeContent = &runtime.RawExtension{Raw: raw}
	}
	return readDesire
}

// seedRestoreCluster creates the HCP and its ServiceProviderCluster carrying the
// given recovery requests and statuses. setPlacement controls whether the
// management cluster has been resolved, which is what gates the ReadDesire read.
func seedRestoreCluster(
	ctx context.Context,
	t *testing.T,
	db *corecosmosstoragetesting.MockResourcesDBClient,
	resourceID *azcorearm.ResourceID,
	setPlacement bool,
	requests []coreapi.RecoveryRequest,
	statuses []coreapi.RecoveryStatus,
) {
	t.Helper()
	seedHCPCluster(ctx, t, db, resourceID, "4.20", "")
	spc, err := corecosmosstorage.GetOrCreateServiceProviderCluster(ctx, db, resourceID)
	require.NoError(t, err)
	if setPlacement {
		spc.Status.ManagementClusterResourceID = restoreManagementClusterResourceID
	}
	spc.Spec.RecoveryRequests = requests
	spc.Status.Recoveries = statuses
	_, err = db.ServiceProviderClusters(resourceID.SubscriptionID, resourceID.ResourceGroupName, resourceID.Name).Replace(ctx, spc, nil)
	require.NoError(t, err)
}

func TestGetRestoreStatusObservedFromReadDesire(t *testing.T) {
	started := metav1.NewTime(time.Date(2026, 11, 13, 18, 3, 40, 0, time.UTC))
	observed := &hcprecoveryv1alpha1.HCPRecoveryStatus{
		Phase:       hcprecoveryv1alpha1.RestoreStateRestoring,
		StartedAt:   &started,
		RestoreName: "r1-restore",
		EtcdSnapshot: &hcprecoveryv1alpha1.EtcdSnapshotDetails{
			SourceMember: "etcd-0",
			SHA256:       strings.Repeat("a", 64),
			Revision:     48213,
		},
		Conditions: []metav1.Condition{
			{
				Type: hcprecoveryv1alpha1.ConditionEtcdSnapshotDistributed, Status: metav1.ConditionFalse,
				Reason: "Copying", Message: "2/3 volumes verified", LastTransitionTime: started,
			},
		},
	}

	for _, tc := range []struct {
		name         string
		setPlacement bool
		registerKube bool
		readDesire   *kubeapplierapi.ReadDesire
		expectStatus *hcprecoveryv1alpha1.HCPRecoveryStatus
	}{
		{
			// A Pending recovery is polled before placement resolves; the
			// endpoint must still answer rather than failing the precondition.
			name: "placement unresolved still returns the cosmos fields",
		},
		{
			name: "kube-applier client unavailable still returns the cosmos fields", setPlacement: true,
		},
		{
			name: "no ReadDesire yet leaves status nil", setPlacement: true, registerKube: true,
		},
		{
			name: "ReadDesire without kubeContent leaves status nil", setPlacement: true, registerKube: true,
			readDesire: newRecoveryReadDesire(t, "r1", nil),
		},
		{
			name: "observed HCPRecovery status is proxied verbatim", setPlacement: true, registerKube: true,
			readDesire: newRecoveryReadDesire(t, "r1", observed), expectStatus: observed,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			rid, err := azcorearm.ParseResourceID(coreapitesting.TestClusterResourceID)
			require.NoError(t, err)
			ctx = utils.ContextWithResourceID(ctx, rid)

			db := corecosmosstoragetesting.NewMockResourcesDBClient()
			seedRestoreCluster(ctx, t, db, rid, tc.setPlacement,
				[]coreapi.RecoveryRequest{{RecoveryID: "r1", VeleroBackupName: "backup-1"}},
				[]coreapi.RecoveryStatus{{RecoveryID: "r1", State: coreapi.RecoveryStateInProgress}},
			)

			kubeClients := kubeappliercosmosstoragetesting.NewMockKubeApplierDBClients()
			if tc.registerKube {
				kubeClient := kubeappliercosmosstoragetesting.NewMockKubeApplierDBClient()
				kubeClients.Register(restoreManagementClusterResourceID, kubeClient)
				if tc.readDesire != nil {
					crud, err := kubeClient.ReadDesiresForCluster(rid.SubscriptionID, rid.ResourceGroupName, rid.Name)
					require.NoError(t, err)
					_, err = crud.Create(ctx, tc.readDesire, nil)
					require.NoError(t, err)
				}
			}

			recorder := httptest.NewRecorder()
			require.NoError(t, NewGetRestoreStatusHandler(db, kubeClients).
				ServeHTTP(recorder, httptest.NewRequest("GET", "/restore?recoveryID=r1", nil).WithContext(ctx)))
			require.Equal(t, http.StatusOK, recorder.Code)

			var response RestoreResponse
			require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
			require.Equal(t, "r1", response.RecoveryID)
			require.Equal(t, "backup-1", response.BackupID)
			require.Equal(t, coreapi.RecoveryStateInProgress, response.RecoveryState)

			// Compare the serialized forms: a metav1.Time survives the round
			// trip as the same instant but not the same *time.Location, so
			// require.Equal on the structs would fail on the pointer alone.
			if tc.expectStatus == nil {
				require.Nil(t, response.Status)
				return
			}
			require.NotNil(t, response.Status)
			expected, err := json.Marshal(tc.expectStatus)
			require.NoError(t, err)
			actual, err := json.Marshal(response.Status)
			require.NoError(t, err)
			require.JSONEq(t, string(expected), string(actual))
		})
	}
}

func TestGetRestoreStatusListsEveryRecoveryNewestFirst(t *testing.T) {
	ctx := t.Context()
	rid, err := azcorearm.ParseResourceID(coreapitesting.TestClusterResourceID)
	require.NoError(t, err)
	ctx = utils.ContextWithResourceID(ctx, rid)

	completed := metav1.NewTime(time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC))
	db := corecosmosstoragetesting.NewMockResourcesDBClient()
	seedRestoreCluster(ctx, t, db, rid, true,
		[]coreapi.RecoveryRequest{
			{RecoveryID: "r1", VeleroBackupName: "backup-1"},
			{RecoveryID: "r2", VeleroBackupName: "backup-2"},
		},
		[]coreapi.RecoveryStatus{
			{RecoveryID: "r1", State: coreapi.RecoveryStateCompleted, StartedAt: &completed, CompletedAt: &completed},
			{RecoveryID: "r2", State: coreapi.RecoveryStateInProgress},
		},
	)

	// Only the older recovery has an observed HCPRecovery, so the list must
	// carry a status for it and leave the newer one's nil.
	kubeClients := kubeappliercosmosstoragetesting.NewMockKubeApplierDBClients()
	kubeClient := kubeappliercosmosstoragetesting.NewMockKubeApplierDBClient()
	kubeClients.Register(restoreManagementClusterResourceID, kubeClient)
	crud, err := kubeClient.ReadDesiresForCluster(rid.SubscriptionID, rid.ResourceGroupName, rid.Name)
	require.NoError(t, err)
	_, err = crud.Create(ctx, newRecoveryReadDesire(t, "r1", &hcprecoveryv1alpha1.HCPRecoveryStatus{
		Phase: hcprecoveryv1alpha1.RestoreStateCompleted,
	}), nil)
	require.NoError(t, err)

	recorder := httptest.NewRecorder()
	require.NoError(t, NewGetRestoreStatusHandler(db, kubeClients).
		ServeHTTP(recorder, httptest.NewRequest("GET", "/restore", nil).WithContext(ctx)))
	require.Equal(t, http.StatusOK, recorder.Code)

	var response RestoreListResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	require.Equal(t, rid.String(), response.ResourceID)
	require.Len(t, response.Recoveries, 2)

	require.Equal(t, "r2", response.Recoveries[0].RecoveryID, "requests are appended, so the newest must come first")
	require.Equal(t, coreapi.RecoveryStateInProgress, response.Recoveries[0].RecoveryState)
	require.Nil(t, response.Recoveries[0].Status)

	require.Equal(t, "r1", response.Recoveries[1].RecoveryID)
	require.Equal(t, "2026-11-02T12:00:00Z", response.Recoveries[1].CompletedAt)
	require.NotNil(t, response.Recoveries[1].Status)
	require.Equal(t, hcprecoveryv1alpha1.RestoreStateCompleted, response.Recoveries[1].Status.Phase)
}

func TestGetRestoreStatusListsEmptyWhenNoRecoveries(t *testing.T) {
	ctx := t.Context()
	rid, err := azcorearm.ParseResourceID(coreapitesting.TestClusterResourceID)
	require.NoError(t, err)
	ctx = utils.ContextWithResourceID(ctx, rid)

	db := corecosmosstoragetesting.NewMockResourcesDBClient()
	seedRestoreCluster(ctx, t, db, rid, true, nil, nil)

	recorder := httptest.NewRecorder()
	require.NoError(t, NewGetRestoreStatusHandler(db, kubeappliercosmosstoragetesting.NewMockKubeApplierDBClients()).
		ServeHTTP(recorder, httptest.NewRequest("GET", "/restore", nil).WithContext(ctx)))
	require.JSONEq(t, `{"resourceID":"`+rid.String()+`","recoveries":[]}`, recorder.Body.String())
}
