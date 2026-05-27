package recoverycontroller

import (
	"encoding/json"
	"testing"

	workv1 "open-cluster-management.io/api/work/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func makeInnerManifestWork(labels map[string]string, configs []workv1.ManifestConfigOption) *workv1.ManifestWork {
	return &workv1.ManifestWork{
		TypeMeta: metav1.TypeMeta{
			APIVersion: innerManifestWorkAPIVersion,
			Kind:       innerManifestWorkKind,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-inner-mw",
			Namespace: "test-ns",
			Labels:    labels,
		},
		Spec: workv1.ManifestWorkSpec{
			ManifestConfigs: configs,
		},
	}
}

func wrapInOuterBundle(t *testing.T, name string, inner *workv1.ManifestWork) *workv1.ManifestWork {
	t.Helper()
	raw, err := json.Marshal(inner)
	require.NoError(t, err)
	return &workv1.ManifestWork{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: workv1.ManifestWorkSpec{
			Workload: workv1.ManifestsTemplate{
				Manifests: []workv1.Manifest{
					{RawExtension: runtime.RawExtension{Raw: raw}},
				},
			},
		},
	}
}

func ssaConfigs() []workv1.ManifestConfigOption {
	return []workv1.ManifestConfigOption{
		{
			ResourceIdentifier: workv1.ResourceIdentifier{
				Group:    "hypershift.openshift.io",
				Resource: "hostedclusters",
				Name:     "test-hc",
			},
			UpdateStrategy: &workv1.UpdateStrategy{
				Type: workv1.UpdateStrategyTypeServerSideApply,
			},
		},
	}
}

func readOnlyConfigs() []workv1.ManifestConfigOption {
	return []workv1.ManifestConfigOption{
		{
			ResourceIdentifier: workv1.ResourceIdentifier{
				Group:    "hypershift.openshift.io",
				Resource: "hostedclusters",
				Name:     "test-hc",
			},
			UpdateStrategy: &workv1.UpdateStrategy{
				Type: workv1.UpdateStrategyTypeReadOnly,
			},
		},
	}
}

func TestExtractInnerManifestWork(t *testing.T) {
	t.Run("valid inner MW", func(t *testing.T) {
		inner := makeInnerManifestWork(nil, ssaConfigs())
		outer := wrapInOuterBundle(t, "bundle-1", inner)

		got, err := extractInnerManifestWork(outer)
		require.NoError(t, err)
		assert.Equal(t, "test-inner-mw", got.Name)
		assert.Equal(t, workv1.UpdateStrategyTypeServerSideApply, got.Spec.ManifestConfigs[0].UpdateStrategy.Type)
	})

	t.Run("no manifests", func(t *testing.T) {
		outer := &workv1.ManifestWork{
			ObjectMeta: metav1.ObjectMeta{Name: "empty"},
			Spec:       workv1.ManifestWorkSpec{},
		}
		_, err := extractInnerManifestWork(outer)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "no manifests")
	})

	t.Run("empty raw payload", func(t *testing.T) {
		outer := &workv1.ManifestWork{
			ObjectMeta: metav1.ObjectMeta{Name: "empty-raw"},
			Spec: workv1.ManifestWorkSpec{
				Workload: workv1.ManifestsTemplate{
					Manifests: []workv1.Manifest{
						{RawExtension: runtime.RawExtension{Raw: nil}},
					},
				},
			},
		}
		_, err := extractInnerManifestWork(outer)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "empty manifest payload")
	})
}

func TestContainsInnerManifestWork(t *testing.T) {
	t.Run("true for inner MW bundle", func(t *testing.T) {
		inner := makeInnerManifestWork(nil, ssaConfigs())
		outer := wrapInOuterBundle(t, "bundle-1", inner)
		assert.True(t, containsInnerManifestWork(outer))
	})

	t.Run("false for HCPRecovery CR bundle", func(t *testing.T) {
		recovery := hcpRecoveryCR{
			TypeMeta: metav1.TypeMeta{
				APIVersion: hcpRecoveryAPIVersion,
				Kind:       hcpRecoveryKind,
			},
			ObjectMeta: metav1.ObjectMeta{Name: "recovery-test"},
		}
		raw, err := json.Marshal(recovery)
		require.NoError(t, err)

		outer := &workv1.ManifestWork{
			ObjectMeta: metav1.ObjectMeta{Name: "recovery-bundle"},
			Spec: workv1.ManifestWorkSpec{
				Workload: workv1.ManifestsTemplate{
					Manifests: []workv1.Manifest{
						{RawExtension: runtime.RawExtension{Raw: raw}},
					},
				},
			},
		}
		assert.False(t, containsInnerManifestWork(outer))
	})

	t.Run("false for empty manifests", func(t *testing.T) {
		outer := &workv1.ManifestWork{
			ObjectMeta: metav1.ObjectMeta{Name: "empty"},
			Spec:       workv1.ManifestWorkSpec{},
		}
		assert.False(t, containsInnerManifestWork(outer))
	})
}

func TestNormalStrategyForManifestWork(t *testing.T) {
	tests := []struct {
		name     string
		labels   map[string]string
		expected workv1.UpdateStrategyType
	}{
		{
			name:     "namespace MW",
			labels:   map[string]string{labelContainsNamespaces: "true"},
			expected: workv1.UpdateStrategyTypeCreateOnly,
		},
		{
			name:     "HostedCluster MW",
			labels:   map[string]string{labelHostedCluster: "test-cluster"},
			expected: workv1.UpdateStrategyTypeServerSideApply,
		},
		{
			name:     "NodePool MW (old label)",
			labels:   map[string]string{labelNodePool: "test-np"},
			expected: workv1.UpdateStrategyTypeServerSideApply,
		},
		{
			name:     "NodePool MW (new label)",
			labels:   map[string]string{labelNodePoolOcm: "test-np-ocm"},
			expected: workv1.UpdateStrategyTypeServerSideApply,
		},
		{
			name:     "default (no recognized labels)",
			labels:   map[string]string{"some-other-label": "val"},
			expected: workv1.UpdateStrategyTypeReadOnly,
		},
		{
			name:     "nil labels",
			labels:   nil,
			expected: workv1.UpdateStrategyTypeReadOnly,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			innerMW := makeInnerManifestWork(tc.labels, nil)
			assert.Equal(t, tc.expected, normalStrategyForManifestWork(innerMW))
		})
	}
}

func TestBuildReadOnlyPatch(t *testing.T) {
	inner := makeInnerManifestWork(
		map[string]string{labelHostedCluster: "test"},
		ssaConfigs(),
	)
	outer := wrapInOuterBundle(t, "bundle-1", inner)

	patchData, err := buildReadOnlyPatch(outer)
	require.NoError(t, err)

	// Apply the patch to verify the inner MW was modified
	var patch struct {
		Spec struct {
			Workload struct {
				Manifests []json.RawMessage `json:"manifests"`
			} `json:"workload"`
		} `json:"spec"`
	}
	require.NoError(t, json.Unmarshal(patchData, &patch))
	require.Len(t, patch.Spec.Workload.Manifests, 1)

	var patchedInner workv1.ManifestWork
	require.NoError(t, json.Unmarshal(patch.Spec.Workload.Manifests[0], &patchedInner))

	require.Len(t, patchedInner.Spec.ManifestConfigs, 1)
	assert.Equal(t, workv1.UpdateStrategyTypeReadOnly, patchedInner.Spec.ManifestConfigs[0].UpdateStrategy.Type)
}

func TestBuildRestorePatch(t *testing.T) {
	t.Run("HostedCluster restores to SSA", func(t *testing.T) {
		inner := makeInnerManifestWork(
			map[string]string{labelHostedCluster: "test"},
			readOnlyConfigs(),
		)
		outer := wrapInOuterBundle(t, "bundle-1", inner)

		patchData, err := buildRestorePatch(outer)
		require.NoError(t, err)

		var patch struct {
			Spec struct {
				Workload struct {
					Manifests []json.RawMessage `json:"manifests"`
				} `json:"workload"`
			} `json:"spec"`
		}
		require.NoError(t, json.Unmarshal(patchData, &patch))

		var patchedInner workv1.ManifestWork
		require.NoError(t, json.Unmarshal(patch.Spec.Workload.Manifests[0], &patchedInner))

		require.Len(t, patchedInner.Spec.ManifestConfigs, 1)
		assert.Equal(t, workv1.UpdateStrategyTypeServerSideApply, patchedInner.Spec.ManifestConfigs[0].UpdateStrategy.Type)
	})

	t.Run("namespace restores to CreateOnly", func(t *testing.T) {
		inner := makeInnerManifestWork(
			map[string]string{labelContainsNamespaces: "true"},
			readOnlyConfigs(),
		)
		outer := wrapInOuterBundle(t, "bundle-ns", inner)

		patchData, err := buildRestorePatch(outer)
		require.NoError(t, err)

		var patch struct {
			Spec struct {
				Workload struct {
					Manifests []json.RawMessage `json:"manifests"`
				} `json:"workload"`
			} `json:"spec"`
		}
		require.NoError(t, json.Unmarshal(patchData, &patch))

		var patchedInner workv1.ManifestWork
		require.NoError(t, json.Unmarshal(patch.Spec.Workload.Manifests[0], &patchedInner))

		assert.Equal(t, workv1.UpdateStrategyTypeCreateOnly, patchedInner.Spec.ManifestConfigs[0].UpdateStrategy.Type)
	})
}

func TestIsAllReadOnly(t *testing.T) {
	t.Run("all ReadOnly", func(t *testing.T) {
		inner := makeInnerManifestWork(nil, readOnlyConfigs())
		outer := wrapInOuterBundle(t, "bundle-1", inner)
		assert.True(t, isAllReadOnly(outer))
	})

	t.Run("not all ReadOnly", func(t *testing.T) {
		inner := makeInnerManifestWork(nil, ssaConfigs())
		outer := wrapInOuterBundle(t, "bundle-1", inner)
		assert.False(t, isAllReadOnly(outer))
	})

	t.Run("no manifest configs", func(t *testing.T) {
		inner := makeInnerManifestWork(nil, nil)
		outer := wrapInOuterBundle(t, "bundle-1", inner)
		assert.False(t, isAllReadOnly(outer))
	})

	t.Run("non-MW bundle returns false", func(t *testing.T) {
		raw, _ := json.Marshal(map[string]string{"apiVersion": "v1", "kind": "ConfigMap"})
		outer := &workv1.ManifestWork{
			ObjectMeta: metav1.ObjectMeta{Name: "non-mw"},
			Spec: workv1.ManifestWorkSpec{
				Workload: workv1.ManifestsTemplate{
					Manifests: []workv1.Manifest{
						{RawExtension: runtime.RawExtension{Raw: raw}},
					},
				},
			},
		}
		assert.False(t, isAllReadOnly(outer))
	})

	t.Run("mixed strategies", func(t *testing.T) {
		inner := makeInnerManifestWork(nil, []workv1.ManifestConfigOption{
			{
				ResourceIdentifier: workv1.ResourceIdentifier{Name: "a"},
				UpdateStrategy:     &workv1.UpdateStrategy{Type: workv1.UpdateStrategyTypeReadOnly},
			},
			{
				ResourceIdentifier: workv1.ResourceIdentifier{Name: "b"},
				UpdateStrategy:     &workv1.UpdateStrategy{Type: workv1.UpdateStrategyTypeServerSideApply},
			},
		})
		outer := wrapInOuterBundle(t, "bundle-mixed", inner)
		assert.False(t, isAllReadOnly(outer))
	})
}
