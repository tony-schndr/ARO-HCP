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

package verifiers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/blang/semver/v4"
	"github.com/stretchr/testify/require"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/utils/set"

	hypershiftv1beta1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
)

func TestNodeVersionInMinor(t *testing.T) {
	tests := []struct {
		name            string
		nodeName        string
		kubeletVersion  string
		expectedVersion string
		wantEmpty       bool
		wantSubstring   string
	}{
		{
			name:            "matching kubelet version OCP 4.22 = k8s 1.35",
			nodeName:        "node-1",
			kubeletVersion:  "v1.35.6+abc123",
			expectedVersion: "4.22",
			wantEmpty:       true,
		},
		{
			name:            "wrong kubelet minor",
			nodeName:        "node-2",
			kubeletVersion:  "v1.34.6+abc123",
			expectedVersion: "4.22",
			wantEmpty:       false,
			wantSubstring:   "KubeletVersion v1.34.6+abc123 has minor 34, expected 35 for OCP 4.22",
		},
		{
			name:            "unexpected Kubernetes major version",
			nodeName:        "node-3",
			kubeletVersion:  "v2.35.0+bogus",
			expectedVersion: "4.22",
			wantEmpty:       false,
			wantSubstring:   "unexpected major 2, expected 1",
		},
		{
			name:            "unparseable kubelet version",
			nodeName:        "node-4",
			kubeletVersion:  "not-a-version",
			expectedVersion: "4.22",
			wantEmpty:       false,
			wantSubstring:   "cannot parse KubeletVersion",
		},
		{
			name:            "boundary OCP 4.14 = k8s 1.27",
			nodeName:        "node-5",
			kubeletVersion:  "v1.27.5+abc",
			expectedVersion: "4.14",
			wantEmpty:       true,
		},
		{
			name:            "unmapped future OCP minor fails loudly instead of guessing",
			nodeName:        "node-6",
			kubeletVersion:  "v1.36.0+xyz",
			expectedVersion: "4.23",
			wantEmpty:       false,
			wantSubstring:   "no known Kubernetes minor mapping for OCP 4.23",
		},
		{
			name:            "unmapped OCP major fails loudly instead of guessing",
			nodeName:        "node-8",
			kubeletVersion:  "v1.36.0+xyz",
			expectedVersion: "5.0",
			wantEmpty:       false,
			wantSubstring:   "no known Kubernetes minor mapping for OCP major 5",
		},
		{
			name:            "matching kubelet version OCP 4.20 = k8s 1.33",
			nodeName:        "node-7",
			kubeletVersion:  "v1.33.6+abc123",
			expectedVersion: "4.20",
			wantEmpty:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: tt.nodeName,
				},
				Status: corev1.NodeStatus{
					NodeInfo: corev1.NodeSystemInfo{
						KubeletVersion: tt.kubeletVersion,
					},
				},
			}

			expectedSemver, err := semver.ParseTolerant(tt.expectedVersion)
			if err != nil {
				t.Fatalf("failed to parse expectedVersion %q: %v", tt.expectedVersion, err)
			}

			v := verifyNodePoolUpgrade{
				expectedVersion: tt.expectedVersion,
			}

			got := v.nodeVersionInMinor(node, expectedSemver)

			if tt.wantEmpty && got != "" {
				t.Errorf("expected empty result, got %q", got)
			}
			if !tt.wantEmpty && got == "" {
				t.Errorf("expected non-empty result containing %q, got empty", tt.wantSubstring)
			}
			if !tt.wantEmpty && tt.wantSubstring != "" {
				if !strings.Contains(got, tt.wantSubstring) {
					t.Errorf("expected result to contain %q, got %q", tt.wantSubstring, got)
				}
			}
		})
	}
}

func TestOcpToK8sMinor(t *testing.T) {
	// Verify the known-good OCP-to-Kubernetes minor mappings we've tested against.
	knownMappings := []struct {
		ocpMinor uint64
		k8sMinor uint64
	}{
		{14, 27}, // OCP 4.14 = k8s 1.27
		{15, 28}, // OCP 4.15 = k8s 1.28
		{16, 29}, // OCP 4.16 = k8s 1.29
		{17, 30}, // OCP 4.17 = k8s 1.30
		{18, 31}, // OCP 4.18 = k8s 1.31
		{19, 32}, // OCP 4.19 = k8s 1.32
		{20, 33}, // OCP 4.20 = k8s 1.33
		{21, 34}, // OCP 4.21 = k8s 1.34
		{22, 35}, // OCP 4.22 = k8s 1.35
	}

	for _, m := range knownMappings {
		got, ok := ocpToK8sMinor[m.ocpMinor]
		if !ok {
			t.Errorf("OCP 4.%d: expected an entry in ocpToK8sMinor, found none", m.ocpMinor)
			continue
		}
		if got != m.k8sMinor {
			t.Errorf("OCP 4.%d: expected k8s minor %d, got %d", m.ocpMinor, m.k8sMinor, got)
		}
	}

	if _, ok := ocpToK8sMinor[23]; ok {
		t.Errorf("OCP 4.23 should not be mapped yet; add it deliberately once verified, not accidentally")
	}
}

func TestNodeReleaseImagesUpdated(t *testing.T) {
	previous := set.New[string]("img-1", "img-2", "img-3", "img-4", "img-5", "img-6", "img-7")
	v := verifyNodePoolUpgrade{previousReleaseImages: previous}

	t.Run("all images unchanged returns reason with image count", func(t *testing.T) {
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
			Status: corev1.NodeStatus{
				Images: []corev1.ContainerImage{
					{Names: []string{"img-1"}},
					{Names: []string{"img-2"}},
					{Names: []string{"img-3"}},
					{Names: []string{"img-4"}},
					{Names: []string{"img-5"}},
					{Names: []string{"img-6"}},
					{Names: []string{"img-7"}},
				},
			},
		}

		got := v.nodeReleaseImagesUpdated(node)
		if got == "" {
			t.Fatal("expected a non-empty reason since no image differs from previous")
		}
		expected := "node-1 (release images unchanged: 7 image entries on node, none differ from pre-upgrade set)"
		if got != expected {
			t.Errorf("expected %q, got %q", expected, got)
		}
	})

	t.Run("node with no images returns reason", func(t *testing.T) {
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "node-2"},
			Status:     corev1.NodeStatus{},
		}

		got := v.nodeReleaseImagesUpdated(node)
		if got == "" {
			t.Fatal("expected a non-empty reason for node with no images")
		}
		expected := "node-2 (node has no images yet)"
		if got != expected {
			t.Errorf("expected %q, got %q", expected, got)
		}
	})

	t.Run("at least one new image returns empty reason", func(t *testing.T) {
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "node-3"},
			Status: corev1.NodeStatus{
				Images: []corev1.ContainerImage{
					{Names: []string{"img-1"}},
					{Names: []string{"new-img"}},
				},
			},
		}

		got := v.nodeReleaseImagesUpdated(node)
		if got != "" {
			t.Errorf("expected empty reason when a new image is present, got %q", got)
		}
	})
}

func TestExtractNodePoolName(t *testing.T) {
	tests := []struct {
		name          string
		nodePoolLabel string
		want          string
	}{
		{
			name:          "standard format with cluster prefix",
			nodePoolLabel: "np-delete-hcp-cluster-np-delete-main",
			want:          "np-delete-main",
		},
		{
			name:          "simple nodepool name",
			nodePoolLabel: "cluster-workers",
			want:          "workers",
		},
		{
			name:          "single component fallback",
			nodePoolLabel: "workers",
			want:          "workers",
		},
		{
			name:          "multiple dashes in cluster name",
			nodePoolLabel: "my-long-cluster-name-np-1",
			want:          "name-np-1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractNodePoolName(tt.nodePoolLabel)
			if got != tt.want {
				t.Errorf("extractNodePoolName(%q) = %q, want %q", tt.nodePoolLabel, got, tt.want)
			}
		})
	}
}

func TestVerifyNodePoolConverged(t *testing.T) {
	tests := []struct {
		name        string
		count       int
		expected    int
		mutate      func(*corev1.NodeList)
		wantMessage string
	}{
		{name: "two nodes before backup", count: 2, expected: 2},
		{name: "three reconciled nodes", count: 3, expected: 3},
		{name: "only backup nodes returned", count: 2, expected: 3, wantMessage: "found 2 total and 2 matching"},
		{name: "removed third node restored", count: 3, expected: 2, wantMessage: "found 3 total and 3 matching"},
		{
			name: "stale extra node", count: 4, expected: 3,
			mutate: func(nodes *corev1.NodeList) {
				nodes.Items[3].Status.Conditions[0].Status = corev1.ConditionFalse
			},
			wantMessage: "found 4 total and 4 matching",
		},
		{
			name: "node in another pool", count: 3, expected: 3,
			mutate: func(nodes *corev1.NodeList) {
				nodes.Items[2].Labels[hypershiftv1beta1.NodePoolLabel] = "recovery-cluster-np-2"
			},
			wantMessage: "found 3 total and 2 matching",
		},
		{
			name: "missing node pool label", count: 3, expected: 3,
			mutate: func(nodes *corev1.NodeList) {
				nodes.Items[2].Labels = nil
			},
			wantMessage: "found 3 total and 2 matching",
		},
		{
			name: "node not ready", count: 3, expected: 3,
			mutate: func(nodes *corev1.NodeList) {
				nodes.Items[2].Status.Conditions[0].Status = corev1.ConditionFalse
			},
			wantMessage: "worker-2 (status=False",
		},
		{
			name: "cordoned node", count: 3, expected: 3,
			mutate: func(nodes *corev1.NodeList) {
				nodes.Items[2].Spec.Unschedulable = true
			},
			wantMessage: "unschedulable=true",
		},
		{
			name: "terminating node", count: 3, expected: 3,
			mutate: func(nodes *corev1.NodeList) {
				now := metav1.Now()
				nodes.Items[2].DeletionTimestamp = &now
			},
			wantMessage: "terminating=true",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodes := corev1.NodeList{}
			for i := range tt.count {
				nodes.Items = append(nodes.Items, corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name:   fmt.Sprintf("worker-%d", i),
						Labels: map[string]string{hypershiftv1beta1.NodePoolLabel: "recovery-cluster-np-1"},
					},
					Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
						{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
					}},
				})
			}
			if tt.mutate != nil {
				tt.mutate(&nodes)
			}
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path != "/api/v1/nodes" {
					http.NotFound(writer, request)
					return
				}
				writer.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(writer).Encode(nodes); err != nil {
					t.Errorf("failed to encode nodes: %v", err)
				}
			}))
			defer server.Close()

			err := VerifyNodePoolConverged("np-1", tt.expected, 100*time.Millisecond).
				Verify(context.Background(), &rest.Config{Host: server.URL})
			if tt.wantMessage == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tt.wantMessage)
				require.ErrorContains(t, err, fmt.Sprintf("expected=%d", tt.expected))
				require.ErrorContains(t, err, "np-1")
			}
		})
	}
}
