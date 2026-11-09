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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	applyv1 "k8s.io/client-go/applyconfigurations/meta/v1"

	hcprecoveryv1alpha1 "github.com/Azure/ARO-HCP/hcp-recovery/pkg/apis/hcprecovery/v1alpha1"
)

// Condition builders — each recovery step has a success (True) and failure (False) variant.

func RecoveryInputsValidatedCondition(generation int64, now time.Time) *applyv1.ConditionApplyConfiguration {
	return applyv1.Condition().
		WithType(hcprecoveryv1alpha1.ConditionRecoveryInputsValidated).
		WithStatus(metav1.ConditionTrue).
		WithReason(RecoveryInputsValidatedReason).
		WithMessage("Recovery specification, backup, and target cluster resources are valid and consistent").
		WithObservedGeneration(generation).
		WithLastTransitionTime(metav1.NewTime(now))
}

func RecoveryInputsNotValidatedCondition(reason, message string, generation int64, now time.Time) *applyv1.ConditionApplyConfiguration {
	return applyv1.Condition().
		WithType(hcprecoveryv1alpha1.ConditionRecoveryInputsValidated).
		WithStatus(metav1.ConditionFalse).
		WithReason(reason).
		WithMessage(message).
		WithObservedGeneration(generation).
		WithLastTransitionTime(metav1.NewTime(now))
}

func EtcdRuntimeDetailsCollectedCondition(reason, message string, generation int64, now time.Time) *applyv1.ConditionApplyConfiguration {
	return EtcdRuntimeDetailsNotCollectedCondition(reason, message, generation, now).
		WithStatus(metav1.ConditionTrue)
}

func EtcdRuntimeDetailsNotCollectedCondition(reason, message string, generation int64, now time.Time) *applyv1.ConditionApplyConfiguration {
	return applyv1.Condition().
		WithType(hcprecoveryv1alpha1.EtcdRuntimeDetailsCollectedCondition).
		WithStatus(metav1.ConditionFalse).
		WithReason(reason).
		WithMessage(message).
		WithObservedGeneration(generation).
		WithLastTransitionTime(metav1.NewTime(now))
}

func HostedClusterPausedCondition(generation int64, now time.Time) *applyv1.ConditionApplyConfiguration {
	return applyv1.Condition().
		WithType(hcprecoveryv1alpha1.ConditionHostedClusterPaused).
		WithStatus(metav1.ConditionTrue).
		WithReason("ClusterPaused").
		WithMessage("HostedCluster has been paused").
		WithObservedGeneration(generation).
		WithLastTransitionTime(metav1.NewTime(now))
}

func HostedClusterNotPausedCondition(reason, message string, generation int64, now time.Time) *applyv1.ConditionApplyConfiguration {
	return applyv1.Condition().
		WithType(hcprecoveryv1alpha1.ConditionHostedClusterPaused).
		WithStatus(metav1.ConditionFalse).
		WithReason(reason).
		WithMessage(message).
		WithObservedGeneration(generation).
		WithLastTransitionTime(metav1.NewTime(now))
}

func KubeAPIServerDeploymentLookUpFailedCondition(reason, message string, generation int64, now time.Time) *applyv1.ConditionApplyConfiguration {
	return applyv1.Condition().
		WithType(hcprecoveryv1alpha1.ConditionKubeAPIServerLookupFailed).
		WithStatus(metav1.ConditionFalse).
		WithReason(reason).
		WithMessage(message).
		WithObservedGeneration(generation).
		WithLastTransitionTime(metav1.NewTime(now))
}

func KubeAPIServerScaledDownToZeroReplicasCondition(generation int64, now time.Time) *applyv1.ConditionApplyConfiguration {
	return applyv1.Condition().
		WithType(hcprecoveryv1alpha1.ConditionKubeAPIServerScaledToZero).
		WithStatus(metav1.ConditionTrue).
		WithReason("KubeAPIServerScaledToZeroReplicas").
		WithMessage("The kube-apiserver deployment is scaled to zero replicas").
		WithObservedGeneration(generation).
		WithLastTransitionTime(metav1.NewTime(now))
}

func KubeAPIServerNotScaledToZeroCondition(reason, message string, generation int64, now time.Time) *applyv1.ConditionApplyConfiguration {
	return applyv1.Condition().
		WithType(hcprecoveryv1alpha1.ConditionKubeAPIServerScaledToZero).
		WithStatus(metav1.ConditionFalse).
		WithReason(reason).
		WithMessage(message).
		WithObservedGeneration(generation).
		WithLastTransitionTime(metav1.NewTime(now))
}

func EtcdScaledDownToZeroReplicasCondition(conditionType string, generation int64, now time.Time) *applyv1.ConditionApplyConfiguration {
	return applyv1.Condition().
		WithType(conditionType).
		WithStatus(metav1.ConditionTrue).
		WithReason("EtcdScaledToZeroReplicas").
		WithMessage("The etcd statefulset is scaled to zero replicas").
		WithObservedGeneration(generation).
		WithLastTransitionTime(metav1.NewTime(now))
}

func EtcdNotScaledToZeroCondition(conditionType, reason, message string, generation int64, now time.Time) *applyv1.ConditionApplyConfiguration {
	return applyv1.Condition().
		WithType(conditionType).
		WithStatus(metav1.ConditionFalse).
		WithReason(reason).
		WithMessage(message).
		WithObservedGeneration(generation).
		WithLastTransitionTime(metav1.NewTime(now))
}

func ScaledDownValidatedCondition(generation int64, now time.Time) *applyv1.ConditionApplyConfiguration {
	return ScaledDownNotValidatedCondition(
		"PodsCleanedUp", "HostedCluster is paused, kube-apiserver and etcd replicas are zero, and no kube-apiserver pods, etcd pods, or etcd PVCs remain", generation, now).
		WithStatus(metav1.ConditionTrue)
}

func ScaledDownNotValidatedCondition(reason, message string, generation int64, now time.Time) *applyv1.ConditionApplyConfiguration {
	return applyv1.Condition().
		WithType(hcprecoveryv1alpha1.ConditionScaledDownValidated).
		WithStatus(metav1.ConditionFalse).
		WithReason(reason).
		WithMessage(message).
		WithObservedGeneration(generation).
		WithLastTransitionTime(metav1.NewTime(now))
}

func EtcdPVCsDeletedCondition(generation int64, now time.Time) *applyv1.ConditionApplyConfiguration {
	return EtcdPVCsNotDeletedCondition(
		"EtcdPVCsDeleted", "No etcd PVCs remain in the cache or API server", generation, now).
		WithStatus(metav1.ConditionTrue)
}

func EtcdPVCsNotDeletedCondition(reason, message string, generation int64, now time.Time) *applyv1.ConditionApplyConfiguration {
	return applyv1.Condition().
		WithType(hcprecoveryv1alpha1.ConditionEtcdPVCsDeleted).
		WithStatus(metav1.ConditionFalse).
		WithReason(reason).
		WithMessage(message).
		WithObservedGeneration(generation).
		WithLastTransitionTime(metav1.NewTime(now))
}

func EtcdPodsGoneCondition(generation int64, now time.Time) *applyv1.ConditionApplyConfiguration {
	return EtcdPodsNotGoneCondition(
		"EtcdPodsGone", "No etcd pods remain, so the restored etcd volumes can be mounted", generation, now).
		WithStatus(metav1.ConditionTrue)
}

func EtcdPodsNotGoneCondition(reason, message string, generation int64, now time.Time) *applyv1.ConditionApplyConfiguration {
	return applyv1.Condition().
		WithType(hcprecoveryv1alpha1.ConditionEtcdPodsGone).
		WithStatus(metav1.ConditionFalse).
		WithReason(reason).
		WithMessage(message).
		WithObservedGeneration(generation).
		WithLastTransitionTime(metav1.NewTime(now))
}

func EtcdHolderPodsReadyCondition(generation int64, now time.Time) *applyv1.ConditionApplyConfiguration {
	return EtcdHolderPodsNotReadyCondition(
		"EtcdHolderPodsReady", "A holder pod is running for every etcd volume", generation, now).
		WithStatus(metav1.ConditionTrue)
}

func EtcdHolderPodsNotReadyCondition(reason, message string, generation int64, now time.Time) *applyv1.ConditionApplyConfiguration {
	return applyv1.Condition().
		WithType(hcprecoveryv1alpha1.ConditionEtcdHolderPodsReady).
		WithStatus(metav1.ConditionFalse).
		WithReason(reason).
		WithMessage(message).
		WithObservedGeneration(generation).
		WithLastTransitionTime(metav1.NewTime(now))
}

func EtcdSnapshotDistributedCondition(message string, generation int64, now time.Time) *applyv1.ConditionApplyConfiguration {
	return EtcdSnapshotNotDistributedCondition("EtcdSnapshotDistributed", message, generation, now).
		WithStatus(metav1.ConditionTrue)
}

func EtcdSnapshotNotDistributedCondition(reason, message string, generation int64, now time.Time) *applyv1.ConditionApplyConfiguration {
	return applyv1.Condition().
		WithType(hcprecoveryv1alpha1.ConditionEtcdSnapshotDistributed).
		WithStatus(metav1.ConditionFalse).
		WithReason(reason).
		WithMessage(message).
		WithObservedGeneration(generation).
		WithLastTransitionTime(metav1.NewTime(now))
}

func EtcdSnapshotRestoredCondition(message string, generation int64, now time.Time) *applyv1.ConditionApplyConfiguration {
	return EtcdSnapshotNotRestoredCondition("EtcdSnapshotRestored", message, generation, now).
		WithStatus(metav1.ConditionTrue)
}

func EtcdSnapshotNotRestoredCondition(reason, message string, generation int64, now time.Time) *applyv1.ConditionApplyConfiguration {
	return applyv1.Condition().
		WithType(hcprecoveryv1alpha1.ConditionEtcdSnapshotRestored).
		WithStatus(metav1.ConditionFalse).
		WithReason(reason).
		WithMessage(message).
		WithObservedGeneration(generation).
		WithLastTransitionTime(metav1.NewTime(now))
}

func EtcdScaledUpCondition(message string, generation int64, now time.Time) *applyv1.ConditionApplyConfiguration {
	return EtcdNotScaledUpCondition("EtcdScaledUp", message, generation, now).
		WithStatus(metav1.ConditionTrue)
}

func EtcdNotScaledUpCondition(reason, message string, generation int64, now time.Time) *applyv1.ConditionApplyConfiguration {
	return applyv1.Condition().
		WithType(hcprecoveryv1alpha1.ConditionEtcdScaledUp).
		WithStatus(metav1.ConditionFalse).
		WithReason(reason).
		WithMessage(message).
		WithObservedGeneration(generation).
		WithLastTransitionTime(metav1.NewTime(now))
}

func EtcdHealthyCondition(message string, generation int64, now time.Time) *applyv1.ConditionApplyConfiguration {
	return EtcdNotHealthyCondition("EtcdHealthy", message, generation, now).
		WithStatus(metav1.ConditionTrue)
}

func EtcdNotHealthyCondition(reason, message string, generation int64, now time.Time) *applyv1.ConditionApplyConfiguration {
	return applyv1.Condition().
		WithType(hcprecoveryv1alpha1.ConditionEtcdHealthy).
		WithStatus(metav1.ConditionFalse).
		WithReason(reason).
		WithMessage(message).
		WithObservedGeneration(generation).
		WithLastTransitionTime(metav1.NewTime(now))
}

func VeleroRestoreCreatedCondition(generation int64, now time.Time) *applyv1.ConditionApplyConfiguration {
	return VeleroRestoreNotCreatedCondition(
		VeleroRestoreCreatedReason, "Velero restore has been created", generation, now).
		WithStatus(metav1.ConditionTrue)
}

func VeleroRestoreNotCreatedCondition(reason, message string, generation int64, now time.Time) *applyv1.ConditionApplyConfiguration {
	return applyv1.Condition().
		WithType(hcprecoveryv1alpha1.ConditionVeleroRestoreCreated).
		WithStatus(metav1.ConditionFalse).
		WithReason(reason).
		WithMessage(message).
		WithObservedGeneration(generation).
		WithLastTransitionTime(metav1.NewTime(now))
}

func VeleroRestoreCompletedCondition(generation int64, now time.Time) *applyv1.ConditionApplyConfiguration {
	return applyv1.Condition().
		WithType(hcprecoveryv1alpha1.ConditionVeleroRestoreCompleted).
		WithStatus(metav1.ConditionTrue).
		WithReason("RestoreCompleted").
		WithMessage("Velero restore operation has completed successfully").
		WithObservedGeneration(generation).
		WithLastTransitionTime(metav1.NewTime(now))
}

func VeleroRestoreNotCompletedCondition(reason, message string, generation int64, now time.Time) *applyv1.ConditionApplyConfiguration {
	return applyv1.Condition().
		WithType(hcprecoveryv1alpha1.ConditionVeleroRestoreCompleted).
		WithStatus(metav1.ConditionFalse).
		WithReason(reason).
		WithMessage(message).
		WithObservedGeneration(generation).
		WithLastTransitionTime(metav1.NewTime(now))
}

func HostedClusterUnpausedCondition(generation int64, now time.Time) *applyv1.ConditionApplyConfiguration {
	return applyv1.Condition().
		WithType(hcprecoveryv1alpha1.ConditionHostedClusterUnpaused).
		WithStatus(metav1.ConditionTrue).
		WithReason("ClusterUnpaused").
		WithMessage("HostedCluster has been unpaused after successful restore").
		WithObservedGeneration(generation).
		WithLastTransitionTime(metav1.NewTime(now))
}

func HostedClusterNotUnpausedCondition(reason, message string, generation int64, now time.Time) *applyv1.ConditionApplyConfiguration {
	return applyv1.Condition().
		WithType(hcprecoveryv1alpha1.ConditionHostedClusterUnpaused).
		WithStatus(metav1.ConditionFalse).
		WithReason(reason).
		WithMessage(message).
		WithObservedGeneration(generation).
		WithLastTransitionTime(metav1.NewTime(now))
}

func HealthCheckedCondition(generation int64, now time.Time) *applyv1.ConditionApplyConfiguration {
	return applyv1.Condition().
		WithType(hcprecoveryv1alpha1.ConditionHealthChecked).
		WithStatus(metav1.ConditionTrue).
		WithReason("HealthCheckPassed").
		WithMessage("Post-restore health checks have passed").
		WithObservedGeneration(generation).
		WithLastTransitionTime(metav1.NewTime(now))
}

func HealthCheckFailedCondition(reason, message string, generation int64, now time.Time) *applyv1.ConditionApplyConfiguration {
	return applyv1.Condition().
		WithType(hcprecoveryv1alpha1.ConditionHealthChecked).
		WithStatus(metav1.ConditionFalse).
		WithReason(reason).
		WithMessage(message).
		WithObservedGeneration(generation).
		WithLastTransitionTime(metav1.NewTime(now))
}
