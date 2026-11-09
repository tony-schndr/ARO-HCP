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
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/require"
	velerov1api "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	azcorearm "github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"

	hyperv1beta1 "github.com/openshift/hypershift/api/hypershift/v1beta1"

	"github.com/Azure/ARO-HCP/internal/api/coreapi"
	"github.com/Azure/ARO-HCP/internal/api/kubeapplierapi"
	"github.com/Azure/ARO-HCP/internal/api/metadataapi"
	"github.com/Azure/ARO-HCP/internal/apihelpers/kubeapplierapihelpers"
	"github.com/Azure/ARO-HCP/internal/apitesting/coreapitesting"
	"github.com/Azure/ARO-HCP/internal/backup"
	"github.com/Azure/ARO-HCP/internal/controllerutils"
	"github.com/Azure/ARO-HCP/internal/database/cosmosstorage/corecosmosstorage"
	"github.com/Azure/ARO-HCP/internal/database/cosmosstorage/cosmosstorageutils"
	"github.com/Azure/ARO-HCP/internal/database/cosmosstoragetesting/corecosmosstoragetesting"
	"github.com/Azure/ARO-HCP/internal/database/cosmosstoragetesting/kubeappliercosmosstoragetesting"
	"github.com/Azure/ARO-HCP/internal/utils"
)

func TestPatchBackupScheduleHandler(t *testing.T) {
	tests := []struct {
		name               string
		body               io.Reader
		skipResourceID     bool
		seedHCP            bool
		seedSPC            bool
		existingState      coreapi.BackupScheduleState
		expectedStatusCode int
		expectedError      string
		expectedState      coreapi.BackupScheduleState
	}{
		{
			name:               "invalid JSON body",
			body:               strings.NewReader(`{not json`),
			expectedStatusCode: http.StatusBadRequest,
			expectedError:      "invalid JSON body",
		},
		{
			name:               "invalid state value",
			body:               strings.NewReader(`{"state":"InvalidState"}`),
			expectedStatusCode: http.StatusBadRequest,
			expectedError:      `invalid state "InvalidState"`,
		},
		{
			name:               "empty state",
			body:               strings.NewReader(`{"state":""}`),
			expectedStatusCode: http.StatusBadRequest,
			expectedError:      `invalid state ""`,
		},
		{
			name:           "missing resource ID in context",
			body:           strings.NewReader(`{"state":"Enabled"}`),
			skipResourceID: true,
			expectedError:  "failed to resolve HCP context",
		},
		{
			name:               "HCP cluster not found",
			body:               strings.NewReader(`{"state":"Enabled"}`),
			expectedStatusCode: http.StatusNotFound,
			expectedError:      "not found",
		},
		{
			name:               "ServiceProviderCluster not found",
			body:               strings.NewReader(`{"state":"Enabled"}`),
			seedHCP:            true,
			expectedStatusCode: http.StatusNotFound,
			expectedError:      "ServiceProviderCluster not found",
		},
		{
			name:               "set Enabled on SPC with no prior state",
			body:               strings.NewReader(`{"state":"Enabled"}`),
			seedHCP:            true,
			seedSPC:            true,
			expectedStatusCode: http.StatusOK,
			expectedState:      coreapi.BackupScheduleStateEnabled,
		},
		{
			name:               "set Disabled on SPC with no prior state",
			body:               strings.NewReader(`{"state":"Disabled"}`),
			seedHCP:            true,
			seedSPC:            true,
			expectedStatusCode: http.StatusOK,
			expectedState:      coreapi.BackupScheduleStateDisabled,
		},
		{
			name:               "change Enabled to Disabled",
			body:               strings.NewReader(`{"state":"Disabled"}`),
			seedHCP:            true,
			seedSPC:            true,
			existingState:      coreapi.BackupScheduleStateEnabled,
			expectedStatusCode: http.StatusOK,
			expectedState:      coreapi.BackupScheduleStateDisabled,
		},
		{
			name:               "change Disabled to Enabled",
			body:               strings.NewReader(`{"state":"Enabled"}`),
			seedHCP:            true,
			seedSPC:            true,
			existingState:      coreapi.BackupScheduleStateDisabled,
			expectedStatusCode: http.StatusOK,
			expectedState:      coreapi.BackupScheduleStateEnabled,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := utils.ContextWithLogger(context.Background(), testr.New(t))
			mockResourcesDBClient := corecosmosstoragetesting.NewMockResourcesDBClient()

			resourceID, err := azcorearm.ParseResourceID(coreapitesting.TestClusterResourceID)
			require.NoError(t, err)

			if tt.seedHCP {
				hcp := &coreapi.Cluster{
					CosmosMetadata: coreapi.CosmosMetadata{
						ResourceID:   resourceID,
						PartitionKey: strings.ToLower(resourceID.SubscriptionID),
					},
					TrackedResource: coreapi.TrackedResource{
						Resource: coreapi.Resource{ID: resourceID},
					},
				}
				_, err = mockResourcesDBClient.HCPClusters(resourceID.SubscriptionID, resourceID.ResourceGroupName).Create(ctx, hcp, nil)
				require.NoError(t, err)
			}

			if tt.seedSPC {
				spc, err := corecosmosstorage.GetOrCreateServiceProviderCluster(ctx, mockResourcesDBClient, resourceID)
				require.NoError(t, err)
				if tt.existingState != "" {
					spc.Spec.BackupScheduleState = tt.existingState
					_, err = mockResourcesDBClient.ServiceProviderClusters(resourceID.SubscriptionID, resourceID.ResourceGroupName, resourceID.Name).Replace(ctx, spc, nil)
					require.NoError(t, err)
				}
			}

			handler := NewHCPPatchBackupScheduleHandler(mockResourcesDBClient)

			if !tt.skipResourceID {
				ctx = utils.ContextWithResourceID(ctx, resourceID)
			}

			req := httptest.NewRequest(http.MethodPatch, "/backupschedules", tt.body)
			req = req.WithContext(ctx)
			recorder := httptest.NewRecorder()

			err = handler.ServeHTTP(recorder, req)

			if tt.expectedStatusCode >= 400 {
				if err == nil {
					t.Fatalf("expected error but got none")
				}
				var cloudErr *coreapi.CloudError
				if !errors.As(err, &cloudErr) {
					t.Fatalf("expected CloudError but got %T: %v", err, err)
				}
				if cloudErr.StatusCode != tt.expectedStatusCode {
					t.Errorf("expected status %d, got %d", tt.expectedStatusCode, cloudErr.StatusCode)
				}
				if tt.expectedError != "" && !strings.Contains(err.Error(), tt.expectedError) {
					t.Errorf("expected error containing %q, got %q", tt.expectedError, err.Error())
				}
				return
			}

			if tt.expectedError != "" {
				if err == nil {
					t.Fatalf("expected error but got none")
				}
				if !strings.Contains(err.Error(), tt.expectedError) {
					t.Errorf("expected error containing %q, got %q", tt.expectedError, err.Error())
				}
				return
			}

			if err != nil {
				t.Fatalf("expected no error but got %v", err)
			}

			var response BackupSchedulePatchResponse
			require.NoError(t, json.NewDecoder(recorder.Body).Decode(&response))
			if response.State != tt.expectedState {
				t.Errorf("expected response state %q, got %q", tt.expectedState, response.State)
			}

			spc, err := mockResourcesDBClient.ServiceProviderClusters(resourceID.SubscriptionID, resourceID.ResourceGroupName, resourceID.Name).Get(ctx, coreapi.ServiceProviderClusterResourceName)
			require.NoError(t, err)
			if spc.Spec.BackupScheduleState != tt.expectedState {
				t.Errorf("expected DB state %q, got %q", tt.expectedState, spc.Spec.BackupScheduleState)
			}
		})
	}
}

func TestGetBackupScheduleHandler(t *testing.T) {
	managementClusterResourceID := metadataapi.Must(azcorearm.ParseResourceID(
		"/subscriptions/" + coreapitesting.TestSubscriptionID + "/resourceGroups/mgmt-rg/providers/Microsoft.ContainerService/managedClusters/mgmt-cluster",
	))

	makeReadDesire := func(name, targetName string) *kubeapplierapi.ReadDesire {
		resourceIDStr := kubeapplierapihelpers.ToClusterScopedReadDesireResourceIDString(
			coreapitesting.TestSubscriptionID, coreapitesting.TestResourceGroupName, coreapitesting.TestClusterName, name,
		)
		resourceID := metadataapi.Must(azcorearm.ParseResourceID(resourceIDStr))
		return &kubeapplierapi.ReadDesire{
			CosmosMetadata: coreapi.CosmosMetadata{
				ResourceID:   resourceID,
				PartitionKey: strings.ToLower(managementClusterResourceID.String()),
			},
			Spec: kubeapplierapi.ReadDesireSpec{
				ManagementCluster: managementClusterResourceID,
				TargetItem: kubeapplierapi.ResourceReference{
					Name: targetName,
				},
			},
			Tags: map[string]string{backup.DesireTagKeySchedule: ""},
		}
	}

	seedClusterWithMgmtPlacement := func(
		ctx context.Context,
		t *testing.T,
		mockResourcesDBClient *corecosmosstoragetesting.MockResourcesDBClient,
		resourceID *azcorearm.ResourceID,
		backupState coreapi.BackupScheduleState,
	) {
		t.Helper()
		hcp := &coreapi.Cluster{
			CosmosMetadata: coreapi.CosmosMetadata{
				ResourceID:   resourceID,
				PartitionKey: strings.ToLower(resourceID.SubscriptionID),
			},
			TrackedResource: coreapi.TrackedResource{
				Resource: coreapi.Resource{ID: resourceID},
			},
		}
		_, err := mockResourcesDBClient.HCPClusters(resourceID.SubscriptionID, resourceID.ResourceGroupName).Create(ctx, hcp, nil)
		require.NoError(t, err)

		spc, err := corecosmosstorage.GetOrCreateServiceProviderCluster(ctx, mockResourcesDBClient, resourceID)
		require.NoError(t, err)
		spc.Status.ManagementClusterResourceID = managementClusterResourceID
		if backupState != "" {
			spc.Spec.BackupScheduleState = backupState
		}
		_, err = mockResourcesDBClient.ServiceProviderClusters(resourceID.SubscriptionID, resourceID.ResourceGroupName, resourceID.Name).Replace(ctx, spc, nil)
		require.NoError(t, err)
	}

	tests := []struct {
		name               string
		skipResourceID     bool
		seedCluster        bool
		backupState        coreapi.BackupScheduleState
		setMgmtPlacement   bool
		registerKubeClient bool
		readDesires        []*kubeapplierapi.ReadDesire
		expectedStatusCode int
		expectedError      string
		expectedState      coreapi.BackupScheduleState
		expectedSchedules  []BackupScheduleDetail
	}{
		{
			name:           "missing resource ID in context",
			skipResourceID: true,
			expectedError:  "failed to resolve HCP context",
		},
		{
			name:               "HCP cluster not found",
			expectedStatusCode: http.StatusNotFound,
			expectedError:      "not found",
		},
		{
			name:               "management cluster placement not resolved",
			seedCluster:        true,
			expectedStatusCode: http.StatusPreconditionFailed,
			expectedError:      "management cluster placement not resolved",
		},
		{
			name:               "kube-applier client not available",
			seedCluster:        true,
			setMgmtPlacement:   true,
			expectedStatusCode: http.StatusPreconditionFailed,
			expectedError:      "kube-applier client not available",
		},
		{
			name:               "empty state defaults to Enabled",
			seedCluster:        true,
			setMgmtPlacement:   true,
			registerKubeClient: true,
			expectedStatusCode: http.StatusOK,
			expectedState:      coreapi.BackupScheduleStateEnabled,
			expectedSchedules:  []BackupScheduleDetail{},
		},
		{
			name:               "explicit Disabled state preserved",
			seedCluster:        true,
			setMgmtPlacement:   true,
			registerKubeClient: true,
			backupState:        coreapi.BackupScheduleStateDisabled,
			expectedStatusCode: http.StatusOK,
			expectedState:      coreapi.BackupScheduleStateDisabled,
			expectedSchedules:  []BackupScheduleDetail{},
		},
		{
			name:               "returns schedule details from ReadDesires",
			seedCluster:        true,
			setMgmtPlacement:   true,
			registerKubeClient: true,
			readDesires: func() []*kubeapplierapi.ReadDesire {
				lastBackup := metav1.NewTime(time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC))
				scheduleJSON, _ := json.Marshal(velerov1api.Schedule{
					Spec: velerov1api.ScheduleSpec{
						Paused: true,
					},
					Status: velerov1api.ScheduleStatus{
						Phase:      velerov1api.SchedulePhaseEnabled,
						LastBackup: &lastBackup,
					},
				})
				rd := makeReadDesire(backup.BackupScheduleDesireNamePrefix+"hourly", "hourly")
				rd.Status.KubeContent = &runtime.RawExtension{Raw: scheduleJSON}
				return []*kubeapplierapi.ReadDesire{rd}
			}(),
			expectedStatusCode: http.StatusOK,
			expectedState:      coreapi.BackupScheduleStateEnabled,
			expectedSchedules: []BackupScheduleDetail{
				{
					Name:                 "hourly",
					LastBackupTime:       "2026-08-01T12:00:00Z",
					BackupExecutionState: BackupExecutionStatePaused,
				},
			},
		},
		{
			name:               "ReadDesire without KubeContent returns name only",
			seedCluster:        true,
			setMgmtPlacement:   true,
			registerKubeClient: true,
			readDesires: []*kubeapplierapi.ReadDesire{
				makeReadDesire(backup.BackupScheduleDesireNamePrefix+"daily", "daily"),
			},
			expectedStatusCode: http.StatusOK,
			expectedState:      coreapi.BackupScheduleStateEnabled,
			expectedSchedules: []BackupScheduleDetail{
				{Name: "daily"},
			},
		},
		{
			name:               "returns target Schedule name when desire name differs",
			seedCluster:        true,
			setMgmtPlacement:   true,
			registerKubeClient: true,
			readDesires: []*kubeapplierapi.ReadDesire{
				makeReadDesire(backup.BackupScheduleDesireNamePrefix+"10min", "hosted-cluster-namespace-10min"),
			},
			expectedStatusCode: http.StatusOK,
			expectedState:      coreapi.BackupScheduleStateEnabled,
			expectedSchedules: []BackupScheduleDetail{
				{Name: "hosted-cluster-namespace-10min"},
			},
		},
		{
			name:               "ReadDesire without schedule tag is skipped",
			seedCluster:        true,
			setMgmtPlacement:   true,
			registerKubeClient: true,
			readDesires: func() []*kubeapplierapi.ReadDesire {
				rd := makeReadDesire(backup.BackupScheduleDesireNamePrefix+"untagged", "untagged")
				rd.Tags = map[string]string{}
				return []*kubeapplierapi.ReadDesire{rd}
			}(),
			expectedStatusCode: http.StatusOK,
			expectedState:      coreapi.BackupScheduleStateEnabled,
			expectedSchedules:  []BackupScheduleDetail{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := utils.ContextWithLogger(context.Background(), testr.New(t))
			mockResourcesDBClient := corecosmosstoragetesting.NewMockResourcesDBClient()
			mockKubeApplierClients := kubeappliercosmosstoragetesting.NewMockKubeApplierDBClients()

			resourceID, err := azcorearm.ParseResourceID(coreapitesting.TestClusterResourceID)
			require.NoError(t, err)

			if tt.seedCluster {
				if tt.setMgmtPlacement {
					seedClusterWithMgmtPlacement(ctx, t, mockResourcesDBClient, resourceID, tt.backupState)
				} else {
					hcp := &coreapi.Cluster{
						CosmosMetadata: coreapi.CosmosMetadata{
							ResourceID:   resourceID,
							PartitionKey: strings.ToLower(resourceID.SubscriptionID),
						},
						TrackedResource: coreapi.TrackedResource{
							Resource: coreapi.Resource{ID: resourceID},
						},
					}
					_, err = mockResourcesDBClient.HCPClusters(resourceID.SubscriptionID, resourceID.ResourceGroupName).Create(ctx, hcp, nil)
					require.NoError(t, err)

					_, err = corecosmosstorage.GetOrCreateServiceProviderCluster(ctx, mockResourcesDBClient, resourceID)
					require.NoError(t, err)
				}
			}

			if tt.registerKubeClient {
				mockKubeApplierClient := kubeappliercosmosstoragetesting.NewMockKubeApplierDBClient()
				mockKubeApplierClients.Register(managementClusterResourceID, mockKubeApplierClient)

				if len(tt.readDesires) > 0 {
					readDesireCRUD, err := mockKubeApplierClient.ReadDesiresForCluster(
						resourceID.SubscriptionID, resourceID.ResourceGroupName, resourceID.Name,
					)
					require.NoError(t, err)
					for _, rd := range tt.readDesires {
						_, err = readDesireCRUD.Create(ctx, rd, nil)
						require.NoError(t, err)
					}
				}
			}

			handler := NewHCPGetBackupScheduleHandler(mockResourcesDBClient, mockKubeApplierClients)

			if !tt.skipResourceID {
				ctx = utils.ContextWithResourceID(ctx, resourceID)
			}

			req := httptest.NewRequest(http.MethodGet, "/backupschedules", nil)
			req = req.WithContext(ctx)
			recorder := httptest.NewRecorder()

			err = handler.ServeHTTP(recorder, req)

			if tt.expectedStatusCode >= 400 {
				if err == nil {
					t.Fatalf("expected error but got none")
				}
				var cloudErr *coreapi.CloudError
				if !errors.As(err, &cloudErr) {
					t.Fatalf("expected CloudError but got %T: %v", err, err)
				}
				if cloudErr.StatusCode != tt.expectedStatusCode {
					t.Errorf("expected status %d, got %d", tt.expectedStatusCode, cloudErr.StatusCode)
				}
				if tt.expectedError != "" && !strings.Contains(err.Error(), tt.expectedError) {
					t.Errorf("expected error containing %q, got %q", tt.expectedError, err.Error())
				}
				return
			}

			if tt.expectedError != "" {
				if err == nil {
					t.Fatalf("expected error but got none")
				}
				if !strings.Contains(err.Error(), tt.expectedError) {
					t.Errorf("expected error containing %q, got %q", tt.expectedError, err.Error())
				}
				return
			}

			if err != nil {
				t.Fatalf("expected no error but got %v", err)
			}

			var response BackupScheduleResponse
			require.NoError(t, json.NewDecoder(recorder.Body).Decode(&response))
			if response.State != tt.expectedState {
				t.Errorf("expected state %q, got %q", tt.expectedState, response.State)
			}
			require.Len(t, response.Schedules, len(tt.expectedSchedules))
			for i, expected := range tt.expectedSchedules {
				actual := response.Schedules[i]
				if actual.Name != expected.Name {
					t.Errorf("schedule[%d] name: expected %q, got %q", i, expected.Name, actual.Name)
				}
				if actual.LastBackupTime != expected.LastBackupTime {
					t.Errorf("schedule[%d] lastBackupTime: expected %q, got %q", i, expected.LastBackupTime, actual.LastBackupTime)
				}
				if actual.BackupExecutionState != expected.BackupExecutionState {
					t.Errorf("schedule[%d] state: expected %q, got %q", i, expected.BackupExecutionState, actual.BackupExecutionState)
				}
			}
		})
	}
}

func TestPostOnDemandBackupHandler(t *testing.T) {
	managementClusterResourceID := metadataapi.Must(azcorearm.ParseResourceID(
		"/subscriptions/" + coreapitesting.TestSubscriptionID + "/resourceGroups/mgmt-rg/providers/Microsoft.ContainerService/managedClusters/mgmt-cluster",
	))

	// hostedClusterReadDesire is the readonly HostedCluster mirror the handler
	// reads to decide whether the cluster is safe to snapshot and which CMK the
	// snapshot will be encrypted under.
	hostedClusterReadDesire := func(t *testing.T, mutate func(*hyperv1beta1.HostedCluster), observed bool) *kubeapplierapi.ReadDesire {
		t.Helper()
		resourceID := metadataapi.Must(azcorearm.ParseResourceID(kubeapplierapihelpers.ToClusterScopedReadDesireResourceIDString(
			coreapitesting.TestSubscriptionID, coreapitesting.TestResourceGroupName, coreapitesting.TestClusterName,
			kubeapplierapi.ReadDesireNameReadonlyHostedCluster,
		)))
		hostedCluster := &hyperv1beta1.HostedCluster{}
		hostedCluster.Status.SecretEncryption.ActiveKey.Azure = hyperv1beta1.AzureKMSKey{
			KeyVaultName: "vault", KeyName: "key", KeyVersion: "v1",
		}
		if mutate != nil {
			mutate(hostedCluster)
		}
		raw, err := json.Marshal(hostedCluster)
		require.NoError(t, err)
		readDesire := &kubeapplierapi.ReadDesire{
			CosmosMetadata: coreapi.CosmosMetadata{ResourceID: resourceID, PartitionKey: strings.ToLower(managementClusterResourceID.String())},
			Spec:           kubeapplierapi.ReadDesireSpec{ManagementCluster: managementClusterResourceID},
			Status:         kubeapplierapi.ReadDesireStatus{KubeContent: &runtime.RawExtension{Raw: raw}},
		}
		if observed {
			readDesire.Status.Conditions = []metav1.Condition{{
				Type:               kubeapplierapi.ConditionTypeSuccessful,
				Status:             metav1.ConditionTrue,
				Reason:             "AsExpected",
				LastTransitionTime: metav1.Now(),
			}}
		}
		return readDesire
	}

	for _, test := range []struct {
		name                string
		paused, deleting    bool
		omitCluster         bool
		omitPlacement       bool
		omitNamespaces      bool
		omitKubeClient      bool
		omitHostedCluster   bool
		mutateReadDesire    func(*kubeapplierapi.ReadDesire)
		unobserved          bool
		rotating            bool
		noCMK               bool
		existingRequested   bool
		existingRotation    bool
		status              int
		expectedError       string
		expectedFingerprint string
	}{
		{
			name:                "creates the backup desires",
			status:              http.StatusOK,
			expectedFingerprint: backup.AzureKMSKeyFingerprint("vault", "key", "v1"),
		},
		{
			name:   "non-CMK cluster gets no fingerprint annotation",
			noCMK:  true,
			status: http.StatusOK,
		},
		{
			// A rotation-triggered backup is a different flow and must not block an SRE request.
			name:                "rotation backup in flight does not block",
			existingRotation:    true,
			status:              http.StatusOK,
			expectedFingerprint: backup.AzureKMSKeyFingerprint("vault", "key", "v1"),
		},
		{name: "paused", paused: true, status: http.StatusConflict},
		{name: "deleting", deleting: true, status: http.StatusConflict},
		{name: "missing cluster", omitCluster: true, status: http.StatusNotFound},
		{name: "placement unresolved", omitPlacement: true, status: http.StatusPreconditionFailed},
		{name: "namespaces unresolved", omitNamespaces: true, status: http.StatusPreconditionFailed},
		{name: "kube-applier client unavailable", omitKubeClient: true, status: http.StatusPreconditionFailed},
		{
			name:              "HostedCluster not mirrored yet",
			omitHostedCluster: true,
			status:            http.StatusConflict,
			expectedError:     "HostedCluster ReadDesire is not available yet",
		},
		{
			name: "HostedCluster content missing",
			mutateReadDesire: func(readDesire *kubeapplierapi.ReadDesire) {
				readDesire.Status.KubeContent = nil
			},
			status:        http.StatusConflict,
			expectedError: "HostedCluster ReadDesire has no kubeContent",
		},
		{
			name: "HostedCluster content empty",
			mutateReadDesire: func(readDesire *kubeapplierapi.ReadDesire) {
				readDesire.Status.KubeContent = &runtime.RawExtension{}
			},
			status:        http.StatusConflict,
			expectedError: "HostedCluster ReadDesire has no kubeContent",
		},
		{
			name: "HostedCluster content invalid",
			mutateReadDesire: func(readDesire *kubeapplierapi.ReadDesire) {
				readDesire.Status.KubeContent = &runtime.RawExtension{Raw: []byte(`{"status":"invalid"}`)}
			},
			status:        http.StatusInternalServerError,
			expectedError: "failed to unmarshal HostedCluster from ReadDesire kubeContent",
		},
		{
			name:          "HostedCluster not observed",
			unobserved:    true,
			status:        http.StatusConflict,
			expectedError: "waiting for an observed HostedCluster",
		},
		{name: "KMS rotation in progress", rotating: true, status: http.StatusConflict},
		{name: "requested backup already in flight", existingRequested: true, status: http.StatusConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := utils.ContextWithLogger(t.Context(), testr.New(t))
			rid, err := azcorearm.ParseResourceID(coreapitesting.TestClusterResourceID)
			require.NoError(t, err)
			ctx = utils.ContextWithResourceID(ctx, rid)

			db := corecosmosstoragetesting.NewMockResourcesDBClient()
			if !test.omitCluster {
				seedHCPCluster(ctx, t, db, rid, "4.22", "nightly")
				if test.deleting {
					cluster, err := db.HCPClusters(rid.SubscriptionID, rid.ResourceGroupName).Get(ctx, rid.Name)
					require.NoError(t, err)
					now := metav1.Now()
					cluster.ServiceProviderProperties.DeletionTimestamp = &now
					_, err = db.HCPClusters(rid.SubscriptionID, rid.ResourceGroupName).Replace(ctx, cluster, nil)
					require.NoError(t, err)
				}

				spc, err := corecosmosstorage.GetOrCreateServiceProviderCluster(ctx, db, rid)
				require.NoError(t, err)
				if test.paused {
					spc.Spec.BackupScheduleState = coreapi.BackupScheduleStateDisabled
				}
				if !test.omitPlacement {
					spc.Status.ManagementClusterResourceID = managementClusterResourceID
				}
				if !test.omitNamespaces {
					spc.Status.HostedClusterNamespace = "hc-ns"
					spc.Status.ControlPlaneNamespace = "cp-ns"
				}
				_, err = db.ServiceProviderClusters(rid.SubscriptionID, rid.ResourceGroupName, rid.Name).Replace(ctx, spc, nil)
				require.NoError(t, err)
			}

			kubeApplierClients := kubeappliercosmosstoragetesting.NewMockKubeApplierDBClients()
			var applyDesireCRUD cosmosstorageutils.ResourceCRUD[kubeapplierapi.ApplyDesire, *kubeapplierapi.ApplyDesire]
			var readDesireCRUD cosmosstorageutils.ResourceCRUD[kubeapplierapi.ReadDesire, *kubeapplierapi.ReadDesire]
			if !test.omitKubeClient {
				kubeApplierClient := kubeappliercosmosstoragetesting.NewMockKubeApplierDBClient()
				kubeApplierClients.Register(managementClusterResourceID, kubeApplierClient)
				applyDesireCRUD, err = kubeApplierClient.ApplyDesiresForCluster(rid.SubscriptionID, rid.ResourceGroupName, rid.Name)
				require.NoError(t, err)
				readDesireCRUD, err = kubeApplierClient.ReadDesiresForCluster(rid.SubscriptionID, rid.ResourceGroupName, rid.Name)
				require.NoError(t, err)

				if !test.omitHostedCluster {
					readDesire := hostedClusterReadDesire(t, func(hostedCluster *hyperv1beta1.HostedCluster) {
						if test.rotating {
							hostedCluster.Status.SecretEncryption.TargetKey.Azure = hyperv1beta1.AzureKMSKey{
								KeyVaultName: "vault", KeyName: "key", KeyVersion: "v2",
							}
						}
						if test.noCMK {
							hostedCluster.Status.SecretEncryption.ActiveKey.Azure = hyperv1beta1.AzureKMSKey{}
						}
					}, !test.unobserved)
					if test.mutateReadDesire != nil {
						test.mutateReadDesire(readDesire)
					}
					_, err = readDesireCRUD.Create(ctx, readDesire, nil)
					require.NoError(t, err)
				}

				// Both seeds carry the same on-demand tag; the owner is what makes
				// one an outstanding admin request and the other a key rotation
				// this endpoint must not treat as one.
				for _, seed := range []struct {
					enabled        bool
					controllerName string
					name           string
				}{
					{enabled: test.existingRequested, controllerName: backup.OnDemandBackupControllerName, name: "backup-existing"},
					{enabled: test.existingRotation, controllerName: backup.KeyRotationBackupControllerName, name: "hc-ns-keyrotation-abc"},
				} {
					if !seed.enabled {
						continue
					}
					veleroBackup := backup.NewBackup(seed.name, rid.String(), "", "hc-ns", "cp-ns", backup.OnDemandBackupTTL)
					applyDesire, err := backup.BuildBackupApplyDesire(
						rid.SubscriptionID, rid.ResourceGroupName, rid.Name,
						managementClusterResourceID, veleroBackup, seed.controllerName,
					)
					require.NoError(t, err)
					_, err = applyDesireCRUD.Create(ctx, applyDesire, nil)
					require.NoError(t, err)
				}
			}

			recorder := httptest.NewRecorder()
			err = NewHCPPostOnDemandBackupHandler(db, kubeApplierClients).
				ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/backups", nil).WithContext(ctx))

			if test.status != http.StatusOK {
				if test.status == http.StatusInternalServerError {
					var unmarshalErr *json.UnmarshalTypeError
					require.ErrorAs(t, err, &unmarshalErr, "expected the decode error to be preserved")
				} else {
					var cloudErr *coreapi.CloudError
					require.ErrorAs(t, err, &cloudErr, "expected a CloudError")
					require.Equal(t, test.status, cloudErr.StatusCode)
				}
				if test.expectedError != "" {
					require.ErrorContains(t, err, test.expectedError)
				}
				if applyDesireCRUD != nil {
					// A rejected request must not create a desire. Only the one
					// deliberately pre-seeded by the in-flight case may remain.
					expected := 0
					if test.existingRequested {
						expected = 1
					}
					iterator, listErr := applyDesireCRUD.List(ctx, nil)
					require.NoError(t, listErr)
					found := 0
					for _, applyDesire := range iterator.Items(ctx) {
						// Owner-scoped: the rotation seed shares the tag and
						// would otherwise be counted as an admin request.
						if backup.OwnsBackupDesire(applyDesire.Tags, backup.OnDemandBackupControllerName) {
							found++
						}
					}
					require.Equal(t, expected, found, "a rejected request must not create a requested-backup ApplyDesire")
				}
				return
			}
			require.NoError(t, err)

			var response OnDemandBackupDetail
			require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
			require.Equal(t, "Pending", response.Phase)
			require.NotEmpty(t, response.Name)

			desireName := backup.OndemandBackupDesireName(response.Name)
			applyDesire, err := applyDesireCRUD.Get(ctx, desireName)
			require.NoError(t, err, "handler must create the backup ApplyDesire")
			require.Contains(t, applyDesire.Tags, backup.DesireTagKeyOndemandBackup)
			// Admin-requested backups share the tag with rotation backups; the
			// owner is what keeps the KeyRotationBackup sweep off this desire.
			require.Equal(t, backup.OnDemandBackupControllerName, applyDesire.Tags[kubeapplierapi.TagControllerName],
				"admin-requested backups must not be mistaken for rotation backups")
			require.Equal(t, backup.VeleroBackupResource, applyDesire.Spec.TargetItem.Resource)
			require.Equal(t, response.Name, applyDesire.Spec.TargetItem.Name)

			var veleroBackup velerov1api.Backup
			require.NoError(t, json.Unmarshal(applyDesire.Spec.ServerSideApply.KubeContent.Raw, &veleroBackup))
			require.Equal(t, response.Name, veleroBackup.Name)
			require.Equal(t, backup.OnDemandBackupTTL, veleroBackup.Spec.TTL.Duration)
			require.Equal(t, []string{"hc-ns", "cp-ns"}, veleroBackup.Spec.IncludedNamespaces)
			require.Equal(t, rid.String(), veleroBackup.Annotations[controllerutils.HcpClusterAzureResourceIdAnnotation])
			if test.expectedFingerprint == "" {
				require.NotContains(t, veleroBackup.Annotations, controllerutils.HcpClusterKmsKeyFingerprintAnnotation,
					"a cluster without a customer-managed key must not carry a fingerprint")
			} else {
				require.Equal(t, test.expectedFingerprint, veleroBackup.Annotations[controllerutils.HcpClusterKmsKeyFingerprintAnnotation])
			}
			require.NotEmpty(t, veleroBackup.Spec.Hooks.Resources, "etcd snapshot hooks must be configured")

			readDesire, err := readDesireCRUD.Get(ctx, desireName)
			require.NoError(t, err, "handler must create the paired backup ReadDesire")
			require.Equal(t, applyDesire.Spec.TargetItem, readDesire.Spec.TargetItem)
			require.Contains(t, readDesire.Tags, backup.DesireTagKeyOndemandBackup)
			require.Equal(t, backup.OnDemandBackupControllerName, readDesire.Tags[kubeapplierapi.TagControllerName],
				"the ReadDesire's owner is what the backend sweep keys on to reap it")

			kubeApplierClient, err := kubeApplierClients.For(ctx, managementClusterResourceID)
			require.NoError(t, err)
			changes, err := kubeApplierClient.ReadChangeFeed(ctx, nil)
			require.NoError(t, err)
			var createdDesires []string
			for _, item := range changes.Items {
				var document coreapi.CosmosMetadata
				require.NoError(t, json.Unmarshal(item, &document))
				if document.ResourceID.Name == desireName {
					createdDesires = append(createdDesires, document.ResourceID.String())
				}
			}
			require.Equal(t, []string{applyDesire.ResourceID.String(), readDesire.ResourceID.String()}, createdDesires,
				"ApplyDesire must be persisted first so cleanup cannot retire the ReadDesire during request creation")
		})
	}
}

func TestGetOnDemandBackupsHandler(t *testing.T) {
	managementClusterResourceID := metadataapi.Must(azcorearm.ParseResourceID(
		"/subscriptions/" + coreapitesting.TestSubscriptionID + "/resourceGroups/mgmt-rg/providers/Microsoft.ContainerService/managedClusters/mgmt-cluster",
	))

	makeReadDesire := func(name string) *kubeapplierapi.ReadDesire {
		resourceIDStr := kubeapplierapihelpers.ToClusterScopedReadDesireResourceIDString(
			coreapitesting.TestSubscriptionID, coreapitesting.TestResourceGroupName, coreapitesting.TestClusterName, name,
		)
		resourceID := metadataapi.Must(azcorearm.ParseResourceID(resourceIDStr))
		return &kubeapplierapi.ReadDesire{
			CosmosMetadata: coreapi.CosmosMetadata{
				ResourceID:   resourceID,
				PartitionKey: strings.ToLower(managementClusterResourceID.String()),
			},
			Spec: kubeapplierapi.ReadDesireSpec{
				ManagementCluster: managementClusterResourceID,
			},
			// Defaults to a key-rotation backup; cases that want an
			// admin-requested one override the owner.
			Tags: map[string]string{
				backup.DesireTagKeyOndemandBackup: "",
				kubeapplierapi.TagControllerName:  backup.KeyRotationBackupControllerName,
			},
		}
	}

	seedCluster := func(ctx context.Context, t *testing.T, mockResourcesDBClient *corecosmosstoragetesting.MockResourcesDBClient, resourceID *azcorearm.ResourceID) {
		t.Helper()
		hcp := &coreapi.Cluster{
			CosmosMetadata: coreapi.CosmosMetadata{ResourceID: resourceID, PartitionKey: strings.ToLower(resourceID.SubscriptionID)},
			TrackedResource: coreapi.TrackedResource{
				Resource: coreapi.Resource{ID: resourceID},
			},
		}
		_, err := mockResourcesDBClient.HCPClusters(resourceID.SubscriptionID, resourceID.ResourceGroupName).Create(ctx, hcp, nil)
		require.NoError(t, err)
		spc, err := corecosmosstorage.GetOrCreateServiceProviderCluster(ctx, mockResourcesDBClient, resourceID)
		require.NoError(t, err)
		spc.Status.ManagementClusterResourceID = managementClusterResourceID
		_, err = mockResourcesDBClient.ServiceProviderClusters(resourceID.SubscriptionID, resourceID.ResourceGroupName, resourceID.Name).Replace(ctx, spc, nil)
		require.NoError(t, err)
	}

	tests := []struct {
		name               string
		skipResourceID     bool
		doSeedCluster      bool
		registerKubeClient bool
		readDesires        []*kubeapplierapi.ReadDesire
		expectedStatusCode int
		expectedError      string
		expectedBackups    []OnDemandBackupDetail
	}{
		{
			name:           "missing resource ID in context",
			skipResourceID: true,
			expectedError:  "failed to resolve HCP context",
		},
		{
			name:               "HCP cluster not found",
			expectedStatusCode: http.StatusNotFound,
			expectedError:      "not found",
		},
		{
			name:               "no on-demand backups returns empty list",
			doSeedCluster:      true,
			registerKubeClient: true,
			expectedStatusCode: http.StatusOK,
			expectedBackups:    []OnDemandBackupDetail{},
		},
		{
			name:               "returns backup details from ReadDesires",
			doSeedCluster:      true,
			registerKubeClient: true,
			readDesires: func() []*kubeapplierapi.ReadDesire {
				startTime := metav1.NewTime(time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC))
				completionTime := metav1.NewTime(time.Date(2026, 8, 1, 12, 5, 0, 0, time.UTC))
				backupJSON, _ := json.Marshal(velerov1api.Backup{
					ObjectMeta: metav1.ObjectMeta{
						Annotations: map[string]string{
							controllerutils.HcpClusterKmsKeyFingerprintAnnotation: "abc123fingerprint",
						},
					},
					Status: velerov1api.BackupStatus{
						Phase:               velerov1api.BackupPhaseCompleted,
						StartTimestamp:      &startTime,
						CompletionTimestamp: &completionTime,
					},
				})
				rd := makeReadDesire(backup.OndemandBackupDesireNamePrefix + "cluster1-keyrotation-abc123fingerprint")
				rd.Status.KubeContent = &runtime.RawExtension{Raw: backupJSON}
				return []*kubeapplierapi.ReadDesire{rd}
			}(),
			expectedStatusCode: http.StatusOK,
			expectedBackups: []OnDemandBackupDetail{
				{
					Name:              "cluster1-keyrotation-abc123fingerprint",
					Phase:             "Completed",
					KMSKeyFingerprint: "abc123fingerprint",
					StartTime:         "2026-08-01T12:00:00Z",
					CompletionTime:    "2026-08-01T12:05:00Z",
				},
			},
		},
		{
			name:               "ReadDesire without KubeContent returns name only",
			doSeedCluster:      true,
			registerKubeClient: true,
			readDesires: []*kubeapplierapi.ReadDesire{
				makeReadDesire(backup.OndemandBackupDesireNamePrefix + "cluster1-keyrotation-def456"),
			},
			expectedStatusCode: http.StatusOK,
			expectedBackups: []OnDemandBackupDetail{
				{Name: "cluster1-keyrotation-def456"},
			},
		},
		{
			name:               "ReadDesire without on-demand tag is skipped",
			doSeedCluster:      true,
			registerKubeClient: true,
			readDesires: func() []*kubeapplierapi.ReadDesire {
				rd := makeReadDesire(backup.OndemandBackupDesireNamePrefix + "untagged")
				rd.Tags = map[string]string{backup.DesireTagKeySchedule: ""}
				return []*kubeapplierapi.ReadDesire{rd}
			}(),
			expectedStatusCode: http.StatusOK,
			expectedBackups:    []OnDemandBackupDetail{},
		},
		{
			name:               "admin-requested backup is listed",
			doSeedCluster:      true,
			registerKubeClient: true,
			readDesires: func() []*kubeapplierapi.ReadDesire {
				rd := makeReadDesire(backup.OndemandBackupDesireNamePrefix + "backup-1")
				rd.Tags[kubeapplierapi.TagControllerName] = backup.OnDemandBackupControllerName
				rd.Status.KubeContent = &runtime.RawExtension{Raw: []byte(`{"status":{"phase":"Completed"}}`)}
				return []*kubeapplierapi.ReadDesire{rd}
			}(),
			expectedStatusCode: http.StatusOK,
			expectedBackups:    []OnDemandBackupDetail{{Name: "backup-1", Phase: "Completed"}},
		},
		{
			// This endpoint is the one place both flows are deliberately in
			// scope, so it filters on the tag alone and ignores the owner.
			name:               "both rotation and admin-requested backups are listed",
			doSeedCluster:      true,
			registerKubeClient: true,
			readDesires: func() []*kubeapplierapi.ReadDesire {
				rotation := makeReadDesire(backup.OndemandBackupDesireNamePrefix + "cluster1-keyrotation-abc123")
				rotation.Status.KubeContent = &runtime.RawExtension{Raw: []byte(`{"status":{"phase":"Completed"}}`)}
				requested := makeReadDesire(backup.OndemandBackupDesireNamePrefix + "backup-1")
				requested.Tags[kubeapplierapi.TagControllerName] = backup.OnDemandBackupControllerName
				requested.Status.KubeContent = &runtime.RawExtension{Raw: []byte(`{"status":{"phase":"InProgress"}}`)}
				return []*kubeapplierapi.ReadDesire{rotation, requested}
			}(),
			expectedStatusCode: http.StatusOK,
			expectedBackups: []OnDemandBackupDetail{
				{Name: "cluster1-keyrotation-abc123", Phase: "Completed"},
				{Name: "backup-1", Phase: "InProgress"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := utils.ContextWithLogger(context.Background(), testr.New(t))
			mockResourcesDBClient := corecosmosstoragetesting.NewMockResourcesDBClient()
			mockKubeApplierClients := kubeappliercosmosstoragetesting.NewMockKubeApplierDBClients()

			resourceID, err := azcorearm.ParseResourceID(coreapitesting.TestClusterResourceID)
			require.NoError(t, err)

			if tt.doSeedCluster {
				seedCluster(ctx, t, mockResourcesDBClient, resourceID)
			}

			if tt.registerKubeClient {
				mockKubeApplierClient := kubeappliercosmosstoragetesting.NewMockKubeApplierDBClient()
				mockKubeApplierClients.Register(managementClusterResourceID, mockKubeApplierClient)

				if len(tt.readDesires) > 0 {
					readDesireCRUD, err := mockKubeApplierClient.ReadDesiresForCluster(
						resourceID.SubscriptionID, resourceID.ResourceGroupName, resourceID.Name,
					)
					require.NoError(t, err)
					for _, rd := range tt.readDesires {
						_, err = readDesireCRUD.Create(ctx, rd, nil)
						require.NoError(t, err)
					}
				}
			}

			handler := NewHCPGetOnDemandBackupsHandler(mockResourcesDBClient, mockKubeApplierClients)

			if !tt.skipResourceID {
				ctx = utils.ContextWithResourceID(ctx, resourceID)
			}

			req := httptest.NewRequest(http.MethodGet, "/backups", nil)
			req = req.WithContext(ctx)
			recorder := httptest.NewRecorder()

			err = handler.ServeHTTP(recorder, req)

			if tt.expectedStatusCode >= 400 {
				if err == nil {
					t.Fatalf("expected error but got none")
				}
				var cloudErr *coreapi.CloudError
				if !errors.As(err, &cloudErr) {
					t.Fatalf("expected CloudError but got %T: %v", err, err)
				}
				if cloudErr.StatusCode != tt.expectedStatusCode {
					t.Errorf("expected status %d, got %d", tt.expectedStatusCode, cloudErr.StatusCode)
				}
				if tt.expectedError != "" && !strings.Contains(err.Error(), tt.expectedError) {
					t.Errorf("expected error containing %q, got %q", tt.expectedError, err.Error())
				}
				return
			}

			if tt.expectedError != "" {
				if err == nil {
					t.Fatalf("expected error but got none")
				}
				if !strings.Contains(err.Error(), tt.expectedError) {
					t.Errorf("expected error containing %q, got %q", tt.expectedError, err.Error())
				}
				return
			}

			if err != nil {
				t.Fatalf("expected no error but got %v", err)
			}

			var response OnDemandBackupResponse
			require.NoError(t, json.NewDecoder(recorder.Body).Decode(&response))
			// Order-independent: the handler returns backups in whatever order
			// the ReadDesire listing yields, which is not guaranteed stable.
			require.ElementsMatch(t, tt.expectedBackups, response.Backups, "listed backups do not match")
		})
	}
}
