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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	configv1 "github.com/openshift/api/config/v1"
)

func TestLatestClusterVersionHistoryEntryForMinor(t *testing.T) {
	started := func(day int) metav1.Time {
		return metav1.NewTime(time.Date(2026, 11, day, 0, 0, 0, 0, time.UTC))
	}
	// Newest first, matching how the CVO maintains status.history: the 4.22 line was
	// entered twice, which is what a restore that rolls past an upgrade produces.
	history := []configv1.UpdateHistory{
		{Version: "4.22.0-0.nightly-2026-11-04-000000", State: configv1.PartialUpdate, StartedTime: started(4)},
		{Version: "4.21.0-0.nightly-2026-11-03-000000", State: configv1.CompletedUpdate, StartedTime: started(3)},
		{Version: "4.22.0-0.nightly-2026-11-02-000000", State: configv1.CompletedUpdate, StartedTime: started(2)},
		{Version: "4.21.0-0.nightly-2026-11-01-000000", State: configv1.CompletedUpdate, StartedTime: started(1)},
	}

	for _, tt := range []struct {
		name        string
		history     []configv1.UpdateHistory
		wantVersion string
		expectEntry string
		expectErr   bool
	}{
		{
			name:        "exact build tag matches the newest entry in its minor line",
			history:     history,
			wantVersion: "4.22.0-0.nightly-2026-11-02-000000",
			expectEntry: "4.22.0-0.nightly-2026-11-04-000000",
		},
		{
			name:        "bare minor line matches the newest entry in that line",
			history:     history,
			wantVersion: "4.21",
			expectEntry: "4.21.0-0.nightly-2026-11-03-000000",
		},
		{
			name:        "absent minor line returns no entry",
			history:     history,
			wantVersion: "4.20",
		},
		{
			name:        "empty history returns no entry",
			history:     nil,
			wantVersion: "4.22",
		},
		{
			name:        "entries without a version are skipped",
			history:     []configv1.UpdateHistory{{Version: ""}, {Version: "4.22.1", StartedTime: started(1)}},
			wantVersion: "4.22",
			expectEntry: "4.22.1",
		},
		{
			name:        "unparseable wanted version is an error",
			history:     history,
			wantVersion: "not-a-version",
			expectErr:   true,
		},
		{
			name:        "unparseable history version is an error",
			history:     []configv1.UpdateHistory{{Version: "garbage"}},
			wantVersion: "4.22",
			expectErr:   true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			entry, err := LatestClusterVersionHistoryEntryForMinor(tt.history, tt.wantVersion)
			if tt.expectErr {
				if err == nil {
					t.Fatalf("expected an error, got entry %+v", entry)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.expectEntry == "" {
				if entry != nil {
					t.Fatalf("expected no entry, got %q", entry.Version)
				}
				return
			}
			if entry == nil {
				t.Fatalf("expected entry %q, got none", tt.expectEntry)
			}
			if entry.Version != tt.expectEntry {
				t.Fatalf("expected entry %q, got %q", tt.expectEntry, entry.Version)
			}
		})
	}
}
