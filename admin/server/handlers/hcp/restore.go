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
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"

	hcprecoveryv1alpha1 "github.com/Azure/ARO-HCP/hcp-recovery/pkg/apis/hcprecovery/v1alpha1"
	"github.com/Azure/ARO-HCP/internal/api/coreapi"
	"github.com/Azure/ARO-HCP/internal/api/kubeapplierapi"
	"github.com/Azure/ARO-HCP/internal/apihelpers/coreapihelpers"
	"github.com/Azure/ARO-HCP/internal/backup"
	"github.com/Azure/ARO-HCP/internal/database/cosmosstorage/corecosmosstorage"
	"github.com/Azure/ARO-HCP/internal/database/cosmosstorage/cosmosstorageutils"
	"github.com/Azure/ARO-HCP/internal/database/cosmosstorage/kubeappliercosmosstorage"
	"github.com/Azure/ARO-HCP/internal/utils"
)

type RestoreRequest struct {
	BackupID string `json:"backupID"`
}

// RecoveryDetail describes one recovery: the request and the backend's progress
// recorded on the ServiceProviderCluster, plus the HCPRecovery observed on the
// management cluster through the recovery ReadDesire.
type RecoveryDetail struct {
	RecoveryID    string                `json:"recoveryID"`
	RecoveryState coreapi.RecoveryState `json:"recoveryState"`
	BackupID      string                `json:"backupID"`
	StartedAt     string                `json:"startedAt,omitempty"`
	CompletedAt   string                `json:"completedAt,omitempty"`

	// Status is the observed HCPRecovery status, verbatim, decoded from the
	// recovery ReadDesire's kubeContent. RecoveryState collapses the whole
	// restore to one of five values; this is where the phase, the per-step
	// conditions, the Velero restore name and the etcd snapshot details live.
	//
	// Nil until the backend has created the HCPRecovery and kube-applier has
	// observed it, so a Pending recovery carries no status at all.
	Status *hcprecoveryv1alpha1.HCPRecoveryStatus `json:"status,omitempty"`
}

// RestoreResponse is returned by POST /restore and by GET /restore?recoveryID=<id>.
// RecoveryDetail is embedded, so the recovery's own fields marshal flat.
type RestoreResponse struct {
	ResourceID string `json:"resourceID"`
	RecoveryDetail
}

// RestoreListResponse is returned by GET /restore with no recoveryID.
type RestoreListResponse struct {
	ResourceID string           `json:"resourceID"`
	Recoveries []RecoveryDetail `json:"recoveries"`
}

// newRecoveryDetail projects one recovery out of the ServiceProviderCluster. It
// returns a 404 CloudError when the cluster has no request with this ID, since
// a recovery that was never requested cannot be reported on.
func newRecoveryDetail(recoveryID string, spc *coreapi.ServiceProviderCluster) (RecoveryDetail, error) {
	detail := RecoveryDetail{RecoveryID: recoveryID}
	found := false
	for _, req := range spc.Spec.RecoveryRequests {
		if req.RecoveryID == recoveryID {
			detail.BackupID = req.VeleroBackupName
			detail.RecoveryState = coreapi.RecoveryStatePending
			found = true
			break
		}
	}
	if !found {
		return detail, coreapi.NewCloudError(http.StatusNotFound, coreapi.CloudErrorCodeResourceNotFound, "", "recovery %s not found", recoveryID)
	}
	for _, recovery := range spc.Status.Recoveries {
		if recovery.RecoveryID != recoveryID {
			continue
		}
		detail.RecoveryState = recovery.State
		if recovery.StartedAt != nil {
			detail.StartedAt = recovery.StartedAt.UTC().Format(time.RFC3339)
		}
		if recovery.CompletedAt != nil {
			detail.CompletedAt = recovery.CompletedAt.UTC().Format(time.RFC3339)
		}
		break
	}
	return detail, nil
}

// observedRecoveryStatus decodes the HCPRecovery mirrored into the recovery
// ReadDesire's kubeContent. A nil desire or empty kubeContent yields a nil
// status: kube-applier has not observed the HCPRecovery, either because the
// backend has not created it yet or because the informer has not synced.
func observedRecoveryStatus(readDesire *kubeapplierapi.ReadDesire) (*hcprecoveryv1alpha1.HCPRecoveryStatus, error) {
	if readDesire == nil || readDesire.Status.KubeContent == nil || len(readDesire.Status.KubeContent.Raw) == 0 {
		return nil, nil
	}
	recovery := &hcprecoveryv1alpha1.HCPRecovery{}
	if err := json.Unmarshal(readDesire.Status.KubeContent.Raw, recovery); err != nil {
		return nil, fmt.Errorf("failed to unmarshal observed HCPRecovery: %w", err)
	}
	return &recovery.Status, nil
}

type PostRestoreHandler struct {
	resourcesDBClient corecosmosstorage.ResourcesDBClient
}

func NewPostRestoreHandler(resourcesDBClient corecosmosstorage.ResourcesDBClient) *PostRestoreHandler {
	return &PostRestoreHandler{resourcesDBClient: resourcesDBClient}
}

func (h *PostRestoreHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) error {
	var req RestoreRequest
	if err := json.NewDecoder(request.Body).Decode(&req); err != nil {
		return coreapi.NewCloudError(http.StatusBadRequest, coreapi.CloudErrorCodeInvalidRequestContent, "", "invalid JSON body: %v", err)
	}
	if req.BackupID == "" {
		return coreapi.NewCloudError(http.StatusBadRequest, coreapi.CloudErrorCodeInvalidRequestContent, "", "backupID is required")
	}
	details, err := getClusterDetails(request, h.resourcesDBClient)
	if err != nil {
		return utils.TrackError(err)
	}
	if details.cluster.ServiceProviderProperties.DeletionTimestamp != nil {
		return coreapi.NewCloudError(http.StatusConflict, coreapi.CloudErrorCodeConflict, "", "HCP cluster is being deleted")
	}
	terminalIDs := make(map[string]bool)
	for _, status := range details.serviceProviderCluster.Status.Recoveries {
		if !status.State.IsTerminal() {
			return coreapi.NewCloudError(http.StatusConflict, coreapi.CloudErrorCodeConflict, "", "recovery already in progress (id: %s)", status.RecoveryID)
		}
		terminalIDs[status.RecoveryID] = true
	}
	for _, pending := range details.serviceProviderCluster.Spec.RecoveryRequests {
		if !terminalIDs[pending.RecoveryID] {
			return coreapi.NewCloudError(http.StatusConflict, coreapi.CloudErrorCodeConflict, "", "recovery already queued (id: %s)", pending.RecoveryID)
		}
	}
	recoveryID := uuid.NewString()
	replacement := details.serviceProviderCluster.DeepCopy()
	replacement.Spec.RecoveryRequests = append(replacement.Spec.RecoveryRequests, coreapi.RecoveryRequest{
		RecoveryID: recoveryID, VeleroBackupName: req.BackupID,
	})
	resourceID := details.cluster.ResourceID
	spc, err := h.resourcesDBClient.ServiceProviderClusters(resourceID.SubscriptionID, resourceID.ResourceGroupName, resourceID.Name).Replace(request.Context(), replacement, nil)
	if cosmosstorageutils.IsPreconditionFailedError(err) {
		return coreapi.NewCloudError(http.StatusConflict, coreapi.CloudErrorCodeConflict, "", "ETag conflict, retry the operation")
	}
	if err != nil {
		return utils.TrackError(fmt.Errorf("failed to enqueue recovery: %w", err))
	}
	// A recovery this young has no HCPRecovery to observe yet, so the detail is
	// projected from Cosmos alone and Status is left nil.
	detail, err := newRecoveryDetail(recoveryID, spc)
	if err != nil {
		return err
	}
	response := RestoreResponse{ResourceID: resourceID.String(), RecoveryDetail: detail}
	_, err = coreapihelpers.WriteJSONResponse(writer, http.StatusOK, response)
	return utils.TrackError(err)
}

type GetRestoreStatusHandler struct {
	resourcesDBClient    corecosmosstorage.ResourcesDBClient
	kubeApplierDBClients kubeappliercosmosstorage.KubeApplierDBClients
}

func NewGetRestoreStatusHandler(resourcesDBClient corecosmosstorage.ResourcesDBClient, kubeApplierDBClients kubeappliercosmosstorage.KubeApplierDBClients) *GetRestoreStatusHandler {
	return &GetRestoreStatusHandler{resourcesDBClient: resourcesDBClient, kubeApplierDBClients: kubeApplierDBClients}
}

// ServeHTTP reports one recovery when recoveryID is supplied, and every recovery
// for the cluster when it is not.
func (h *GetRestoreStatusHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) error {
	ctx := request.Context()
	recoveryID := request.URL.Query().Get("recoveryID")
	details, err := getClusterDetails(request, h.resourcesDBClient)
	if err != nil {
		return utils.TrackError(err)
	}
	spc := details.serviceProviderCluster

	if recoveryID != "" {
		detail, err := newRecoveryDetail(recoveryID, spc)
		if err != nil {
			return err
		}
		one := []RecoveryDetail{detail}
		if err := h.addObservedStatus(ctx, details, one); err != nil {
			return err
		}
		response := RestoreResponse{ResourceID: details.cluster.ResourceID.String(), RecoveryDetail: one[0]}
		_, err = coreapihelpers.WriteJSONResponse(writer, http.StatusOK, response)
		return utils.TrackError(err)
	}

	// Requests are appended in chronological order, so walking backwards puts
	// the newest recovery first without relying on StartedAt, which is nil
	// until the backend picks the request up.
	recoveries := make([]RecoveryDetail, 0, len(spc.Spec.RecoveryRequests))
	for _, request := range slices.Backward(spc.Spec.RecoveryRequests) {
		detail, err := newRecoveryDetail(request.RecoveryID, spc)
		if err != nil {
			return err
		}
		recoveries = append(recoveries, detail)
	}
	if err := h.addObservedStatus(ctx, details, recoveries); err != nil {
		return err
	}
	response := RestoreListResponse{ResourceID: details.cluster.ResourceID.String(), Recoveries: recoveries}
	_, err = coreapihelpers.WriteJSONResponse(writer, http.StatusOK, response)
	return utils.TrackError(err)
}

// addObservedStatus fills in Status for each detail from its recovery ReadDesire.
//
// Unlike the backup handlers, unresolved placement or an unavailable kube-applier
// client is not an error here: a recovery sits in Pending precisely while those
// are unresolved, and failing the request would make this endpoint useless in the
// state it is polled in most. Those cases leave every Status nil and the
// Cosmos-level fields are still reported.
func (h *GetRestoreStatusHandler) addObservedStatus(ctx context.Context, details *clusterDetails, recoveries []RecoveryDetail) error {
	if len(recoveries) == 0 {
		return nil
	}
	managementCluster := details.serviceProviderCluster.Status.ManagementClusterResourceID
	if managementCluster == nil {
		return nil
	}
	kubeApplierClient, err := h.kubeApplierDBClients.For(ctx, managementCluster)
	if err != nil {
		return utils.TrackError(fmt.Errorf("failed to get kube-applier client: %w", err))
	}
	if kubeApplierClient == nil {
		return nil
	}
	resourceID := details.cluster.ResourceID
	readDesireCRUD, err := kubeApplierClient.ReadDesiresForCluster(resourceID.SubscriptionID, resourceID.ResourceGroupName, resourceID.Name)
	if err != nil {
		return utils.TrackError(fmt.Errorf("failed to get ReadDesire CRUD: %w", err))
	}

	byName := map[string]*kubeapplierapi.ReadDesire{}
	iterator, err := readDesireCRUD.List(ctx, nil)
	if err != nil {
		return utils.TrackError(fmt.Errorf("failed to list ReadDesires: %w", err))
	}
	for _, readDesire := range iterator.Items(ctx) {
		if readDesire == nil || readDesire.ResourceID == nil {
			continue
		}
		byName[readDesire.ResourceID.Name] = readDesire
	}
	if err := iterator.GetError(); err != nil {
		return utils.TrackError(fmt.Errorf("failed to iterate ReadDesires: %w", err))
	}
	for i := range recoveries {
		status, err := observedRecoveryStatus(byName[backup.RecoveryDesireName(recoveries[i].RecoveryID)])
		if err != nil {
			return utils.TrackError(err)
		}
		recoveries[i].Status = status
	}
	return nil
}
