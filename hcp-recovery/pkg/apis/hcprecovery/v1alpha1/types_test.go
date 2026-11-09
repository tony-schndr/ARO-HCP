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

package v1alpha1

import (
	"encoding/json"
	"os"
	"testing"

	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	apiservervalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	celconfig "k8s.io/apiserver/pkg/apis/cel"

	"sigs.k8s.io/yaml"
)

func TestHostedClusterNameValidation(t *testing.T) {
	raw, err := os.ReadFile("../../../../deploy/templates/hcprecovery-crd.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatal(err)
	}
	var props apiextensions.JSONSchemaProps
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(
		crd.Spec.Versions[0].Schema.OpenAPIV3Schema, &props, nil); err != nil {
		t.Fatal(err)
	}
	validator, _, err := apiservervalidation.NewSchemaValidator(&props)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name  string
		value interface{}
		valid bool
	}{
		{name: "missing"},
		{name: "empty", value: ""},
		{name: "named target", value: "cluster", valid: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			spec := map[string]interface{}{
				"hcpClusterAzureResourceId": "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.RedHatOpenShift/hcpOpenShiftClusters/cluster",
				"hostedClusterNamespace":    "hosted-cluster",
				"controlPlaneNamespace":     "hosted-cluster-cluster",
				"veleroBackupName":          "backup",
			}
			if tt.value != nil {
				spec["hostedClusterName"] = tt.value
			}
			errs := apiservervalidation.ValidateCustomResource(field.NewPath(""), map[string]interface{}{"spec": spec}, validator)
			if tt.valid != (len(errs) == 0) {
				t.Fatalf("valid=%v, got validation errors=%v", tt.valid, errs)
			}
		})
	}
}

func TestEtcdRuntimeDetailsJSON(t *testing.T) {
	zero, uid, gid := int64(0), int64(1001), int64(1002)
	for _, tt := range []struct {
		name    string
		details *EtcdRuntimeDetails
		want    string
	}{
		{
			name: "not yet collected",
			want: `{"restoreName":"restore"}`,
		},
		{
			name:    "unspecified pod identity",
			details: &EtcdRuntimeDetails{Image: "etcd:test"},
			want:    `{"restoreName":"restore","etcdRuntimeDetails":{"image":"etcd:test"}}`,
		},
		{
			name:    "explicit root identity",
			details: &EtcdRuntimeDetails{Image: "etcd:test", RunAsUser: &zero, FSGroup: &zero},
			want:    `{"restoreName":"restore","etcdRuntimeDetails":{"image":"etcd:test","runAsUser":0,"fsGroup":0}}`,
		},
		{
			name:    "non-root identity",
			details: &EtcdRuntimeDetails{Image: "etcd:test", RunAsUser: &uid, FSGroup: &gid},
			want:    `{"restoreName":"restore","etcdRuntimeDetails":{"image":"etcd:test","runAsUser":1001,"fsGroup":1002}}`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			status := HCPRecoveryStatus{RestoreName: "restore", EtcdRuntimeDetails: tt.details}
			data, err := json.Marshal(status)
			if err != nil {
				t.Fatal(err)
			}

			if string(data) != tt.want {
				t.Fatalf("unexpected status JSON: got %s, want %s", data, tt.want)
			}
			var decoded HCPRecoveryStatus
			if err := json.Unmarshal(data, &decoded); err != nil {
				t.Fatal(err)
			}
			roundTrip, err := json.Marshal(decoded)
			if err != nil {
				t.Fatal(err)
			}
			if string(roundTrip) != tt.want {
				t.Fatalf("status JSON changed after round trip: got %s, want %s", roundTrip, tt.want)
			}
		})
	}
}

func TestRecoveryPhaseTransitionValidation(t *testing.T) {
	raw, err := os.ReadFile("../../../../deploy/templates/hcprecovery-crd.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatal(err)
	}
	var props apiextensions.JSONSchemaProps
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(
		crd.Spec.Versions[0].Schema.OpenAPIV3Schema, &props, nil); err != nil {
		t.Fatal(err)
	}
	structural, err := schema.NewStructural(&props)
	if err != nil {
		t.Fatal(err)
	}
	validator := cel.NewValidator(structural, true, celconfig.PerCallLimit)
	if validator == nil {
		t.Fatal("CRD has no CEL validation")
	}
	schemaValidator, _, err := apiservervalidation.NewSchemaValidator(&props)
	if err != nil {
		t.Fatal(err)
	}
	object := func(phase RestoreState) map[string]interface{} {
		obj := map[string]interface{}{"spec": map[string]interface{}{
			"hcpClusterAzureResourceId": "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.RedHatOpenShift/hcpOpenShiftClusters/cluster",
			"hostedClusterName":         "cluster",
			"hostedClusterNamespace":    "hosted-cluster",
			"controlPlaneNamespace":     "hosted-cluster-cluster",
			"veleroBackupName":          "backup",
		}}
		if phase != "" {
			obj["status"] = map[string]interface{}{"phase": string(phase)}
		}
		return obj
	}
	// The allowed edges are restated here rather than derived, so changing the state
	// machine has to be a deliberate edit. They must match validPhaseTransition in the
	// controller, which is the client-side mirror of this rule.
	advance := map[RestoreState]RestoreState{
		"":                     RestoreStatePreparing,
		RestoreStatePreparing:  RestoreStateRestoring,
		RestoreStateRestoring:  RestoreStateValidating,
		RestoreStateValidating: RestoreStateCompleted,
	}
	// Failure is reachable from every phase. Preparation reads and scales down only,
	// but the spec is immutable, so a rejected request there cannot be corrected on the
	// object either; every phase after it is rewriting etcd's volumes and cannot be
	// resumed part way.
	states := []RestoreState{"", RestoreStatePreparing, RestoreStateRestoring, RestoreStateValidating,
		RestoreStateCompleted, RestoreStateFailed}
	for _, from := range states {
		for _, to := range states {
			t.Run(string(from)+"->"+string(to), func(t *testing.T) {
				errs := apiservervalidation.ValidateCustomResource(field.NewPath(""), object(to), schemaValidator)
				celErrs, _ := validator.Validate(t.Context(), field.NewPath(""), structural,
					object(to), object(from), celconfig.RuntimeCELCostBudget)
				errs = append(errs, celErrs...)
				next, hasNext := advance[from]
				allowed := from == to || (hasNext && next == to) ||
					to == RestoreStateFailed
				if allowed != (len(errs) == 0) {
					t.Fatalf("transition allowed=%v, got validation errors=%v", allowed, errs)
				}
			})
		}
	}
	for _, phase := range []RestoreState{"VeleroRestoring", "PreparingVolumes", "DistributingBackup", "RestoringEtcd", "StartingEtcd", "Resuming", "Unknown"} {
		t.Run("reject-"+string(phase), func(t *testing.T) {
			if errs := apiservervalidation.ValidateCustomResource(field.NewPath(""), object(phase), schemaValidator); len(errs) == 0 {
				t.Fatalf("expected phase %q to be rejected by the schema", phase)
			}
		})
	}
}
