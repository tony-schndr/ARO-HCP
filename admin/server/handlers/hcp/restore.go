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

	"github.com/Azure/ARO-HCP/internal/api"
	"github.com/Azure/ARO-HCP/internal/database"
	"github.com/Azure/ARO-HCP/internal/utils"
)

type RestoreRequest struct {
	BackupID string `json:"backupID"`
}

type RestoreResponse struct {
	ResourceID    string `json:"resourceID"`
	RecoveryState string `json:"recoveryState"`
	BackupID      string `json:"backupID,omitempty"`
	StartedAt     string `json:"startedAt,omitempty"`
	CompletedAt   string `json:"completedAt,omitempty"`
	Phase         string `json:"phase,omitempty"`
	LastCondition string `json:"lastCondition,omitempty"`
}

func newRestoreResponse(resourceID string, spc *api.ServiceProviderCluster) RestoreResponse {
	resp := RestoreResponse{
		ResourceID:    resourceID,
		RecoveryState: string(spc.Status.RecoveryState),
		BackupID:      spc.Status.RecoveryBackupID,
		Phase:         spc.Status.RecoveryPhase,
		LastCondition: spc.Status.RecoveryLastConditionType,
	}
	if spc.Status.RecoveryStartedAt != nil {
		resp.StartedAt = spc.Status.RecoveryStartedAt.String()
	}
	if spc.Status.RecoveryCompletedAt != nil {
		resp.CompletedAt = spc.Status.RecoveryCompletedAt.String()
	}
	return resp
}

func PostRestore(dbClient database.ResourcesDBClient) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		resourceID, err := utils.ResourceIDFromContext(request.Context())
		if err != nil {
			http.Error(writer, fmt.Sprintf("failed to get resource ID: %v", err), http.StatusInternalServerError)
			return
		}

		var req RestoreRequest
		if err := json.NewDecoder(request.Body).Decode(&req); err != nil {
			http.Error(writer, fmt.Sprintf("invalid JSON body: %v", err), http.StatusBadRequest)
			return
		}

		if req.BackupID == "" {
			http.Error(writer, "backupID is required", http.StatusBadRequest)
			return
		}

		spc, err := database.GetOrCreateServiceProviderCluster(request.Context(), dbClient, resourceID)
		if err != nil {
			http.Error(writer, fmt.Sprintf("failed to get service provider cluster: %v", err), http.StatusInternalServerError)
			return
		}

		switch spc.Status.RecoveryState {
		case api.RecoveryStatePending,
			api.RecoveryStateReadOnlyPending,
			api.RecoveryStateRecoveryCRCreated,
			api.RecoveryStateMonitoring,
			api.RecoveryStateRestoring:
			http.Error(writer, fmt.Sprintf("recovery already in progress (state: %s)", spc.Status.RecoveryState), http.StatusConflict)
			return
		}

		spc.Status.RecoveryState = api.RecoveryStatePending
		spc.Status.RecoveryBackupID = req.BackupID
		spc.Status.RecoveryManifestWorkName = ""
		spc.Status.RecoveryPhase = ""
		spc.Status.RecoveryStartedAt = nil
		spc.Status.RecoveryCompletedAt = nil
		spc.Status.RecoveryLastConditionType = ""
		spc.Status.RecoveryLastConditionMessage = ""

		spcCRUD := dbClient.ServiceProviderClusters(resourceID.SubscriptionID, resourceID.ResourceGroupName, resourceID.Name)
		spc, err = spcCRUD.Replace(request.Context(), spc, nil)
		if err != nil {
			http.Error(writer, fmt.Sprintf("failed to update recovery state: %v", err), http.StatusInternalServerError)
			return
		}

		response := newRestoreResponse(resourceID.String(), spc)

		writer.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(writer).Encode(response); err != nil {
			http.Error(writer, fmt.Sprintf("failed to encode output: %v", err), http.StatusInternalServerError)
			return
		}
	})
}

func GetRestoreStatus(dbClient database.ResourcesDBClient) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		resourceID, err := utils.ResourceIDFromContext(request.Context())
		if err != nil {
			http.Error(writer, fmt.Sprintf("failed to get resource ID: %v", err), http.StatusInternalServerError)
			return
		}

		spc, err := database.GetOrCreateServiceProviderCluster(request.Context(), dbClient, resourceID)
		if err != nil {
			http.Error(writer, fmt.Sprintf("failed to get service provider cluster: %v", err), http.StatusInternalServerError)
			return
		}

		response := newRestoreResponse(resourceID.String(), spc)

		writer.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(writer).Encode(response); err != nil {
			http.Error(writer, fmt.Sprintf("failed to encode output: %v", err), http.StatusInternalServerError)
			return
		}
	})
}
