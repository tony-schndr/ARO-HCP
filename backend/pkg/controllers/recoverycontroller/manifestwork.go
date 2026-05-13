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

func buildReadOnlyPatch(mw *workv1.ManifestWork) ([]byte, error) {
	return buildUpdateStrategyPatch(mw, workv1.UpdateStrategyTypeReadOnly)
}

func buildServerSideApplyPatch(mw *workv1.ManifestWork) ([]byte, error) {
	return buildUpdateStrategyPatch(mw, workv1.UpdateStrategyTypeServerSideApply)
}

func buildUpdateStrategyPatch(mw *workv1.ManifestWork, strategyType workv1.UpdateStrategyType) ([]byte, error) {
	configs := make([]workv1.ManifestConfigOption, len(mw.Spec.ManifestConfigs))
	for i, config := range mw.Spec.ManifestConfigs {
		configs[i] = workv1.ManifestConfigOption{
			ResourceIdentifier: config.ResourceIdentifier,
			UpdateStrategy: &workv1.UpdateStrategy{
				Type: strategyType,
			},
			FeedbackRules: config.FeedbackRules,
		}
	}

	return json.Marshal(map[string]any{
		"spec": map[string]any{
			"manifestConfigs": configs,
		},
	})
}

func isAllReadOnly(mw *workv1.ManifestWork) bool {
	if len(mw.Spec.ManifestConfigs) == 0 {
		return false
	}
	for _, config := range mw.Spec.ManifestConfigs {
		if config.UpdateStrategy == nil || config.UpdateStrategy.Type != workv1.UpdateStrategyTypeReadOnly {
			return false
		}
	}
	return true
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
