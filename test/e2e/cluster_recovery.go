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

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azkeys"

	configv1client "github.com/openshift/client-go/config/clientset/versioned/typed/config/v1"

	"github.com/Azure/ARO-HCP/admin/server/handlers/hcp"
	clusterversion "github.com/Azure/ARO-HCP/backend/pkg/controllers/cluster/version"
	hcprecoveryv1alpha1 "github.com/Azure/ARO-HCP/hcp-recovery/pkg/apis/hcprecovery/v1alpha1"
	"github.com/Azure/ARO-HCP/internal/api/coreapi"
	"github.com/Azure/ARO-HCP/internal/api/metadataapi"
	"github.com/Azure/ARO-HCP/internal/backup"
	hcpsdk20240610preview "github.com/Azure/ARO-HCP/test/sdk/v20240610preview/resourcemanager/redhatopenshifthcp/armredhatopenshifthcp"
	hcpsdk20260901preview "github.com/Azure/ARO-HCP/test/sdk/v20260901preview/resourcemanager/redhatopenshifthcp/armredhatopenshifthcp"
	"github.com/Azure/ARO-HCP/test/util/framework"
	"github.com/Azure/ARO-HCP/test/util/labels"
	"github.com/Azure/ARO-HCP/test/util/timing"
	"github.com/Azure/ARO-HCP/test/util/verifiers"
)

type recoveryTestEnv struct {
	// KubeClient and AdminRESTConfig address the hosted cluster. Both are replaced with
	// freshly issued credentials after the recovery completes, so a hook must read them off
	// the env rather than capturing them.
	KubeClient      kubernetes.Interface
	AdminRESTConfig *rest.Config

	HCPClient         *hcpsdk20240610preview.HcpOpenShiftClustersClient
	NodePoolsClient   *hcpsdk20240610preview.NodePoolsClient
	NodePoolName      string
	ResourceGroupName string
	ClusterParams     framework.ClusterParams20240610
	ChannelGroup      string

	// InstallVersion is the control plane version ID the cluster was created with, and
	// UpgradeVersion the one resolved for recoveryScenario.upgradeMinor (empty when the
	// scenario does not upgrade). Both are recoveryVersion.ControlPlaneID, so they are an
	// exact build on nightly and a bare major.minor line elsewhere; compare them by
	// major.minor rather than by string equality.
	InstallVersion string
	UpgradeVersion string

	// UpgradeStartedBeforeRestore is scratch space for a scenario to hand state from one hook
	// to the next.
	UpgradeStartedBeforeRestore time.Time

	Suffix string
}

type recoveryScenario struct {
	// upgradeMinor, when set, is the minor version line the scenario upgrades the control
	// plane to. It is resolved to an exact build into recoveryTestEnv.UpgradeVersion before
	// any Azure resource is created, so an unavailable build skips the spec instead of
	// stranding a cluster. The scenario's hooks own triggering the upgrade.
	upgradeMinor string

	// initialNodePoolReplicas defaults to two when unset.
	initialNodePoolReplicas int32

	rotateCMKAfterBackup  bool
	expectedFailureReason string
	postFailure           func(ctx context.Context, env *recoveryTestEnv)

	preBackup   func(ctx context.Context, env *recoveryTestEnv)
	postBackup  func(ctx context.Context, env *recoveryTestEnv)
	postRestore func(ctx context.Context, env *recoveryTestEnv)
}

var _ = Describe("SRE", func() {
	DescribeTable("should be able to recover an HCP cluster from a backup",
		labels.MIContainers(1),
		func(ctx context.Context, version, channelGroup string, scenario recoveryScenario) {
			suffix := rand.String(6)
			clusterName := "recovery-" + suffix
			tc := framework.NewTestContext()
			if tc.UsePooledIdentities() {
				err := tc.AssignIdentityContainers(ctx, 1, framework.IdentityContainerAssignmentRetryInterval)
				Expect(err).NotTo(HaveOccurred(), "failed to assign pooled identity containers")
			}

			By(fmt.Sprintf("resolving the %s %s version to install", version, channelGroup))
			installVersion := resolveRecoveryVersion(ctx, channelGroup, version)

			// Resolving the upgrade target before anything is created keeps a missing build
			// a skip rather than a cluster that is created and then abandoned mid-spec.
			var upgradeVersion recoveryVersion
			if scenario.upgradeMinor != "" {
				By(fmt.Sprintf("resolving the %s %s version to upgrade to after the backup", scenario.upgradeMinor, channelGroup))
				upgradeVersion = resolveRecoveryVersion(ctx, channelGroup, scenario.upgradeMinor)
			}

			By("creating a resource group")
			resourceGroup, err := tc.NewResourceGroup(ctx, "hcp-recovery", tc.Location())
			Expect(err).NotTo(HaveOccurred(), "failed to create recovery test resource group")
			clusterParams := framework.NewDefaultClusterParams20240610()
			clusterParams.ClusterName = clusterName
			clusterParams.ManagedResourceGroupName = framework.SuffixName(*resourceGroup.Name+"-"+suffix, "-managed", 64)
			clusterParams.OpenshiftVersionId = installVersion.ControlPlaneID
			clusterParams.ChannelGroup = channelGroup
			infraParameters := map[string]any{}
			if scenario.rotateCMKAfterBackup {
				infraParameters["assignKeyVaultCryptoOfficer"] = true
				clusterParams.Tags[metadataapi.TagClusterBackupScheduleOverride] = to.Ptr(string(coreapi.BackupScheduleStateEnabled))
			}

			By("creating customer resources")
			clusterParams, err = tc.CreateClusterCustomerResources20240610(ctx, resourceGroup, clusterParams, infraParameters, TestArtifactsFS, framework.RBACScopeResourceGroup)
			Expect(err).NotTo(HaveOccurred(), "failed to create recovery test customer resources")

			By(fmt.Sprintf("creating an HCP cluster with version %q", installVersion.ControlPlaneID))
			err = tc.CreateHCPClusterFromParam20240610(ctx, GinkgoLogr, *resourceGroup.Name, clusterParams, framework.ClusterCreationTimeout)
			Expect(err).NotTo(HaveOccurred(), "failed to create recovery test HCP cluster")

			By("verifying the cluster is viable")
			hcpClient := tc.Get20240610ClientFactoryOrDie(ctx).NewHcpOpenShiftClustersClient()
			adminRESTConfig, err := tc.GetAdminRESTConfigForHCPCluster20240610(ctx, hcpClient, *resourceGroup.Name, clusterName, framework.GetAdminRESTConfigTimeout)
			Expect(err).NotTo(HaveOccurred(), "failed to get initial cluster credentials")
			Expect(verifiers.VerifyHCPCluster(ctx, adminRESTConfig)).To(Succeed(), "cluster should be viable before recovery")

			By("creating a node pool")
			nodePoolParams := framework.NewDefaultNodePoolParams20240610()
			nodePoolParams.NodePoolName = "np-1"
			nodePoolParams.ClusterName = clusterName
			nodePoolParams.Replicas = scenario.initialNodePoolReplicas
			if nodePoolParams.Replicas == 0 {
				nodePoolParams.Replicas = 2
			}
			nodePoolParams.OpenshiftVersionId = installVersion.NodePoolID
			nodePoolParams.ChannelGroup = channelGroup
			err = tc.CreateNodePoolFromParam20240610(ctx, GinkgoLogr, *resourceGroup.Name, clusterParams.ManagedResourceGroupName, clusterName, nodePoolParams, framework.NodePoolCreationTimeout)
			Expect(err).NotTo(HaveOccurred(), "failed to create recovery test node pool")

			// Console stays Available=False with "no OIDC client found" until an
			// ExternalAuth names a console client.
			By("configuring a temporary Dex identity provider so the console operator can become available")
			_, err = tc.SetupConsoleFixture20240610(ctx, *resourceGroup.Name, clusterName, adminRESTConfig, 15*time.Minute)
			Expect(err).NotTo(HaveOccurred(), "failed to set up Console login fixture for cluster %q", clusterName)

			// Operator availability is the signal in which the restore is deemed successful.
			By("verifying all cluster operators are available before the backup")
			expectClusterOperatorsAvailable(ctx, adminRESTConfig, "before the backup is taken")

			kubeClient, err := kubernetes.NewForConfig(adminRESTConfig)
			Expect(err).NotTo(HaveOccurred(), "failed to create initial Kubernetes client")
			hcpResourceID := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.RedHatOpenshift/hcpOpenShiftClusters/%s",
				metadataapi.Must(tc.SubscriptionID(ctx)), *resourceGroup.Name, clusterName)
			httpClient, adminAPIAddress, err := tc.NewAdminAPIHTTPClient(ctx)
			Expect(err).NotTo(HaveOccurred(), "failed to create admin API HTTP client")
			env := &recoveryTestEnv{
				KubeClient:        kubeClient,
				AdminRESTConfig:   adminRESTConfig,
				HCPClient:         hcpClient,
				NodePoolsClient:   tc.Get20240610ClientFactoryOrDie(ctx).NewNodePoolsClient(),
				NodePoolName:      nodePoolParams.NodePoolName,
				ResourceGroupName: *resourceGroup.Name,
				ClusterParams:     clusterParams,
				ChannelGroup:      channelGroup,
				InstallVersion:    installVersion.ControlPlaneID,
				UpgradeVersion:    upgradeVersion.ControlPlaneID,
				Suffix:            suffix,
			}

			By("preparing the cluster state to include in the backup")
			scenario.preBackup(ctx, env)

			By("requesting an on-demand backup through the admin API")
			createdBackup, err := framework.DoAdminAPIRequest[hcp.OnDemandBackupDetail](ctx, httpClient, http.MethodPost,
				fmt.Sprintf("%s/admin/v1/hcp%s/backups", adminAPIAddress, hcpResourceID), http.StatusOK, nil)
			Expect(err).NotTo(HaveOccurred(), "failed to request recovery test backup")
			Expect(createdBackup.Name).NotTo(BeEmpty(), "backup request should return a backup name")

			By(fmt.Sprintf("waiting for backup %s to complete", createdBackup.Name))
			completedBackup, err := framework.WaitForOnDemandBackupCompleted(ctx, httpClient, adminAPIAddress, hcpResourceID, createdBackup.Name)
			Expect(err).NotTo(HaveOccurred(), "recovery requires a completed backup")

			By("changing the cluster state after the backup")
			scenario.postBackup(ctx, env)

			if scenario.rotateCMKAfterBackup {
				originalFingerprint := backup.AzureKMSKeyFingerprint(clusterParams.KeyVaultName, clusterParams.EtcdEncryptionKeyName, clusterParams.EtcdEncryptionKeyVersion)
				Expect(completedBackup.KMSKeyFingerprint).To(Equal(originalFingerprint), "backup must record the original CMK before rotation")

				By("creating a new customer-managed key version after the backup")
				credential, err := tc.AzureCredential()
				Expect(err).NotTo(HaveOccurred(), "failed to get credentials for CMK rotation")
				keyClient, err := azkeys.NewClient(fmt.Sprintf("https://%s.vault.azure.net/", clusterParams.KeyVaultName), credential, nil)
				Expect(err).NotTo(HaveOccurred(), "failed to create the Key Vault client for CMK rotation")
				start := time.Now()
				key, err := keyClient.CreateKey(ctx, clusterParams.EtcdEncryptionKeyName, azkeys.CreateKeyParameters{
					Kty: to.Ptr(azkeys.KeyTypeRSA), KeySize: to.Ptr(int32(2048)),
				}, nil)
				tc.RecordTestOperation(timing.KindDataPlaneKey, "Create recovery test CMK version", start, time.Now(), err)
				Expect(err).NotTo(HaveOccurred(), "failed to create a new CMK version")
				Expect(key.Key).NotTo(BeNil(), "created CMK response Key was nil")
				Expect(key.Key.KID).NotTo(BeNil(), "created CMK response key ID was nil")
				keyVersion := key.Key.KID.Version()
				Expect(keyVersion).NotTo(BeEmpty(), "new CMK version must not be empty")
				Expect(keyVersion).NotTo(Equal(clusterParams.EtcdEncryptionKeyVersion), "rotation must produce a different CMK version")

				By("rotating the cluster to the new CMK and completing re-encryption")
				_, err = framework.UpdateHCPCluster20260901(ctx, tc.Get20260901ClientFactoryOrDie(ctx).NewHcpOpenShiftClustersClient(),
					*resourceGroup.Name, clusterName, hcpsdk20260901preview.HcpOpenShiftClusterUpdate{
						Properties: &hcpsdk20260901preview.HcpOpenShiftClusterPropertiesUpdate{
							Etcd: &hcpsdk20260901preview.EtcdProfileUpdate{
								DataEncryption: &hcpsdk20260901preview.EtcdDataEncryptionProfileUpdate{
									CustomerManaged: &hcpsdk20260901preview.CustomerManagedEncryptionProfileUpdate{
										Kms: &hcpsdk20260901preview.KmsEncryptionProfileUpdate{
											ActiveKey: &hcpsdk20260901preview.KmsKeyUpdate{Version: to.Ptr(keyVersion)},
										},
									},
								},
							},
						},
					}, HCPClusterReencryptionUpgradeTimeout)
				Expect(err).NotTo(HaveOccurred(), "failed to complete CMK rotation before requesting recovery")
				Expect(verifiers.VerifyStorageVersionMigrationSucceeded().Verify(ctx, adminRESTConfig)).To(Succeed(),
					"re-encryption must complete before testing old-key backup rejection")

				By("waiting for a completed rotation backup to prove the new active key is observed")
				expectedFingerprint := backup.AzureKMSKeyFingerprint(clusterParams.KeyVaultName, clusterParams.EtcdEncryptionKeyName, keyVersion)
				var rotationBackupName string
				Eventually(ctx, func() (bool, error) {
					resp, err := getOnDemandBackupsViaAdminAPI(ctx, httpClient, adminAPIAddress, hcpResourceID)
					if err != nil {
						return false, err
					}
					for _, b := range resp.Backups {
						if b.KMSKeyFingerprint == expectedFingerprint {
							rotationBackupName = b.Name
							return true, nil
						}
					}
					return false, nil
				}, framework.BackupWaitTimeout, framework.BackupWaitInterval).Should(BeTrue(),
					"rotation must produce a backup carrying the new observed active key fingerprint")
				_, err = framework.WaitForOnDemandBackupCompleted(ctx, httpClient, adminAPIAddress, hcpResourceID, rotationBackupName)
				Expect(err).NotTo(HaveOccurred(), "post-rotation backup must complete before testing old-key recovery")
			}

			By(fmt.Sprintf("requesting recovery from backup %s", createdBackup.Name))
			restoreResp, err := createRestoreViaAdminAPI(ctx, httpClient, adminAPIAddress, hcpResourceID, createdBackup.Name)
			Expect(err).NotTo(HaveOccurred(), "failed to request cluster recovery")
			Expect(restoreResp.RecoveryID).NotTo(BeEmpty(), "restore request should return a recovery ID")
			Expect(restoreResp.RecoveryState).To(Equal(coreapi.RecoveryStatePending), "new recovery should be Pending")

			expectedState := coreapi.RecoveryStateCompleted
			recoveryTimeout := 60 * time.Minute
			if scenario.expectedFailureReason != "" {
				expectedState = coreapi.RecoveryStateFailed
				recoveryTimeout = 10 * time.Minute
			}
			By(fmt.Sprintf("waiting for recovery state %s", expectedState))
			// RecoveryState sits on InProgress for most of the hour, so the
			// HCPRecovery phase proxied through the admin API is what makes a
			// stalled restore legible from the CI log alone.
			var previousState coreapi.RecoveryState
			var previousRecoveryPhase hcprecoveryv1alpha1.RestoreState
			var finalRecovery hcp.RestoreResponse
			Eventually(ctx, func() (coreapi.RecoveryState, error) {
				resp, err := getRestoreStatusViaAdminAPI(ctx, httpClient, adminAPIAddress, hcpResourceID, restoreResp.RecoveryID)
				if err != nil {
					return "", err
				}
				finalRecovery = resp
				var phase hcprecoveryv1alpha1.RestoreState
				if resp.Status != nil {
					phase = resp.Status.Phase
				}
				if resp.RecoveryState != previousState || phase != previousRecoveryPhase {
					GinkgoLogr.Info("waiting for recovery", "recoveryID", resp.RecoveryID, "observedState", resp.RecoveryState, "observedPhase", phase, "expectedState", expectedState)
					previousState, previousRecoveryPhase = resp.RecoveryState, phase
				}
				if (resp.RecoveryState == coreapi.RecoveryStateFailed || resp.RecoveryState == coreapi.RecoveryStateCompleted) && resp.RecoveryState != expectedState {
					StopTrying(fmt.Sprintf("recovery %s ended in state %s, phase %q; expected %s. conditions: %s",
						resp.RecoveryID, resp.RecoveryState, phase, expectedState, formatRecoveryConditions(resp.Status))).Now()
				}
				if scenario.expectedFailureReason != "" && phase != "" &&
					phase != hcprecoveryv1alpha1.RestoreStatePreparing && phase != hcprecoveryv1alpha1.RestoreStateFailed {
					StopTrying(fmt.Sprintf("recovery %s advanced to %s; expected input rejection before restore",
						resp.RecoveryID, phase)).Now()
				}
				return resp.RecoveryState, nil
			}, recoveryTimeout, 30*time.Second).Should(Equal(expectedState), "cluster recovery should reach %s", expectedState)

			if scenario.expectedFailureReason != "" {
				By("verifying recovery was rejected during input validation before any restore work")
				Expect(finalRecovery.Status).NotTo(BeNil(), "failed recovery must expose its HCPRecovery status")
				Expect(finalRecovery.Status.Phase).To(Equal(hcprecoveryv1alpha1.RestoreStateFailed), "invalid recovery inputs must be terminal")
				condition := meta.FindStatusCondition(finalRecovery.Status.Conditions, hcprecoveryv1alpha1.ConditionRecoveryInputsValidated)
				Expect(condition).NotTo(BeNil(), "failed recovery must report input validation")
				Expect(condition.Status).To(Equal(metav1.ConditionFalse), "incompatible CMK inputs must fail validation")
				Expect(condition.Reason).To(Equal(scenario.expectedFailureReason), "recovery must fail for the expected validation reason")
				Expect(condition.Message).To(ContainSubstring("observed active key"), "failure must explain the incompatible key combination")
				Expect(finalRecovery.Status.EtcdRuntimeDetails).To(BeNil(), "CMK rejection must precede runtime collection and workload mutation")
				Expect(finalRecovery.Status.RestoreName).To(BeEmpty(), "CMK rejection must not create a Velero Restore")
				Expect(finalRecovery.Status.CompletedAt).NotTo(BeNil(), "rejected recovery must record its completion")

				scenario.postFailure(ctx, env)
				Expect(verifiers.VerifyHCPCluster(ctx, adminRESTConfig)).To(Succeed(), "cluster must remain viable after rejecting the old-key backup")
				expectClusterOperatorsAvailable(ctx, adminRESTConfig, "after rejecting recovery from an incompatible CMK backup")
				return
			}

			By("getting fresh admin credentials after recovery")
			adminRESTConfig, err = tc.GetAdminRESTConfigForHCPCluster20240610(ctx, hcpClient, *resourceGroup.Name, clusterName, framework.GetAdminRESTConfigTimeout)
			Expect(err).NotTo(HaveOccurred(), "failed to get cluster credentials after recovery")
			env.AdminRESTConfig = adminRESTConfig
			env.KubeClient, err = kubernetes.NewForConfig(adminRESTConfig)
			Expect(err).NotTo(HaveOccurred(), "failed to create Kubernetes client after recovery")

			By("verifying the cluster state after recovery")
			scenario.postRestore(ctx, env)

			// Serving traffic and returning the restored data is not the same as having
			// recovered. A restore that leaves etcd resuming below the revision clients
			// already observed strands their watch caches, which surfaces here and
			// nowhere else: the API answers, the data is correct, and operators like
			// console, dns, ingress and monitoring sit degraded.
			Expect(verifiers.VerifyHCPCluster(ctx, adminRESTConfig)).To(Succeed(), "cluster should be viable after recovery")
			expectClusterOperatorsAvailable(ctx, adminRESTConfig, "after recovery")
		},
		Entry("and restore a deleted ConfigMap on 4.22 "+framework.DefaultOpenshiftChannelGroup(),
			labels.RequireNothing, labels.Critical, labels.Positive,
			labels.AroRpApiCompatible, labels.DevelopmentOnly, labels.Slow,
			"4.22", framework.DefaultOpenshiftChannelGroup(),
			recoveryScenario{
				preBackup:   createRecoveryTestConfigMap,
				postBackup:  deleteRecoveryTestConfigMap,
				postRestore: expectRecoveryTestConfigMapRestored,
			},
		),
		Entry("and reject a backup taken before CMK rotation on 4.22 "+framework.DefaultOpenshiftChannelGroup(),
			labels.RequireNothing, labels.High, labels.Negative,
			labels.DevelopmentOnly, labels.Slow,
			"4.22", framework.DefaultOpenshiftChannelGroup(),
			recoveryScenario{
				rotateCMKAfterBackup:  true,
				expectedFailureReason: "BackupKMSKeyFingerprintMismatch",
				preBackup:             createRecoveryTestConfigMap,
				postBackup:            deleteRecoveryTestConfigMap,
				postFailure: func(ctx context.Context, env *recoveryTestEnv) {
					By("verifying the rejected recovery did not restore the deleted ConfigMap")
					_, err := env.KubeClient.CoreV1().ConfigMaps("default").Get(ctx, recoveryTestConfigMapName, metav1.GetOptions{})
					Expect(apierrors.IsNotFound(err)).To(BeTrue(), "ConfigMap must remain deleted after CMK rejection; observed error: %v", err)
				},
			},
		),
		Entry("after scaling a single node pool from two to three nodes on 4.22 "+framework.DefaultOpenshiftChannelGroup(),
			labels.RequireNothing, labels.Critical, labels.Positive,
			labels.AroRpApiCompatible, labels.DevelopmentOnly, labels.Slow,
			"4.22", framework.DefaultOpenshiftChannelGroup(),
			recoveryScenario{
				preBackup: func(ctx context.Context, env *recoveryTestEnv) {
					expectRecoveryNodePool(ctx, env, 2, "before the backup")
					createRecoveryTestConfigMap(ctx, env)
				},
				postBackup: func(ctx context.Context, env *recoveryTestEnv) {
					deleteRecoveryTestConfigMap(ctx, env)

					scaleRecoveryNodePool(ctx, env, 3)
					expectRecoveryNodePool(ctx, env, 3, "after scaling and before the restore")
				},
				postRestore: func(ctx context.Context, env *recoveryTestEnv) {
					expectRecoveryTestConfigMapRestored(ctx, env)

					// Restoring guest etcd must not roll back the customer's ARM replica
					// setting. The third node, absent from the backup, must reconcile.
					expectRecoveryNodePool(ctx, env, 3, "after restoring the two-node backup")
				},
			},
		),
		Entry("after scaling a single node pool from three to two nodes on 4.22 "+framework.DefaultOpenshiftChannelGroup(),
			labels.RequireNothing, labels.Critical, labels.Positive,
			labels.AroRpApiCompatible, labels.DevelopmentOnly, labels.Slow,
			"4.22", framework.DefaultOpenshiftChannelGroup(),
			recoveryScenario{
				initialNodePoolReplicas: 3,
				preBackup: func(ctx context.Context, env *recoveryTestEnv) {
					expectRecoveryNodePool(ctx, env, 3, "before the backup")
					createRecoveryTestConfigMap(ctx, env)
				},
				postBackup: func(ctx context.Context, env *recoveryTestEnv) {
					deleteRecoveryTestConfigMap(ctx, env)

					scaleRecoveryNodePool(ctx, env, 2)
					expectRecoveryNodePool(ctx, env, 2, "after scaling and before the restore")
				},
				postRestore: func(ctx context.Context, env *recoveryTestEnv) {
					expectRecoveryTestConfigMapRestored(ctx, env)

					// The backup contains the removed third node. Recovery must reconcile
					// it away without reverting the customer's two-replica ARM setting.
					expectRecoveryNodePool(ctx, env, 2, "after restoring the three-node backup")
				},
			},
		),
		// A restore rolls the hosted cluster's etcd back past a completed minor upgrade: the
		// restored ClusterVersion names the install version while the control plane pods on
		// the management cluster, and the HostedCluster that drives them, are already at the
		// upgrade version. The HostedCluster is the source of truth and is untouched by the
		// restore, so the control plane must notice the reverted ClusterVersion and drive the
		// upgrade again rather than sit split-brained or wedge its operators.
		//
		// This runs on candidate rather than stable because 4.21 -> 4.22 needs a published
		// y-stream upgrade edge and stable has none: as of 2026-11-04 channel stable-4.22
		// carries both 4.21.x and 4.22.x releases but zero edges between them (checked for
		// arch multi and amd64), while candidate-4.22 carries 42. The only stable y-stream
		// pair with edges today is 4.20 -> 4.21, so a stable entry would have to drop a minor.
		Entry("after upgrading the control plane from 4.21 to 4.22 candidate",
			labels.RequireNothing, labels.Critical, labels.Positive,
			labels.AroRpApiCompatible, labels.DevelopmentOnly, labels.Slow,
			"4.21", "candidate",
			recoveryScenario{
				upgradeMinor: "4.22",
				preBackup:    createRecoveryTestConfigMap,
				postBackup: func(ctx context.Context, env *recoveryTestEnv) {
					deleteRecoveryTestConfigMap(ctx, env)

					By(fmt.Sprintf("upgrading the control plane from %s to %s", env.InstallVersion, env.UpgradeVersion))
					upgradeVersionLine, upgradeTags := framework.ControlPlaneExactVersionPatchTags(env.UpgradeVersion)
					_, err := framework.UpdateHCPCluster20240610(ctx, env.HCPClient, env.ResourceGroupName, env.ClusterParams.ClusterName,
						hcpsdk20240610preview.HcpOpenShiftClusterUpdate{
							Tags: framework.TagsForPatch(env.ClusterParams.Tags, upgradeTags),
							Properties: &hcpsdk20240610preview.HcpOpenShiftClusterPropertiesUpdate{
								Version: &hcpsdk20240610preview.VersionProfile{
									ID:           to.Ptr(upgradeVersionLine),
									ChannelGroup: to.Ptr(env.ChannelGroup),
								},
							},
						}, framework.HCPClusterVersionUpgradeTimeout)
					Expect(err).NotTo(HaveOccurred(), "failed to trigger upgrade of cluster %q to %s", env.ClusterParams.ClusterName, env.UpgradeVersion)

					// The restore has to roll back a finished upgrade, not one still in
					// flight, or the test says nothing about which state the control plane
					// converges on.
					By(fmt.Sprintf("waiting for the control plane to settle on %s before the restore", env.UpgradeVersion))
					Eventually(ctx, func() error {
						return errors.Join(
							verifiers.VerifyHostedControlPlaneVersionCompleted(env.UpgradeVersion).Verify(ctx, env.AdminRESTConfig),
							// ClusterVersion goes Completed before the operators finish
							// rolling out, and a backup taken mid-rollout would capture
							// operators the restore then has to converge for unrelated
							// reasons.
							verifiers.VerifyAllClusterOperatorsAtClusterVersion().Verify(ctx, env.AdminRESTConfig),
						)
					}, framework.HCPClusterVersionUpgradeTimeout, 2*time.Minute).Should(Succeed(),
						"control plane should complete the upgrade to %s before the restore", env.UpgradeVersion)

					By("verifying all cluster operators are available after the upgrade")
					expectClusterOperatorsAvailable(ctx, env.AdminRESTConfig, "after the upgrade and before the restore")

					// Recording when this upgrade started is what makes the post-restore
					// assertion falsifiable. The restored ClusterVersion carries this same
					// history entry, so a cluster that is simply still at the upgrade version
					// cannot pass for one that reverted and upgraded again.
					startedTime, err := clusterVersionUpgradeStartTime(ctx, env.AdminRESTConfig, env.UpgradeVersion)
					Expect(err).NotTo(HaveOccurred(), "failed to read the pre-restore clusterversion upgrade start time")
					env.UpgradeStartedBeforeRestore = startedTime
					GinkgoLogr.Info("recorded the pre-restore upgrade start time",
						"version", env.UpgradeVersion, "startedTime", startedTime.Format(time.RFC3339))
				},
				postRestore: func(ctx context.Context, env *recoveryTestEnv) {
					// Logged, not asserted: the control plane re-drives the upgrade as soon
					// as the hosted cluster resumes, and that can beat this read. The
					// falsifiable proof of the rollback is the restored ConfigMap plus the
					// upgrade start time below.
					By("recording the cluster version observed immediately after the restore")
					configClient, err := configv1client.NewForConfig(env.AdminRESTConfig)
					Expect(err).NotTo(HaveOccurred(), "failed to create config client after recovery")
					clusterVersion, err := configClient.ClusterVersions().Get(ctx, "version", metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred(), "failed to read clusterversion after recovery")
					GinkgoLogr.Info("clusterversion observed immediately after recovery",
						"installVersion", env.InstallVersion, "upgradeVersion", env.UpgradeVersion,
						"desired", clusterVersion.Status.Desired.Version,
						"history", framework.SummarizeClusterVersionHistory(clusterVersion.Status.History))

					expectRecoveryTestConfigMapRestored(ctx, env)

					// ClusterVersion reporting the update Completed is not the end of the
					// upgrade. The restore rolls every ClusterOperator object back to the
					// version it reported before the upgrade, and those operators re-roll
					// their operands out on their own schedule: a cluster whose
					// ClusterVersion says 4.22 can still have, say, monitoring reporting
					// 4.21 with Progressing=True minutes later. Asserting only on
					// ClusterVersion ends the test in the middle of that churn, so wait for
					// every operator to report the driven version and stop progressing.
					By(fmt.Sprintf("verifying the control plane completes the upgrade to %s again after recovery", env.UpgradeVersion))
					Eventually(ctx, func() error {
						return errors.Join(
							verifiers.VerifyHostedControlPlaneVersionCompleted(env.UpgradeVersion).Verify(ctx, env.AdminRESTConfig),
							verifiers.VerifyHostedControlPlaneUpgradeStartedAfter(env.UpgradeVersion, env.UpgradeStartedBeforeRestore).Verify(ctx, env.AdminRESTConfig),
							verifiers.VerifyAllClusterOperatorsAtClusterVersion().Verify(ctx, env.AdminRESTConfig),
						)
					}, framework.HCPClusterVersionUpgradeTimeout, 2*time.Minute).Should(Succeed(),
						"the restore should revert the control plane to %s and the hosted cluster should then upgrade it back to %s, with every cluster operator settled on it",
						env.InstallVersion, env.UpgradeVersion)
				},
			},
		),
	)
})

// recoveryVersion carries the two forms of one version line, because the RP accepts different
// forms for the two resources. A cluster's version.id may be a bare major.minor that the RP
// resolves, but a node pool's version.id must be a concrete Major.Minor.Patch — the RP rejects
// a bare line there with "No Major.Minor.Patch elements found". See the same split in
// framework.DefaultOpenshiftNodePoolVersionId.
type recoveryVersion struct {
	// ControlPlaneID is sent as the cluster's version.id and to ControlPlaneExactVersionPatchTags.
	ControlPlaneID string
	// NodePoolID is sent as the node pool's version.id, always a concrete Major.Minor.Patch.
	NodePoolID string
}

// resolveRecoveryVersion returns the version IDs to hand the RP for a minor line, skipping the
// spec when that line has no usable build rather than failing it.
//
// The nightly channel group is absent from the update service graph, so it resolves to an exact
// accepted build tag that serves as both forms. Every other channel group lets the control plane
// take the bare major.minor and resolve it, while the node pool gets the concrete z-stream the
// same selector the RP uses picks for that line, so both land on the same release.
//
// Note what this does not check: that an upgrade edge exists between two lines. The graph
// publishes y-stream edges per channel, and a pair can have releases on both lines with no edge
// between them, which the RP rejects only once the upgrade is requested.
func resolveRecoveryVersion(ctx context.Context, channelGroup, minorLine string) recoveryVersion {
	GinkgoHelper()

	if channelGroup == "nightly" {
		versionID, err := framework.GetLatestNightlyInstallVersion(ctx, channelGroup, minorLine)
		if framework.IsVersionNotFoundError(err) || framework.IsIncompatibleNightlyVersionError(err) {
			Skip(fmt.Sprintf("no usable nightly version for %s in %s channel: %v", minorLine, channelGroup, err))
		}
		Expect(err).NotTo(HaveOccurred(), "failed to resolve nightly version for %s", minorLine)
		return recoveryVersion{ControlPlaneID: versionID, NodePoolID: versionID}
	}

	channel := fmt.Sprintf("%s-%s", channelGroup, minorLine)
	release, err := framework.SelectControlPlaneVersion(ctx, http.DefaultTransport.RoundTrip, nil, channel, clusterversion.GetZStreamOffset(channelGroup))
	if err != nil {
		Skip(fmt.Sprintf("failed to resolve a version for channel %s: %v", channel, err))
	}
	if release == nil {
		Skip(fmt.Sprintf("no version resolved for channel %s", channel))
	}
	resolved := recoveryVersion{ControlPlaneID: minorLine, NodePoolID: release.Version}
	GinkgoLogr.Info("resolved version line", "channel", channel,
		"controlPlaneVersionID", resolved.ControlPlaneID, "nodePoolVersionID", resolved.NodePoolID)
	return resolved
}

const recoveryTestConfigMapName = "recovery-test-cm"

func scaleRecoveryNodePool(ctx context.Context, env *recoveryTestEnv, replicas int32) {
	GinkgoHelper()

	By(fmt.Sprintf("scaling the existing node pool to %d nodes after the backup", replicas))
	_, err := framework.UpdateNodePoolAndWait20240610(ctx, env.NodePoolsClient,
		env.ResourceGroupName, env.ClusterParams.ClusterName, env.NodePoolName,
		hcpsdk20240610preview.NodePoolUpdate{
			Properties: &hcpsdk20240610preview.NodePoolPropertiesUpdate{
				Replicas: to.Ptr(replicas),
			},
		}, framework.NodePoolScalingTimeout)
	Expect(err).NotTo(HaveOccurred(), "failed to scale recovery test node pool %q to %d nodes", env.NodePoolName, replicas)
}

func expectRecoveryNodePool(ctx context.Context, env *recoveryTestEnv, replicas int32, when string) {
	GinkgoHelper()

	By(fmt.Sprintf("verifying ARM still reports exactly one node pool with %d replicas %s", replicas, when))
	var nodePools []*hcpsdk20240610preview.NodePool
	pager := env.NodePoolsClient.NewListByParentPager(env.ResourceGroupName, env.ClusterParams.ClusterName, nil)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		Expect(err).NotTo(HaveOccurred(), "failed to list recovery test node pools %s", when)
		nodePools = append(nodePools, page.Value...)
	}
	Expect(nodePools).To(HaveLen(1), "expected exactly one ARM node pool %s", when)
	nodePool := nodePools[0]
	Expect(nodePool).NotTo(BeNil(), "ARM node pool was nil %s", when)
	Expect(nodePool.Name).To(HaveValue(Equal(env.NodePoolName)), "unexpected ARM node pool %s", when)
	Expect(nodePool.Properties).NotTo(BeNil(), "ARM node pool Properties was nil %s", when)
	Expect(nodePool.Properties.Replicas).To(HaveValue(Equal(replicas)), "ARM node pool should retain %d replicas %s", replicas, when)

	By(fmt.Sprintf("verifying the customer sees exactly %d ready, schedulable nodes in the same pool %s", replicas, when))
	Expect(verifiers.VerifyNodePoolConverged(env.NodePoolName, int(replicas), framework.NodePoolScalingTimeout).
		Verify(ctx, env.AdminRESTConfig)).To(Succeed(),
		"customer cluster should have exactly %d ready, schedulable nodes belonging to node pool %q, with no stale or extra nodes %s",
		replicas, env.NodePoolName, when)
}

// createRecoveryTestConfigMap writes the test data whose presence after the restore shows the
// recovery served the backup's contents rather than whatever etcd happened to hold.
func createRecoveryTestConfigMap(ctx context.Context, env *recoveryTestEnv) {
	By("creating a ConfigMap to capture in the etcd backup")
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: recoveryTestConfigMapName, Namespace: "default"},
		Data:       map[string]string{"test-key": "test-value-" + env.Suffix},
	}
	_, err := env.KubeClient.CoreV1().ConfigMaps("default").Create(ctx, cm, metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred(), "failed to create recovery test ConfigMap")
}

func deleteRecoveryTestConfigMap(ctx context.Context, env *recoveryTestEnv) {
	By("deleting the ConfigMap so only recovery can bring it back")
	err := env.KubeClient.CoreV1().ConfigMaps("default").Delete(ctx, recoveryTestConfigMapName, metav1.DeleteOptions{})
	Expect(err).NotTo(HaveOccurred(), "failed to delete recovery test ConfigMap")
	Eventually(ctx, func() (bool, error) {
		_, err := env.KubeClient.CoreV1().ConfigMaps("default").Get(ctx, recoveryTestConfigMapName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}, time.Minute, 5*time.Second).Should(BeTrue(), "ConfigMap must be absent before recovery")
}

func expectRecoveryTestConfigMapRestored(ctx context.Context, env *recoveryTestEnv) {
	By("verifying the deleted ConfigMap and its original data were restored")
	Eventually(ctx, func() error {
		cm, err := env.KubeClient.CoreV1().ConfigMaps("default").Get(ctx, recoveryTestConfigMapName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("failed to get restored ConfigMap default/%s: %w", recoveryTestConfigMapName, err)
		}
		expected := "test-value-" + env.Suffix
		if cm.Data["test-key"] != expected {
			return fmt.Errorf("restored ConfigMap data: expected %q, observed %q", expected, cm.Data["test-key"])
		}
		return nil
	}, 5*time.Minute, 15*time.Second).Should(Succeed(), "recovery should restore the ConfigMap from the selected backup")
}

// clusterVersionUpgradeStartTime returns when the newest clusterversion status.history entry in
// targetVersion's minor line was started.
func clusterVersionUpgradeStartTime(ctx context.Context, adminRESTConfig *rest.Config, targetVersion string) (time.Time, error) {
	configClient, err := configv1client.NewForConfig(adminRESTConfig)
	if err != nil {
		return time.Time{}, fmt.Errorf("failed to create config client: %w", err)
	}
	clusterVersion, err := configClient.ClusterVersions().Get(ctx, "version", metav1.GetOptions{})
	if err != nil {
		return time.Time{}, fmt.Errorf("failed to get clusterversion %q: %w", "version", err)
	}
	entry, err := framework.LatestClusterVersionHistoryEntryForMinor(clusterVersion.Status.History, targetVersion)
	if err != nil {
		return time.Time{}, err
	}
	if entry == nil {
		return time.Time{}, fmt.Errorf("clusterversion status.history has no entry for %q: %v",
			targetVersion, framework.SummarizeClusterVersionHistory(clusterVersion.Status.History))
	}
	return entry.StartedTime.Time, nil
}

func createRestoreViaAdminAPI(ctx context.Context, httpClient *http.Client, adminAPIAddress, resourceID, backupName string) (hcp.RestoreResponse, error) {
	body, err := json.Marshal(hcp.RestoreRequest{BackupID: backupName})
	if err != nil {
		return hcp.RestoreResponse{}, fmt.Errorf("failed to marshal restore request: %w", err)
	}
	return framework.DoAdminAPIRequest[hcp.RestoreResponse](ctx, httpClient, http.MethodPost,
		fmt.Sprintf("%s/admin/v1/hcp%s/restore", adminAPIAddress, resourceID), http.StatusOK, bytes.NewReader(body))
}

func getRestoreStatusViaAdminAPI(ctx context.Context, httpClient *http.Client, adminAPIAddress, resourceID, recoveryID string) (hcp.RestoreResponse, error) {
	return framework.DoAdminAPIRequest[hcp.RestoreResponse](ctx, httpClient, http.MethodGet,
		fmt.Sprintf("%s/admin/v1/hcp%s/restore?recoveryID=%s", adminAPIAddress, resourceID, url.QueryEscape(recoveryID)), http.StatusOK, nil)
}

// formatRecoveryConditions renders the HCPRecovery conditions that are not True,
// which is where the reason a failed recovery stopped is recorded.
func formatRecoveryConditions(status *hcprecoveryv1alpha1.HCPRecoveryStatus) string {
	if status == nil {
		return "<no HCPRecovery observed>"
	}
	var unmet []string
	for _, condition := range status.Conditions {
		if condition.Status == metav1.ConditionTrue {
			continue
		}
		unmet = append(unmet, fmt.Sprintf("%s=%s(%s): %s", condition.Type, condition.Status, condition.Reason, condition.Message))
	}
	if len(unmet) == 0 {
		return "<all conditions True>"
	}
	return strings.Join(unmet, "; ")
}

// expectClusterOperatorsAvailable asserts every ClusterOperator reports Available=True.
func expectClusterOperatorsAvailable(ctx context.Context, adminRESTConfig *rest.Config, when string) {
	GinkgoHelper()

	Expect(verifiers.VerifyAllClusterOperatorsAvailable().Verify(ctx, adminRESTConfig)).To(Succeed(),
		"cluster operators should be available %s", when)
}
