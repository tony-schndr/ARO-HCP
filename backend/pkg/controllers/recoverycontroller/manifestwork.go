// Copyright 2026 Microsoft Corporation
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package recoverycontroller

import (
	"encoding/json"
	"fmt"
	"strings"

	workv1 "open-cluster-management.io/api/work/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

const (
	recoveryManagedByK8sLabelKey   = "aro-hcp.azure.com/recovery-managed-by"
	recoveryManagedByK8sLabelValue = "recovery-controller"

	hcpRecoveryNamespace = "hcp-recovery"

	hcpRecoveryAPIVersion = "hcprecovery.aro-hcp.azure.com/v1alpha1"
	hcpRecoveryKind       = "HCPRecovery"
	hcpRecoveryGroup      = "hcprecovery.aro-hcp.azure.com"
	hcpRecoveryResource   = "hcprecoveries"

	innerManifestWorkAPIVersion = "work.open-cluster-management.io/v1"
	innerManifestWorkKind       = "ManifestWork"

	labelContainsNamespaces = "containsNamespaces"
	labelHostedCluster      = "api.openshift.com/hosted-cluster"
	labelNodePool           = "api.openshift.com/nodepool"
	labelNodePoolOcm        = "api.openshift.com/nodepool-ocm"
)

type manifestWorkPatch struct {
	Name string
	Data []byte
}

// hcpRecoveryCR is a minimal typed representation of the HCPRecovery CR
// used to construct the ManifestWork payload without importing the hcp-recovery module.
type hcpRecoveryCR struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Spec              hcpRecoverySpec `json:"spec"`
}

type hcpRecoverySpec struct {
	ClusterId string `json:"clusterId"`
	BackupId  string `json:"backupId"`
}

func buildRecoveryManifestWork(
	maestroBundleNamespacedName types.NamespacedName,
	clusterID string,
	backupID string,
) (*workv1.ManifestWork, error) {
	recovery := hcpRecoveryCR{
		TypeMeta: metav1.TypeMeta{
			APIVersion: hcpRecoveryAPIVersion,
			Kind:       hcpRecoveryKind,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("recovery-%s", clusterID),
			Namespace: hcpRecoveryNamespace,
		},
		Spec: hcpRecoverySpec{
			ClusterId: clusterID,
			BackupId:  backupID,
		},
	}

	raw, err := json.Marshal(recovery)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal HCPRecovery CR: %w", err)
	}

	return &workv1.ManifestWork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maestroBundleNamespacedName.Name,
			Namespace: maestroBundleNamespacedName.Namespace,
			Labels: map[string]string{
				recoveryManagedByK8sLabelKey: recoveryManagedByK8sLabelValue,
			},
		},
		Spec: workv1.ManifestWorkSpec{
			Workload: workv1.ManifestsTemplate{
				Manifests: []workv1.Manifest{
					{
						RawExtension: runtime.RawExtension{Raw: raw},
					},
				},
			},
			ManifestConfigs: []workv1.ManifestConfigOption{
				{
					ResourceIdentifier: workv1.ResourceIdentifier{
						Group:     hcpRecoveryGroup,
						Resource:  hcpRecoveryResource,
						Name:      recovery.Name,
						Namespace: recovery.Namespace,
					},
					UpdateStrategy: &workv1.UpdateStrategy{
						Type: workv1.UpdateStrategyTypeServerSideApply,
					},
					FeedbackRules: []workv1.FeedbackRule{
						{
							Type: workv1.JSONPathsType,
							JsonPaths: []workv1.JsonPath{
								{
									Name: "status",
									Path: ".status",
								},
							},
						},
					},
				},
			},
		},
	}, nil
}

func buildReadOnlyPatch(outerMW *workv1.ManifestWork) ([]byte, error) {
	innerMW, err := extractInnerManifestWork(outerMW)
	if err != nil {
		return nil, err
	}

	for i := range innerMW.Spec.ManifestConfigs {
		innerMW.Spec.ManifestConfigs[i].UpdateStrategy = &workv1.UpdateStrategy{
			Type: workv1.UpdateStrategyTypeReadOnly,
		}
	}

	return buildInnerManifestWorkPatch(innerMW)
}

func buildRestorePatch(outerMW *workv1.ManifestWork) ([]byte, error) {
	innerMW, err := extractInnerManifestWork(outerMW)
	if err != nil {
		return nil, err
	}

	strategy := normalStrategyForManifestWork(innerMW)
	for i := range innerMW.Spec.ManifestConfigs {
		innerMW.Spec.ManifestConfigs[i].UpdateStrategy = &workv1.UpdateStrategy{
			Type: strategy,
		}
	}

	return buildInnerManifestWorkPatch(innerMW)
}

func buildInnerManifestWorkPatch(innerMW *workv1.ManifestWork) ([]byte, error) {
	modifiedBytes, err := json.Marshal(innerMW)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize modified inner ManifestWork: %w", err)
	}

	return json.Marshal(map[string]any{
		"spec": map[string]any{
			"workload": map[string]any{
				"manifests": []json.RawMessage{json.RawMessage(modifiedBytes)},
			},
		},
	})
}

func isClusterManifestWork(mw *workv1.ManifestWork, clusterID string) bool {
	if strings.HasPrefix(mw.Name, clusterID) {
		return true
	}
	for _, config := range mw.Spec.ManifestConfigs {
		if strings.Contains(config.ResourceIdentifier.Namespace, clusterID) ||
			strings.Contains(config.ResourceIdentifier.Name, clusterID) {
			return true
		}
	}
	return false
}

func isAllReadOnly(outerMW *workv1.ManifestWork) bool {
	innerMW, err := extractInnerManifestWork(outerMW)
	if err != nil {
		return false
	}
	if len(innerMW.Spec.ManifestConfigs) == 0 {
		return false
	}
	for _, config := range innerMW.Spec.ManifestConfigs {
		if config.UpdateStrategy == nil || config.UpdateStrategy.Type != workv1.UpdateStrategyTypeReadOnly {
			return false
		}
	}
	return true
}

func extractInnerManifestWork(outerMW *workv1.ManifestWork) (*workv1.ManifestWork, error) {
	if len(outerMW.Spec.Workload.Manifests) == 0 {
		return nil, fmt.Errorf("no manifests in ManifestWork %s", outerMW.Name)
	}

	raw := outerMW.Spec.Workload.Manifests[0].Raw
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty manifest payload in ManifestWork %s", outerMW.Name)
	}

	innerMW := &workv1.ManifestWork{}
	if err := json.Unmarshal(raw, innerMW); err != nil {
		return nil, fmt.Errorf("failed to unmarshal inner ManifestWork from %s: %w", outerMW.Name, err)
	}
	return innerMW, nil
}

func containsInnerManifestWork(outerMW *workv1.ManifestWork) bool {
	if len(outerMW.Spec.Workload.Manifests) == 0 {
		return false
	}

	raw := outerMW.Spec.Workload.Manifests[0].Raw
	if len(raw) == 0 {
		return false
	}

	var meta struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		return false
	}
	return meta.APIVersion == innerManifestWorkAPIVersion && meta.Kind == innerManifestWorkKind
}

func normalStrategyForManifestWork(innerMW *workv1.ManifestWork) workv1.UpdateStrategyType {
	if _, ok := innerMW.Labels[labelContainsNamespaces]; ok {
		return workv1.UpdateStrategyTypeCreateOnly
	}
	if _, ok := innerMW.Labels[labelHostedCluster]; ok {
		return workv1.UpdateStrategyTypeServerSideApply
	}
	if _, ok := innerMW.Labels[labelNodePool]; ok {
		return workv1.UpdateStrategyTypeServerSideApply
	}
	if _, ok := innerMW.Labels[labelNodePoolOcm]; ok {
		return workv1.UpdateStrategyTypeServerSideApply
	}
	return workv1.UpdateStrategyTypeReadOnly
}

// extractRecoveryFeedback extracts the HCPRecovery status from ManifestWork feedback.
func extractRecoveryFeedback(mfw *workv1.ManifestWork) (phase string, lastConditionType string, lastConditionMessage string) {
	if mfw == nil {
		return "", "", ""
	}

	for _, manifest := range mfw.Status.ResourceStatus.Manifests {
		for _, feedback := range manifest.StatusFeedbacks.Values {
			if feedback.Name != "status" || feedback.Value.Type != workv1.JsonRaw || feedback.Value.JsonRaw == nil {
				continue
			}

			var status struct {
				Phase      string             `json:"phase,omitempty"`
				Conditions []metav1.Condition `json:"conditions,omitempty"`
			}
			if err := json.Unmarshal([]byte(*feedback.Value.JsonRaw), &status); err != nil {
				continue
			}

			phase = status.Phase

			for _, cond := range status.Conditions {
				if cond.Status == metav1.ConditionTrue {
					lastConditionType = cond.Type
					lastConditionMessage = cond.Message
				}
			}
		}
	}
	return phase, lastConditionType, lastConditionMessage
}
