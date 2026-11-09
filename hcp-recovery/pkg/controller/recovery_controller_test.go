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
	"errors"
	"reflect"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"

	hcprecoveryv1alpha1 "github.com/Azure/ARO-HCP/hcp-recovery/pkg/apis/hcprecovery/v1alpha1"
	recoveryapply "github.com/Azure/ARO-HCP/hcp-recovery/pkg/generated/applyconfiguration/hcprecovery/v1alpha1"
	"github.com/Azure/ARO-HCP/internal/backup"
)

func TestScaleDownKubeAPIServerUsesMergePatch(t *testing.T) {
	for _, patchErr := range []error{nil, errors.New("scale patch failed")} {
		name := "success"
		if patchErr != nil {
			name = "error"
		}
		t.Run(name, func(t *testing.T) {
			f := newRecoveryTest(t, nil)
			f.kubeClient.PrependReactor("patch", "deployments", func(action clienttesting.Action) (bool, runtime.Object, error) {
				patch, ok := action.(clienttesting.PatchActionImpl)
				if !ok {
					t.Fatalf("expected patch action, got %T", action)
				}
				if patch.GetNamespace() != f.recovery.Spec.ControlPlaneNamespace ||
					patch.GetName() != "kube-apiserver" ||
					patch.GetSubresource() != "scale" ||
					patch.GetPatchType() != types.MergePatchType {
					t.Fatalf("unexpected scale patch target or type: %#v", patch)
				}
				if string(patch.GetPatch()) != `{"spec":{"replicas":0}}` {
					t.Fatalf("unexpected scale patch: %s", patch.GetPatch())
				}
				opts := patch.GetPatchOptions()
				if opts.FieldManager != fieldManager || opts.Force != nil {
					t.Fatalf("expected named field manager without force: %#v", opts)
				}
				return true, f.deployment.DeepCopy(), patchErr
			})

			result, err := f.runStep(f.scaleDownKubeAPIServer, f.recovery)
			if result.outcome == stepContinue || err != nil {
				t.Fatalf("expected scale attempt to stop the pass without returning an error, got outcome=%v err=%v", result.outcome, err)
			}
			wantStatusWrites := 0
			if patchErr != nil {
				wantStatusWrites = 1
			}
			if len(f.kubeClient.Actions()) != 1 || len(f.client.Actions()) != wantStatusWrites {
				t.Fatalf("expected a scale patch and %d failure status writes, got kube actions=%v recovery actions=%v",
					wantStatusWrites,
					f.kubeClient.Actions(), f.client.Actions())
			}
		})
	}
}

func TestStatusConflictRetriesAfterCacheRefresh(t *testing.T) {
	f := newRecoveryTest(t, func(f *recoveryTest) {
		f.recovery.Status.Conditions = []metav1.Condition{{
			Type: hcprecoveryv1alpha1.ConditionHostedClusterPaused, Status: metav1.ConditionTrue,
			Reason: "ClusterPaused", LastTransitionTime: metav1.NewTime(testNow),
		}}
		startedAt := metav1.NewTime(testNow)
		f.recovery.Status.StartedAt = &startedAt
	})
	recovery := f.recovery
	indexer := f.informers["HCPRecovery"].GetStore()
	current := recovery.DeepCopy()
	current.ResourceVersion = "2"
	current.Status.RestoreName = "concurrent-status"
	if err := f.client.Tracker().Update(hcprecoveryv1alpha1.SchemeGroupVersion.WithResource("hcprecoveries"), current, current.Namespace); err != nil {
		t.Fatal(err)
	}
	writeErr := apierrors.NewConflict(hcprecoveryv1alpha1.Resource("hcprecoveries"), recovery.Name, errors.New("stale resource version"))
	f.client.PrependReactor("patch", "hcprecoveries", func(action clienttesting.Action) (bool, runtime.Object, error) {
		patch, ok := action.(clienttesting.PatchAction)
		if !ok {
			t.Fatalf("expected patch action, got %T", action)
		}
		var update recoveryapply.HCPRecoveryApplyConfiguration
		if err := json.Unmarshal(patch.GetPatch(), &update); err != nil {
			t.Fatal(err)
		}
		if *update.ResourceVersion != current.ResourceVersion {
			return true, nil, writeErr
		}
		return false, nil, nil
	})
	key := recovery.Namespace + "/" + recovery.Name
	if err := f.syncHandler(f.ctx, key); !errors.Is(err, writeErr) {
		t.Fatalf("expected status error to propagate, got %v", err)
	}
	f.client.ClearActions()
	f.workqueue.Add(key)
	if !f.processNextWorkItem(f.ctx) || f.workqueue.NumRequeues(key) != 1 {
		t.Fatal("status-write failure was not rate-limited for retry")
	}
	if err := indexer.Update(current); err != nil {
		t.Fatal(err)
	}
	f.waitFor("retry was not enqueued", func() bool { return f.workqueue.Len() > 0 })
	if !f.processNextWorkItem(f.ctx) || f.workqueue.NumRequeues(key) != 0 {
		t.Fatal("successful retry did not clear the rate limiter")
	}
	if len(f.client.Actions()) != 2 {
		t.Fatalf("expected one failed patch and one successful patch, got %v", f.client.Actions())
	}
	for _, action := range f.client.Actions() {
		assertStatusPatch(t, action)
	}
	persisted, err := f.client.HcprecoveryV1alpha1().HCPRecoveries(recovery.Namespace).Get(f.ctx, recovery.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !meta.IsStatusConditionTrue(persisted.Status.Conditions, hcprecoveryv1alpha1.ConditionRecoveryInputsValidated) || persisted.Status.RestoreName != current.Status.RestoreName {
		t.Fatalf("retry did not persist validation and retain unrelated status: %#v", persisted.Status)
	}
	if err := indexer.Update(persisted); err != nil {
		t.Fatal(err)
	}
	f.client.ClearActions()
	if result, err := f.runStep(f.validateRecoveryInputs, persisted); result.outcome != stepContinue || err != nil || len(f.client.Actions()) != 0 {
		t.Fatalf("persisted success should prevent another write: outcome=%v err=%v actions=%v", result.outcome, err, f.client.Actions())
	}
}

func TestSyncHandlerSkipsTerminalRecoveries(t *testing.T) {
	for _, phase := range []hcprecoveryv1alpha1.RestoreState{hcprecoveryv1alpha1.RestoreStateFailed, hcprecoveryv1alpha1.RestoreStateCompleted} {
		t.Run(string(phase), func(t *testing.T) {
			f := newRecoveryTest(t, func(f *recoveryTest) { f.recovery.Status.Phase = phase })
			f.TimeNow = func() time.Time {
				t.Fatal("terminal recovery reached step execution")
				return testNow
			}
			if err := f.syncHandler(f.ctx, f.recovery.Namespace+"/"+f.recovery.Name); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSyncHandlerMissingRecovery(t *testing.T) {
	f := newRecoveryTest(t, nil)
	if err := f.syncHandler(f.ctx, f.recovery.Namespace+"/deleted"); err != nil {
		t.Fatalf("deleted recovery should not be retried: %v", err)
	}
	if err := f.syncHandler(f.ctx, "invalid/key/format"); err == nil {
		t.Fatal("expected invalid key error")
	}
}

func TestRecoveryInformerRequeuesStatus(t *testing.T) {
	f := newRecoveryTest(t, func(f *recoveryTest) {
		f.recovery.Status.Phase = ""
		f.recovery.Status.StartedAt = nil
	})
	f.startInformers()
	recovery, hc := f.recovery, f.hc
	for _, step := range []struct {
		name     string
		writes   int
		observed func() bool
	}{
		{"phase and start time initialization", 1, func() bool {
			cached, err := f.hcpRecoveryLister.HCPRecoveries(recovery.Namespace).Get(recovery.Name)
			return err == nil && cached.Status.Phase == hcprecoveryv1alpha1.RestoreStatePreparing &&
				cached.Status.StartedAt != nil && cached.Status.StartedAt.Time.Equal(testNow)
		}},
		{"recovery input validation", 1, func() bool {
			cached, err := f.hcpRecoveryLister.HCPRecoveries(recovery.Namespace).Get(recovery.Name)
			return err == nil && meta.IsStatusConditionTrue(cached.Status.Conditions, hcprecoveryv1alpha1.ConditionRecoveryInputsValidated)
		}},
		{"etcd runtime details", 1, func() bool {
			cached, err := f.hcpRecoveryLister.HCPRecoveries(recovery.Namespace).Get(recovery.Name)
			return err == nil && meta.IsStatusConditionTrue(cached.Status.Conditions, hcprecoveryv1alpha1.EtcdRuntimeDetailsCollectedCondition)
		}},
		{"HostedCluster pause", 0, func() bool {
			cached, err := f.hypershiftLister.HostedClusters(hc.Namespace).Get(hc.Name)
			return err == nil && cached.Spec.PausedUntil != nil && *cached.Spec.PausedUntil == "true"
		}},
		{"pause condition", 1, func() bool {
			cached, err := f.hcpRecoveryLister.HCPRecoveries(recovery.Namespace).Get(recovery.Name)
			return err == nil && meta.IsStatusConditionTrue(cached.Status.Conditions, hcprecoveryv1alpha1.ConditionHostedClusterPaused)
		}},
		{"completed pause", 0, nil},
	} {
		f.waitFor(step.name+" was not enqueued", func() bool { return f.workqueue.Len() > 0 })
		f.client.ClearActions()
		if !f.processNextWorkItem(f.ctx) || len(f.client.Actions()) != step.writes {
			t.Fatalf("%s: expected %d status writes and no API reads, got %v", step.name, step.writes, f.client.Actions())
		}
		if step.observed != nil {
			f.waitFor(step.name+" did not reach informer cache", step.observed)
		}
	}
	cached, err := f.hcpRecoveryLister.HCPRecoveries(recovery.Namespace).Get(recovery.Name)
	if err != nil || cached.Status.StartedAt == nil || !cached.Status.StartedAt.Time.Equal(testNow) {
		t.Fatalf("initialization did not preserve startedAt: recovery=%v err=%v", cached, err)
	}
	if !reflect.DeepEqual(cached.Status.EtcdRuntimeDetails, recovery.Status.EtcdRuntimeDetails) {
		t.Fatalf("reconciliation lost etcd runtime details: %#v", cached.Status.EtcdRuntimeDetails)
	}
}

func TestHostedClusterUpdateEnqueuesMatchingRecoveries(t *testing.T) {
	f := newRecoveryTest(t, nil)
	recovery, hc := f.recovery, f.hc
	second := recovery.DeepCopy()
	second.Name = "second-recovery"
	second.Namespace = "another-recovery-namespace"
	unrelated := recovery.DeepCopy()
	unrelated.Name = "unrelated-recovery"
	unrelated.Spec.HostedClusterNamespace = "another-hosted-cluster-namespace"
	for _, obj := range []*hcprecoveryv1alpha1.HCPRecovery{recovery, second, unrelated} {
		if err := f.informers["HCPRecovery"].GetStore().Add(obj); err != nil {
			t.Fatal(err)
		}
	}
	f.startInformers("HostedCluster")
	for _, pausedUntil := range []string{"true", "false"} {
		updated := hc.DeepCopy()
		updated.Spec.PausedUntil = ptr.To(pausedUntil)
		if _, err := f.hcClient.HypershiftV1beta1().HostedClusters(hc.Namespace).
			Update(f.ctx, updated, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		f.waitFor("HostedCluster update did not enqueue matching recoveries", func() bool { return f.workqueue.Len() >= 2 })
		if f.workqueue.Len() != 2 {
			t.Fatalf("expected only two matching recoveries, got %d keys", f.workqueue.Len())
		}
		want := map[string]bool{
			recovery.Namespace + "/" + recovery.Name: true,
			second.Namespace + "/" + second.Name:     true,
		}
		for range 2 {
			key, shutdown := f.workqueue.Get()
			f.workqueue.Done(key)
			if shutdown || !want[key] {
				t.Fatalf("unexpected recovery key %q", key)
			}
			delete(want, key)
		}
	}
}

func TestHostedControlPlaneEventsEnqueueMatchingRecoveries(t *testing.T) {
	f := newRecoveryTest(t, nil)
	second := f.recovery.DeepCopy()
	second.Name = "second-recovery"
	unrelated := f.recovery.DeepCopy()
	unrelated.Name = "unrelated-recovery"
	unrelated.Spec.ControlPlaneNamespace = "another-control-plane"
	unrelated.Spec.HostedClusterNamespace = "another-hosted-cluster"
	for _, recovery := range []*hcprecoveryv1alpha1.HCPRecovery{second, unrelated} {
		if err := f.informers["HCPRecovery"].GetStore().Add(recovery); err != nil {
			t.Fatal(err)
		}
	}
	assertQueued := func(event string) {
		t.Helper()
		f.waitFor("HostedControlPlane "+event+" did not enqueue matching recoveries", func() bool { return f.workqueue.Len() >= 2 })
		if f.workqueue.Len() != 2 {
			t.Fatalf("expected only two matching recoveries after %s, got %d", event, f.workqueue.Len())
		}
		want := map[string]bool{
			f.recovery.Namespace + "/" + f.recovery.Name: true,
			second.Namespace + "/" + second.Name:         true,
		}
		for range 2 {
			key, shutdown := f.workqueue.Get()
			f.workqueue.Done(key)
			if shutdown || !want[key] {
				t.Fatalf("unexpected recovery key %q after %s", key, event)
			}
			delete(want, key)
		}
	}
	f.startInformers("HostedControlPlane")
	assertQueued("add")
	hcp := f.hcp.DeepCopy()
	hcp.ResourceVersion = "2"
	hcp.Status.Conditions = nil
	if _, err := f.hcClient.HypershiftV1beta1().HostedControlPlanes(hcp.Namespace).UpdateStatus(f.ctx, hcp, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	assertQueued("status update")
	if err := f.hcClient.HypershiftV1beta1().HostedControlPlanes(hcp.Namespace).Delete(f.ctx, hcp.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	assertQueued("delete")
	f.onHostedControlPlaneDelete(cache.DeletedFinalStateUnknown{Key: hcp.Namespace + "/" + hcp.Name, Obj: hcp})
	assertQueued("deletion tombstone")
}

func TestDeploymentUpdateEnqueuesReplicaChanges(t *testing.T) {
	for _, tt := range []struct {
		name        string
		oldReplicas *int32
		newReplicas *int32
		wantQueued  bool
	}{
		{name: "scale down", oldReplicas: ptr.To[int32](3), newReplicas: ptr.To[int32](0), wantQueued: true},
		{name: "unchanged replicas", oldReplicas: ptr.To[int32](3), newReplicas: ptr.To[int32](3)},
		{name: "nil and zero replicas", newReplicas: ptr.To[int32](0)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newRecoveryTest(t, nil)
			oldDeployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: "kube-apiserver", Namespace: f.recovery.Spec.ControlPlaneNamespace},
				Spec:       appsv1.DeploymentSpec{Replicas: tt.oldReplicas},
			}
			newDeployment := oldDeployment.DeepCopy()
			newDeployment.Spec.Replicas = tt.newReplicas
			f.onDeploymentUpdate(oldDeployment, newDeployment)
			if got := f.workqueue.Len() > 0; got != tt.wantQueued {
				t.Fatalf("queued recovery=%v, want %v", got, tt.wantQueued)
			}
		})
	}
}

func TestEnqueueRecoveryHandlesDeletionTombstone(t *testing.T) {
	f := newRecoveryTest(t, nil)
	key := f.recovery.Namespace + "/" + f.recovery.Name
	f.enqueueRecovery(cache.DeletedFinalStateUnknown{Key: key, Obj: f.recovery})
	if f.workqueue.Len() != 1 {
		t.Fatal("deleted recovery was not enqueued")
	}
	got, shutdown := f.workqueue.Get()
	defer f.workqueue.Done(got)
	if shutdown || got != key {
		t.Fatalf("expected recovery key %q, got %q (shutdown=%v)", key, got, shutdown)
	}
}

func TestPodEventsEnqueueMatchingRecoveries(t *testing.T) {
	for _, tt := range []struct {
		name       string
		app        string
		namespace  string
		wantQueued bool
		deleted    bool
	}{
		{name: "etcd", app: "etcd", wantQueued: true},
		{name: "kube-apiserver", app: "kube-apiserver", wantQueued: true},
		{name: "unrelated pod", app: "other"},
		{name: "unlabeled pod"},
		{name: "unrelated namespace", app: "etcd", namespace: "other"},
		{name: "delete etcd", app: "etcd", wantQueued: true, deleted: true},
		{name: "delete kube-apiserver", app: "kube-apiserver", wantQueued: true, deleted: true},
		{name: "delete unrelated pod", app: "other", deleted: true},
		{name: "delete unlabeled pod", deleted: true},
		{name: "delete unrelated namespace", app: "etcd", namespace: "other", deleted: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newRecoveryTest(t, nil)
			second := f.recovery.DeepCopy()
			second.Name = "second-recovery"
			second.Namespace = "another-recovery-namespace"
			unrelated := f.recovery.DeepCopy()
			unrelated.Name = "unrelated-recovery"
			unrelated.Spec.ControlPlaneNamespace = "unrelated-control-plane"
			unrelated.Spec.HostedClusterNamespace = "unrelated-hosted-cluster"
			for _, recovery := range []*hcprecoveryv1alpha1.HCPRecovery{second, unrelated} {
				if err := f.informers["HCPRecovery"].GetStore().Add(recovery); err != nil {
					t.Fatal(err)
				}
			}
			namespace := tt.namespace
			if namespace == "" {
				namespace = f.recovery.Spec.ControlPlaneNamespace
			}
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-pod", Namespace: namespace, ResourceVersion: "1",
					Labels: map[string]string{"app": tt.app},
				},
			}
			if _, err := f.kubeClient.CoreV1().Pods(namespace).Create(f.ctx, pod, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			f.startInformers("Pod")
			pod.ResourceVersion = "2"
			pod.Status.Phase = corev1.PodRunning
			if tt.deleted {
				if err := f.kubeClient.CoreV1().Pods(namespace).Delete(f.ctx, pod.Name, metav1.DeleteOptions{}); err != nil {
					t.Fatal(err)
				}
			} else if _, err := f.kubeClient.CoreV1().Pods(namespace).UpdateStatus(f.ctx, pod, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			if !tt.wantQueued {
				f.waitFor("pod event did not reach the cache", func() bool {
					cached, err := f.podLister.Pods(namespace).Get(pod.Name)
					if tt.deleted {
						return apierrors.IsNotFound(err)
					}
					return err == nil && cached.ResourceVersion == "2"
				})
				select {
				case <-f.ctx.Done():
					t.Fatal(f.ctx.Err())
				case <-time.After(100 * time.Millisecond):
				}
				if f.workqueue.Len() != 0 {
					t.Fatalf("unrelated pod event enqueued %d recoveries", f.workqueue.Len())
				}
				return
			}
			f.waitFor("pod event did not enqueue matching recoveries", func() bool { return f.workqueue.Len() >= 2 })
			if f.workqueue.Len() != 2 {
				t.Fatalf("expected two matching recoveries, got %d", f.workqueue.Len())
			}
			want := map[string]bool{
				f.recovery.Namespace + "/" + f.recovery.Name: true,
				second.Namespace + "/" + second.Name:         true,
			}
			for range 2 {
				key, shutdown := f.workqueue.Get()
				f.workqueue.Done(key)
				if shutdown || !want[key] {
					t.Fatalf("unexpected recovery key %q", key)
				}
				delete(want, key)
			}
		})
	}
}

func TestPodDeleteHandlesDeletionTombstone(t *testing.T) {
	f := newRecoveryTest(t, nil)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "etcd-0", Namespace: f.recovery.Spec.ControlPlaneNamespace,
		Labels: map[string]string{"app": "etcd"},
	}}
	f.onPodDelete(cache.DeletedFinalStateUnknown{Key: pod.Namespace + "/" + pod.Name, Obj: pod})
	if f.workqueue.Len() != 1 {
		t.Fatal("pod deletion tombstone did not enqueue recovery")
	}
	key, shutdown := f.workqueue.Get()
	defer f.workqueue.Done(key)
	if shutdown || key != f.recovery.Namespace+"/"+f.recovery.Name {
		t.Fatalf("unexpected recovery key %q (shutdown=%v)", key, shutdown)
	}
}

func TestRestoreEventsEnqueueMatchingRecoveries(t *testing.T) {
	f := newRecoveryTest(t, nil)
	second := f.recovery.DeepCopy()
	second.Name = "second-recovery"
	second.Namespace = "another-recovery-namespace"
	unrelated := f.recovery.DeepCopy()
	unrelated.Name = "unrelated-recovery"
	unrelated.Spec.ControlPlaneNamespace = "unrelated-control-plane"
	unrelated.Spec.HostedClusterNamespace = "unrelated-hosted-cluster"
	for _, recovery := range []*hcprecoveryv1alpha1.HCPRecovery{second, unrelated} {
		if err := f.informers["HCPRecovery"].GetStore().Add(recovery); err != nil {
			t.Fatal(err)
		}
	}
	f.startInformers("Restore")
	assertQueued := func(event string) {
		t.Helper()
		f.waitFor("Restore "+event+" did not enqueue matching recoveries", func() bool { return f.workqueue.Len() >= 2 })
		if f.workqueue.Len() != 2 {
			t.Fatalf("expected two matching recoveries after %s, got %d", event, f.workqueue.Len())
		}
		want := map[string]bool{
			f.recovery.Namespace + "/" + f.recovery.Name: true,
			second.Namespace + "/" + second.Name:         true,
		}
		for range 2 {
			key, shutdown := f.workqueue.Get()
			f.workqueue.Done(key)
			if shutdown || !want[key] {
				t.Fatalf("unexpected recovery key %q after %s", key, event)
			}
			delete(want, key)
		}
	}
	restore := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "velero.io/v1",
		"kind":       "Restore",
		"metadata": map[string]interface{}{
			"name": "restore", "namespace": backup.VeleroNamespace, "resourceVersion": "1",
			"labels": map[string]interface{}{
				restoreControlPlaneNamespaceLabel: f.recovery.Spec.ControlPlaneNamespace,
			},
		},
		"spec": map[string]interface{}{"backupName": f.recovery.Spec.VeleroBackupName},
	}}
	created, err := f.restoreClient.Create(f.ctx, restore, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	assertQueued("creation")
	if got, err := f.restoreLister.ByNamespace(backup.VeleroNamespace).Get(created.GetName()); err != nil || !reflect.DeepEqual(got, created) {
		t.Fatalf("created Restore did not reach lister: got=%v err=%v", got, err)
	}

	updated := created.DeepCopy()
	updated.SetResourceVersion("2")
	updated.Object["status"] = map[string]interface{}{"phase": "Completed"}
	if _, err := f.restoreClient.UpdateStatus(f.ctx, updated, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	assertQueued("status update")

	if err := f.restoreClient.Delete(f.ctx, created.GetName(), metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	assertQueued("deletion")
	if _, err := f.restoreLister.ByNamespace(backup.VeleroNamespace).Get(created.GetName()); !apierrors.IsNotFound(err) {
		t.Fatalf("expected deleted Restore to be absent from lister, got %v", err)
	}
}

func TestEnqueueRestoreHandlesLabelsAndTombstones(t *testing.T) {
	f := newRecoveryTest(t, nil)
	restore := &unstructured.Unstructured{}
	restore.SetName("restore")
	restore.SetNamespace(backup.VeleroNamespace)
	restore.SetLabels(map[string]string{restoreControlPlaneNamespaceLabel: f.recovery.Spec.ControlPlaneNamespace})
	unlabeled := restore.DeepCopy()
	unlabeled.SetLabels(nil)
	emptyLabel := restore.DeepCopy()
	emptyLabel.SetLabels(map[string]string{restoreControlPlaneNamespaceLabel: ""})
	unmatched := restore.DeepCopy()
	unmatched.SetLabels(map[string]string{restoreControlPlaneNamespaceLabel: "unrelated-control-plane"})
	original := restore.DeepCopy()
	for _, tt := range []struct {
		name       string
		obj        interface{}
		wantQueued bool
	}{
		{name: "labeled Restore", obj: restore, wantQueued: true},
		{name: "deletion tombstone", obj: cache.DeletedFinalStateUnknown{Key: "velero/restore", Obj: restore}, wantQueued: true},
		{name: "missing label", obj: unlabeled},
		{name: "empty label", obj: emptyLabel},
		{name: "unmatched label", obj: unmatched},
		{name: "unlabeled deletion tombstone", obj: cache.DeletedFinalStateUnknown{Key: "velero/restore", Obj: unlabeled}},
		{name: "malformed object", obj: struct{}{}},
		{name: "malformed tombstone", obj: cache.DeletedFinalStateUnknown{Key: "velero/restore", Obj: struct{}{}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f.enqueueRestore(tt.obj)
			wantCount := 0
			if tt.wantQueued {
				wantCount = 1
			}
			if f.workqueue.Len() != wantCount {
				t.Fatalf("expected %d queued recoveries, got %d", wantCount, f.workqueue.Len())
			}
			if tt.wantQueued {
				key, shutdown := f.workqueue.Get()
				defer f.workqueue.Done(key)
				if shutdown || key != f.recovery.Namespace+"/"+f.recovery.Name {
					t.Fatalf("unexpected recovery key %q (shutdown=%v)", key, shutdown)
				}
			}
		})
	}
	if !reflect.DeepEqual(restore, original) {
		t.Fatal("Restore event routing mutated its object")
	}
}

func TestRestoreLister(t *testing.T) {
	f := newRecoveryTest(t, nil)
	restore := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "velero.io/v1",
		"kind":       "Restore",
		"metadata":   map[string]interface{}{"name": "restore", "namespace": backup.VeleroNamespace},
	}}
	if err := f.informers["Restore"].GetStore().Add(restore); err != nil {
		t.Fatal(err)
	}
	if got, err := f.restoreLister.ByNamespace(restore.GetNamespace()).Get(restore.GetName()); err != nil || !reflect.DeepEqual(got, restore) {
		t.Fatalf("Restore lister not wired to informer: got=%v err=%v", got, err)
	}
}

func TestRunWaitsForCachesAndCancellation(t *testing.T) {
	for _, name := range []string{"HostedCluster", "HCPRecovery", "Backup", "Restore"} {
		t.Run(name, func(t *testing.T) {
			f := newRecoveryTest(t, nil)
			var ready []string
			for kind := range f.informers {
				if kind != name {
					ready = append(ready, kind)
				}
			}
			f.startInformers(ready...)
			unsynced := 0
			for _, hasSynced := range f.hasSynced {
				if !hasSynced() {
					unsynced++
				}
			}
			if unsynced != 1 {
				t.Fatalf("expected only the %s cache to block startup, got %d unsynced caches", name, unsynced)
			}
			done := make(chan error, 1)
			go func() { done <- f.Run(f.ctx) }()
			select {
			case err := <-done:
				t.Fatalf("controller returned before %s cache synced: %v", name, err)
			case <-time.After(100 * time.Millisecond):
			}
			for _, action := range f.client.Actions() {
				if action.GetVerb() == "patch" {
					t.Fatalf("validation ran before %s cache synced", name)
				}
			}
			f.startInformers(name)
			select {
			case err := <-done:
				t.Fatalf("controller returned before cancellation: %v", err)
			case <-time.After(100 * time.Millisecond):
			}
			if f.workqueue.ShuttingDown() {
				t.Fatal("controller shut down its queue while still running")
			}
			f.cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("controller did not stop after cancellation")
			}
			if !f.workqueue.ShuttingDown() {
				t.Fatal("controller did not shut down its queue")
			}
		})
	}
}
