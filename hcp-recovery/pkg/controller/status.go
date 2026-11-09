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

package controller

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	applyv1 "k8s.io/client-go/applyconfigurations/meta/v1"
	"k8s.io/utils/ptr"

	hcprecoveryv1alpha1 "github.com/Azure/ARO-HCP/hcp-recovery/pkg/apis/hcprecovery/v1alpha1"
	hcprecoveryapply "github.com/Azure/ARO-HCP/hcp-recovery/pkg/generated/applyconfiguration/hcprecovery/v1alpha1"
)

type Status struct {
	applyConfig *hcprecoveryapply.HCPRecoveryStatusApplyConfiguration
}

func NewStatus(status hcprecoveryv1alpha1.HCPRecoveryStatus) *Status {
	return &Status{
		applyConfig: ApplyConfigForStatus(status),
	}
}

func (s *Status) WithConditions(updated ...*applyv1.ConditionApplyConfiguration) *Status {
	// Build a map of existing conditions by type for timestamp preservation
	existingByType := make(map[string]*applyv1.ConditionApplyConfiguration)
	for i := range s.applyConfig.Conditions {
		if s.applyConfig.Conditions[i].Type != nil {
			existingByType[*s.applyConfig.Conditions[i].Type] = &s.applyConfig.Conditions[i]
		}
	}

	updatedTypes := sets.New[string]()
	for _, condition := range updated {
		if condition.Type == nil {
			panic(fmt.Errorf("programmer error: must set a type for condition: %#v", condition))
		}
		updatedTypes.Insert(*condition.Type)

		// If the condition content hasn't changed, preserve the existing timestamp
		if existing, ok := existingByType[*condition.Type]; ok {
			if conditionContentEqual(condition, existing) {
				condition.LastTransitionTime = existing.LastTransitionTime
			}
		}
	}
	conditions := make([]*applyv1.ConditionApplyConfiguration, 0, len(updated)+len(s.applyConfig.Conditions))
	conditions = append(conditions, updated...)
	for _, condition := range s.applyConfig.Conditions {
		if !updatedTypes.Has(*condition.Type) {
			conditions = append(conditions, &condition)
		}
	}
	// Clear existing conditions and set the new merged list
	s.applyConfig.Conditions = nil
	s.applyConfig.WithConditions(conditions...)
	return s
}

// conditionContentEqual returns true if two conditions have the same
// status, reason, message, and observed generation (ignoring timestamps).
func conditionContentEqual(a, b *applyv1.ConditionApplyConfiguration) bool {
	if (a.Status == nil) != (b.Status == nil) || (a.Status != nil && *a.Status != *b.Status) {
		return false
	}
	if (a.Reason == nil) != (b.Reason == nil) || (a.Reason != nil && *a.Reason != *b.Reason) {
		return false
	}
	if (a.Message == nil) != (b.Message == nil) || (a.Message != nil && *a.Message != *b.Message) {
		return false
	}
	if (a.ObservedGeneration == nil) != (b.ObservedGeneration == nil) || (a.ObservedGeneration != nil && *a.ObservedGeneration != *b.ObservedGeneration) {
		return false
	}
	return true
}

func (s *Status) WithPhase(phase hcprecoveryv1alpha1.RestoreState) *Status {
	s.applyConfig.WithPhase(phase)
	return s
}

func (s *Status) WithStartedAt(t metav1.Time) *Status {
	s.applyConfig.WithStartedAt(t)
	return s
}

func (s *Status) WithCompletedAt(t metav1.Time) *Status {
	s.applyConfig.WithCompletedAt(t)
	return s
}

func (s *Status) WithRestoredToTimestamp(t metav1.Time) *Status {
	s.applyConfig.WithRestoredToTimestamp(t)
	return s
}

func (s *Status) WithRestoreName(name string) *Status {
	s.applyConfig.WithRestoreName(name)
	return s
}

func (s *Status) WithEtcdSnapshot(snapshot hcprecoveryv1alpha1.EtcdSnapshotDetails) *Status {
	s.applyConfig.WithEtcdSnapshot(hcprecoveryapply.EtcdSnapshotDetails().
		WithSourceMember(snapshot.SourceMember).
		WithSHA256(snapshot.SHA256).
		WithRevision(snapshot.Revision))
	return s
}

// AsApplyConfiguration returns the apply configuration for the status and a boolean indicating
// if the status needs to be updated. The needsUpdate check is required because the controller
// uses an action-based reconciliation pattern where each sync loop performs at most one mutating
// action. The controller must know whether a status update is necessary before deciding to emit
// it as the action for the current loop iteration, rather than falling through to the next step.
func (s *Status) AsApplyConfiguration(recovery *hcprecoveryv1alpha1.HCPRecovery) (*hcprecoveryapply.HCPRecoveryApplyConfiguration, bool) {
	var needsUpdate bool

	if s.applyConfig.Phase != nil && *s.applyConfig.Phase != recovery.Status.Phase {
		needsUpdate = true
	}

	if s.applyConfig.StartedAt != nil {
		if recovery.Status.StartedAt == nil {
			needsUpdate = true
		} else if !s.applyConfig.StartedAt.Equal(recovery.Status.StartedAt) {
			needsUpdate = true
		}
	}

	if s.applyConfig.CompletedAt != nil {
		if recovery.Status.CompletedAt == nil {
			needsUpdate = true
		} else if !s.applyConfig.CompletedAt.Equal(recovery.Status.CompletedAt) {
			needsUpdate = true
		}
	}

	if s.applyConfig.RestoredToTimestamp != nil {
		if recovery.Status.RestoredToTimestamp == nil {
			needsUpdate = true
		} else if !s.applyConfig.RestoredToTimestamp.Equal(recovery.Status.RestoredToTimestamp) {
			needsUpdate = true
		}
	}

	if (s.applyConfig.RestoreName != nil && *s.applyConfig.RestoreName != recovery.Status.RestoreName) ||
		(s.applyConfig.RestoreName == nil && recovery.Status.RestoreName != "") {
		needsUpdate = true
	}

	if !etcdSnapshotEqual(s.applyConfig.EtcdSnapshot, recovery.Status.EtcdSnapshot) {
		needsUpdate = true
	}

	if !conditionsEqual(s.applyConfig.Conditions, recovery.Status.Conditions) {
		needsUpdate = true
	}

	cfg := hcprecoveryapply.HCPRecovery(recovery.Name, recovery.Namespace).
		WithUID(recovery.UID).
		WithResourceVersion(recovery.ResourceVersion)
	cfg.Status = s.applyConfig
	return cfg, needsUpdate
}

// etcdSnapshotEqual reports whether the apply configuration already describes the
// snapshot recorded in status. A nil apply configuration never clears status, because
// steps that do not touch the snapshot build their configuration from the current status.
func etcdSnapshotEqual(applySnapshot *hcprecoveryapply.EtcdSnapshotDetailsApplyConfiguration, statusSnapshot *hcprecoveryv1alpha1.EtcdSnapshotDetails) bool {
	if applySnapshot == nil {
		return statusSnapshot == nil
	}
	if statusSnapshot == nil {
		return false
	}
	return ptr.Deref(applySnapshot.SourceMember, "") == statusSnapshot.SourceMember &&
		ptr.Deref(applySnapshot.SHA256, "") == statusSnapshot.SHA256 &&
		ptr.Deref(applySnapshot.Revision, 0) == statusSnapshot.Revision
}

func conditionsEqual(applyConditions []applyv1.ConditionApplyConfiguration, statusConditions []metav1.Condition) bool {
	if len(applyConditions) != len(statusConditions) {
		return false
	}

	applyMap := make(map[string]*applyv1.ConditionApplyConfiguration)
	for i := range applyConditions {
		if applyConditions[i].Type != nil {
			applyMap[*applyConditions[i].Type] = &applyConditions[i]
		}
	}

	for _, statusCond := range statusConditions {
		applyCond, exists := applyMap[statusCond.Type]
		if !exists {
			return false
		}
		if applyCond.Status == nil || *applyCond.Status != statusCond.Status {
			return false
		}
		if applyCond.Reason == nil || *applyCond.Reason != statusCond.Reason {
			return false
		}
		if applyCond.Message == nil || *applyCond.Message != statusCond.Message {
			return false
		}
		if applyCond.ObservedGeneration == nil && statusCond.ObservedGeneration != 0 {
			return false
		}
		if applyCond.ObservedGeneration != nil && *applyCond.ObservedGeneration != statusCond.ObservedGeneration {
			return false
		}
	}

	return true
}

func ApplyConfigForStatus(status hcprecoveryv1alpha1.HCPRecoveryStatus) *hcprecoveryapply.HCPRecoveryStatusApplyConfiguration {
	cfg := hcprecoveryapply.HCPRecoveryStatus()

	if status.Phase != "" {
		cfg.WithPhase(status.Phase)
	}
	if status.StartedAt != nil {
		cfg.WithStartedAt(*status.StartedAt)
	}
	if status.CompletedAt != nil {
		cfg.WithCompletedAt(*status.CompletedAt)
	}
	if status.RestoredToTimestamp != nil {
		cfg.WithRestoredToTimestamp(*status.RestoredToTimestamp)
	}
	if status.RestoreName != "" {
		cfg.WithRestoreName(status.RestoreName)
	}
	if details := status.EtcdRuntimeDetails; details != nil {
		applyDetails := hcprecoveryapply.EtcdRuntimeDetails().WithImage(details.Image)
		if details.RunAsUser != nil {
			applyDetails.WithRunAsUser(*details.RunAsUser)
		}
		if details.FSGroup != nil {
			applyDetails.WithFSGroup(*details.FSGroup)
		}
		cfg.WithEtcdRuntimeDetails(applyDetails)
	}
	if snapshot := status.EtcdSnapshot; snapshot != nil {
		cfg.WithEtcdSnapshot(hcprecoveryapply.EtcdSnapshotDetails().
			WithSourceMember(snapshot.SourceMember).
			WithSHA256(snapshot.SHA256).
			WithRevision(snapshot.Revision))
	}

	conditions := make([]*applyv1.ConditionApplyConfiguration, 0, len(status.Conditions))
	for _, c := range status.Conditions {
		conditions = append(conditions, &applyv1.ConditionApplyConfiguration{
			Type:               &c.Type,
			Status:             &c.Status,
			Reason:             &c.Reason,
			Message:            &c.Message,
			ObservedGeneration: &c.ObservedGeneration,
			LastTransitionTime: &c.LastTransitionTime,
		})
	}
	cfg.WithConditions(conditions...)

	return cfg
}
