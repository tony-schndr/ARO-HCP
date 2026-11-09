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
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"

	hypershiftv1beta1 "github.com/openshift/hypershift/api/hypershift/v1beta1"

	hcprecoveryv1alpha1 "github.com/Azure/ARO-HCP/hcp-recovery/pkg/apis/hcprecovery/v1alpha1"
)

const (
	EtcdScaleUpPatchFailedReason = "EtcdScaleUpPatchFailed"
	EtcdPodsStartingReason       = "EtcdPodsStarting"
)

// scaleUpEtcd restores the StatefulSet to its full member count. All three members were
// restored with the same membership list, so they come up as one cluster rather than
// trying to join an existing one — which is why the StatefulSet goes straight to its
// full size instead of growing one at a time.
func (c *RecoveryController) scaleUpEtcd(ctx context.Context, recovery *hcprecoveryv1alpha1.HCPRecovery) (stepResult, error) {
	etcdStatefulSet, err := c.statefulsetLister.StatefulSets(recovery.Spec.ControlPlaneNamespace).Get(etcd)
	if err != nil {
		err = fmt.Errorf("failed to get etcd statefulset in namespace %q: %w", recovery.Spec.ControlPlaneNamespace, err)
		return c.retryableError(recovery, EtcdNotScaledUpCondition(
			EtcdStatefulSetLookupFailedReason, err.Error(), recovery.Generation, c.TimeNow()), err), nil
	}
	if etcdStatefulSet.Spec.Replicas == nil || *etcdStatefulSet.Spec.Replicas != etcdMembers {
		_, err = c.kubeClientset.AppsV1().
			StatefulSets(recovery.Spec.ControlPlaneNamespace).
			Patch(ctx, etcd, types.MergePatchType,
				[]byte(fmt.Sprintf(`{"spec":{"replicas":%d}}`, etcdMembers)),
				metav1.PatchOptions{FieldManager: fieldManager},
				"scale")
		if err != nil {
			err = fmt.Errorf("failed to scale etcd statefulset to %d replicas: %w", etcdMembers, err)
			return c.retryableError(recovery, EtcdNotScaledUpCondition(
				EtcdScaleUpPatchFailedReason, err.Error(), recovery.Generation, c.TimeNow()), err), nil
		}
		// The patch lands as a watch event on the StatefulSet.
		return yield(), nil
	}
	return proceed(), nil
}

// waitForEtcdPodsReady waits for every etcd pod to report all of its containers ready,
// which is the runbook's "3x 4/4 Running". Readiness only means the members are
// serving; whether they agree on one cluster is checked separately.
func (c *RecoveryController) waitForEtcdPodsReady(ctx context.Context, recovery *hcprecoveryv1alpha1.HCPRecovery) (stepResult, error) {
	selector := labels.SelectorFromSet(labels.Set{"app": etcd})
	pods, err := c.podLister.Pods(recovery.Spec.ControlPlaneNamespace).List(selector)
	if err != nil {
		err = fmt.Errorf("failed to list etcd pods from cache in namespace %q: %w", recovery.Spec.ControlPlaneNamespace, err)
		return c.retryableError(recovery, EtcdNotScaledUpCondition(
			EtcdPodsCachedListFailedReason, err.Error(), recovery.Generation, c.TimeNow()), err), nil
	}
	ready := int32(0)
	for _, pod := range pods {
		if podReady(pod) {
			ready++
		}
	}
	if ready != etcdMembers {
		// Pod readiness changes raise events, so there is nothing to poll for here.
		statusUpdate, needsUpdate := NewStatus(recovery.Status).
			WithConditions(EtcdNotScaledUpCondition(EtcdPodsStartingReason,
				fmt.Sprintf("%d of %d etcd pods are ready", ready, etcdMembers),
				recovery.Generation, c.TimeNow())).
			AsApplyConfiguration(recovery)
		return waiting(statusUpdate, needsUpdate), nil
	}

	statusUpdate, needsUpdate := NewStatus(recovery.Status).
		WithConditions(EtcdScaledUpCondition(
			fmt.Sprintf("All %d etcd pods are ready", etcdMembers), recovery.Generation, c.TimeNow())).
		AsApplyConfiguration(recovery)
	return satisfied(statusUpdate, needsUpdate), nil
}

const (
	EtcdHealthCheckCreateFailedReason = "EtcdHealthCheckCreateFailed"
	EtcdHealthCheckLookupFailedReason = "EtcdHealthCheckLookupFailed"
	EtcdHealthCheckRunningReason      = "EtcdHealthCheckRunning"
	EtcdHealthCheckFailedReason       = "EtcdHealthCheckFailed"
	EtcdHealthCheckDeleteFailedReason = "EtcdHealthCheckDeleteFailed"
)

// createEtcdHealthCheckPod asks etcd whether the restore produced one cluster. Running
// pods prove nothing on their own: a botched restore yields members that are each
// individually healthy but belong to different clusters, so the members have to be
// interrogated and compared.
//
// The checks poll inside the pod because etcd's internal state has no Kubernetes object
// to watch. Polling from the controller would mean either holding a worker or
// re-creating pods on a timer.
func (c *RecoveryController) createEtcdHealthCheckPod(ctx context.Context, recovery *hcprecoveryv1alpha1.HCPRecovery) (stepResult, error) {
	if meta.IsStatusConditionTrue(recovery.Status.Conditions, hcprecoveryv1alpha1.ConditionEtcdHealthy) {
		return proceed(), nil
	}
	name := etcdHealthCheckPodName(recovery)
	_, err := c.podLister.Pods(recovery.Namespace).Get(name)
	if err == nil {
		return proceed(), nil
	}
	if !apierrors.IsNotFound(err) {
		err = fmt.Errorf("failed to get etcd health check pod %s/%s: %w", recovery.Namespace, name, err)
		return c.retryableError(recovery, EtcdNotHealthyCondition(
			EtcdHealthCheckLookupFailedReason, err.Error(), recovery.Generation, c.TimeNow()), err), nil
	}

	pod := etcdHealthCheckPod(recovery, c.workerImage, c.workerServiceAccount)
	_, err = c.kubeClientset.CoreV1().Pods(recovery.Namespace).Create(ctx, pod, metav1.CreateOptions{FieldManager: fieldManager})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		err = fmt.Errorf("failed to create etcd health check pod %s/%s: %w", recovery.Namespace, name, err)
		return c.retryableError(recovery, EtcdNotHealthyCondition(
			EtcdHealthCheckCreateFailedReason, err.Error(), recovery.Generation, c.TimeNow()), err), nil
	}
	return yield(), nil
}

// waitForEtcdHealthCheck records the verdict. The pod retries for as long as etcd is
// allowed to take to settle, so a pod that failed means etcd did not converge and the
// HostedCluster must not be unpaused on top of it.
func (c *RecoveryController) waitForEtcdHealthCheck(ctx context.Context, recovery *hcprecoveryv1alpha1.HCPRecovery) (stepResult, error) {
	if meta.IsStatusConditionTrue(recovery.Status.Conditions, hcprecoveryv1alpha1.ConditionEtcdHealthy) {
		return proceed(), nil
	}

	name := etcdHealthCheckPodName(recovery)
	pod, err := c.podLister.Pods(recovery.Namespace).Get(name)
	if err != nil {
		err = fmt.Errorf("failed to get etcd health check pod %s/%s: %w", recovery.Namespace, name, err)
		return c.retryableError(recovery, EtcdNotHealthyCondition(
			EtcdHealthCheckLookupFailedReason, err.Error(), recovery.Generation, c.TimeNow()), err), nil
	}

	phase, message := podResult(pod)
	switch phase {
	case corev1.PodSucceeded:
	case corev1.PodFailed:
		return c.permanentError(recovery, EtcdNotHealthyCondition(
			EtcdHealthCheckFailedReason,
			fmt.Sprintf("etcd did not converge into one healthy cluster: %s", message),
			recovery.Generation, c.TimeNow())), nil
	default:
		statusUpdate, needsUpdate := NewStatus(recovery.Status).
			WithConditions(EtcdNotHealthyCondition(EtcdHealthCheckRunningReason,
				fmt.Sprintf("Health check pod %s/%s is %s", recovery.Namespace, name, phase),
				recovery.Generation, c.TimeNow())).
			AsApplyConfiguration(recovery)
		return waiting(statusUpdate, needsUpdate), nil
	}

	statusUpdate, needsUpdate := NewStatus(recovery.Status).
		WithConditions(EtcdHealthyCondition(message, recovery.Generation, c.TimeNow())).
		AsApplyConfiguration(recovery)
	return satisfied(statusUpdate, needsUpdate), nil
}

// deleteEtcdHealthCheckPod reaps the completed check. The pod holds no etcd volume
// and is owned by the HCPRecovery, so unpausing need not wait for its deletion.
func (c *RecoveryController) deleteEtcdHealthCheckPod(ctx context.Context, recovery *hcprecoveryv1alpha1.HCPRecovery) (stepResult, error) {
	selector, err := workerPodSelector(recovery, etcdHealthCheckApp)
	if err != nil {
		return c.permanentError(recovery, EtcdNotHealthyCondition(
			ScaledDownInvalidPodSelectorReason, err.Error(), recovery.Generation, c.TimeNow())), nil
	}
	acted, _, err := c.deleteWorkerPods(ctx, recovery.Namespace, selector)
	if err != nil {
		return c.retryableError(recovery, EtcdNotHealthyCondition(
			EtcdHealthCheckDeleteFailedReason, err.Error(), recovery.Generation, c.TimeNow()), err), nil
	}
	if acted {
		return yield(), nil
	}

	return proceed(), nil
}

const (
	HostedClusterUnpauseFailedReason = "HostedClusterUnpauseFailed"
	HostedClusterNotReadyReason      = "HostedClusterNotReady"
)

// unpauseHostedCluster hands the control plane back to the HyperShift operator. Nothing
// before this point may run with the cluster unpaused: the operator would recreate the
// workloads the recovery just tore down and remount the volumes it is rewriting.
func (c *RecoveryController) unpauseHostedCluster(ctx context.Context, recovery *hcprecoveryv1alpha1.HCPRecovery) (stepResult, error) {
	hostedCluster, err := c.hypershiftLister.HostedClusters(recovery.Spec.HostedClusterNamespace).Get(recovery.Spec.HostedClusterName)
	if err != nil {
		err = fmt.Errorf("failed to get HostedCluster %s/%s: %w", recovery.Spec.HostedClusterNamespace, recovery.Spec.HostedClusterName, err)
		return c.retryableError(recovery, HostedClusterNotUnpausedCondition(
			HostedClusterLookupFailedReason, err.Error(), recovery.Generation, c.TimeNow()), err), nil
	}

	if paused := hostedCluster.Spec.PausedUntil; paused != nil && strings.EqualFold(*paused, "true") {
		// An apply configuration cannot express removal of a field, and leaving
		// pausedUntil set to anything keeps the operator from reconciling, so clear it
		// with an explicit null.
		_, err = c.hypershiftClient.HypershiftV1beta1().HostedClusters(hostedCluster.Namespace).
			Patch(ctx, hostedCluster.Name, types.MergePatchType,
				[]byte(`{"spec":{"pausedUntil":null}}`), metav1.PatchOptions{FieldManager: fieldManager})
		if err != nil {
			err = fmt.Errorf("failed to unpause HostedCluster %s/%s: %w", hostedCluster.Namespace, hostedCluster.Name, err)
			return c.retryableError(recovery, HostedClusterNotUnpausedCondition(
				HostedClusterUnpauseFailedReason, err.Error(), recovery.Generation, c.TimeNow()), err), nil
		}
		// The patch lands as a watch event on the HostedCluster.
		return yield(), nil
	}

	statusUpdate, needsUpdate := NewStatus(recovery.Status).
		WithConditions(HostedClusterUnpausedCondition(recovery.Generation, c.TimeNow())).
		AsApplyConfiguration(recovery)
	return satisfied(statusUpdate, needsUpdate), nil
}

// validateHostedClusterHealthy waits for the control plane to come back on the restored
// etcd. The HostedCluster only reports these conditions once the operator has resumed
// and rebuilt its workloads, so this is what distinguishes "etcd is healthy" from "the
// cluster recovered".
func (c *RecoveryController) validateHostedClusterHealthy(ctx context.Context, recovery *hcprecoveryv1alpha1.HCPRecovery) (stepResult, error) {
	hostedCluster, err := c.hypershiftLister.HostedClusters(recovery.Spec.HostedClusterNamespace).Get(recovery.Spec.HostedClusterName)
	if err != nil {
		err = fmt.Errorf("failed to get HostedCluster %s/%s: %w", recovery.Spec.HostedClusterNamespace, recovery.Spec.HostedClusterName, err)
		return c.retryableError(recovery, HealthCheckFailedCondition(
			HostedClusterLookupFailedReason, err.Error(), recovery.Generation, c.TimeNow()), err), nil
	}

	required := []hypershiftv1beta1.ConditionType{
		hypershiftv1beta1.EtcdAvailable,
		hypershiftv1beta1.HostedClusterAvailable,
	}
	var unmet []string
	for _, conditionType := range required {
		condition := meta.FindStatusCondition(hostedCluster.Status.Conditions, string(conditionType))
		switch {
		case condition == nil:
			unmet = append(unmet, fmt.Sprintf("%s is not reported", conditionType))
		case condition.Status != metav1.ConditionTrue:
			unmet = append(unmet, fmt.Sprintf("%s is %s (%s)", conditionType, condition.Status, condition.Reason))
		}
	}
	if len(unmet) > 0 {
		// HostedCluster status changes raise informer events, so waiting costs nothing.
		err = fmt.Errorf("HostedCluster %s/%s has not recovered: %s",
			recovery.Spec.HostedClusterNamespace, recovery.Spec.HostedClusterName, strings.Join(unmet, "; "))
		return c.retryableError(recovery, HealthCheckFailedCondition(
			HostedClusterNotReadyReason, err.Error(), recovery.Generation, c.TimeNow()), err), nil
	}

	statusUpdate, needsUpdate := NewStatus(recovery.Status).
		WithConditions(HealthCheckedCondition(recovery.Generation, c.TimeNow())).
		AsApplyConfiguration(recovery)
	return satisfied(statusUpdate, needsUpdate), nil
}
