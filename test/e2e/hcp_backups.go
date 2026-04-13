// Copyright 2025 Microsoft Corporation
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
	"fmt"
	"io"
	"net/http"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/Azure/ARO-HCP/internal/api"
	"github.com/Azure/ARO-HCP/test/util/framework"
	"github.com/Azure/ARO-HCP/test/util/labels"
)

type backupResponse struct {
	Name                string `json:"name"`
	StartTimestamp      string `json:"startTimestamp"`
	CompletionTimestamp string `json:"completionTimestamp"`
	Phase               string `json:"phase"`
}

type listBackupsResponse struct {
	ResourceID string           `json:"resourceID"`
	Backups    []backupResponse `json:"backups"`
}

type getBackupResponse struct {
	ResourceID string         `json:"resourceID"`
	Backup     backupResponse `json:"backup"`
}

type backupProfileResponse struct {
	ResourceID       string `json:"resourceID"`
	State            string `json:"state"`
	LastBackupTime   string `json:"lastBackupTime,omitempty"`
	LastBackupStatus string `json:"lastBackupStatus,omitempty"`
}

type backupProfilePatchRequest struct {
	State string `json:"state"`
}

// TODO: a ridiculous timeout for now until instant access or etcdctl snapshotting is available
const backupTimeout = 45 * time.Minute

var _ = Describe("Backups", func() {
	// TODO: Enable and determine how this behaves if backup schedules are set to 5 minute cron
	XIt("should be created by the schedule for an HCP cluster",
		labels.RequireNothing,
		labels.High,
		labels.Positive,
		labels.CoreInfraService,
		labels.DevelopmentOnly,
		labels.Slow,
		func(ctx context.Context) {
			const (
				backupNetworkSecurityGroupName = "backup-nsg-name"
				backupVnetName                 = "backup-vnet-name"
				backupVnetSubnetName           = "backup-vnet-subnet1"
				backupClusterName              = "backup-hcp-cluster"
			)

			tc := framework.NewTestContext()

			if tc.UsePooledIdentities() {
				err := tc.AssignIdentityContainers(ctx, 1, 60*time.Second)
				Expect(err).NotTo(HaveOccurred())
			}

			By("creating a resource group")
			resourceGroup, err := tc.NewResourceGroup(ctx, "backup-e2e", tc.Location())
			Expect(err).NotTo(HaveOccurred())

			By("creating cluster parameters")
			clusterParams := framework.NewDefaultClusterParams()
			clusterParams.ClusterName = backupClusterName
			managedResourceGroupName := framework.SuffixName(*resourceGroup.Name, "-managed", 64)
			clusterParams.ManagedResourceGroupName = managedResourceGroupName

			By("creating customer resources")
			clusterParams, err = tc.CreateClusterCustomerResources(ctx,
				resourceGroup,
				clusterParams,
				map[string]interface{}{
					"customerNsgName":        backupNetworkSecurityGroupName,
					"customerVnetName":       backupVnetName,
					"customerVnetSubnetName": backupVnetSubnetName,
				},
				TestArtifactsFS,
				framework.RBACScopeResourceGroup,
			)
			Expect(err).NotTo(HaveOccurred())

			By("creating the HCP cluster")
			err = tc.CreateHCPClusterFromParam(
				ctx,
				GinkgoLogr,
				*resourceGroup.Name,
				clusterParams,
				45*time.Minute,
			)
			Expect(err).NotTo(HaveOccurred())

			hcpResourceID := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.RedHatOpenshift/hcpOpenShiftClusters/%s",
				api.Must(tc.SubscriptionID(ctx)), *resourceGroup.Name, backupClusterName)

			By("creating admin API HTTP client")
			httpClient, adminAPIAddress, err := tc.NewAdminAPIHTTPClient(ctx)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for at least one backup to appear")
			var backups []backupResponse
			Eventually(func() (int, error) {
				resp, err := listBackupsViaAdminAPI(ctx, httpClient, adminAPIAddress, hcpResourceID)
				if err != nil {
					return 0, err
				}
				backups = resp.Backups
				return len(backups), nil
			}, 10*time.Minute, 30*time.Second).Should(BeNumerically(">", 0))

			By("waiting for a backup to complete")
			var completedBackupName string
			Eventually(func() (string, error) {
				resp, err := listBackupsViaAdminAPI(ctx, httpClient, adminAPIAddress, hcpResourceID)
				if err != nil {
					return "", err
				}
				for _, b := range resp.Backups {
					if b.Phase == "Completed" {
						completedBackupName = b.Name
						return b.Phase, nil
					}
				}
				phases := make([]string, len(resp.Backups))
				for i, b := range resp.Backups {
					phases[i] = fmt.Sprintf("%s=%s", b.Name, b.Phase)
				}
				return fmt.Sprintf("no completed backup yet: %v", phases), nil
			}, backupTimeout, 15*time.Second).Should(Equal("Completed"))

			By(fmt.Sprintf("verifying backup %s via get endpoint", completedBackupName))
			getResp, err := getBackupViaAdminAPI(ctx, httpClient, adminAPIAddress, hcpResourceID, completedBackupName)
			Expect(err).NotTo(HaveOccurred())
			Expect(getResp.Backup.Name).To(Equal(completedBackupName))
			Expect(getResp.Backup.Phase).To(Equal("Completed"))
			Expect(getResp.Backup.StartTimestamp).NotTo(BeEmpty())
			Expect(getResp.Backup.CompletionTimestamp).NotTo(BeEmpty())
		})

	It("can be created on-demand for an HCP cluster",
		labels.RequireNothing,
		labels.High,
		labels.Positive,
		labels.CoreInfraService,
		labels.DevelopmentOnly,
		labels.Slow,
		func(ctx context.Context) {
			const (
				manualBackupNsgName        = "manual-bkp-nsg-name"
				manualBackupVnetName       = "manual-bkp-vnet-name"
				manualBackupVnetSubnetName = "manual-bkp-vnet-subnet1"
				manualBackupClusterName    = "manual-bkp-cluster"
			)

			tc := framework.NewTestContext()

			if tc.UsePooledIdentities() {
				err := tc.AssignIdentityContainers(ctx, 1, 60*time.Second)
				Expect(err).NotTo(HaveOccurred())
			}

			By("creating a resource group")
			resourceGroup, err := tc.NewResourceGroup(ctx, "manual-bkp-e2e", tc.Location())
			Expect(err).NotTo(HaveOccurred())

			By("creating cluster parameters")
			clusterParams := framework.NewDefaultClusterParams()
			clusterParams.ClusterName = manualBackupClusterName
			managedResourceGroupName := framework.SuffixName(*resourceGroup.Name, "-managed", 64)
			clusterParams.ManagedResourceGroupName = managedResourceGroupName

			By("creating customer resources")
			clusterParams, err = tc.CreateClusterCustomerResources(ctx,
				resourceGroup,
				clusterParams,
				map[string]interface{}{
					"customerNsgName":        manualBackupNsgName,
					"customerVnetName":       manualBackupVnetName,
					"customerVnetSubnetName": manualBackupVnetSubnetName,
				},
				TestArtifactsFS,
				framework.RBACScopeResourceGroup,
			)
			Expect(err).NotTo(HaveOccurred())

			By("creating the HCP cluster")
			err = tc.CreateHCPClusterFromParam(
				ctx,
				GinkgoLogr,
				*resourceGroup.Name,
				clusterParams,
				45*time.Minute,
			)
			Expect(err).NotTo(HaveOccurred())

			hcpResourceID := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.RedHatOpenshift/hcpOpenShiftClusters/%s",
				api.Must(tc.SubscriptionID(ctx)), *resourceGroup.Name, manualBackupClusterName)

			By("creating admin API HTTP client")
			httpClient, adminAPIAddress, err := tc.NewAdminAPIHTTPClient(ctx)
			Expect(err).NotTo(HaveOccurred())

			By("creating a manual on-demand backup")
			createdBackup, err := createBackupViaAdminAPI(ctx, httpClient, adminAPIAddress, hcpResourceID)
			Expect(err).NotTo(HaveOccurred())
			Expect(createdBackup.Name).NotTo(BeEmpty())

			By(fmt.Sprintf("waiting for backup %s to complete", createdBackup.Name))
			Eventually(func() (string, error) {
				resp, err := getBackupViaAdminAPI(ctx, httpClient, adminAPIAddress, hcpResourceID, createdBackup.Name)
				if err != nil {
					return "", err
				}
				return resp.Backup.Phase, nil
			}, backupTimeout, 15*time.Second).Should(Equal("Completed"))

			By("verifying completed backup details")
			getResp, err := getBackupViaAdminAPI(ctx, httpClient, adminAPIAddress, hcpResourceID, createdBackup.Name)
			Expect(err).NotTo(HaveOccurred())
			Expect(getResp.Backup.Name).To(Equal(createdBackup.Name))
			Expect(getResp.Backup.Phase).To(Equal("Completed"))
			Expect(getResp.Backup.StartTimestamp).NotTo(BeEmpty())
			Expect(getResp.Backup.CompletionTimestamp).NotTo(BeEmpty())
		})

	It("can pause and activate backup schedules via the admin API",
		labels.RequireNothing,
		labels.High,
		labels.Positive,
		labels.CoreInfraService,
		labels.DevelopmentOnly,
		labels.Slow,
		func(ctx context.Context) {
			const (
				profileNsgName        = "profile-nsg-name"
				profileVnetName       = "profile-vnet-name"
				profileVnetSubnetName = "profile-vnet-subnet1"
				profileClusterName    = "profile-hcp-cluster"
			)

			tc := framework.NewTestContext()

			if tc.UsePooledIdentities() {
				err := tc.AssignIdentityContainers(ctx, 1, 60*time.Second)
				Expect(err).NotTo(HaveOccurred())
			}

			By("creating a resource group")
			resourceGroup, err := tc.NewResourceGroup(ctx, "profile-e2e", tc.Location())
			Expect(err).NotTo(HaveOccurred())

			By("creating cluster parameters")
			clusterParams := framework.NewDefaultClusterParams()
			clusterParams.ClusterName = profileClusterName
			managedResourceGroupName := framework.SuffixName(*resourceGroup.Name, "-managed", 64)
			clusterParams.ManagedResourceGroupName = managedResourceGroupName

			By("creating customer resources")
			clusterParams, err = tc.CreateClusterCustomerResources(ctx,
				resourceGroup,
				clusterParams,
				map[string]interface{}{
					"customerNsgName":        profileNsgName,
					"customerVnetName":       profileVnetName,
					"customerVnetSubnetName": profileVnetSubnetName,
				},
				TestArtifactsFS,
				framework.RBACScopeResourceGroup,
			)
			Expect(err).NotTo(HaveOccurred())

			By("creating the HCP cluster")
			err = tc.CreateHCPClusterFromParam(
				ctx,
				GinkgoLogr,
				*resourceGroup.Name,
				clusterParams,
				45*time.Minute,
			)
			Expect(err).NotTo(HaveOccurred())

			hcpResourceID := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.RedHatOpenshift/hcpOpenShiftClusters/%s",
				api.Must(tc.SubscriptionID(ctx)), *resourceGroup.Name, profileClusterName)

			By("creating admin API HTTP client")
			httpClient, adminAPIAddress, err := tc.NewAdminAPIHTTPClient(ctx)
			Expect(err).NotTo(HaveOccurred())

			By("verifying default backup profile state is Active")
			profile, err := getBackupProfileViaAdminAPI(ctx, httpClient, adminAPIAddress, hcpResourceID)
			Expect(err).NotTo(HaveOccurred())
			Expect(profile.ResourceID).NotTo(BeEmpty())
			Expect(profile.State).To(Equal("Active"))

			By("pausing the backup schedule")
			profile, err = patchBackupProfileViaAdminAPI(ctx, httpClient, adminAPIAddress, hcpResourceID, "Paused")
			Expect(err).NotTo(HaveOccurred())
			Expect(profile.State).To(Equal("Paused"))

			By("verifying backup profile state persisted as Paused")
			profile, err = getBackupProfileViaAdminAPI(ctx, httpClient, adminAPIAddress, hcpResourceID)
			Expect(err).NotTo(HaveOccurred())
			Expect(profile.State).To(Equal("Paused"))

			By("activating the backup schedule")
			profile, err = patchBackupProfileViaAdminAPI(ctx, httpClient, adminAPIAddress, hcpResourceID, "Active")
			Expect(err).NotTo(HaveOccurred())
			Expect(profile.State).To(Equal("Active"))

			By("verifying backup profile state persisted as Active")
			profile, err = getBackupProfileViaAdminAPI(ctx, httpClient, adminAPIAddress, hcpResourceID)
			Expect(err).NotTo(HaveOccurred())
			Expect(profile.State).To(Equal("Active"))

			By("verifying invalid state returns 400")
			statusCode, _, err := patchBackupProfileRawViaAdminAPI(ctx, httpClient, adminAPIAddress, hcpResourceID, "InvalidState")
			Expect(err).NotTo(HaveOccurred())
			Expect(statusCode).To(Equal(http.StatusBadRequest))
		})
})

func listBackupsViaAdminAPI(ctx context.Context, httpClient *http.Client, adminAPIAddress, resourceID string) (listBackupsResponse, error) {
	url := fmt.Sprintf("%s/admin/v1/hcp%s/backups", adminAPIAddress, resourceID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return listBackupsResponse{}, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return listBackupsResponse{}, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return listBackupsResponse{}, fmt.Errorf("list backups returned %d: %s", resp.StatusCode, string(body))
	}

	var result listBackupsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return listBackupsResponse{}, fmt.Errorf("failed to decode response: %w", err)
	}
	return result, nil
}

func getBackupViaAdminAPI(ctx context.Context, httpClient *http.Client, adminAPIAddress, resourceID, backupName string) (getBackupResponse, error) {
	url := fmt.Sprintf("%s/admin/v1/hcp%s/backups/%s", adminAPIAddress, resourceID, backupName)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return getBackupResponse{}, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return getBackupResponse{}, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return getBackupResponse{}, fmt.Errorf("get backup returned %d: %s", resp.StatusCode, string(body))
	}

	var result getBackupResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return getBackupResponse{}, fmt.Errorf("failed to decode response: %w", err)
	}
	return result, nil
}

func createBackupViaAdminAPI(ctx context.Context, httpClient *http.Client, adminAPIAddress, resourceID string) (backupResponse, error) {
	url := fmt.Sprintf("%s/admin/v1/hcp%s/backups", adminAPIAddress, resourceID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return backupResponse{}, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return backupResponse{}, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return backupResponse{}, fmt.Errorf("create backup returned %d: %s", resp.StatusCode, string(body))
	}

	var result backupResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return backupResponse{}, fmt.Errorf("failed to decode response: %w", err)
	}
	return result, nil
}

func getBackupProfileViaAdminAPI(ctx context.Context, httpClient *http.Client, adminAPIAddress, resourceID string) (backupProfileResponse, error) {
	url := fmt.Sprintf("%s/admin/v1/hcp%s/backupProfile", adminAPIAddress, resourceID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return backupProfileResponse{}, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return backupProfileResponse{}, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return backupProfileResponse{}, fmt.Errorf("get backup profile returned %d: %s", resp.StatusCode, string(body))
	}

	var result backupProfileResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return backupProfileResponse{}, fmt.Errorf("failed to decode response: %w", err)
	}
	return result, nil
}

func patchBackupProfileViaAdminAPI(ctx context.Context, httpClient *http.Client, adminAPIAddress, resourceID, state string) (backupProfileResponse, error) {
	url := fmt.Sprintf("%s/admin/v1/hcp%s/backupProfile", adminAPIAddress, resourceID)

	body, err := json.Marshal(backupProfilePatchRequest{State: state})
	if err != nil {
		return backupProfileResponse{}, fmt.Errorf("failed to marshal request body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, url, bytes.NewReader(body))
	if err != nil {
		return backupProfileResponse{}, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return backupProfileResponse{}, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return backupProfileResponse{}, fmt.Errorf("patch backup profile returned %d: %s", resp.StatusCode, string(respBody))
	}

	var result backupProfileResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return backupProfileResponse{}, fmt.Errorf("failed to decode response: %w", err)
	}
	return result, nil
}

func patchBackupProfileRawViaAdminAPI(ctx context.Context, httpClient *http.Client, adminAPIAddress, resourceID, state string) (int, string, error) {
	url := fmt.Sprintf("%s/admin/v1/hcp%s/backupProfile", adminAPIAddress, resourceID)

	body, err := json.Marshal(backupProfilePatchRequest{State: state})
	if err != nil {
		return 0, "", fmt.Errorf("failed to marshal request body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, url, bytes.NewReader(body))
	if err != nil {
		return 0, "", fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, "", fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(respBody), nil
}
