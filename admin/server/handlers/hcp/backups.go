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
	"encoding/json"
	"fmt"
	"net/http"
	"path"
	"strings"
	"time"

	velerov1api "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"

	"k8s.io/apimachinery/pkg/runtime"
	utilsclock "k8s.io/utils/clock"

	azcorearm "github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"

	"github.com/Azure/ARO-HCP/internal/api"
	"github.com/Azure/ARO-HCP/internal/api/kubeapplier"
	"github.com/Azure/ARO-HCP/internal/backup"
	"github.com/Azure/ARO-HCP/internal/database"
	"github.com/Azure/ARO-HCP/internal/utils"
)

var (
	hostedClusterReadDesireName = strings.ToLower(string(api.MaestroBundleInternalNameReadonlyHypershiftHostedCluster))
)

const (
	veleroBackupGroup    = "velero.io"
	veleroBackupVersion  = "v1"
	veleroBackupResource = "backups"
	veleroNamespace      = "velero"
)

func ondemandDesireName(backupName string) string {
	return backup.OndemandDesireNamePrefix + backupName
}

type backupContext struct {
	resourceID                  *azcorearm.ResourceID
	hcp                         *api.HCPOpenShiftCluster
	managementClusterResourceID *azcorearm.ResourceID
	time                        utilsclock.PassiveClock
}

func resolveBackupContext(
	request *http.Request,
	resourceDBClient database.ResourcesDBClient,
	clock utilsclock.PassiveClock,
) (*backupContext, int, error) {
	resourceID, err := utils.ResourceIDFromContext(request.Context())
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("failed to get resource ID: %w", err)
	}

	hcp, err := resourceDBClient.HCPClusters(resourceID.SubscriptionID, resourceID.ResourceGroupName).Get(request.Context(), resourceID.Name)
	if database.IsNotFoundError(err) {
		return nil, http.StatusNotFound, fmt.Errorf("HCP %s not found", resourceID.String())
	}
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("failed to get HCP: %w", err)
	}

	if hcp.ServiceProviderProperties.ClusterServiceID == nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("cluster %s has no ClusterServiceID", resourceID.String())
	}

	spc, err := database.GetOrCreateServiceProviderCluster(request.Context(), resourceDBClient, resourceID)
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("failed to get ServiceProviderCluster: %w", err)
	}

	if spc.Status.ManagementClusterResourceID == nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("management cluster placement not resolved for cluster %s", resourceID.String())
	}

	return &backupContext{
		resourceID:                  resourceID,
		hcp:                         hcp,
		managementClusterResourceID: spc.Status.ManagementClusterResourceID,
		time:                        clock,
	}, http.StatusOK, nil
}

func (b *backupContext) clusterServiceID() string {
	return path.Base(b.hcp.ServiceProviderProperties.ClusterServiceID.String())
}

func buildOnDemandBackupDesires(
	subscriptionID, resourceGroupName, clusterName, backupName string,
	mcResourceID *azcorearm.ResourceID,
	backup *velerov1api.Backup,
) (*kubeapplier.ApplyDesire, *kubeapplier.ReadDesire, error) {
	desireName := ondemandDesireName(backupName)

	adResourceIDStr := kubeapplier.ToClusterScopedApplyDesireResourceIDString(
		subscriptionID, resourceGroupName, clusterName, desireName,
	)
	adResourceID, err := azcorearm.ParseResourceID(adResourceIDStr)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse ApplyDesire resource ID: %w", err)
	}

	raw, err := json.Marshal(backup)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to marshal backup: %w", err)
	}

	partitionKey := strings.ToLower(mcResourceID.String())

	ad := &kubeapplier.ApplyDesire{
		CosmosMetadata: api.CosmosMetadata{ResourceID: adResourceID, PartitionKey: partitionKey},
		Spec: kubeapplier.ApplyDesireSpec{
			ManagementCluster: mcResourceID,
			TargetItem: kubeapplier.ResourceReference{
				Group:     veleroBackupGroup,
				Version:   veleroBackupVersion,
				Resource:  veleroBackupResource,
				Namespace: veleroNamespace,
				Name:      backupName,
			},
			KubeContent: &runtime.RawExtension{Raw: raw},
		},
	}

	rdResourceIDStr := kubeapplier.ToClusterScopedReadDesireResourceIDString(
		subscriptionID, resourceGroupName, clusterName, desireName,
	)
	rdResourceID, err := azcorearm.ParseResourceID(rdResourceIDStr)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse ReadDesire resource ID: %w", err)
	}

	rd := &kubeapplier.ReadDesire{
		CosmosMetadata: api.CosmosMetadata{ResourceID: rdResourceID, PartitionKey: partitionKey},
		Spec: kubeapplier.ReadDesireSpec{
			ManagementCluster: mcResourceID,
			TargetItem: kubeapplier.ResourceReference{
				Group:     veleroBackupGroup,
				Version:   veleroBackupVersion,
				Resource:  veleroBackupResource,
				Namespace: veleroNamespace,
				Name:      backupName,
			},
		},
	}

	return ad, rd, nil
}

func GetBackup(
	resourcesDBClient database.ResourcesDBClient,
	kubeApplierDBClients database.KubeApplierDBClients,
	clock utilsclock.PassiveClock,
) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		bCtx, status, err := resolveBackupContext(request, resourcesDBClient, clock)
		if err != nil {
			http.Error(writer, fmt.Sprintf("failed to resolve backup context: %v", err), status)
			return
		}

		backupName := request.PathValue("backupName")
		if backupName == "" {
			http.Error(writer, "backupName is required", http.StatusBadRequest)
			return
		}

		kaClient := kubeApplierDBClients.For(request.Context(), bCtx.managementClusterResourceID)
		if kaClient == nil {
			http.Error(writer, "kube-applier client not available for management cluster", http.StatusInternalServerError)
			return
		}

		rdCrud, err := kaClient.ReadDesiresForCluster(bCtx.resourceID.SubscriptionID, bCtx.resourceID.ResourceGroupName, bCtx.resourceID.Name)
		if err != nil {
			http.Error(writer, fmt.Sprintf("failed to get ReadDesire CRUD: %v", err), http.StatusInternalServerError)
			return
		}

		desireName := ondemandDesireName(backupName)
		rd, err := rdCrud.Get(request.Context(), desireName)
		if err != nil {
			if database.IsNotFoundError(err) {
				http.Error(writer, fmt.Sprintf("backup %s not found", backupName), http.StatusNotFound)
				return
			}
			http.Error(writer, fmt.Sprintf("failed to get ReadDesire: %v", err), http.StatusInternalServerError)
			return
		}

		backup := backupResponseFromReadDesire(rd, backupName)
		response := GetBackupResponse{ResourceID: bCtx.hcp.ID.String(), Backup: backup}

		writer.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(writer).Encode(response); err != nil {
			http.Error(writer, fmt.Sprintf("failed to encode output: %v", err), http.StatusInternalServerError)
			return
		}
	})
}

func backupResponseFromReadDesire(rd *kubeapplier.ReadDesire, backupName string) BackupResponse {
	resp := BackupResponse{
		Name:  backupName,
		Phase: "New",
	}
	if rd.Status.KubeContent == nil || rd.Status.KubeContent.Raw == nil {
		return resp
	}
	var backup velerov1api.Backup
	if err := json.Unmarshal(rd.Status.KubeContent.Raw, &backup); err != nil {
		return resp
	}
	resp.Phase = string(backup.Status.Phase)
	if backup.Status.StartTimestamp != nil {
		resp.StartTimestamp = backup.Status.StartTimestamp.String()
	}
	if backup.Status.CompletionTimestamp != nil {
		resp.CompletionTimestamp = backup.Status.CompletionTimestamp.String()
	}
	return resp
}

func CreateBackup(
	resourcesDBClient database.ResourcesDBClient,
	kubeApplierDBClients database.KubeApplierDBClients,
	clock utilsclock.PassiveClock,
) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		bCtx, status, err := resolveBackupContext(request, resourcesDBClient, clock)
		if err != nil {
			http.Error(writer, fmt.Sprintf("failed to resolve backup context: %v", err), status)
			return
		}

		clusterServiceID := bCtx.clusterServiceID()
		clusterName := bCtx.hcp.CustomerProperties.DNS.BaseDomainPrefix
		if clusterName == "" {
			http.Error(writer, "cluster has no BaseDomainPrefix", http.StatusInternalServerError)
			return
		}

		kaClient := kubeApplierDBClients.For(request.Context(), bCtx.managementClusterResourceID)
		if kaClient == nil {
			http.Error(writer, "kube-applier client not available for management cluster", http.StatusInternalServerError)
			return
		}

		rdCrud, err := kaClient.ReadDesiresForCluster(bCtx.resourceID.SubscriptionID, bCtx.resourceID.ResourceGroupName, bCtx.resourceID.Name)
		if err != nil {
			http.Error(writer, fmt.Sprintf("failed to get ReadDesire CRUD: %v", err), http.StatusInternalServerError)
			return
		}

		hcReadDesire, err := rdCrud.Get(request.Context(), hostedClusterReadDesireName)
		if err != nil {
			if database.IsNotFoundError(err) {
				http.Error(writer, "HostedCluster ReadDesire not found — cluster may not be fully provisioned", http.StatusPreconditionFailed)
				return
			}
			http.Error(writer, fmt.Sprintf("failed to get HostedCluster ReadDesire: %v", err), http.StatusInternalServerError)
			return
		}

		hcNamespace := hcReadDesire.Spec.TargetItem.Namespace
		if hcNamespace == "" {
			http.Error(writer, "HostedCluster ReadDesire has empty namespace", http.StatusInternalServerError)
			return
		}
		hcpNamespace := fmt.Sprintf("%s-%s", hcNamespace, clusterName)

		timestamp := bCtx.time.Now().Format("20060102150405")
		backupName := fmt.Sprintf("%s-%s", clusterServiceID, timestamp)
		ttl := 7 * 24 * time.Hour
		hcpBackup := backup.NewBackup(backupName, clusterServiceID, ttl, hcNamespace, hcpNamespace)

		adCrud, err := kaClient.ApplyDesiresForCluster(bCtx.resourceID.SubscriptionID, bCtx.resourceID.ResourceGroupName, bCtx.resourceID.Name)
		if err != nil {
			http.Error(writer, fmt.Sprintf("failed to get ApplyDesire CRUD: %v", err), http.StatusInternalServerError)
			return
		}

		ad, rd, err := buildOnDemandBackupDesires(
			bCtx.resourceID.SubscriptionID, bCtx.resourceID.ResourceGroupName, bCtx.resourceID.Name,
			backupName, bCtx.managementClusterResourceID, hcpBackup,
		)
		if err != nil {
			http.Error(writer, fmt.Sprintf("failed to build desires: %v", err), http.StatusInternalServerError)
			return
		}

		if _, err := adCrud.Create(request.Context(), ad, nil); err != nil {
			http.Error(writer, fmt.Sprintf("failed to create ApplyDesire: %v", err), http.StatusInternalServerError)
			return
		}
		if _, err := rdCrud.Create(request.Context(), rd, nil); err != nil {
			http.Error(writer, fmt.Sprintf("failed to create ReadDesire: %v", err), http.StatusInternalServerError)
			return
		}

		response := BackupResponse{
			Name:  backupName,
			Phase: "New",
		}

		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusAccepted)
		if err := json.NewEncoder(writer).Encode(response); err != nil {
			http.Error(writer, fmt.Sprintf("failed to encode output: %v", err), http.StatusInternalServerError)
			return
		}
	})
}

type GetBackupResponse struct {
	ResourceID string         `json:"resourceID"`
	Backup     BackupResponse `json:"backup"`
}

type BackupResponse struct {
	Name                string `json:"name"`
	StartTimestamp      string `json:"startTimestamp"`
	CompletionTimestamp string `json:"completionTimestamp"`
	Phase               string `json:"phase"`
}

// BackupScheduleResponse is the JSON response for backup schedule endpoints.
type BackupScheduleResponse struct {
	ResourceID string                 `json:"resourceID"`
	State      string                 `json:"state"`
	Schedules  []BackupScheduleDetail `json:"schedules"`
}

// BackupScheduleDetail holds per-schedule status from the Velero Schedule ReadDesire.
type BackupScheduleDetail struct {
	Name            string `json:"name"`
	LastBackupTime  string `json:"lastBackupTime,omitempty"`
	LastBackupPhase string `json:"lastBackupPhase,omitempty"`
}

// BackupSchedulePatchRequest is the JSON body for PATCH requests.
type BackupSchedulePatchRequest struct {
	State string `json:"state"`
}

// BackupSchedulePatchResponse is the JSON response for PATCH requests.
type BackupSchedulePatchResponse struct {
	ResourceID string `json:"resourceID"`
	State      string `json:"state"`
}

// HCPGetBackupScheduleHandler handles GET requests for backup schedule.
type HCPGetBackupScheduleHandler struct {
	resourcesDBClient    database.ResourcesDBClient
	kubeApplierDBClients database.KubeApplierDBClients
}

// NewHCPGetBackupScheduleHandler creates a new backup schedule GET handler.
func NewHCPGetBackupScheduleHandler(resourcesDBClient database.ResourcesDBClient, kubeApplierDBClients database.KubeApplierDBClients) *HCPGetBackupScheduleHandler {
	return &HCPGetBackupScheduleHandler{
		resourcesDBClient:    resourcesDBClient,
		kubeApplierDBClients: kubeApplierDBClients,
	}
}

func (h *HCPGetBackupScheduleHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	resourceID, err := utils.ResourceIDFromContext(request.Context())
	if err != nil {
		http.Error(writer, fmt.Sprintf("failed to get resource ID: %v", err), http.StatusInternalServerError)
		return
	}

	spc, err := database.GetOrCreateServiceProviderCluster(request.Context(), h.resourcesDBClient, resourceID)
	if err != nil {
		http.Error(writer, fmt.Sprintf("failed to get service provider cluster: %v", err), http.StatusInternalServerError)
		return
	}

	state := string(spc.Spec.BackupState)
	if len(state) == 0 {
		state = string(api.BackupScheduleStateActive)
	}

	response := BackupScheduleResponse{
		ResourceID: resourceID.String(),
		State:      state,
		Schedules:  []BackupScheduleDetail{},
	}

	if spc.Status.ManagementClusterResourceID == nil {
		http.Error(writer, "management cluster placement not resolved", http.StatusInternalServerError)
		return
	}

	kaClient := h.kubeApplierDBClients.For(request.Context(), spc.Status.ManagementClusterResourceID)
	if kaClient == nil {
		http.Error(writer, "kube-applier client not available for management cluster", http.StatusInternalServerError)
		return
	}

	rdCrud, err := kaClient.ReadDesiresForCluster(resourceID.SubscriptionID, resourceID.ResourceGroupName, resourceID.Name)
	if err != nil {
		http.Error(writer, fmt.Sprintf("failed to get ReadDesire CRUD: %v", err), http.StatusInternalServerError)
		return
	}

	iterator, err := rdCrud.List(request.Context(), nil)
	if err != nil {
		http.Error(writer, fmt.Sprintf("failed to list ReadDesires: %v", err), http.StatusInternalServerError)
		return
	}

	for _, rd := range iterator.Items(request.Context()) {
		if !strings.HasPrefix(rd.ResourceID.Name, backup.BackupDesireNamePrefix) {
			continue
		}
		scheduleName := strings.TrimPrefix(rd.ResourceID.Name, backup.BackupDesireNamePrefix)
		detail := BackupScheduleDetail{Name: scheduleName}
		if rd.Status.KubeContent != nil && rd.Status.KubeContent.Raw != nil {
			var schedule velerov1api.Schedule
			if err := json.Unmarshal(rd.Status.KubeContent.Raw, &schedule); err == nil {
				if schedule.Status.LastBackup != nil {
					detail.LastBackupTime = schedule.Status.LastBackup.String()
				}
				detail.LastBackupPhase = string(schedule.Status.Phase)
			}
		}
		response.Schedules = append(response.Schedules, detail)
	}

	writer.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(writer).Encode(response); err != nil {
		http.Error(writer, fmt.Sprintf("failed to encode output: %v", err), http.StatusInternalServerError)
		return
	}
}

// HCPPatchBackupScheduleHandler handles PATCH requests to update backup schedule state.
type HCPPatchBackupScheduleHandler struct {
	resourcesDBClient database.ResourcesDBClient
}

// NewHCPPatchBackupScheduleHandler creates a new backup schedule PATCH handler.
func NewHCPPatchBackupScheduleHandler(resourcesDBClient database.ResourcesDBClient) *HCPPatchBackupScheduleHandler {
	return &HCPPatchBackupScheduleHandler{resourcesDBClient: resourcesDBClient}
}

func (h *HCPPatchBackupScheduleHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	resourceID, err := utils.ResourceIDFromContext(request.Context())
	if err != nil {
		http.Error(writer, fmt.Sprintf("failed to get resource ID: %v", err), http.StatusInternalServerError)
		return
	}

	var patch BackupSchedulePatchRequest
	if err := json.NewDecoder(request.Body).Decode(&patch); err != nil {
		http.Error(writer, fmt.Sprintf("invalid JSON body: %v", err), http.StatusBadRequest)
		return
	}

	state := api.BackupScheduleState(patch.State)
	if state != api.BackupScheduleStateActive && state != api.BackupScheduleStatePaused {
		http.Error(writer, fmt.Sprintf("invalid state %q: must be %q or %q", patch.State, api.BackupScheduleStateActive, api.BackupScheduleStatePaused), http.StatusBadRequest)
		return
	}

	spc, err := database.GetOrCreateServiceProviderCluster(request.Context(), h.resourcesDBClient, resourceID)
	if err != nil {
		http.Error(writer, fmt.Sprintf("failed to get service provider cluster: %v", err), http.StatusInternalServerError)
		return
	}

	spc.Spec.BackupState = state

	spcCRUD := h.resourcesDBClient.ServiceProviderClusters(resourceID.SubscriptionID, resourceID.ResourceGroupName, resourceID.Name)
	spc, err = spcCRUD.Replace(request.Context(), spc, nil)
	if err != nil {
		http.Error(writer, fmt.Sprintf("failed to update backup state: %v", err), http.StatusInternalServerError)
		return
	}

	response := BackupSchedulePatchResponse{
		ResourceID: resourceID.String(),
		State:      string(spc.Spec.BackupState),
	}

	writer.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(writer).Encode(response); err != nil {
		http.Error(writer, fmt.Sprintf("failed to encode output: %v", err), http.StatusInternalServerError)
		return
	}
}
