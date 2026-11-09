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
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"

	hcprecoveryv1alpha1 "github.com/Azure/ARO-HCP/hcp-recovery/pkg/apis/hcprecovery/v1alpha1"
	"github.com/Azure/ARO-HCP/internal/backup"
)

const (
	VeleroRestoreCreatedReason          = "RestoreCreated"
	VeleroRestoreLookupFailedReason     = "RestoreLookupFailed"
	VeleroRestoreConversionFailedReason = "RestoreConversionFailed"
	VeleroRestoreCreateFailedReason     = "RestoreCreateFailed"
	VeleroRestoreFailedReason           = "RestoreFailed"
)

func (c *RecoveryController) createVeleroRestoreStep(ctx context.Context, recovery *hcprecoveryv1alpha1.HCPRecovery) (stepResult, error) {
	if recovery.Status.RestoreName == "" {
		restoreName := strings.Join([]string{recovery.Name, "restore"}, "-")
		statusUpdate, needsUpdate := NewStatus(recovery.Status).
			WithRestoreName(restoreName).
			AsApplyConfiguration(recovery)
		if needsUpdate {
			return satisfied(statusUpdate, needsUpdate), nil
		}
	}

	cachedRestore, err := c.restoreLister.ByNamespace(backup.VeleroNamespace).Get(recovery.Status.RestoreName)
	if err != nil && !apierrors.IsNotFound(err) {
		err = fmt.Errorf("failed to get restore %s/%s: %w", backup.VeleroNamespace, recovery.Status.RestoreName, err)
		return c.retryableError(recovery, VeleroRestoreNotCreatedCondition(
			VeleroRestoreLookupFailedReason, err.Error(), recovery.Generation, c.TimeNow()), err), nil
	}
	if err == nil && cachedRestore != nil {
		statusUpdate, needsUpdate := NewStatus(recovery.Status).
			WithConditions(VeleroRestoreCreatedCondition(recovery.Generation, c.TimeNow())).
			AsApplyConfiguration(recovery)
		return satisfied(statusUpdate, needsUpdate), nil
	}

	restore := &velerov1.Restore{
		TypeMeta: metav1.TypeMeta{APIVersion: velerov1.SchemeGroupVersion.String(), Kind: "Restore"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      recovery.Status.RestoreName,
			Namespace: backup.VeleroNamespace,
			Labels: map[string]string{
				restoreControlPlaneNamespaceLabel: recovery.Spec.ControlPlaneNamespace,
			},
		},
		Spec: velerov1.RestoreSpec{
			BackupName:             recovery.Spec.VeleroBackupName,
			RestorePVs:             new(true),
			ExistingResourcePolicy: velerov1.PolicyTypeUpdate,
			IncludedResources: []string{
				"pvc",
				"pv",
				"statefulsets",
			},
			ItemOperationTimeout: metav1.Duration{Duration: 15 * time.Minute},
		},
	}
	data, err := runtime.DefaultUnstructuredConverter.ToUnstructured(restore)
	if err != nil {
		err = fmt.Errorf("failed to convert restore %s/%s to unstructured: %w", backup.VeleroNamespace, recovery.Status.RestoreName, err)
		return c.permanentError(recovery, VeleroRestoreNotCreatedCondition(
			VeleroRestoreConversionFailedReason, err.Error(), recovery.Generation, c.TimeNow())), nil
	}
	obj := &unstructured.Unstructured{
		Object: data,
	}

	_, err = c.restoreClient.Create(ctx, obj, metav1.CreateOptions{})
	if err != nil {
		err = fmt.Errorf("failed to create restore %s/%s: %w", backup.VeleroNamespace, recovery.Status.RestoreName, err)
		return c.retryableError(recovery, VeleroRestoreNotCreatedCondition(
			VeleroRestoreCreateFailedReason, err.Error(), recovery.Generation, c.TimeNow()), err), nil
	}
	// The Restore event triggers a status update once creation is observed in the cache.
	return yield(), nil
}

func (c *RecoveryController) waitForVeleroRestoreCompletion(ctx context.Context, recovery *hcprecoveryv1alpha1.HCPRecovery) (stepResult, error) {
	obj, err := c.restoreLister.ByNamespace(backup.VeleroNamespace).Get(recovery.Status.RestoreName)
	if apierrors.IsNotFound(err) {
		return yield(), nil
	}
	if err != nil {
		err = fmt.Errorf("failed to get restore %s/%s: %w", backup.VeleroNamespace, recovery.Status.RestoreName, err)
		return c.retryableError(recovery, VeleroRestoreNotCompletedCondition(
			VeleroRestoreLookupFailedReason, err.Error(), recovery.Generation, c.TimeNow()), err), nil
	}

	restoreObject, ok := obj.(*unstructured.Unstructured)
	if !ok || restoreObject == nil {
		return c.permanentError(recovery, VeleroRestoreNotCompletedCondition(
			VeleroRestoreConversionFailedReason, fmt.Sprintf("expected an unstructured Restore, got %T", obj), recovery.Generation, c.TimeNow())), nil
	}
	var restore velerov1.Restore
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(restoreObject.Object, &restore); err != nil {
		return c.permanentError(recovery, VeleroRestoreNotCompletedCondition(
			VeleroRestoreConversionFailedReason,
			fmt.Sprintf("failed to convert restore %s/%s: %v", backup.VeleroNamespace, recovery.Status.RestoreName, err),
			recovery.Generation, c.TimeNow())), nil
	}
	if !isRestoreTerminal(restore) {
		return yield(), nil
	}

	if restore.Status.Phase != velerov1.RestorePhaseCompleted {
		return c.permanentError(recovery, VeleroRestoreNotCompletedCondition(
			VeleroRestoreFailedReason,
			fmt.Sprintf("Velero restore %s/%s ended in phase %s: %d errors, validation errors: %v",
				backup.VeleroNamespace, recovery.Status.RestoreName, restore.Status.Phase, restore.Status.Errors, restore.Status.ValidationErrors),
			recovery.Generation, c.TimeNow())), nil
	}
	statusUpdate, needsUpdate := NewStatus(recovery.Status).
		WithConditions(VeleroRestoreCompletedCondition(recovery.Generation, c.TimeNow())).
		AsApplyConfiguration(recovery)
	return satisfied(statusUpdate, needsUpdate), nil
}

func isRestoreTerminal(restore velerov1.Restore) bool {
	switch restore.Status.Phase {
	case velerov1.RestorePhaseCompleted, velerov1.RestorePhaseFailed, velerov1.RestorePhaseFailedValidation, velerov1.RestorePhasePartiallyFailed:
		return true
	default:
		return false
	}
}

const (
	EtcdPodsCachedListFailedReason = "EtcdPodsCachedListFailed"
	EtcdPodsLiveListFailedReason   = "EtcdPodsLiveListFailed"
	EtcdPodsRemainingReason        = "EtcdPodsRemaining"
)

// scaleDownEtcdAfterRestore reruns the shared scale-down defined in preparing.go, under
// its own condition so the second pass does not overwrite the first one's record.
func (c *RecoveryController) scaleDownEtcdAfterRestore(ctx context.Context, recovery *hcprecoveryv1alpha1.HCPRecovery) (stepResult, error) {
	return c.scaleDownEtcd(ctx, recovery, hcprecoveryv1alpha1.ConditionEtcdScaledToZeroAfterVeleroRestore)
}

// validateEtcdPodsGone confirms the second scale-down actually released the etcd
// volumes. The Velero restore recreates the etcd StatefulSet at its original replica
// count, so etcd pods come back and remount the PVCs; the holder pods created in the
// next step mount those same ReadWriteOnce volumes and would sit unschedulable
// forever if an etcd pod still held one.
func (c *RecoveryController) validateEtcdPodsGone(ctx context.Context, recovery *hcprecoveryv1alpha1.HCPRecovery) (stepResult, error) {
	selector := labels.SelectorFromSet(labels.Set{"app": etcd})

	pods, err := c.podLister.Pods(recovery.Spec.ControlPlaneNamespace).List(selector)
	if err != nil {
		err = fmt.Errorf("failed to list etcd pods from cache in namespace %q: %w", recovery.Spec.ControlPlaneNamespace, err)
		return c.retryableError(recovery, EtcdPodsNotGoneCondition(
			EtcdPodsCachedListFailedReason, err.Error(), recovery.Generation, c.TimeNow()), err), nil
	}
	if len(pods) > 0 {
		if err := c.reapTerminalPod(ctx, pods); err != nil {
			return c.retryableError(recovery, EtcdPodsNotGoneCondition(
				TerminalPodDeleteFailedReason, err.Error(), recovery.Generation, c.TimeNow()), err), nil
		}
		err = fmt.Errorf("cache check shows %d etcd pods still remaining after the post-restore scale-down: %s", len(pods), blockingPods(pods))
		return c.retryableError(recovery, EtcdPodsNotGoneCondition(
			EtcdPodsRemainingReason, err.Error(), recovery.Generation, c.TimeNow()), err), nil
	}

	// A stale cache would let the holder pods race the etcd pods for the volumes, so
	// confirm with the API server before creating the holder pods.
	livePods, err := c.kubeClientset.CoreV1().Pods(recovery.Spec.ControlPlaneNamespace).
		List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		err = fmt.Errorf("failed to list etcd pods from API server in namespace %q: %w", recovery.Spec.ControlPlaneNamespace, err)
		return c.retryableError(recovery, EtcdPodsNotGoneCondition(
			EtcdPodsLiveListFailedReason, err.Error(), recovery.Generation, c.TimeNow()), err), nil
	}
	if len(livePods.Items) > 0 {
		for i := range livePods.Items {
			pods = append(pods, &livePods.Items[i])
		}
		if err := c.reapTerminalPod(ctx, pods); err != nil {
			return c.retryableError(recovery, EtcdPodsNotGoneCondition(
				TerminalPodDeleteFailedReason, err.Error(), recovery.Generation, c.TimeNow()), err), nil
		}
		err = fmt.Errorf("live check shows %d etcd pods still remaining after the post-restore scale-down: %s", len(pods), blockingPods(pods))
		return c.retryableError(recovery, EtcdPodsNotGoneCondition(
			EtcdPodsRemainingReason, err.Error(), recovery.Generation, c.TimeNow()), err), nil
	}

	statusUpdate, needsUpdate := NewStatus(recovery.Status).
		WithConditions(EtcdPodsGoneCondition(recovery.Generation, c.TimeNow())).
		AsApplyConfiguration(recovery)
	return satisfied(statusUpdate, needsUpdate), nil
}

const (
	EtcdHolderPodCreateFailedReason    = "EtcdHolderPodCreateFailed"
	EtcdHolderPodLookupFailedReason    = "EtcdHolderPodLookupFailed"
	EtcdHolderPodTerminatedReason      = "EtcdHolderPodTerminated"
	EtcdHolderPodsStartingReason       = "EtcdHolderPodsStarting"
	SnapshotTransferCreateFailedReason = "SnapshotTransferCreateFailed"
	SnapshotTransferLookupFailedReason = "SnapshotTransferLookupFailed"
	SnapshotTransferRunningReason      = "SnapshotTransferRunning"
	SnapshotTransferFailedReason       = "SnapshotTransferFailed"
	SnapshotResultInvalidReason        = "SnapshotResultInvalid"
	DistributionPodsDeleteFailedReason = "DistributionPodsDeleteFailed"
	DistributionPodsRemainingReason    = "DistributionPodsRemaining"
)

// createEtcdHolderPods puts one pod on each etcd volume. The volumes sit in different
// availability zones, so nothing can mount more than one of them; a pod per volume is
// what makes all of them reachable at the same time.
func (c *RecoveryController) createEtcdHolderPods(ctx context.Context, recovery *hcprecoveryv1alpha1.HCPRecovery) (stepResult, error) {
	// The holders exist only to make the volumes reachable for the copy. Once the copy
	// is recorded they are torn down later in this same phase, so recreating them here
	// would fight that teardown forever.
	if recovery.Status.EtcdSnapshot != nil {
		return proceed(), nil
	}
	etcdStatefulSet, err := c.statefulsetLister.StatefulSets(recovery.Spec.ControlPlaneNamespace).Get(etcd)
	if err != nil {
		err = fmt.Errorf("failed to get etcd statefulset in namespace %q: %w", recovery.Spec.ControlPlaneNamespace, err)
		return c.retryableError(recovery, EtcdHolderPodsNotReadyCondition(
			EtcdStatefulSetLookupFailedReason, err.Error(), recovery.Generation, c.TimeNow()), err), nil
	}

	for index := int32(0); index < etcdMembers; index++ {
		name := etcdHolderPodName(index)
		_, err := c.podLister.Pods(recovery.Spec.ControlPlaneNamespace).Get(name)
		if err == nil {
			continue
		}
		if !apierrors.IsNotFound(err) {
			err = fmt.Errorf("failed to get holder pod %s/%s: %w", recovery.Spec.ControlPlaneNamespace, name, err)
			return c.retryableError(recovery, EtcdHolderPodsNotReadyCondition(
				EtcdHolderPodLookupFailedReason, err.Error(), recovery.Generation, c.TimeNow()), err), nil
		}
		_, err = c.kubeClientset.CoreV1().Pods(recovery.Spec.ControlPlaneNamespace).
			Create(ctx, etcdHolderPod(recovery, etcdStatefulSet, index), metav1.CreateOptions{FieldManager: fieldManager})
		// AlreadyExists means a previous attempt created it before the cache caught up.
		if err != nil && !apierrors.IsAlreadyExists(err) {
			err = fmt.Errorf("failed to create holder pod %s/%s: %w", recovery.Spec.ControlPlaneNamespace, name, err)
			return c.retryableError(recovery, EtcdHolderPodsNotReadyCondition(
				EtcdHolderPodCreateFailedReason, err.Error(), recovery.Generation, c.TimeNow()), err), nil
		}
		return yield(), nil
	}
	return proceed(), nil
}

// waitForHolderPodsReady blocks the phase until every volume is reachable. A holder pod
// that stopped — drained or evicted while it slept — is deleted so the create step
// replaces it, rather than failing a restore that is still perfectly recoverable.
func (c *RecoveryController) waitForHolderPodsReady(ctx context.Context, recovery *hcprecoveryv1alpha1.HCPRecovery) (stepResult, error) {
	if recovery.Status.EtcdSnapshot != nil {
		return proceed(), nil
	}
	notReady := 0
	for index := int32(0); index < etcdMembers; index++ {
		name := etcdHolderPodName(index)
		pod, err := c.podLister.Pods(recovery.Spec.ControlPlaneNamespace).Get(name)
		if err != nil {
			err = fmt.Errorf("failed to get holder pod %s/%s: %w", recovery.Spec.ControlPlaneNamespace, name, err)
			return c.retryableError(recovery, EtcdHolderPodsNotReadyCondition(
				EtcdHolderPodLookupFailedReason, err.Error(), recovery.Generation, c.TimeNow()), err), nil
		}
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			if pod.DeletionTimestamp != nil {
				return yield(), nil
			}
			phase, message := podResult(pod)
			klog.FromContext(ctx).Info("Recreating stopped etcd holder pod",
				"pod", name, "phase", phase, "message", message)
			err := c.kubeClientset.CoreV1().Pods(recovery.Spec.ControlPlaneNamespace).
				Delete(ctx, name, metav1.DeleteOptions{})
			if err != nil && !apierrors.IsNotFound(err) {
				err = fmt.Errorf("failed to delete stopped holder pod %s/%s: %w", recovery.Spec.ControlPlaneNamespace, name, err)
				return c.retryableError(recovery, EtcdHolderPodsNotReadyCondition(
					EtcdHolderPodTerminatedReason, err.Error(), recovery.Generation, c.TimeNow()), err), nil
			}
			return yield(), nil
		}
		if !podReady(pod) {
			notReady++
		}
	}
	if notReady > 0 {
		// Scheduling and volume attachment produce pod events, so there is nothing to
		// poll for — the informer brings us back when they are ready.
		statusUpdate, needsUpdate := NewStatus(recovery.Status).
			WithConditions(EtcdHolderPodsNotReadyCondition(EtcdHolderPodsStartingReason,
				fmt.Sprintf("Waiting for %d of %d etcd holder pods to become ready", notReady, etcdMembers),
				recovery.Generation, c.TimeNow())).
			AsApplyConfiguration(recovery)
		return waiting(statusUpdate, needsUpdate), nil
	}

	statusUpdate, needsUpdate := NewStatus(recovery.Status).
		WithConditions(EtcdHolderPodsReadyCondition(recovery.Generation, c.TimeNow())).
		AsApplyConfiguration(recovery)
	return satisfied(statusUpdate, needsUpdate), nil
}

// createSnapshotTransferPod starts the pod that copies etcd-0's snapshot onto every
// other volume. The work runs in a pod rather than in the controller so a multi-minute
// copy of a multi-hundred-megabyte snapshot never occupies a controller worker.
func (c *RecoveryController) createSnapshotTransferPod(ctx context.Context, recovery *hcprecoveryv1alpha1.HCPRecovery) (stepResult, error) {
	if recovery.Status.EtcdSnapshot != nil {
		return proceed(), nil
	}
	name := snapshotTransferPodName(recovery)
	_, err := c.podLister.Pods(recovery.Namespace).Get(name)
	if err == nil {
		return proceed(), nil
	}
	if !apierrors.IsNotFound(err) {
		err = fmt.Errorf("failed to get snapshot transfer pod %s/%s: %w", recovery.Namespace, name, err)
		return c.retryableError(recovery, EtcdSnapshotNotDistributedCondition(
			SnapshotTransferLookupFailedReason, err.Error(), recovery.Generation, c.TimeNow()), err), nil
	}

	pod := snapshotTransferPod(recovery, c.workerImage, c.workerServiceAccount)
	_, err = c.kubeClientset.CoreV1().Pods(recovery.Namespace).Create(ctx, pod, metav1.CreateOptions{FieldManager: fieldManager})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		err = fmt.Errorf("failed to create snapshot transfer pod %s/%s: %w", recovery.Namespace, name, err)
		return c.retryableError(recovery, EtcdSnapshotNotDistributedCondition(
			SnapshotTransferCreateFailedReason, err.Error(), recovery.Generation, c.TimeNow()), err), nil
	}
	return yield(), nil
}

// waitForSnapshotTransfer records which snapshot every volume now holds. The transfer
// pod retries internally and refuses to exit 0 unless all the copies match, so a pod
// that failed has exhausted its own retries and the restore cannot safely continue.
func (c *RecoveryController) waitForSnapshotTransfer(ctx context.Context, recovery *hcprecoveryv1alpha1.HCPRecovery) (stepResult, error) {
	if recovery.Status.EtcdSnapshot != nil {
		statusUpdate, needsUpdate := NewStatus(recovery.Status).
			WithConditions(EtcdSnapshotDistributedCondition(
				distributedMessage(*recovery.Status.EtcdSnapshot), recovery.Generation, c.TimeNow())).
			AsApplyConfiguration(recovery)
		return satisfied(statusUpdate, needsUpdate), nil
	}

	name := snapshotTransferPodName(recovery)
	pod, err := c.podLister.Pods(recovery.Namespace).Get(name)
	if err != nil {
		err = fmt.Errorf("failed to get snapshot transfer pod %s/%s: %w", recovery.Namespace, name, err)
		return c.retryableError(recovery, EtcdSnapshotNotDistributedCondition(
			SnapshotTransferLookupFailedReason, err.Error(), recovery.Generation, c.TimeNow()), err), nil
	}

	phase, message := podResult(pod)
	switch phase {
	case corev1.PodSucceeded:
	case corev1.PodFailed:
		return c.permanentError(recovery, EtcdSnapshotNotDistributedCondition(
			SnapshotTransferFailedReason,
			fmt.Sprintf("Snapshot transfer pod %s/%s failed: %s", recovery.Namespace, name, message),
			recovery.Generation, c.TimeNow())), nil
	default:
		statusUpdate, needsUpdate := NewStatus(recovery.Status).
			WithConditions(EtcdSnapshotNotDistributedCondition(SnapshotTransferRunningReason,
				fmt.Sprintf("Snapshot transfer pod %s/%s is %s", recovery.Namespace, name, phase),
				recovery.Generation, c.TimeNow())).
			AsApplyConfiguration(recovery)
		return waiting(statusUpdate, needsUpdate), nil
	}

	var snapshot hcprecoveryv1alpha1.EtcdSnapshotDetails
	if err := json.Unmarshal([]byte(message), &snapshot); err != nil {
		return c.permanentError(recovery, EtcdSnapshotNotDistributedCondition(
			SnapshotResultInvalidReason,
			fmt.Sprintf("Snapshot transfer pod %s/%s reported %q, which is not a snapshot result: %v",
				recovery.Namespace, name, message, err),
			recovery.Generation, c.TimeNow())), nil
	}
	if snapshot.SourceMember == "" || snapshot.SHA256 == "" {
		return c.permanentError(recovery, EtcdSnapshotNotDistributedCondition(
			SnapshotResultInvalidReason,
			fmt.Sprintf("Snapshot transfer pod %s/%s reported an incomplete snapshot result: %q",
				recovery.Namespace, name, message),
			recovery.Generation, c.TimeNow())), nil
	}

	statusUpdate, needsUpdate := NewStatus(recovery.Status).
		WithEtcdSnapshot(snapshot).
		WithConditions(EtcdSnapshotDistributedCondition(distributedMessage(snapshot), recovery.Generation, c.TimeNow())).
		AsApplyConfiguration(recovery)
	return satisfied(statusUpdate, needsUpdate), nil
}

// deleteDistributionPods releases the volumes before the restore pods claim them. Both
// sets mount the same ReadWriteOnce PVCs, so a leftover holder pod would leave a
// restore pod unschedulable.
func (c *RecoveryController) deleteDistributionPods(ctx context.Context, recovery *hcprecoveryv1alpha1.HCPRecovery) (stepResult, error) {
	selector, err := workerPodSelector(recovery, etcdHolderApp)
	if err != nil {
		return c.permanentError(recovery, EtcdSnapshotNotDistributedCondition(
			ScaledDownInvalidPodSelectorReason, err.Error(), recovery.Generation, c.TimeNow())), nil
	}
	acted, remaining, err := c.deleteWorkerPods(ctx, recovery.Spec.ControlPlaneNamespace, selector)
	if err != nil {
		return c.retryableError(recovery, EtcdSnapshotNotDistributedCondition(
			DistributionPodsDeleteFailedReason, err.Error(), recovery.Generation, c.TimeNow()), err), nil
	}
	if acted {
		return yield(), nil
	}
	if remaining {
		err = fmt.Errorf("waiting for etcd holder pods in namespace %q to be deleted", recovery.Spec.ControlPlaneNamespace)
		return c.retryableError(recovery, EtcdSnapshotNotDistributedCondition(
			DistributionPodsRemainingReason, err.Error(), recovery.Generation, c.TimeNow()), err), nil
	}

	// The transfer pod holds no etcd volume, so member restore need not wait for its
	// deletion. Its owner reference cleans it up if this never runs.
	transferSelector, err := workerPodSelector(recovery, snapshotTransferApp)
	if err != nil {
		return c.permanentError(recovery, EtcdSnapshotNotDistributedCondition(
			ScaledDownInvalidPodSelectorReason, err.Error(), recovery.Generation, c.TimeNow())), nil
	}
	if acted, _, err := c.deleteWorkerPods(ctx, recovery.Namespace, transferSelector); err != nil {
		return c.retryableError(recovery, EtcdSnapshotNotDistributedCondition(
			DistributionPodsDeleteFailedReason, err.Error(), recovery.Generation, c.TimeNow()), err), nil
	} else if acted {
		return yield(), nil
	}

	return proceed(), nil
}

func distributedMessage(snapshot hcprecoveryv1alpha1.EtcdSnapshotDetails) string {
	return fmt.Sprintf("Every etcd volume holds the snapshot from %s at revision %d (sha256 %s)",
		snapshot.SourceMember, snapshot.Revision, snapshot.SHA256)
}

const (
	EtcdRestorePodCreateFailedReason  = "EtcdRestorePodCreateFailed"
	EtcdRestorePodLookupFailedReason  = "EtcdRestorePodLookupFailed"
	EtcdRestorePodRunningReason       = "EtcdRestorePodRunning"
	EtcdRestorePodFailedReason        = "EtcdRestorePodFailed"
	EtcdRestorePodsDeleteFailedReason = "EtcdRestorePodsDeleteFailed"
	EtcdRestorePodsRemainingReason    = "EtcdRestorePodsRemaining"
)

// createEtcdRestorePods rebuilds every member's data directory from the snapshot that
// distribution placed on its volume. Each pod runs etcdutl against the volume it mounts,
// which is the only way to reach a volume that is bound to one availability zone.
func (c *RecoveryController) createEtcdRestorePods(ctx context.Context, recovery *hcprecoveryv1alpha1.HCPRecovery) (stepResult, error) {
	// These pods are reaped later in this same phase to free the volumes for etcd, so
	// once every member is rebuilt they must not be recreated.
	if meta.IsStatusConditionTrue(recovery.Status.Conditions, hcprecoveryv1alpha1.ConditionEtcdSnapshotRestored) {
		return proceed(), nil
	}
	etcdStatefulSet, err := c.statefulsetLister.StatefulSets(recovery.Spec.ControlPlaneNamespace).Get(etcd)
	if err != nil {
		err = fmt.Errorf("failed to get etcd statefulset in namespace %q: %w", recovery.Spec.ControlPlaneNamespace, err)
		return c.retryableError(recovery, EtcdSnapshotNotRestoredCondition(
			EtcdStatefulSetLookupFailedReason, err.Error(), recovery.Generation, c.TimeNow()), err), nil
	}

	for index := int32(0); index < etcdMembers; index++ {
		name := etcdRestorePodName(index)
		_, err := c.podLister.Pods(recovery.Spec.ControlPlaneNamespace).Get(name)
		if err == nil {
			continue
		}
		if !apierrors.IsNotFound(err) {
			err = fmt.Errorf("failed to get etcd restore pod %s/%s: %w", recovery.Spec.ControlPlaneNamespace, name, err)
			return c.retryableError(recovery, EtcdSnapshotNotRestoredCondition(
				EtcdRestorePodLookupFailedReason, err.Error(), recovery.Generation, c.TimeNow()), err), nil
		}
		_, err = c.kubeClientset.CoreV1().Pods(recovery.Spec.ControlPlaneNamespace).
			Create(ctx, etcdRestorePod(recovery, etcdStatefulSet, index), metav1.CreateOptions{FieldManager: fieldManager})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			err = fmt.Errorf("failed to create etcd restore pod %s/%s: %w", recovery.Spec.ControlPlaneNamespace, name, err)
			return c.retryableError(recovery, EtcdSnapshotNotRestoredCondition(
				EtcdRestorePodCreateFailedReason, err.Error(), recovery.Generation, c.TimeNow()), err), nil
		}
		return yield(), nil
	}
	return proceed(), nil
}

// waitForEtcdRestorePods holds the phase until every member has been rebuilt. A restore
// pod that failed is terminal: it already skips volumes it has rebuilt, so a failure
// means etcdutl itself rejected the snapshot and starting etcd would be unsafe.
func (c *RecoveryController) waitForEtcdRestorePods(ctx context.Context, recovery *hcprecoveryv1alpha1.HCPRecovery) (stepResult, error) {
	if meta.IsStatusConditionTrue(recovery.Status.Conditions, hcprecoveryv1alpha1.ConditionEtcdSnapshotRestored) {
		return proceed(), nil
	}
	pending := 0
	for index := int32(0); index < etcdMembers; index++ {
		name := etcdRestorePodName(index)
		pod, err := c.podLister.Pods(recovery.Spec.ControlPlaneNamespace).Get(name)
		if err != nil {
			err = fmt.Errorf("failed to get etcd restore pod %s/%s: %w", recovery.Spec.ControlPlaneNamespace, name, err)
			return c.retryableError(recovery, EtcdSnapshotNotRestoredCondition(
				EtcdRestorePodLookupFailedReason, err.Error(), recovery.Generation, c.TimeNow()), err), nil
		}
		phase, message := podResult(pod)
		switch phase {
		case corev1.PodSucceeded:
		case corev1.PodFailed:
			return c.permanentError(recovery, EtcdSnapshotNotRestoredCondition(
				EtcdRestorePodFailedReason,
				fmt.Sprintf("Restore pod %s/%s failed for %s: %s",
					recovery.Spec.ControlPlaneNamespace, name, etcdMemberName(index), message),
				recovery.Generation, c.TimeNow())), nil
		default:
			pending++
		}
	}
	if pending > 0 {
		statusUpdate, needsUpdate := NewStatus(recovery.Status).
			WithConditions(EtcdSnapshotNotRestoredCondition(EtcdRestorePodRunningReason,
				fmt.Sprintf("Waiting for %d of %d etcd members to be restored", pending, etcdMembers),
				recovery.Generation, c.TimeNow())).
			AsApplyConfiguration(recovery)
		return waiting(statusUpdate, needsUpdate), nil
	}

	statusUpdate, needsUpdate := NewStatus(recovery.Status).
		WithConditions(EtcdSnapshotRestoredCondition(
			fmt.Sprintf("All %d etcd members were restored from the distributed snapshot", etcdMembers),
			recovery.Generation, c.TimeNow())).
		AsApplyConfiguration(recovery)
	return satisfied(statusUpdate, needsUpdate), nil
}

// deleteEtcdRestorePods releases the volumes so the etcd StatefulSet can mount them
// again. These pods have already exited; they persist only because a pod that ran to
// completion is not removed on its own.
func (c *RecoveryController) deleteEtcdRestorePods(ctx context.Context, recovery *hcprecoveryv1alpha1.HCPRecovery) (stepResult, error) {
	selector, err := workerPodSelector(recovery, etcdRestoreApp)
	if err != nil {
		return c.permanentError(recovery, EtcdSnapshotNotRestoredCondition(
			ScaledDownInvalidPodSelectorReason, err.Error(), recovery.Generation, c.TimeNow())), nil
	}
	acted, remaining, err := c.deleteWorkerPods(ctx, recovery.Spec.ControlPlaneNamespace, selector)
	if err != nil {
		return c.retryableError(recovery, EtcdSnapshotNotRestoredCondition(
			EtcdRestorePodsDeleteFailedReason, err.Error(), recovery.Generation, c.TimeNow()), err), nil
	}
	if acted {
		return yield(), nil
	}
	if remaining {
		err = fmt.Errorf("waiting for etcd restore pods in namespace %q to be deleted", recovery.Spec.ControlPlaneNamespace)
		return c.retryableError(recovery, EtcdSnapshotNotRestoredCondition(
			EtcdRestorePodsRemainingReason, err.Error(), recovery.Generation, c.TimeNow()), err), nil
	}

	return proceed(), nil
}
