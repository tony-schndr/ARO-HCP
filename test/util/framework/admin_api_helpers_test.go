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

package framework

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Azure/ARO-HCP/admin/server/handlers/hcp"
)

func TestWaitForOnDemandBackupCompleted(t *testing.T) {
	for _, phase := range []string{"Completed", "Failed", "PartiallyFailed", "FailedValidation"} {
		t.Run(phase, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/admin/v1/hcp/resource/backups" {
					t.Errorf("unexpected backup lookup: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				err := json.NewEncoder(w).Encode(hcp.OnDemandBackupResponse{Backups: []hcp.OnDemandBackupDetail{
					{Name: "other-backup", Phase: "Failed"},
					{Name: "selected-backup", Phase: phase, KMSKeyFingerprint: "fingerprint"},
				}})
				if err != nil {
					t.Errorf("writing backup response: %v", err)
				}
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()

			backup, err := WaitForOnDemandBackupCompleted(ctx, server.Client(), server.URL, "/resource", "selected-backup")
			if phase == "Completed" {
				if err != nil || backup.Name != "selected-backup" || backup.KMSKeyFingerprint != "fingerprint" {
					t.Fatalf("expected the selected completed backup with its fingerprint, got %#v, err=%v", backup, err)
				}
				return
			}
			expected := "backup selected-backup ended in " + phase + "; expected Completed"
			if err == nil || !strings.Contains(err.Error(), expected) {
				t.Fatalf("expected immediate, diagnostic backup failure containing %q, got %v", expected, err)
			}
		})
	}
}
