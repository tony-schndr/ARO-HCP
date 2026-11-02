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
	"fmt"

	"github.com/blang/semver/v4"

	configv1 "github.com/openshift/api/config/v1"
)

// ClusterVersionHistoryEntrySummary is a compact view of one ClusterVersion status.history entry.
// Prefer SummarizeClusterVersionHistory over using raw []UpdateHistory where values are formatted
// for output (see SummarizeClusterVersionHistory).
type ClusterVersionHistoryEntrySummary struct {
	Version string `json:"version"`
	State   string `json:"state"`
	Image   string `json:"image"`
}

// SummarizeClusterVersionHistory returns a representation of ClusterVersion status.history entries
// that omits *metav1.Time fields from configv1.UpdateHistory, avoiding nil pointer panics when the
// result is logged (for example CompletionTime is nil when state is "Partial") that the GinkgoLogr
// formatter would hit when logging raw history in test output.
func SummarizeClusterVersionHistory(history []configv1.UpdateHistory) []ClusterVersionHistoryEntrySummary {
	out := make([]ClusterVersionHistoryEntrySummary, 0, len(history))
	for _, h := range history {
		out = append(out, ClusterVersionHistoryEntrySummary{
			Version: h.Version,
			State:   string(h.State),
			Image:   h.Image,
		})
	}
	return out
}

// LatestClusterVersionHistoryEntryForMinor returns the newest ClusterVersion status.history entry
// whose version shares a major.minor line with wantVersion. wantVersion may be a bare minor line
// ("4.22") or a full build tag ("4.22.0-0.nightly-2026-11-03-123456"); only major.minor is
// compared, so a caller that resolved an exact nightly tag still matches the build the cluster
// actually landed on. History is ordered newest first, so the first match is the most recent
// attempt at that line. Returns a nil entry when the line is absent from the history.
func LatestClusterVersionHistoryEntryForMinor(history []configv1.UpdateHistory, wantVersion string) (*configv1.UpdateHistory, error) {
	want, err := semver.ParseTolerant(wantVersion)
	if err != nil {
		return nil, fmt.Errorf("parse version %q: %w", wantVersion, err)
	}
	for i := range history {
		if len(history[i].Version) == 0 {
			continue
		}
		got, err := semver.ParseTolerant(history[i].Version)
		if err != nil {
			return nil, fmt.Errorf("parse clusterversion history version %q: %w", history[i].Version, err)
		}
		if got.Major == want.Major && got.Minor == want.Minor {
			return &history[i], nil
		}
	}
	return nil, nil
}
