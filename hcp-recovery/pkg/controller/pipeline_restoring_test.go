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
	"encoding/json"
	"strings"
	"testing"

	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	hypershiftv1beta1 "github.com/openshift/hypershift/api/hypershift/v1beta1"

	hcprecoveryv1alpha1 "github.com/Azure/ARO-HCP/hcp-recovery/pkg/apis/hcprecovery/v1alpha1"
)

// restoreWorld stands in for the cluster the controller is driving: it turns the pods
// the controller creates into pods that ran, applies scale patches, and lets the
// HostedCluster recover once it is unpaused. Without it the phases would have nothing
// to observe and the pipeline could not be exercised end to end.
type restoreWorld struct {
	f *recoveryTest
	// snapshotTransferFails and healthCheckFails make those pods terminate nonzero, to
	// exercise the paths where the recovery has to stop.
	snapshotTransferFails bool
	healthCheckFails      bool
}

func (w *restoreWorld) advance(t *testing.T) {
	t.Helper()
	f := w.f
	w.applyScalePatches(t)

	pods, err := f.kubeClient.CoreV1().Pods("").List(f.ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	live := map[string]bool{}
	for i := range pods.Items {
		pod := pods.Items[i].DeepCopy()
		live[pod.Namespace+"/"+pod.Name] = true
		w.run(pod)
		if _, err := f.kubeClient.CoreV1().Pods(pod.Namespace).UpdateStatus(f.ctx, pod, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		if err := f.informers["Pod"].GetStore().Add(pod); err != nil {
			t.Fatal(err)
		}
	}
	for _, obj := range f.informers["Pod"].GetStore().List() {
		pod := obj.(*corev1.Pod)
		if !live[pod.Namespace+"/"+pod.Name] {
			if err := f.informers["Pod"].GetStore().Delete(pod); err != nil {
				t.Fatal(err)
			}
		}
	}
	f.kubeClient.ClearActions()
}

// run gives a pod the outcome its real counterpart would have had.
func (w *restoreWorld) run(pod *corev1.Pod) {
	succeed := func(message string) {
		pod.Status.Phase = corev1.PodSucceeded
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name:  workerContainerName,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: message}},
		}}
	}
	fail := func(message string) {
		pod.Status.Phase = corev1.PodFailed
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name: workerContainerName,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 1, Message: message,
			}},
		}}
	}

	switch pod.Labels["app"] {
	case etcdHolderApp, etcd:
		pod.Status.Phase = corev1.PodRunning
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	case etcdRestoreApp:
		succeed("")
	case snapshotTransferApp:
		if w.snapshotTransferFails {
			fail("etcd-holder-2: snapshot checksum mismatch")
			return
		}
		succeed(`{"sourceMember":"etcd-0","sha256":"` + testSnapshotSHA + `","revision":4242}`)
	case etcdHealthCheckApp:
		if w.healthCheckFails {
			fail("endpoints report different cluster IDs")
			return
		}
		succeed("members: etcd-0, etcd-1, etcd-2;all 3 endpoints healthy")
	}
}

// applyScalePatches mirrors scale subresource patches onto the StatefulSet and brings
// member pods in or out of existence to match.
func (w *restoreWorld) applyScalePatches(t *testing.T) {
	t.Helper()
	f := w.f
	for _, action := range f.kubeClient.Actions() {
		patch, ok := action.(clienttesting.PatchAction)
		if !ok || action.GetResource().Resource != "statefulsets" || action.GetSubresource() != "scale" {
			continue
		}
		var scale struct {
			Spec struct {
				Replicas int32 `json:"replicas"`
			} `json:"spec"`
		}
		if err := json.Unmarshal(patch.GetPatch(), &scale); err != nil {
			t.Fatal(err)
		}
		statefulSet := f.sts.DeepCopy()
		statefulSet.Spec.Replicas = ptr.To(scale.Spec.Replicas)
		statefulSet.Status.Replicas = scale.Spec.Replicas
		statefulSet.Status.ReadyReplicas = scale.Spec.Replicas
		if err := f.informers["StatefulSet"].GetStore().Update(statefulSet); err != nil {
			t.Fatal(err)
		}
		f.sts = statefulSet

		for i := int32(0); i < testEtcdReplicas; i++ {
			pod := f.etcdPod(i, true)
			if i < scale.Spec.Replicas {
				if _, err := f.kubeClient.CoreV1().Pods(pod.Namespace).Create(f.ctx, pod, metav1.CreateOptions{}); err != nil &&
					!apierrors.IsAlreadyExists(err) {
					t.Fatal(err)
				}
				continue
			}
			if err := f.kubeClient.CoreV1().Pods(pod.Namespace).Delete(f.ctx, pod.Name, metav1.DeleteOptions{}); err != nil &&
				!apierrors.IsNotFound(err) {
				t.Fatal(err)
			}
		}
	}
}

// unpauseRecoversTheHostedCluster lets the control plane come back, but only after the
// HostedCluster has actually been unpaused — the operator does nothing while paused.
func (w *restoreWorld) unpauseRecoversTheHostedCluster(t *testing.T) {
	t.Helper()
	f := w.f
	hostedCluster, err := f.hcClient.HypershiftV1beta1().
		HostedClusters(f.recovery.Spec.HostedClusterNamespace).Get(f.ctx, f.recovery.Spec.HostedClusterName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cached := hostedCluster.DeepCopy()
	if cached.Spec.PausedUntil == nil {
		cached.Status.Conditions = hostedClusterConditions(map[hypershiftv1beta1.ConditionType]metav1.ConditionStatus{
			hypershiftv1beta1.EtcdAvailable:          metav1.ConditionTrue,
			hypershiftv1beta1.HostedClusterAvailable: metav1.ConditionTrue,
		})
	}
	if err := f.informers["HostedCluster"].GetStore().Update(cached); err != nil {
		t.Fatal(err)
	}
	f.hc = cached
}

func newRestorePipelineTest(t *testing.T) (*recoveryTest, *restoreWorld) {
	t.Helper()
	f := newRecoveryTest(t, func(f *recoveryTest) {
		f.recovery.Status.Phase = hcprecoveryv1alpha1.RestoreStateRestoring
		// The Velero restore brought the StatefulSet back at its original size.
		f.sts.Spec.Replicas = ptr.To(testEtcdReplicas)
		f.sts.Status.Replicas = testEtcdReplicas
		f.sts.Status.ReadyReplicas = testEtcdReplicas
		f.hc.Spec.PausedUntil = ptr.To("true")
	})
	f.cacheVeleroRestore(velerov1.RestorePhaseCompleted)
	for i := int32(0); i < testEtcdReplicas; i++ {
		f.addPods(f.etcdPod(i, true))
	}
	return f, &restoreWorld{f: f}
}

// drive reconciles until the phase stops changing or goes terminal, advancing the world
// in between so the controller has something new to observe each time.
func (w *restoreWorld) drive(t *testing.T) *hcprecoveryv1alpha1.HCPRecovery {
	t.Helper()
	f := w.f
	recovery := f.recovery.DeepCopy()
	for range 60 {
		if err := f.reconcile(f.ctx, recovery); err != nil &&
			!strings.Contains(err.Error(), "waiting for") && !strings.Contains(err.Error(), "check shows") {
			t.Fatalf("unexpected reconcile error in phase %q: %v", recovery.Status.Phase, err)
		}
		w.advance(t)
		w.unpauseRecoversTheHostedCluster(t)

		updated, err := f.client.HcprecoveryV1alpha1().HCPRecoveries(recovery.Namespace).
			Get(f.ctx, recovery.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		recovery = updated
		if err := f.informers["HCPRecovery"].GetStore().Update(recovery); err != nil {
			t.Fatal(err)
		}
		switch recovery.Status.Phase {
		case hcprecoveryv1alpha1.RestoreStateCompleted, hcprecoveryv1alpha1.RestoreStateFailed:
			return recovery
		}
	}
	t.Fatalf("the restore never reached a terminal phase, stalled in %q with %#v",
		recovery.Status.Phase, recovery.Status.Conditions)
	return nil
}

// The whole point of the phase machine is that it carries a restore from the end of the
// Velero restore to a recovered cluster without anyone driving it by hand.
func TestRestoreRunsFromRestoringToCompleted(t *testing.T) {
	f, world := newRestorePipelineTest(t)
	recovery := world.drive(t)

	if recovery.Status.Phase != hcprecoveryv1alpha1.RestoreStateCompleted {
		t.Fatalf("expected the restore to complete, got %q with %#v", recovery.Status.Phase, recovery.Status.Conditions)
	}
	for _, conditionType := range []string{
		hcprecoveryv1alpha1.ConditionEtcdScaledToZeroAfterVeleroRestore,
		hcprecoveryv1alpha1.ConditionEtcdPodsGone,
		hcprecoveryv1alpha1.ConditionEtcdHolderPodsReady,
		hcprecoveryv1alpha1.ConditionEtcdSnapshotDistributed,
		hcprecoveryv1alpha1.ConditionEtcdSnapshotRestored,
		hcprecoveryv1alpha1.ConditionEtcdScaledUp,
		hcprecoveryv1alpha1.ConditionEtcdHealthy,
		hcprecoveryv1alpha1.ConditionHostedClusterUnpaused,
		hcprecoveryv1alpha1.ConditionHealthChecked,
	} {
		if !meta.IsStatusConditionTrue(recovery.Status.Conditions, conditionType) {
			t.Errorf("expected %s to be recorded true, got %#v",
				conditionType, meta.FindStatusCondition(recovery.Status.Conditions, conditionType))
		}
	}
	if recovery.Status.EtcdSnapshot == nil || recovery.Status.EtcdSnapshot.SHA256 != testSnapshotSHA {
		t.Fatalf("expected the distributed snapshot to be recorded, got %#v", recovery.Status.EtcdSnapshot)
	}
	if recovery.Status.CompletedAt == nil {
		t.Fatal("expected a completion timestamp")
	}

	// Nothing the recovery created may be left mounting an etcd volume.
	for _, obj := range f.informers["Pod"].GetStore().List() {
		pod := obj.(*corev1.Pod)
		switch pod.Labels["app"] {
		case etcdHolderApp, etcdRestoreApp, snapshotTransferApp, etcdHealthCheckApp:
			t.Errorf("recovery worker pod %s/%s was left behind", pod.Namespace, pod.Name)
		}
	}
	etcdStatefulSet := f.sts
	if ptr.Deref(etcdStatefulSet.Spec.Replicas, 0) != testEtcdReplicas {
		t.Fatalf("expected etcd back at %d replicas, got %v", testEtcdReplicas, etcdStatefulSet.Spec.Replicas)
	}
}

// A copy that could not make every volume identical, or an etcd that never agreed with
// itself, must stop the restore rather than unpause a cluster onto broken data.
func TestRestoreStopsWhenAWorkerPodFails(t *testing.T) {
	for _, tt := range []struct {
		name          string
		configure     func(*restoreWorld)
		conditionType string
	}{
		{
			name:          "snapshot distribution",
			configure:     func(w *restoreWorld) { w.snapshotTransferFails = true },
			conditionType: hcprecoveryv1alpha1.ConditionEtcdSnapshotDistributed,
		},
		{
			name:          "etcd health check",
			configure:     func(w *restoreWorld) { w.healthCheckFails = true },
			conditionType: hcprecoveryv1alpha1.ConditionEtcdHealthy,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f, world := newRestorePipelineTest(t)
			tt.configure(world)
			recovery := world.drive(t)

			if recovery.Status.Phase != hcprecoveryv1alpha1.RestoreStateFailed {
				t.Fatalf("expected the restore to fail, got %q", recovery.Status.Phase)
			}
			if recovery.Status.CompletedAt == nil {
				t.Fatal("a terminal failure must record when it finished")
			}
			condition := meta.FindStatusCondition(recovery.Status.Conditions, tt.conditionType)
			if condition == nil || condition.Status != metav1.ConditionFalse {
				t.Fatalf("expected %s to explain the failure, got %#v", tt.conditionType, condition)
			}
			if meta.IsStatusConditionTrue(recovery.Status.Conditions, hcprecoveryv1alpha1.ConditionHostedClusterUnpaused) {
				t.Fatal("the HostedCluster must not be unpaused after a failed restore")
			}
			hostedCluster, err := f.hcClient.HypershiftV1beta1().
				HostedClusters(f.recovery.Spec.HostedClusterNamespace).
				Get(f.ctx, f.recovery.Spec.HostedClusterName, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if hostedCluster.Spec.PausedUntil == nil {
				t.Fatal("a failed restore must leave the HostedCluster paused")
			}
		})
	}
}
