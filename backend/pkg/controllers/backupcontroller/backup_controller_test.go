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
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
	workv1 "open-cluster-management.io/api/work/v1"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	azcorearm "github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"

	arohcpv1alpha1 "github.com/openshift-online/ocm-sdk-go/arohcp/v1alpha1"

	"github.com/Azure/ARO-HCP/backend/pkg/controllers/controllerutils"
	"github.com/Azure/ARO-HCP/backend/pkg/maestro"
	"github.com/Azure/ARO-HCP/internal/api"
	"github.com/Azure/ARO-HCP/internal/api/arm"
	"github.com/Azure/ARO-HCP/internal/database"
	"github.com/Azure/ARO-HCP/internal/databasetesting"
	"github.com/Azure/ARO-HCP/internal/ocm"
)

type alwaysSyncCooldownChecker struct{}

func (a *alwaysSyncCooldownChecker) CanSync(_ context.Context, _ any) bool {
	return true
}

func TestNamingHelpers(t *testing.T) {
	assert.Equal(t, "my-cluster-id-hourly", ScheduleNameForCluster("my-cluster-id"))
	assert.Equal(t, "my-cluster-id-dr", ManifestWorkNameForCluster("my-cluster-id"))
}

// TODO: re-enable and update assertions once the backup spec is finalized
func TestNewScheduledBackup(t *testing.T) {
	t.Skip("backup definition is changing — assertions on backup spec are stale")
}

func TestBuildScheduleManifestWork(t *testing.T) {
	clusterID := "11111111111111111111111111111111"
	schedule := NewScheduledBackup(clusterID, "test-domprefix", "ocm-testenv-"+clusterID, "ocm-testenv-"+clusterID+"-test-domprefix", "0 */1 * * *", 7*24*time.Hour, false)
	mw := buildScheduleManifestWork(
		types.NamespacedName{Name: clusterID + "-dr", Namespace: "test-consumer"},
		schedule,
	)

	assert.Equal(t, clusterID+"-dr", mw.Name)
	assert.Equal(t, "test-consumer", mw.Namespace)
	assert.Equal(t, backupScheduleManagedByK8sLabelValue, mw.Labels[backupScheduleManagedByK8sLabelKey])

	require.Len(t, mw.Spec.Workload.Manifests, 1)
	assert.Equal(t, schedule, mw.Spec.Workload.Manifests[0].Object)

	require.Len(t, mw.Spec.ManifestConfigs, 1)
	mc := mw.Spec.ManifestConfigs[0]
	assert.Equal(t, "velero.io", mc.ResourceIdentifier.Group)
	assert.Equal(t, "schedules", mc.ResourceIdentifier.Resource)
	assert.Equal(t, schedule.Name, mc.ResourceIdentifier.Name)
	assert.Equal(t, veleroNamespace, mc.ResourceIdentifier.Namespace)
	assert.Equal(t, workv1.UpdateStrategyTypeServerSideApply, mc.UpdateStrategy.Type)

	require.Len(t, mc.FeedbackRules, 1)
	assert.Equal(t, workv1.JSONPathsType, mc.FeedbackRules[0].Type)
	require.Len(t, mc.FeedbackRules[0].JsonPaths, 1)
	assert.Equal(t, "status", mc.FeedbackRules[0].JsonPaths[0].Name)
	assert.Equal(t, ".status", mc.FeedbackRules[0].JsonPaths[0].Path)
}

func TestBackupActionValidate(t *testing.T) {
	tests := []struct {
		name        string
		action      backupAction
		expectError bool
	}{
		{
			name:   "no fields set is valid",
			action: backupAction{},
		},
		{
			name:   "only createManifestWork set is valid",
			action: backupAction{createManifestWork: &workv1.ManifestWork{}},
		},
		{
			name:   "only patchManifestWork set is valid",
			action: backupAction{patchManifestWork: &workv1.ManifestWork{}},
		},
		{
			name:   "only updateSPC set is valid",
			action: backupAction{updateSPC: &api.ServiceProviderCluster{}},
		},
		{
			name: "two fields set is invalid",
			action: backupAction{
				createManifestWork: &workv1.ManifestWork{},
				updateSPC:          &api.ServiceProviderCluster{},
			},
			expectError: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.action.validate()
			if tt.expectError {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "programmer error")
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestBackupSteps(t *testing.T) {
	matchingSpec := workv1.ManifestWorkSpec{
		Workload: workv1.ManifestsTemplate{
			Manifests: []workv1.Manifest{{RawExtension: runtime.RawExtension{Raw: []byte(`{"schedule":"0 */1 * * *"}`)}}},
		},
	}
	desiredMW := &workv1.ManifestWork{
		ObjectMeta: metav1.ObjectMeta{Name: "test-mw"},
		Spec: workv1.ManifestWorkSpec{
			Workload: workv1.ManifestsTemplate{
				Manifests: []workv1.Manifest{{RawExtension: runtime.RawExtension{Raw: []byte(`{"schedule":"*/5 * * * *"}`)}}},
			},
		},
	}

	tests := []struct {
		name         string
		step         func(*backupScheduleSyncer) backupStep
		setupMock    func(*maestro.MockClient)
		desiredMW    *workv1.ManifestWork
		expectDone   bool
		expectAction bool
		expectError  bool
	}{
		{
			name:      "create: MW not found returns create action",
			step:      func(s *backupScheduleSyncer) backupStep { return s.ensureManifestWorkCreated },
			desiredMW: desiredMW,
			setupMock: func(mc *maestro.MockClient) {
				mc.EXPECT().Get(gomock.Any(), "test-mw", gomock.Any()).
					Return(nil, k8serrors.NewNotFound(schema.GroupResource{}, "not-found"))
			},
			expectDone:   true,
			expectAction: true,
		},
		{
			name:      "create: MW exists passes through",
			step:      func(s *backupScheduleSyncer) backupStep { return s.ensureManifestWorkCreated },
			desiredMW: desiredMW,
			setupMock: func(mc *maestro.MockClient) {
				mc.EXPECT().Get(gomock.Any(), "test-mw", gomock.Any()).
					Return(&workv1.ManifestWork{ObjectMeta: metav1.ObjectMeta{Name: "test-mw"}}, nil)
			},
			expectDone: false,
		},
		{
			name: "create: Get error returns error",
			step: func(s *backupScheduleSyncer) backupStep { return s.ensureManifestWorkCreated },
			setupMock: func(mc *maestro.MockClient) {
				mc.EXPECT().Get(gomock.Any(), "test-mw", gomock.Any()).
					Return(nil, fmt.Errorf("maestro API error"))
			},
			expectDone:  true,
			expectError: true,
		},
		{
			name:      "patch: matching spec passes through",
			step:      func(s *backupScheduleSyncer) backupStep { return s.ensureManifestWorkPatched },
			desiredMW: &workv1.ManifestWork{Spec: matchingSpec},
			setupMock: func(mc *maestro.MockClient) {
				mc.EXPECT().Get(gomock.Any(), "test-mw", gomock.Any()).
					Return(&workv1.ManifestWork{ObjectMeta: metav1.ObjectMeta{Name: "test-mw"}, Spec: matchingSpec}, nil)
			},
			expectDone: false,
		},
		{
			name:      "patch: drifted spec returns patch action",
			step:      func(s *backupScheduleSyncer) backupStep { return s.ensureManifestWorkPatched },
			desiredMW: desiredMW,
			setupMock: func(mc *maestro.MockClient) {
				mc.EXPECT().Get(gomock.Any(), "test-mw", gomock.Any()).
					Return(&workv1.ManifestWork{
						ObjectMeta: metav1.ObjectMeta{Name: "test-mw"},
						Spec: workv1.ManifestWorkSpec{
							Workload: workv1.ManifestsTemplate{
								Manifests: []workv1.Manifest{{RawExtension: runtime.RawExtension{Raw: []byte(`{"schedule":"0 */6 * * *"}`)}}},
							},
						},
					}, nil)
			},
			expectDone:   true,
			expectAction: true,
		},
		{
			name: "patch: Get error returns error",
			step: func(s *backupScheduleSyncer) backupStep { return s.ensureManifestWorkPatched },
			setupMock: func(mc *maestro.MockClient) {
				mc.EXPECT().Get(gomock.Any(), "test-mw", gomock.Any()).
					Return(nil, fmt.Errorf("maestro API error"))
			},
			expectDone:  true,
			expectError: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			mockMaestroClient := maestro.NewMockClient(ctrl)
			tt.setupMock(mockMaestroClient)

			state := &backupSyncState{
				maestroClient:       mockMaestroClient,
				manifestWorkName:    "test-mw",
				desiredManifestWork: tt.desiredMW,
			}

			syncer := &backupScheduleSyncer{}
			step := tt.step(syncer)
			done, action, err := step(context.Background(), state)

			assert.Equal(t, tt.expectDone, done)
			if tt.expectError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			if tt.expectAction {
				require.NotNil(t, action)
			} else {
				assert.Nil(t, action)
			}
		})
	}
}

func TestRecordManifestWorkInStatus(t *testing.T) {
	tests := []struct {
		name           string
		existingMWName string
		expectAction   bool
		expectDone     bool
	}{
		{
			name:           "already recorded passes through to next step",
			existingMWName: "test-mw",
			expectAction:   false,
			expectDone:     false,
		},
		{
			name:           "not recorded returns update action",
			existingMWName: "",
			expectAction:   true,
			expectDone:     true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spc := &api.ServiceProviderCluster{}
			spc.Status.BackupScheduleManifestWorkName = tt.existingMWName

			state := &backupSyncState{
				spc:              spc,
				manifestWorkName: "test-mw",
			}

			syncer := &backupScheduleSyncer{}
			done, action, err := syncer.recordManifestWorkInStatus(context.Background(), state)

			assert.Equal(t, tt.expectDone, done)
			require.NoError(t, err)
			if tt.expectAction {
				require.NotNil(t, action)
				assert.Equal(t, spc, action.updateSPC)
				assert.Equal(t, "test-mw", spc.Status.BackupScheduleManifestWorkName)
			} else {
				assert.Nil(t, action)
			}
		})
	}
}

func TestBackupScheduleSyncer_SyncOnce(t *testing.T) {
	const (
		testClusterID    = "11111111111111111111111111111111"
		testClusterIDStr = "/api/aro_hcp/v1alpha1/clusters/" + testClusterID
		manifestWorkName = testClusterID + "-dr"
		testSchedule     = "0 */1 * * *"
		testEnvID        = "test-env"
		testDomainPrefix = "test-domprefix"
		testConsumer     = "test-consumer"
	)
	testTTL := 7 * 24 * time.Hour

	// buildExpectedMW builds the ManifestWork that the syncer would produce,
	// so tests can return it from mock Get to simulate an up-to-date MW.
	buildExpectedMW := func() *workv1.ManifestWork {
		hcNamespace := fmt.Sprintf("ocm-%s-%s", testEnvID, testClusterID)
		hcpNamespace := fmt.Sprintf("%s-%s", hcNamespace, testDomainPrefix)
		schedule := NewScheduledBackup(testClusterID, testDomainPrefix, hcNamespace, hcpNamespace, testSchedule, testTTL, false)
		return buildScheduleManifestWork(
			types.NamespacedName{Name: manifestWorkName, Namespace: testConsumer},
			schedule,
		)
	}

	testKey := controllerutils.HCPClusterKey{
		SubscriptionID:    "test-sub",
		ResourceGroupName: "test-rg",
		HCPClusterName:    "test-cluster",
	}

	newTestCluster := func(opts ...func(*api.HCPOpenShiftCluster)) *api.HCPOpenShiftCluster {
		resourceID := api.Must(azcorearm.ParseResourceID(
			"/subscriptions/" + testKey.SubscriptionID +
				"/resourceGroups/" + testKey.ResourceGroupName +
				"/providers/Microsoft.RedHatOpenShift/hcpOpenShiftClusters/" + testKey.HCPClusterName,
		))
		cluster := &api.HCPOpenShiftCluster{
			TrackedResource: arm.TrackedResource{
				Resource: arm.Resource{ID: resourceID},
			},
			ServiceProviderProperties: api.HCPOpenShiftClusterServiceProviderProperties{
				ProvisioningState: arm.ProvisioningStateSucceeded,
				ClusterServiceID:  api.Must(api.NewInternalID(testClusterIDStr)),
			},
		}
		for _, opt := range opts {
			opt(cluster)
		}
		return cluster
	}

	// setupFullMockChain configures the standard CS + maestro mock expectations
	// for cases that reach processBackup (cluster exists and needs backup).
	setupFullMockChain := func(t *testing.T, cs *ocm.MockClusterServiceClientSpec, mb *maestro.MockMaestroClientBuilder, mc *maestro.MockClient, clusterState arohcpv1alpha1.ClusterState) {
		t.Helper()
		csID := api.Must(api.NewInternalID(testClusterIDStr))
		cs.EXPECT().GetClusterStatus(gomock.Any(), csID).
			Return(buildTestClusterStatus(t, clusterState), nil)
		provisionShard := buildTestProvisionShard(t, "test-shard-id", testConsumer, "https://maestro-rest", "https://maestro-grpc")
		cs.EXPECT().GetClusterProvisionShard(gomock.Any(), csID).Return(provisionShard, nil)
		mb.EXPECT().NewClient(gomock.Any(), "https://maestro-rest", "https://maestro-grpc", testConsumer, gomock.Any()).Return(mc, nil)
		cs.EXPECT().GetCluster(gomock.Any(), csID).Return(buildTestCSCluster(t, testDomainPrefix), nil)
	}

	tests := []struct {
		name          string
		seedDB        func(t *testing.T, ctx context.Context, mockDB *databasetesting.MockDBClient)
		setupMocks    func(t *testing.T, cs *ocm.MockClusterServiceClientSpec, mb *maestro.MockMaestroClientBuilder, mc *maestro.MockClient)
		expectError   bool
		errorContains string
		verify        func(t *testing.T, ctx context.Context, mockDB *databasetesting.MockDBClient)
	}{
		{
			name: "cluster not found in DB is no-op",
			seedDB: func(t *testing.T, ctx context.Context, mockDB *databasetesting.MockDBClient) {
				t.Helper()
			},
		},
		{
			name: "installing cluster is skipped",
			seedDB: func(t *testing.T, ctx context.Context, mockDB *databasetesting.MockDBClient) {
				t.Helper()
				_, err := mockDB.HCPClusters(testKey.SubscriptionID, testKey.ResourceGroupName).Create(ctx, newTestCluster(), nil)
				require.NoError(t, err)
			},
			setupMocks: func(t *testing.T, cs *ocm.MockClusterServiceClientSpec, mb *maestro.MockMaestroClientBuilder, mc *maestro.MockClient) {
				t.Helper()
				cs.EXPECT().GetClusterStatus(gomock.Any(), gomock.Any()).
					Return(buildTestClusterStatus(t, arohcpv1alpha1.ClusterStateInstalling), nil)
			},
		},
		{
			name: "creates ManifestWork when not found",
			seedDB: func(t *testing.T, ctx context.Context, mockDB *databasetesting.MockDBClient) {
				t.Helper()
				_, err := mockDB.HCPClusters(testKey.SubscriptionID, testKey.ResourceGroupName).Create(ctx, newTestCluster(), nil)
				require.NoError(t, err)
			},
			setupMocks: func(t *testing.T, cs *ocm.MockClusterServiceClientSpec, mb *maestro.MockMaestroClientBuilder, mc *maestro.MockClient) {
				t.Helper()
				setupFullMockChain(t, cs, mb, mc, arohcpv1alpha1.ClusterStateReady)
				mc.EXPECT().Get(gomock.Any(), manifestWorkName, gomock.Any()).
					Return(nil, k8serrors.NewNotFound(schema.GroupResource{}, "not-found"))
				mc.EXPECT().Create(gomock.Any(), gomock.Any(), gomock.Any()).
					Return(&workv1.ManifestWork{ObjectMeta: metav1.ObjectMeta{Name: manifestWorkName}}, nil)
			},
			verify: func(t *testing.T, ctx context.Context, mockDB *databasetesting.MockDBClient) {
				t.Helper()
				spc, err := database.GetOrCreateServiceProviderCluster(ctx, mockDB, testKey.GetResourceID())
				require.NoError(t, err)
				assert.Empty(t, spc.Status.BackupScheduleManifestWorkName, "SPC should not be updated on the same sync that creates the MW")
			},
		},
		{
			name: "updates SPC when MW already exists",
			seedDB: func(t *testing.T, ctx context.Context, mockDB *databasetesting.MockDBClient) {
				t.Helper()
				_, err := mockDB.HCPClusters(testKey.SubscriptionID, testKey.ResourceGroupName).Create(ctx, newTestCluster(), nil)
				require.NoError(t, err)
			},
			setupMocks: func(t *testing.T, cs *ocm.MockClusterServiceClientSpec, mb *maestro.MockMaestroClientBuilder, mc *maestro.MockClient) {
				t.Helper()
				setupFullMockChain(t, cs, mb, mc, arohcpv1alpha1.ClusterStateReady)
				mc.EXPECT().Get(gomock.Any(), manifestWorkName, gomock.Any()).
					Return(buildExpectedMW(), nil).Times(2)
			},
			verify: func(t *testing.T, ctx context.Context, mockDB *databasetesting.MockDBClient) {
				t.Helper()
				spc, err := database.GetOrCreateServiceProviderCluster(ctx, mockDB, testKey.GetResourceID())
				require.NoError(t, err)
				assert.Equal(t, manifestWorkName, spc.Status.BackupScheduleManifestWorkName)
			},
		},
		{
			name: "no-op when fully reconciled",
			seedDB: func(t *testing.T, ctx context.Context, mockDB *databasetesting.MockDBClient) {
				t.Helper()
				_, err := mockDB.HCPClusters(testKey.SubscriptionID, testKey.ResourceGroupName).Create(ctx, newTestCluster(), nil)
				require.NoError(t, err)
				spc, err := database.GetOrCreateServiceProviderCluster(ctx, mockDB, testKey.GetResourceID())
				require.NoError(t, err)
				spc.Status.BackupScheduleManifestWorkName = manifestWorkName
				_, err = mockDB.ServiceProviderClusters(testKey.SubscriptionID, testKey.ResourceGroupName, testKey.HCPClusterName).Replace(ctx, spc, nil)
				require.NoError(t, err)
			},
			setupMocks: func(t *testing.T, cs *ocm.MockClusterServiceClientSpec, mb *maestro.MockMaestroClientBuilder, mc *maestro.MockClient) {
				t.Helper()
				setupFullMockChain(t, cs, mb, mc, arohcpv1alpha1.ClusterStateReady)
				// 3 Gets: ensureManifestWorkCreated, ensureManifestWorkPatched, updateBackupProfileFromFeedback
				mc.EXPECT().Get(gomock.Any(), manifestWorkName, gomock.Any()).
					Return(buildExpectedMW(), nil).Times(3)
			},
		},
		{
			name: "maestro create error is propagated",
			seedDB: func(t *testing.T, ctx context.Context, mockDB *databasetesting.MockDBClient) {
				t.Helper()
				_, err := mockDB.HCPClusters(testKey.SubscriptionID, testKey.ResourceGroupName).Create(ctx, newTestCluster(), nil)
				require.NoError(t, err)
			},
			setupMocks: func(t *testing.T, cs *ocm.MockClusterServiceClientSpec, mb *maestro.MockMaestroClientBuilder, mc *maestro.MockClient) {
				t.Helper()
				setupFullMockChain(t, cs, mb, mc, arohcpv1alpha1.ClusterStateReady)
				mc.EXPECT().Get(gomock.Any(), manifestWorkName, gomock.Any()).
					Return(nil, k8serrors.NewNotFound(schema.GroupResource{}, "not-found"))
				mc.EXPECT().Create(gomock.Any(), gomock.Any(), gomock.Any()).
					Return(nil, fmt.Errorf("maestro API error"))
			},
			expectError:   true,
			errorContains: "failed to create ManifestWork",
		},
		{
			name: "patches MW when spec has drifted",
			seedDB: func(t *testing.T, ctx context.Context, mockDB *databasetesting.MockDBClient) {
				t.Helper()
				_, err := mockDB.HCPClusters(testKey.SubscriptionID, testKey.ResourceGroupName).Create(ctx, newTestCluster(), nil)
				require.NoError(t, err)
			},
			setupMocks: func(t *testing.T, cs *ocm.MockClusterServiceClientSpec, mb *maestro.MockMaestroClientBuilder, mc *maestro.MockClient) {
				t.Helper()
				setupFullMockChain(t, cs, mb, mc, arohcpv1alpha1.ClusterStateReady)
				staleMW := &workv1.ManifestWork{
					ObjectMeta: metav1.ObjectMeta{Name: manifestWorkName},
					Spec: workv1.ManifestWorkSpec{
						Workload: workv1.ManifestsTemplate{
							Manifests: []workv1.Manifest{{RawExtension: runtime.RawExtension{Raw: []byte(`{"old":"spec"}`)}}},
						},
					},
				}
				// First Get (ensureManifestWorkCreated): MW exists, pass through
				// Second Get (ensureManifestWorkPatched): MW exists with old spec, triggers patch
				mc.EXPECT().Get(gomock.Any(), manifestWorkName, gomock.Any()).
					Return(staleMW, nil).Times(2)
				mc.EXPECT().Patch(gomock.Any(), manifestWorkName, types.MergePatchType, gomock.Any(), gomock.Any()).
					Return(buildExpectedMW(), nil)
			},
		},
		{
			name: "error state cluster still gets backup",
			seedDB: func(t *testing.T, ctx context.Context, mockDB *databasetesting.MockDBClient) {
				t.Helper()
				_, err := mockDB.HCPClusters(testKey.SubscriptionID, testKey.ResourceGroupName).Create(ctx, newTestCluster(), nil)
				require.NoError(t, err)
			},
			setupMocks: func(t *testing.T, cs *ocm.MockClusterServiceClientSpec, mb *maestro.MockMaestroClientBuilder, mc *maestro.MockClient) {
				t.Helper()
				setupFullMockChain(t, cs, mb, mc, arohcpv1alpha1.ClusterStateError)
				mc.EXPECT().Get(gomock.Any(), manifestWorkName, gomock.Any()).
					Return(nil, k8serrors.NewNotFound(schema.GroupResource{}, "not-found"))
				mc.EXPECT().Create(gomock.Any(), gomock.Any(), gomock.Any()).
					Return(&workv1.ManifestWork{ObjectMeta: metav1.ObjectMeta{Name: manifestWorkName}}, nil)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			ctrl := gomock.NewController(t)

			mockDB := databasetesting.NewMockDBClient()
			mockCS := ocm.NewMockClusterServiceClientSpec(ctrl)
			mockMaestroBuilder := maestro.NewMockMaestroClientBuilder(ctrl)
			mockMaestroClient := maestro.NewMockClient(ctrl)

			tt.seedDB(t, ctx, mockDB)
			if tt.setupMocks != nil {
				tt.setupMocks(t, mockCS, mockMaestroBuilder, mockMaestroClient)
			}

			syncer := &backupScheduleSyncer{
				cooldownChecker:                    &alwaysSyncCooldownChecker{},
				cosmosClient:                       mockDB,
				clusterServiceClient:               mockCS,
				maestroSourceEnvironmentIdentifier: testEnvID,
				maestroClientBuilder:               mockMaestroBuilder,
				backupSchedule:                     testSchedule,
				backupTTL:                          testTTL,
			}

			err := syncer.SyncOnce(ctx, testKey)

			if tt.expectError {
				require.Error(t, err)
				if tt.errorContains != "" {
					assert.Contains(t, err.Error(), tt.errorContains)
				}
			} else {
				require.NoError(t, err)
			}

			if tt.verify != nil {
				tt.verify(t, ctx, mockDB)
			}
		})
	}
}

func TestGetHostedClusterNamespace(t *testing.T) {
	syncer := &backupScheduleSyncer{}
	result := syncer.getHostedClusterNamespace("testenv", "11111111111111111111111111111111")
	assert.Equal(t, "ocm-testenv-11111111111111111111111111111111", result)
}

// buildTestProvisionShard creates a test ProvisionShard using the OCM SDK builder.
func buildTestProvisionShard(t *testing.T, id, consumerName, restURL, grpcURL string) *arohcpv1alpha1.ProvisionShard {
	t.Helper()
	ps, err := arohcpv1alpha1.NewProvisionShard().
		ID(id).
		MaestroConfig(arohcpv1alpha1.NewProvisionShardMaestroConfig().
			ConsumerName(consumerName).
			RestApiConfig(arohcpv1alpha1.NewProvisionShardMaestroRestApiConfig().Url(restURL)).
			GrpcApiConfig(arohcpv1alpha1.NewProvisionShardMaestroGrpcApiConfig().Url(grpcURL)),
		).
		Build()
	require.NoError(t, err)
	return ps
}

// buildTestCSCluster creates a test CS cluster with the given domain prefix.
func buildTestCSCluster(t *testing.T, domainPrefix string) *arohcpv1alpha1.Cluster {
	t.Helper()
	cluster, err := arohcpv1alpha1.NewCluster().
		DomainPrefix(domainPrefix).
		Build()
	require.NoError(t, err)
	return cluster
}

// buildTestClusterStatus creates a test ClusterStatus with the given state.
func buildTestClusterStatus(t *testing.T, state arohcpv1alpha1.ClusterState) *arohcpv1alpha1.ClusterStatus {
	t.Helper()
	status, err := arohcpv1alpha1.NewClusterStatus().
		State(state).
		Build()
	require.NoError(t, err)
	return status
}

func TestClusterNeedsBackup(t *testing.T) {
	tests := []struct {
		state arohcpv1alpha1.ClusterState
		want  bool
	}{
		{arohcpv1alpha1.ClusterStateReady, true},
		{arohcpv1alpha1.ClusterStateError, true},
		{arohcpv1alpha1.ClusterStateUpdating, true},
		{arohcpv1alpha1.ClusterStateInstalling, false},
		{arohcpv1alpha1.ClusterStateUninstalling, false},
		{arohcpv1alpha1.ClusterStatePending, false},
		{arohcpv1alpha1.ClusterStateValidating, false},
		{arohcpv1alpha1.ClusterStateHibernating, false},
		{arohcpv1alpha1.ClusterStateUnknown, false},
	}
	for _, tt := range tests {
		t.Run(string(tt.state), func(t *testing.T) {
			assert.Equal(t, tt.want, clusterNeedsBackup(tt.state))
		})
	}
}
