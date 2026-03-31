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

package hcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	hypershiftv1beta1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	velerov1api "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/ARO-HCP/internal/api"
	"github.com/Azure/ARO-HCP/internal/database"
	"github.com/Azure/ARO-HCP/internal/mc"
	"github.com/Azure/ARO-HCP/internal/ocm"
	"github.com/Azure/ARO-HCP/internal/recovery"
	"github.com/Azure/ARO-HCP/internal/utils"
)

// MgmtClientFactory creates a controller-runtime client for a management cluster.
type MgmtClientFactory func(ctx context.Context, aksResourceID string, credential azcore.TokenCredential) (ctrlclient.Client, error)

func DefaultMgmtClientFactory(ctx context.Context, aksResourceID string, credential azcore.TokenCredential) (ctrlclient.Client, error) {
	config, err := mc.GetAKSRESTConfig(ctx, aksResourceID, credential)
	if err != nil {
		return nil, err
	}

	scheme := runtime.NewScheme()
	if err := recovery.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("failed to add DR schemes: %w", err)
	}
	client, err := ctrlclient.New(config, ctrlclient.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("failed to create controller-runtime client: %w", err)
	}
	return client, nil
}

type drContext struct {
	resourceID string
	client     ctrlclient.Client
	hcp        *api.HCPOpenShiftCluster
}

// resolveDRContext resolves all the necessary DR context for the request.
func resolveDRContext(
	request *http.Request,
	dbClient database.DBClient,
	csClient ocm.ClusterServiceClientSpec,
	azureCredential azcore.TokenCredential,
	mgmtClientFactory MgmtClientFactory,
) (*drContext, int, error) {
	resourceID, err := utils.ResourceIDFromContext(request.Context())
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("failed to get resource ID: %w", err)
	}

	hcp, err := dbClient.HCPClusters(resourceID.SubscriptionID, resourceID.ResourceGroupName).Get(request.Context(), resourceID.Name)
	if database.IsResponseError(err, http.StatusNotFound) {
		return nil, http.StatusNotFound, fmt.Errorf("HCP %s not found", resourceID.String())
	}

	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("failed to get HCP: %w", err)
	}

	shard, err := csClient.GetClusterProvisionShard(request.Context(), hcp.ServiceProviderProperties.ClusterServiceID)
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("failed to get Management Cluster: %w", err)
	}

	client, err := mgmtClientFactory(request.Context(), shard.AzureShard().AksManagementClusterResourceId(), azureCredential)
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("failed to create management cluster client: %w", err)
	}

	return &drContext{resourceID: resourceID.String(), client: client, hcp: hcp}, http.StatusOK, nil
}

// clusterServiceID returns the short cluster ID (last path segment of the CS ID).
func (d *drContext) clusterServiceID() string {
	return path.Base(d.hcp.ServiceProviderProperties.ClusterServiceID.String())
}

func getBackup(ctx context.Context, client ctrlclient.Client, backupName string) (*velerov1api.Backup, error) {
	backup := &velerov1api.Backup{}
	key := ctrlclient.ObjectKey{Name: backupName, Namespace: "velero"}
	if err := client.Get(ctx, key, backup); err != nil {
		return nil, err
	}
	return backup, nil
}

func listBackupsForCluster(ctx context.Context, client ctrlclient.Client, clusterID string) ([]velerov1api.Backup, error) {
	backupList := &velerov1api.BackupList{}
	if err := client.List(ctx, backupList, ctrlclient.MatchingLabels{"api.openshift.com/id": clusterID}); err != nil {
		return nil, err
	}
	return backupList.Items, nil
}

func createBackupForCluster(ctx context.Context, client ctrlclient.Client, clusterID string) (*velerov1api.Backup, error) {
	hc, err := getHostedCluster(ctx, client, clusterID)
	if err != nil {
		return nil, err
	}
	if hc == nil {
		return nil, fmt.Errorf("hosted cluster %s not found", clusterID)
	}

	now := time.Now().UTC().Format("2006-01-02-150405")
	backupName := fmt.Sprintf("%s-%s", clusterID, now)
	hcpNamespace := fmt.Sprintf("%s-%s", hc.Namespace, hc.Name)

	backup := recovery.NewBackup(backupName, clusterID, hc.Namespace, hcpNamespace)
	if err := client.Create(ctx, backup); err != nil {
		return nil, err
	}

	return backup, nil
}

func getHostedCluster(ctx context.Context, client ctrlclient.Client, clusterID string) (*hypershiftv1beta1.HostedCluster, error) {
	hostedClusters := &hypershiftv1beta1.HostedClusterList{}
	if err := client.List(ctx, hostedClusters, ctrlclient.MatchingLabels{"api.openshift.com/id": clusterID}); err != nil {
		return nil, err
	}
	if len(hostedClusters.Items) == 0 {
		return nil, nil
	} else if len(hostedClusters.Items) > 1 {
		return nil, fmt.Errorf("multiple hosted clusters found for cluster %s", clusterID)
	}
	return &hostedClusters.Items[0], nil
}

type GetBackupResponse struct {
	ResourceID string
	Backup     BackupResponse
}

func GetBackup(dbClient database.DBClient, csClient ocm.ClusterServiceClientSpec, azureCredential azcore.TokenCredential, mgmtClientFactory MgmtClientFactory) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		drCtx, status, err := resolveDRContext(request, dbClient, csClient, azureCredential, mgmtClientFactory)
		if err != nil {
			http.Error(writer, fmt.Sprintf("failed to resolve DR context: %v", err), status)
			return
		}
		backupName := request.PathValue("backupName")
		if backupName == "" {
			http.Error(writer, "backupName is required", http.StatusBadRequest)
			return
		}

		veleroBackup, err := getBackup(request.Context(), drCtx.client, backupName)
		if err != nil {
			http.Error(writer, fmt.Sprintf("failed to get backup: %v", err), http.StatusInternalServerError)
			return
		}

		expectedID := drCtx.clusterServiceID()
		backupClusterID := veleroBackup.Labels["api.openshift.com/id"]
		if backupClusterID != expectedID {
			http.Error(writer, fmt.Sprintf("backup %s does not belong to cluster %s", backupName, drCtx.hcp.ID.String()), http.StatusBadRequest)
			return
		}

		backup := newBackupResponse(*veleroBackup)

		response := GetBackupResponse{ResourceID: drCtx.hcp.ID.String(), Backup: backup}

		writer.Header().Set("Content-Type", "application/json")
		err = json.NewEncoder(writer).Encode(response)
		if err != nil {
			http.Error(writer, fmt.Sprintf("failed to encode output: %v", err), http.StatusInternalServerError)
			return
		}
	})
}

func ListBackups(dbClient database.DBClient, csClient ocm.ClusterServiceClientSpec, azureCredential azcore.TokenCredential, mgmtClientFactory MgmtClientFactory) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		drCtx, status, err := resolveDRContext(request, dbClient, csClient, azureCredential, mgmtClientFactory)
		if err != nil {
			http.Error(writer, fmt.Sprintf("failed to resolve DR context: %v", err), status)
			return
		}

		id := drCtx.clusterServiceID()
		backups, err := listBackupsForCluster(request.Context(), drCtx.client, id)
		if err != nil {
			http.Error(writer, fmt.Sprintf("failed to list backups: %v", err), http.StatusInternalServerError)
			return
		}

		backupsOut := newListBackupsResponse(backups)
		response := ListBackupsResponse{ResourceID: drCtx.hcp.ID.String(), Backups: backupsOut}

		writer.Header().Set("Content-Type", "application/json")
		err = json.NewEncoder(writer).Encode(response)
		if err != nil {
			http.Error(writer, fmt.Sprintf("failed to encode output: %v", err), http.StatusInternalServerError)
			return
		}
	})
}

func CreateBackup(dbClient database.DBClient, csClient ocm.ClusterServiceClientSpec, azureCredential azcore.TokenCredential, mgmtClientFactory MgmtClientFactory) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		drCtx, status, err := resolveDRContext(request, dbClient, csClient, azureCredential, mgmtClientFactory)
		if err != nil {
			http.Error(writer, fmt.Sprintf("failed to resolve DR context: %v", err), status)
			return
		}

		id := drCtx.clusterServiceID()
		backup, err := createBackupForCluster(request.Context(), drCtx.client, id)
		if err != nil {
			http.Error(writer, fmt.Sprintf("failed to create backup: %v", err), http.StatusInternalServerError)
			return
		}

		response := newBackupResponse(*backup)

		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusCreated)
		err = json.NewEncoder(writer).Encode(response)
		if err != nil {
			http.Error(writer, fmt.Sprintf("failed to encode output: %v", err), http.StatusInternalServerError)
			return
		}
	})
}

type ListBackupsResponse struct {
	ResourceID string           `json:"resourceID"`
	Backups    []BackupResponse `json:"backups"`
}

type BackupResponse struct {
	Name                string `json:"name"`
	StartTimestamp      string `json:"startTimestamp"`
	CompletionTimestamp string `json:"completionTimestamp"`
	Phase               string `json:"phase"`
}

func newListBackupsResponse(backups []velerov1api.Backup) []BackupResponse {
	out := make([]BackupResponse, len(backups))
	for i, b := range backups {
		out[i] = newBackupResponse(b)
	}
	return out
}

func newBackupResponse(backup velerov1api.Backup) BackupResponse {
	resp := BackupResponse{
		Name:  backup.Name,
		Phase: string(backup.Status.Phase),
	}
	if backup.Status.StartTimestamp != nil {
		resp.StartTimestamp = backup.Status.StartTimestamp.String()
	}
	if backup.Status.CompletionTimestamp != nil {
		resp.CompletionTimestamp = backup.Status.CompletionTimestamp.String()
	}
	return resp
}
