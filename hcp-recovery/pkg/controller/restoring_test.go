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
	"errors"
	"strings"
	"testing"
	"time"

	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"

	hcprecoveryv1alpha1 "github.com/Azure/ARO-HCP/hcp-recovery/pkg/apis/hcprecovery/v1alpha1"
	"github.com/Azure/ARO-HCP/internal/backup"
)

func TestVeleroRestoreCreationPersistsNameAndWaitsForCache(t *testing.T) {
	f := newRecoveryTest(t, func(f *recoveryTest) {
		f.recovery.Status.Phase = hcprecoveryv1alpha1.RestoreStateRestoring
		f.recovery.Status.RestoreName = ""
	})
	recovery := f.recovery.DeepCopy()
	update := f.requireStepStatusUpdate(f.runStep(f.createVeleroRestoreStep, recovery))
	wantStatus := recovery.DeepCopy().Status
	wantStatus.RestoreName = "recovery-restore"
	if !apiequality.Semantic.DeepEqual(update.Status, wantStatus) || len(f.dynamicClient.Actions()) != 0 {
		t.Fatalf("expected restore name to be persisted before creation, got status=%#v actions=%v", update.Status, f.dynamicClient.Actions())
	}
	recovery.Status = update.Status
	f.client.ClearActions()
	f.startInformers("Restore")
	f.dynamicClient.ClearActions()

	if result, err := f.runStep(f.createVeleroRestoreStep, recovery); result.outcome != stepStop || err != nil {
		t.Fatalf("expected restore creation, got outcome=%v err=%v", result.outcome, err)
	}
	if len(f.client.Actions()) != 0 || len(f.dynamicClient.Actions()) != 1 {
		t.Fatalf("expected only a restore create, got status actions=%v restore actions=%v", f.client.Actions(), f.dynamicClient.Actions())
	}
	action, ok := f.dynamicClient.Actions()[0].(clienttesting.CreateAction)
	if !ok {
		t.Fatalf("expected restore create, got %#v", f.dynamicClient.Actions()[0])
	}
	restore, ok := action.GetObject().(*unstructured.Unstructured)
	if !ok || restore.GetName() != recovery.Status.RestoreName || restore.GetNamespace() != backup.VeleroNamespace ||
		restore.GetKind() != "Restore" || restore.GetAPIVersion() != "velero.io/v1" {
		t.Fatalf("unexpected created restore: %#v", action.GetObject())
	}
	f.waitFor("created restore should enqueue its recovery", func() bool {
		return f.workqueue.Len() == 1
	})
	key, shutdown := f.workqueue.Get()
	f.workqueue.Done(key)
	if shutdown || key != recovery.Namespace+"/"+recovery.Name {
		t.Fatalf("unexpected recovery enqueued for restore: key=%q shutdown=%v", key, shutdown)
	}
	update = f.requireStepStatusUpdate(f.runStep(f.createVeleroRestoreStep, recovery))
	if len(f.dynamicClient.Actions()) != 1 || update.Status.Conditions[0].Type != hcprecoveryv1alpha1.ConditionVeleroRestoreCreated ||
		update.Status.Conditions[0].Status != metav1.ConditionTrue || update.Status.Phase != hcprecoveryv1alpha1.RestoreStateRestoring {
		t.Fatalf("expected cached creation to update status without creating another restore: %#v", update.Status)
	}
}

func TestVeleroRestoreCreationCachedStatus(t *testing.T) {
	f := newRecoveryTest(t, func(f *recoveryTest) {
		f.recovery.Status.Phase = hcprecoveryv1alpha1.RestoreStateRestoring
	})
	restore := &unstructured.Unstructured{}
	restore.SetName(f.recovery.Status.RestoreName)
	restore.SetNamespace(backup.VeleroNamespace)
	if err := f.informers["Restore"].GetStore().Add(restore); err != nil {
		t.Fatal(err)
	}
	update := f.requireStepStatusUpdate(f.runStep(f.createVeleroRestoreStep, f.recovery))
	wantStatus := f.recovery.DeepCopy().Status
	wantStatus.Conditions = append([]metav1.Condition{{
		Type: hcprecoveryv1alpha1.ConditionVeleroRestoreCreated, Status: metav1.ConditionTrue,
		Reason: VeleroRestoreCreatedReason, Message: "Velero restore has been created",
		ObservedGeneration: f.recovery.Generation, LastTransitionTime: metav1.NewTime(testNow),
	}}, wantStatus.Conditions...)
	if !apiequality.Semantic.DeepEqual(update.Status, wantStatus) || len(f.dynamicClient.Actions()) != 0 {
		t.Fatalf("unexpected cached restore status: %#v; actions=%v", update.Status, f.dynamicClient.Actions())
	}

	writeErr := errors.New("status update failed")
	f.writeErr = writeErr
	if result, err := f.runStep(f.createVeleroRestoreStep, f.recovery); result.outcome != stepStop || !errors.Is(err, writeErr) {
		t.Fatalf("expected status write failure, got outcome=%v err=%v", result.outcome, err)
	}
	f.writeErr = nil
	f.client.ClearActions()
	recovery := f.recovery.DeepCopy()
	recovery.Status = update.Status
	f.TimeNow = func() time.Time { return testNow.Add(time.Hour) }
	if result, err := f.runStep(f.createVeleroRestoreStep, recovery); result.outcome != stepContinue || err != nil ||
		len(f.client.Actions()) != 0 || len(f.dynamicClient.Actions()) != 0 {
		t.Fatalf("unchanged creation status should not cause writes, got outcome=%v err=%v", result.outcome, err)
	}
}

func TestVeleroRestoreCreationFailures(t *testing.T) {
	restoreResource := schema.GroupVersionResource{Group: "velero.io", Version: "v1", Resource: "restores"}
	tests := []struct {
		name   string
		reason string
		err    error
	}{
		{name: "lookup failure", reason: VeleroRestoreLookupFailedReason, err: errors.New("cache lookup failed")},
		{name: "create failure", reason: VeleroRestoreCreateFailedReason, err: errors.New("restore create failed")},
		{name: "stale cache", reason: VeleroRestoreCreateFailedReason, err: apierrors.NewAlreadyExists(restoreResource.GroupResource(), "preserve-restore")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newRecoveryTest(t, func(f *recoveryTest) {
				f.recovery.Status.Phase = hcprecoveryv1alpha1.RestoreStateRestoring
			})
			if tt.reason == VeleroRestoreLookupFailedReason {
				f.restoreLister = failingRestoreLister{err: tt.err}
			} else {
				f.dynamicClient.PrependReactor("create", "restores", func(clienttesting.Action) (bool, runtime.Object, error) {
					return true, nil, tt.err
				})
			}
			f.assertStepFailureContract(f.createVeleroRestoreStep, stepFailureExpectation{
				conditionType: hcprecoveryv1alpha1.ConditionVeleroRestoreCreated,
				reason:        tt.reason,
				messageParts:  []string{backup.VeleroNamespace, f.recovery.Status.RestoreName, tt.err.Error()},
				retryError:    func(err error) bool { return errors.Is(err, tt.err) },
			})
		})
	}
}

func TestWaitForVeleroRestoreCompletion(t *testing.T) {
	tests := []struct {
		name        string
		phase       velerov1.RestorePhase
		wantPhase   hcprecoveryv1alpha1.RestoreState
		wantStatus  metav1.ConditionStatus
		wantReason  string
		wantFailure bool
	}{
		{name: "new", phase: velerov1.RestorePhaseNew},
		{name: "in progress", phase: velerov1.RestorePhaseInProgress},
		{name: "waiting for operations", phase: velerov1.RestorePhaseWaitingForPluginOperations},
		{name: "waiting after errors", phase: velerov1.RestorePhaseWaitingForPluginOperationsPartiallyFailed},
		{name: "finalizing", phase: velerov1.RestorePhaseFinalizing},
		{name: "finalizing after errors", phase: velerov1.RestorePhaseFinalizingPartiallyFailed},
		// Velero completion is recorded without advancing Restoring; the offline
		// etcd steps still remain.
		{name: "completed", phase: velerov1.RestorePhaseCompleted, wantPhase: hcprecoveryv1alpha1.RestoreStateRestoring,
			wantStatus: metav1.ConditionTrue, wantReason: "RestoreCompleted"},
		{name: "failed", phase: velerov1.RestorePhaseFailed, wantPhase: hcprecoveryv1alpha1.RestoreStateFailed,
			wantStatus: metav1.ConditionFalse, wantReason: VeleroRestoreFailedReason, wantFailure: true},
		{name: "failed validation", phase: velerov1.RestorePhaseFailedValidation, wantPhase: hcprecoveryv1alpha1.RestoreStateFailed,
			wantStatus: metav1.ConditionFalse, wantReason: VeleroRestoreFailedReason, wantFailure: true},
		{name: "partially failed", phase: velerov1.RestorePhasePartiallyFailed, wantPhase: hcprecoveryv1alpha1.RestoreStateFailed,
			wantStatus: metav1.ConditionFalse, wantReason: VeleroRestoreFailedReason, wantFailure: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newRecoveryTest(t, func(f *recoveryTest) {
				f.recovery.Status.Phase = hcprecoveryv1alpha1.RestoreStateRestoring
			})
			restore := &unstructured.Unstructured{Object: map[string]interface{}{
				"metadata": map[string]interface{}{"name": f.recovery.Status.RestoreName, "namespace": backup.VeleroNamespace},
				"status":   map[string]interface{}{"phase": string(tt.phase)},
			}}
			if err := f.informers["Restore"].GetStore().Add(restore); err != nil {
				t.Fatal(err)
			}
			original := restore.DeepCopy()
			if tt.wantPhase == "" {
				if result, err := f.runStep(f.waitForVeleroRestoreCompletion, f.recovery); result.outcome != stepStop ||
					err != nil || len(f.client.Actions()) != 0 {
					t.Fatalf("expected to stop and wait without writing, got outcome=%v err=%v actions=%v",
						result.outcome, err, f.client.Actions())
				}
				return
			}
			update := f.requireStepStatusUpdate(f.runStep(f.waitForVeleroRestoreCompletion, f.recovery))
			condition := meta.FindStatusCondition(update.Status.Conditions, hcprecoveryv1alpha1.ConditionVeleroRestoreCompleted)
			if condition == nil || condition.Status != tt.wantStatus || condition.Reason != tt.wantReason ||
				condition.ObservedGeneration != f.recovery.Generation || !condition.LastTransitionTime.Equal(&metav1.Time{Time: testNow}) {
				t.Fatalf("unexpected completion condition: %#v", condition)
			}
			wantStatus := f.recovery.DeepCopy().Status
			wantStatus.Phase = tt.wantPhase
			wantStatus.Conditions = append([]metav1.Condition{*condition}, wantStatus.Conditions...)
			if tt.wantFailure {
				completedAt := metav1.NewTime(testNow)
				wantStatus.CompletedAt = &completedAt
			}
			if !apiequality.Semantic.DeepEqual(update.Status, wantStatus) || !apiequality.Semantic.DeepEqual(restore, original) {
				t.Fatalf("unexpected status or mutated restore: status=%#v restore=%#v", update.Status, restore)
			}
			f.writeErr = errors.New("status update failed")
			if result, err := f.runStep(f.waitForVeleroRestoreCompletion, f.recovery); result.outcome == stepContinue ||
				!errors.Is(err, f.writeErr) {
				t.Fatalf("expected status write failure, got outcome=%v err=%v", result.outcome, err)
			}
		})
	}
}

func TestWaitForVeleroRestoreCompletionWaitsForCache(t *testing.T) {
	for _, name := range []string{"missing name", "not cached"} {
		t.Run(name, func(t *testing.T) {
			f := newRecoveryTest(t, func(f *recoveryTest) {
				f.recovery.Status.Phase = hcprecoveryv1alpha1.RestoreStateRestoring
				if name == "missing name" {
					f.recovery.Status.RestoreName = ""
				}
			})
			if result, err := f.runStep(f.waitForVeleroRestoreCompletion, f.recovery); result.outcome != stepStop ||
				err != nil || len(f.client.Actions()) != 0 {
				t.Fatalf("expected to stop and wait without writing, got outcome=%v err=%v actions=%v",
					result.outcome, err, f.client.Actions())
			}
		})
	}
}

func TestWaitForVeleroRestoreCompletionErrors(t *testing.T) {
	for _, name := range []string{"lookup failure", "wrong object type", "invalid restore"} {
		t.Run(name, func(t *testing.T) {
			f := newRecoveryTest(t, func(f *recoveryTest) {
				f.recovery.Status.Phase = hcprecoveryv1alpha1.RestoreStateRestoring
			})
			lookupErr := errors.New("cache lookup failed")
			if name == "lookup failure" {
				f.restoreLister = failingRestoreLister{err: lookupErr}
				f.assertStepFailureContract(f.waitForVeleroRestoreCompletion, stepFailureExpectation{
					conditionType: hcprecoveryv1alpha1.ConditionVeleroRestoreCompleted,
					reason:        VeleroRestoreLookupFailedReason,
					messageParts:  []string{backup.VeleroNamespace, f.recovery.Status.RestoreName, lookupErr.Error()},
					retryError:    func(err error) bool { return errors.Is(err, lookupErr) },
				})
				return
			}
			var obj runtime.Object = &velerov1.Restore{ObjectMeta: metav1.ObjectMeta{
				Name: f.recovery.Status.RestoreName, Namespace: backup.VeleroNamespace,
			}}
			if name == "invalid restore" {
				obj = &unstructured.Unstructured{Object: map[string]interface{}{
					"metadata": map[string]interface{}{"name": f.recovery.Status.RestoreName, "namespace": backup.VeleroNamespace},
					"status":   map[string]interface{}{"phase": int64(42)},
				}}
			}
			f.restoreLister = cache.NewGenericLister(f.informers["Restore"].GetIndexer(), schema.GroupResource{
				Group: "velero.io", Resource: "restores",
			})
			if err := f.informers["Restore"].GetStore().Add(obj); err != nil {
				t.Fatal(err)
			}
			update := f.requireStepStatusUpdate(f.runStep(f.waitForVeleroRestoreCompletion, f.recovery))
			condition := meta.FindStatusCondition(update.Status.Conditions, hcprecoveryv1alpha1.ConditionVeleroRestoreCompleted)
			if update.Status.Phase != hcprecoveryv1alpha1.RestoreStateFailed || update.Status.CompletedAt == nil ||
				condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != VeleroRestoreConversionFailedReason {
				t.Fatalf("expected terminal conversion failure, got %#v", update.Status)
			}
		})
	}
}

func TestReconcileRestoringWaitsForVeleroCompletion(t *testing.T) {
	for _, tt := range []struct {
		name      string
		phase     velerov1.RestorePhase
		wantPhase hcprecoveryv1alpha1.RestoreState
	}{
		{name: "in progress", phase: velerov1.RestorePhaseInProgress},
		{name: "completed", phase: velerov1.RestorePhaseCompleted, wantPhase: hcprecoveryv1alpha1.RestoreStateRestoring},
		{name: "failed", phase: velerov1.RestorePhaseFailed, wantPhase: hcprecoveryv1alpha1.RestoreStateFailed},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newRecoveryTest(t, func(f *recoveryTest) {
				f.recovery.Status.Phase = hcprecoveryv1alpha1.RestoreStateRestoring
			})
			recovery := f.recovery.DeepCopy()
			f.cacheVeleroRestore(tt.phase)
			recovery.Status = f.requireStepStatusUpdate(f.runStep(f.createVeleroRestoreStep, recovery)).Status
			f.client.ClearActions()

			if err := f.reconcile(f.ctx, recovery); err != nil {
				t.Fatal(err)
			}
			if len(f.kubeClient.Actions()) != 0 || len(f.hcClient.Actions()) != 0 || len(f.dynamicClient.Actions()) != 0 {
				t.Fatalf("Restoring must wait for Velero completion to be recorded before scaling down volumes: kube=%v hostedCluster=%v restore=%v",
					f.kubeClient.Actions(), f.hcClient.Actions(), f.dynamicClient.Actions())
			}
			if tt.wantPhase == "" {
				if len(f.client.Actions()) != 0 {
					t.Fatalf("unfinished restore must not advance or write status: %v", f.client.Actions())
				}
				return
			}
			if len(f.updates) != 1 {
				t.Fatalf("expected one status update, got %v", f.updates)
			}
			if tt.wantPhase == hcprecoveryv1alpha1.RestoreStateFailed {
				if f.updates[0].Status.Phase != tt.wantPhase {
					t.Fatalf("expected phase %s, got updates=%v", tt.wantPhase, f.updates)
				}
				return
			}
			if f.updates[0].Status.Phase != hcprecoveryv1alpha1.RestoreStateRestoring ||
				!meta.IsStatusConditionTrue(f.updates[0].Status.Conditions, hcprecoveryv1alpha1.ConditionVeleroRestoreCompleted) {
				t.Fatalf("expected the completion to be recorded before scaling down etcd, got %#v", f.updates[0].Status)
			}
			recovery.Status = f.updates[0].Status
			f.updates = nil
			if err := f.reconcile(f.ctx, recovery); err != nil {
				t.Fatal(err)
			}
			actions := f.kubeClient.Actions()
			if len(f.updates) != 0 || len(actions) != 1 || !actions[0].Matches("patch", "statefulsets") ||
				actions[0].GetSubresource() != "scale" {
				t.Fatalf("expected post-Velero scale-down without advancing Restoring, got updates=%v actions=%v", f.updates, actions)
			}
			patch := actions[0].(clienttesting.PatchAction)
			if patch.GetName() != "etcd" || string(patch.GetPatch()) != `{"spec":{"replicas":0}}` {
				t.Fatalf("unexpected post-Velero scale patch: %#v", patch)
			}
		})
	}
}

type failingRestoreLister struct {
	cache.GenericLister
	err error
}

func (l failingRestoreLister) ByNamespace(string) cache.GenericNamespaceLister {
	return l
}

func (l failingRestoreLister) Get(string) (runtime.Object, error) {
	return nil, l.err
}

func newPreparingVolumesTest(t *testing.T, configure func(*recoveryTest)) *recoveryTest {
	t.Helper()
	return newRecoveryTest(t, func(f *recoveryTest) {
		f.recovery.Status.Phase = hcprecoveryv1alpha1.RestoreStateRestoring
		if configure != nil {
			configure(f)
		}
	})
}

func TestValidateEtcdPodsGoneRecordsSuccessWithoutAdvancing(t *testing.T) {
	f := newPreparingVolumesTest(t, nil)

	update := f.requireStepStatusUpdate(f.runStep(f.validateEtcdPodsGone, f.recovery))
	if update.Status.Phase != f.recovery.Status.Phase {
		t.Fatalf("the phase advance belongs to runPhase, got %q", update.Status.Phase)
	}
	condition := meta.FindStatusCondition(update.Status.Conditions, hcprecoveryv1alpha1.ConditionEtcdPodsGone)
	if condition == nil || condition.Status != metav1.ConditionTrue ||
		condition.Reason != "EtcdPodsGone" || condition.ObservedGeneration != f.recovery.Generation ||
		!condition.LastTransitionTime.Time.Equal(testNow) {
		t.Fatalf("unexpected condition: %#v", condition)
	}

	settled := f.recovery.DeepCopy()
	settled.Status = update.Status
	f.client.ClearActions()
	if result, err := f.runStep(f.validateEtcdPodsGone, settled); result.outcome != stepContinue || err != nil || len(f.client.Actions()) != 0 {
		t.Fatalf("expected an idempotent second pass, got outcome=%v err=%v actions=%v", result.outcome, err, f.client.Actions())
	}
}

// The restored volumes are ReadWriteOnce, so a holder pod cannot mount one while an
// etcd pod still has it. Advancing early would leave the next step stuck on pods that
// never schedule.
func TestValidateEtcdPodsGoneWaitsForEtcdPods(t *testing.T) {
	t.Run("cached pod blocks the handoff", func(t *testing.T) {
		f := newPreparingVolumesTest(t, nil)
		f.addPods(f.etcdPod(0, true))

		f.assertStepFailureContract(f.validateEtcdPodsGone, stepFailureExpectation{
			conditionType: hcprecoveryv1alpha1.ConditionEtcdPodsGone,
			reason:        EtcdPodsRemainingReason,
			messageParts:  []string{"cache check shows 1 etcd pods still remaining", `etcd-0 (phase="Running", terminating=false)`},
			retryError: func(err error) bool {
				return err != nil && strings.Contains(err.Error(), "cache check shows 1 etcd pods still remaining after the post-restore scale-down")
			},
		})
	})

	// A cache that has not caught up would otherwise let the next step race the etcd
	// pods for the volumes.
	t.Run("live pod blocks the handoff even with an empty cache", func(t *testing.T) {
		f := newPreparingVolumesTest(t, nil)
		if _, err := f.kubeClient.CoreV1().Pods(f.recovery.Spec.ControlPlaneNamespace).
			Create(f.ctx, f.etcdPod(0, true), metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		f.kubeClient.ClearActions()

		f.assertStepFailureContract(f.validateEtcdPodsGone, stepFailureExpectation{
			conditionType: hcprecoveryv1alpha1.ConditionEtcdPodsGone,
			reason:        EtcdPodsRemainingReason,
			messageParts:  []string{"live check shows 1 etcd pods still remaining", `etcd-0 (phase="Running", terminating=false)`},
			retryError: func(err error) bool {
				return err != nil && strings.Contains(err.Error(), "live check shows 1 etcd pods still remaining after the post-restore scale-down")
			},
		})
	})
}

func TestValidateEtcdPodsGoneLiveListFailure(t *testing.T) {
	listErr := errors.New("pod list failed")
	f := newPreparingVolumesTest(t, nil)
	f.kubeClient.PrependReactor("list", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, listErr
	})

	f.assertStepFailureContract(f.validateEtcdPodsGone, stepFailureExpectation{
		conditionType: hcprecoveryv1alpha1.ConditionEtcdPodsGone,
		reason:        EtcdPodsLiveListFailedReason,
		messageParts:  []string{"failed to list etcd pods from API server", listErr.Error()},
		retryError:    func(err error) bool { return errors.Is(err, listErr) },
	})
}

// The worker pods carry their own app labels precisely so they do not answer the
// app=etcd selectors the preparation steps rely on.
func TestValidateEtcdPodsGoneIgnoresRecoveryWorkerPods(t *testing.T) {
	f := newPreparingVolumesTest(t, nil)
	f.addPods(
		f.workerPod(etcdHolderPodName(0), etcdHolderApp, f.recovery.Spec.ControlPlaneNamespace, corev1.PodRunning, ""),
		f.workerPod(etcdRestorePodName(0), etcdRestoreApp, f.recovery.Spec.ControlPlaneNamespace, corev1.PodSucceeded, ""),
	)

	update := f.requireStepStatusUpdate(f.runStep(f.validateEtcdPodsGone, f.recovery))
	if !meta.IsStatusConditionTrue(update.Status.Conditions, hcprecoveryv1alpha1.ConditionEtcdPodsGone) {
		t.Fatalf("recovery worker pods must not look like running etcd: %#v", update.Status)
	}
}

func TestRestoringScaleDownEtcdPreservesPreparationCondition(t *testing.T) {
	f := newRecoveryTest(t, func(f *recoveryTest) {
		f.recovery.Status.Phase = hcprecoveryv1alpha1.RestoreStateRestoring
		f.sts.Spec.Replicas = ptr.To[int32](3)
		f.sts.Status.Replicas = 3
	})
	recovery := f.recovery.DeepCopy()
	preparationCondition := metav1.Condition{
		Type: hcprecoveryv1alpha1.ConditionEtcdScaledToZero, Status: metav1.ConditionTrue,
		Reason: "EtcdScaledToZeroReplicas", Message: "The etcd statefulset is scaled to zero replicas",
		ObservedGeneration: recovery.Generation, LastTransitionTime: *recovery.Status.StartedAt,
	}
	recovery.Status.Conditions = append(recovery.Status.Conditions, preparationCondition)
	f.cacheVeleroRestore(velerov1.RestorePhaseCompleted)
	for _, step := range []phaseStep{f.createVeleroRestoreStep, f.waitForVeleroRestoreCompletion} {
		recovery.Status = f.requireStepStatusUpdate(f.runStep(step, recovery)).Status
	}
	f.client.ClearActions()
	if err := f.reconcile(f.ctx, recovery); err != nil {
		t.Fatal(err)
	}
	actions := f.kubeClient.Actions()
	if len(actions) != 1 || actions[0].GetVerb() != "patch" || actions[0].GetSubresource() != "scale" ||
		len(f.client.Actions()) != 0 {
		t.Fatalf("preparation condition must not skip the second scale-down: kube=%v recovery=%v", actions, f.client.Actions())
	}
	patch := actions[0].(clienttesting.PatchAction)
	if patch.GetName() != "etcd" || string(patch.GetPatch()) != `{"spec":{"replicas":0}}` {
		t.Fatalf("unexpected etcd scale patch: %#v", patch)
	}
	etcd := f.sts.DeepCopy()
	etcd.Spec.Replicas = ptr.To[int32](0)
	if err := f.informers["StatefulSet"].GetStore().Update(etcd); err != nil {
		t.Fatal(err)
	}
	f.kubeClient.ClearActions()
	if err := f.reconcile(f.ctx, recovery); err != nil || len(f.client.Actions()) != 0 {
		t.Fatalf("must wait for observed zero replicas before recording success: err=%v actions=%v", err, f.client.Actions())
	}
	etcd = etcd.DeepCopy()
	etcd.Status.Replicas = 0
	if err := f.informers["StatefulSet"].GetStore().Update(etcd); err != nil {
		t.Fatal(err)
	}
	if err := f.reconcile(f.ctx, recovery); err != nil {
		t.Fatal(err)
	}
	if len(f.updates) != 1 {
		t.Fatalf("expected post-restore scale-down status update, got %v", f.updates)
	}
	recovery.Status = f.updates[0].Status
	if !meta.IsStatusConditionTrue(recovery.Status.Conditions, hcprecoveryv1alpha1.ConditionEtcdScaledToZeroAfterVeleroRestore) ||
		!apiequality.Semantic.DeepEqual(meta.FindStatusCondition(recovery.Status.Conditions, preparationCondition.Type), &preparationCondition) {
		t.Fatalf("expected separate post-restore condition and unchanged preparation condition: %v", recovery.Status.Conditions)
	}
	// With no etcd pods left holding the restored volumes, snapshot distribution
	// follows in the same phase.
	f.client.ClearActions()
	f.kubeClient.ClearActions()
	f.updates = nil
	if err := f.reconcile(f.ctx, recovery); err != nil {
		t.Fatal(err)
	}
	for _, action := range f.kubeClient.Actions() {
		if action.GetVerb() != "list" {
			t.Fatalf("completed scale-down must not mutate anything further: %#v", action)
		}
	}
	if len(f.updates) != 1 {
		t.Fatalf("expected the validation to be recorded once, got %v", f.updates)
	}
	if f.updates[0].Status.Phase != hcprecoveryv1alpha1.RestoreStateRestoring ||
		!meta.IsStatusConditionTrue(f.updates[0].Status.Conditions, hcprecoveryv1alpha1.ConditionEtcdPodsGone) {
		t.Fatalf("expected the etcd pods to be recorded gone, got %#v", f.updates[0].Status)
	}
	recovery.Status = f.updates[0].Status
	f.updates = nil
	if err := f.reconcile(f.ctx, recovery); err != nil {
		t.Fatal(err)
	}
	if len(f.updates) != 0 {
		t.Fatalf("snapshot distribution must remain in Restoring, got %v", f.updates)
	}
	if created := f.createdPods(); len(created) != 1 || created[0].Name != etcdHolderPodName(0) {
		t.Fatalf("expected snapshot distribution to create the first holder pod, got %v", created)
	}
}

const testSnapshotSHA = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func newDistributingTest(t *testing.T, configure func(*recoveryTest)) *recoveryTest {
	t.Helper()
	return newRecoveryTest(t, func(f *recoveryTest) {
		f.recovery.Status.Phase = hcprecoveryv1alpha1.RestoreStateRestoring
		if configure != nil {
			configure(f)
		}
	})
}

func (f *recoveryTest) holderPods(phase corev1.PodPhase) []*corev1.Pod {
	pods := make([]*corev1.Pod, 0, testEtcdReplicas)
	for i := int32(0); i < testEtcdReplicas; i++ {
		pods = append(pods, f.workerPod(etcdHolderPodName(i), etcdHolderApp,
			f.recovery.Spec.ControlPlaneNamespace, phase, ""))
	}
	return pods
}

// One pod per volume is the only way to reach volumes pinned to different availability
// zones, and they are created one per reconcile so each loop makes a single change.
func TestCreateEtcdHolderPodsOnePerReconcile(t *testing.T) {
	f := newDistributingTest(t, nil)

	for i := int32(0); i < testEtcdReplicas; i++ {
		result, err := f.runStep(f.createEtcdHolderPods, f.recovery)
		if result.outcome != stepStop || err != nil {
			t.Fatalf("expected holder pod %d to be created, got outcome=%v err=%v", i, result.outcome, err)
		}
		created := f.createdPods()
		if len(created) != 1 || created[0].Name != etcdHolderPodName(i) {
			t.Fatalf("expected exactly one create for %s, got %v", etcdHolderPodName(i), created)
		}
		f.kubeClient.ClearActions()
		if err := f.informers["Pod"].GetStore().Add(created[0]); err != nil {
			t.Fatal(err)
		}
	}

	if result, err := f.runStep(f.createEtcdHolderPods, f.recovery); result.outcome != stepContinue || err != nil ||
		len(f.kubeClient.Actions()) != 0 {
		t.Fatalf("expected the step to fall through once every volume is held, got outcome=%v err=%v actions=%v",
			result.outcome, err, f.kubeClient.Actions())
	}
}

// The pod has to be able to read etcd's files and land where its volume already is, so
// it reuses the StatefulSet's identity and scheduling but not its anti-affinity.
func TestEtcdHolderPodSpec(t *testing.T) {
	f := newDistributingTest(t, nil)
	if _, err := f.createEtcdHolderPods(f.ctx, f.recovery); err != nil {
		t.Fatal(err)
	}
	created := f.createdPods()
	if len(created) != 1 {
		t.Fatalf("expected one pod, got %v", created)
	}
	pod := created[0]

	if pod.Namespace != f.recovery.Spec.ControlPlaneNamespace {
		t.Fatalf("holder pods must live beside the volumes they mount, got namespace %q", pod.Namespace)
	}
	if pod.Labels["app"] == etcd {
		t.Fatal("holder pods must not answer the app=etcd selector used by the preparation steps")
	}
	if pod.Labels["app"] != etcdHolderApp || pod.Labels[recoveryNameLabel] != f.recovery.Name {
		t.Fatalf("holder pod is not routable back to its recovery: %v", pod.Labels)
	}
	if ptr.Deref(pod.Spec.SecurityContext.RunAsUser, -1) != 1000 ||
		ptr.Deref(pod.Spec.SecurityContext.FSGroup, -1) != 2000 {
		t.Fatalf("holder pod must reuse etcd's pod identity: %#v", pod.Spec.SecurityContext)
	}
	if pod.Spec.NodeSelector["workload"] != "control-plane" || len(pod.Spec.Tolerations) != 1 ||
		pod.Spec.PriorityClassName != "hypershift-control-plane" {
		t.Fatalf("holder pod must inherit etcd's scheduling constraints: %#v", pod.Spec)
	}
	if pod.Spec.Affinity != nil {
		t.Fatal("holder pod must not inherit etcd's anti-affinity, which would make it unschedulable")
	}
	if ptr.Deref(pod.Spec.AutomountServiceAccountToken, true) {
		t.Fatal("holder pod only touches its volume and must not carry API credentials")
	}
	if len(pod.Spec.Containers) != 1 || pod.Spec.Containers[0].Image != testEtcdImage {
		t.Fatalf("holder pod must use the captured etcd image: %#v", pod.Spec.Containers)
	}
	if got := pod.Spec.Containers[0].VolumeMounts; len(got) != 1 || got[0].MountPath != etcdDataMountPath {
		t.Fatalf("holder pod must mount the volume where the snapshot lives: %#v", got)
	}
	claim := pod.Spec.Volumes[0].PersistentVolumeClaim
	if claim == nil || claim.ClaimName != "data-etcd-0" {
		t.Fatalf("holder pod must mount its own member's claim: %#v", pod.Spec.Volumes)
	}
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Fatalf("unexpected restart policy %q", pod.Spec.RestartPolicy)
	}
}

func TestCreateEtcdHolderPodsFailures(t *testing.T) {
	t.Run("create failure retries", func(t *testing.T) {
		createErr := errors.New("pod create rejected")
		f := newDistributingTest(t, nil)
		f.kubeClient.PrependReactor("create", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, createErr
		})
		f.assertStepFailureContract(f.createEtcdHolderPods, stepFailureExpectation{
			conditionType: hcprecoveryv1alpha1.ConditionEtcdHolderPodsReady,
			reason:        EtcdHolderPodCreateFailedReason,
			messageParts:  []string{"failed to create holder pod", createErr.Error()},
			retryError:    func(err error) bool { return errors.Is(err, createErr) },
		})
	})

	// A pod created just before the controller restarted is already doing its job.
	t.Run("an already existing pod is accepted", func(t *testing.T) {
		f := newDistributingTest(t, nil)
		f.kubeClient.PrependReactor("create", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewAlreadyExists(corev1.Resource("pods"), etcdHolderPodName(0))
		})
		if result, err := f.runStep(f.createEtcdHolderPods, f.recovery); result.outcome != stepStop || err != nil ||
			len(f.client.Actions()) != 0 {
			t.Fatalf("expected the existing pod to be accepted silently, got outcome=%v err=%v", result.outcome, err)
		}
	})
}

func TestWaitForHolderPodsReady(t *testing.T) {
	t.Run("records readiness once every volume is held", func(t *testing.T) {
		f := newDistributingTest(t, nil)
		f.addPods(f.holderPods(corev1.PodRunning)...)

		update := f.requireStepStatusUpdate(f.runStep(f.waitForHolderPodsReady, f.recovery))
		condition := meta.FindStatusCondition(update.Status.Conditions, hcprecoveryv1alpha1.ConditionEtcdHolderPodsReady)
		if condition == nil || condition.Status != metav1.ConditionTrue {
			t.Fatalf("unexpected condition: %#v", condition)
		}

		settled := f.recovery.DeepCopy()
		settled.Status = update.Status
		f.client.ClearActions()
		if result, err := f.runStep(f.waitForHolderPodsReady, settled); result.outcome != stepContinue || err != nil ||
			len(f.client.Actions()) != 0 {
			t.Fatalf("expected an idempotent second pass, got outcome=%v err=%v", result.outcome, err)
		}
	})

	t.Run("waits without polling while pods start", func(t *testing.T) {
		f := newDistributingTest(t, nil)
		pods := f.holderPods(corev1.PodRunning)
		pods[2].Status.Conditions[0].Status = corev1.ConditionFalse
		f.addPods(pods...)

		update := f.requireStepStatusUpdate(f.runStep(f.waitForHolderPodsReady, f.recovery))
		condition := meta.FindStatusCondition(update.Status.Conditions, hcprecoveryv1alpha1.ConditionEtcdHolderPodsReady)
		if condition == nil || condition.Status != metav1.ConditionFalse ||
			condition.Reason != EtcdHolderPodsStartingReason ||
			!strings.Contains(condition.Message, "1 of 3") {
			t.Fatalf("unexpected condition: %#v", condition)
		}

		// Pod readiness raises informer events, so the step must park rather than
		// return an error to get itself re-queued.
		settled := f.recovery.DeepCopy()
		settled.Status = update.Status
		f.client.ClearActions()
		if result, err := f.runStep(f.waitForHolderPodsReady, settled); result.outcome != stepStop || err != nil ||
			len(f.client.Actions()) != 0 {
			t.Fatalf("expected a silent wait, got outcome=%v err=%v actions=%v", result.outcome, err, f.client.Actions())
		}
	})

	// A sleeping holder pod can still be evicted or drained away. Recreating it keeps a
	// recoverable restore alive instead of failing it.
	t.Run("a stopped holder pod is recreated", func(t *testing.T) {
		f := newDistributingTest(t, nil)
		pods := f.holderPods(corev1.PodRunning)
		pods[1] = f.workerPod(etcdHolderPodName(1), etcdHolderApp,
			f.recovery.Spec.ControlPlaneNamespace, corev1.PodFailed, "evicted")
		f.addPods(pods...)

		result, err := f.runStep(f.waitForHolderPodsReady, f.recovery)
		if result.outcome != stepStop || err != nil {
			t.Fatalf("expected the stopped pod to be deleted, got outcome=%v err=%v", result.outcome, err)
		}
		if deleted := f.deletedPodNames(); len(deleted) != 1 || deleted[0] != etcdHolderPodName(1) {
			t.Fatalf("expected only the stopped pod to be deleted, got %v", deleted)
		}
		if len(f.client.Actions()) != 0 {
			t.Fatalf("deleting the pod is the action for this loop, no status write expected: %v", f.client.Actions())
		}
	})
}

func TestCreateSnapshotTransferPodSpec(t *testing.T) {
	f := newDistributingTest(t, nil)
	f.addPods(f.holderPods(corev1.PodRunning)...)

	if result, err := f.runStep(f.createSnapshotTransferPod, f.recovery); result.outcome != stepStop || err != nil {
		t.Fatalf("expected the transfer pod to be created, got outcome=%v err=%v", result.outcome, err)
	}
	created := f.createdPods()
	if len(created) != 1 {
		t.Fatalf("expected one pod, got %v", created)
	}
	pod := created[0]

	// It mounts no etcd volume, so it can live beside the HCPRecovery and be garbage
	// collected with it.
	if pod.Namespace != f.recovery.Namespace {
		t.Fatalf("transfer pod must run in the controller namespace, got %q", pod.Namespace)
	}
	if len(pod.OwnerReferences) != 1 || pod.OwnerReferences[0].Name != f.recovery.Name {
		t.Fatalf("transfer pod must be owned by its recovery: %#v", pod.OwnerReferences)
	}
	if pod.Spec.ServiceAccountName != testWorkerServiceAccount {
		t.Fatalf("transfer pod must use the worker identity, got %q", pod.Spec.ServiceAccountName)
	}
	if pod.Spec.Containers[0].Image != testWorkerImage {
		t.Fatalf("transfer pod must use the kubectl-bearing image, got %q", pod.Spec.Containers[0].Image)
	}
	if pod.Spec.Containers[0].TerminationMessagePolicy != corev1.TerminationMessageFallbackToLogsOnError {
		t.Fatal("the controller reads this pod's result from its termination message")
	}
	for _, mount := range pod.Spec.Containers[0].VolumeMounts {
		if mount.MountPath == etcdDataMountPath {
			t.Fatal("the transfer pod must not mount an etcd volume; it cannot reach them all")
		}
	}

	env := map[string]string{}
	for _, e := range pod.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	if env["CANONICAL_POD"] != etcdHolderPodName(0) || env["CANONICAL_MEMBER"] != "etcd-0" {
		t.Fatalf("etcd-0 is the canonical snapshot source: %v", env)
	}
	if env["HOLDER_PODS"] != "etcd-holder-0 etcd-holder-1 etcd-holder-2" {
		t.Fatalf("every volume must be a copy target: %q", env["HOLDER_PODS"])
	}
	if env["SNAPSHOT_PATH"] != backup.EtcdSnapshotPath {
		t.Fatalf("the snapshot path must match where the backup hook wrote it: %q", env["SNAPSHOT_PATH"])
	}
	if env["HCP_NS"] != f.recovery.Spec.ControlPlaneNamespace {
		t.Fatalf("unexpected target namespace %q", env["HCP_NS"])
	}
}

func TestWaitForSnapshotTransfer(t *testing.T) {
	result := `{"sourceMember":"etcd-0","sha256":"` + testSnapshotSHA + `","revision":4242}`

	t.Run("records the snapshot every volume now holds", func(t *testing.T) {
		f := newDistributingTest(t, nil)
		f.addPods(f.workerPod(snapshotTransferPodName(f.recovery), snapshotTransferApp,
			f.recovery.Namespace, corev1.PodSucceeded, result))

		update := f.requireStepStatusUpdate(f.runStep(f.waitForSnapshotTransfer, f.recovery))
		want := hcprecoveryv1alpha1.EtcdSnapshotDetails{SourceMember: "etcd-0", SHA256: testSnapshotSHA, Revision: 4242}
		if update.Status.EtcdSnapshot == nil || *update.Status.EtcdSnapshot != want {
			t.Fatalf("expected the reported snapshot to be recorded, got %#v", update.Status.EtcdSnapshot)
		}
		condition := meta.FindStatusCondition(update.Status.Conditions, hcprecoveryv1alpha1.ConditionEtcdSnapshotDistributed)
		if condition == nil || condition.Status != metav1.ConditionTrue ||
			!strings.Contains(condition.Message, testSnapshotSHA) {
			t.Fatalf("unexpected condition: %#v", condition)
		}
	})

	t.Run("waits while the copy runs", func(t *testing.T) {
		f := newDistributingTest(t, nil)
		f.addPods(f.workerPod(snapshotTransferPodName(f.recovery), snapshotTransferApp,
			f.recovery.Namespace, corev1.PodRunning, ""))

		update := f.requireStepStatusUpdate(f.runStep(f.waitForSnapshotTransfer, f.recovery))
		condition := meta.FindStatusCondition(update.Status.Conditions, hcprecoveryv1alpha1.ConditionEtcdSnapshotDistributed)
		if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != SnapshotTransferRunningReason {
			t.Fatalf("unexpected condition: %#v", condition)
		}
		if update.Status.EtcdSnapshot != nil {
			t.Fatal("no snapshot may be recorded before the copy is verified")
		}
	})

	// The pod exhausts its own retries and refuses to exit zero unless every volume
	// matches, so a failure means the restore cannot safely proceed.
	t.Run("a failed copy ends the recovery", func(t *testing.T) {
		f := newDistributingTest(t, nil)
		f.addPods(f.workerPod(snapshotTransferPodName(f.recovery), snapshotTransferApp,
			f.recovery.Namespace, corev1.PodFailed, "etcd-holder-2: snapshot checksum mismatch"))

		f.assertStepFailureContract(f.waitForSnapshotTransfer, stepFailureExpectation{
			conditionType: hcprecoveryv1alpha1.ConditionEtcdSnapshotDistributed,
			reason:        SnapshotTransferFailedReason,
			messageParts:  []string{"Snapshot transfer pod", "snapshot checksum mismatch"},
			permanent:     true,
		})
	})

	t.Run("an unreadable result ends the recovery", func(t *testing.T) {
		f := newDistributingTest(t, nil)
		f.addPods(f.workerPod(snapshotTransferPodName(f.recovery), snapshotTransferApp,
			f.recovery.Namespace, corev1.PodSucceeded, "not json"))

		f.assertStepFailureContract(f.waitForSnapshotTransfer, stepFailureExpectation{
			conditionType: hcprecoveryv1alpha1.ConditionEtcdSnapshotDistributed,
			reason:        SnapshotResultInvalidReason,
			messageParts:  []string{"not a snapshot result"},
			permanent:     true,
		})
	})

	t.Run("an incomplete result ends the recovery", func(t *testing.T) {
		f := newDistributingTest(t, nil)
		f.addPods(f.workerPod(snapshotTransferPodName(f.recovery), snapshotTransferApp,
			f.recovery.Namespace, corev1.PodSucceeded, `{"revision":1}`))

		f.assertStepFailureContract(f.waitForSnapshotTransfer, stepFailureExpectation{
			conditionType: hcprecoveryv1alpha1.ConditionEtcdSnapshotDistributed,
			reason:        SnapshotResultInvalidReason,
			messageParts:  []string{"incomplete snapshot result"},
			permanent:     true,
		})
	})

	// Once recorded, the copy is not repeated even if the pod is gone.
	t.Run("a recorded snapshot survives the pod", func(t *testing.T) {
		f := newDistributingTest(t, func(f *recoveryTest) {
			f.recovery.Status.EtcdSnapshot = &hcprecoveryv1alpha1.EtcdSnapshotDetails{
				SourceMember: "etcd-0", SHA256: testSnapshotSHA, Revision: 4242,
			}
			f.recovery.Status.Conditions = append(f.recovery.Status.Conditions, metav1.Condition{
				Type: hcprecoveryv1alpha1.ConditionEtcdSnapshotDistributed, Status: metav1.ConditionTrue,
				Reason:             "EtcdSnapshotDistributed",
				Message:            distributedMessage(hcprecoveryv1alpha1.EtcdSnapshotDetails{SourceMember: "etcd-0", SHA256: testSnapshotSHA, Revision: 4242}),
				ObservedGeneration: 1, LastTransitionTime: metav1.NewTime(testNow),
			})
		})

		if result, err := f.runStep(f.createSnapshotTransferPod, f.recovery); result.outcome != stepContinue || err != nil ||
			len(f.kubeClient.Actions()) != 0 {
			t.Fatalf("a recorded snapshot must not start another copy: outcome=%v err=%v actions=%v",
				result.outcome, err, f.kubeClient.Actions())
		}
		if result, err := f.runStep(f.waitForSnapshotTransfer, f.recovery); result.outcome != stepContinue || err != nil ||
			len(f.client.Actions()) != 0 {
			t.Fatalf("expected the step to fall through, got outcome=%v err=%v", result.outcome, err)
		}
	})
}

// Both sets of pods mount the same ReadWriteOnce volumes, so the restore pods cannot
// schedule until the holders are gone.
func TestDeleteDistributionPodsClearsVolumesBeforeAdvancing(t *testing.T) {
	f := newDistributingTest(t, func(f *recoveryTest) {
		f.recovery.Status.EtcdSnapshot = &hcprecoveryv1alpha1.EtcdSnapshotDetails{
			SourceMember: "etcd-0", SHA256: testSnapshotSHA, Revision: 4242,
		}
	})
	pods := f.holderPods(corev1.PodRunning)
	f.addPods(pods...)
	f.addPods(f.workerPod(snapshotTransferPodName(f.recovery), snapshotTransferApp,
		f.recovery.Namespace, corev1.PodSucceeded, ""))

	for i := int32(0); i < testEtcdReplicas; i++ {
		result, err := f.runStep(f.deleteDistributionPods, f.recovery)
		if result.outcome != stepStop || err != nil {
			t.Fatalf("expected holder pod %d to be deleted, got outcome=%v err=%v", i, result.outcome, err)
		}
		deleted := f.deletedPodNames()
		if len(deleted) != 1 {
			t.Fatalf("expected one deletion per reconcile, got %v", deleted)
		}
		if err := f.informers["Pod"].GetStore().Delete(pods[i]); err != nil {
			t.Fatal(err)
		}
		f.kubeClient.ClearActions()
	}

	// Only the transfer pod is left; it holds no volume.
	if result, err := f.runStep(f.deleteDistributionPods, f.recovery); result.outcome != stepStop || err != nil {
		t.Fatalf("expected the transfer pod to be reaped, got outcome=%v err=%v", result.outcome, err)
	}
	if deleted := f.deletedPodNames(); len(deleted) != 1 || deleted[0] != snapshotTransferPodName(f.recovery) {
		t.Fatalf("expected the transfer pod to be deleted, got %v", deleted)
	}
}

func TestDeleteDistributionPodsWaitsForLiveHolders(t *testing.T) {
	f := newDistributingTest(t, nil)
	// Nothing in the cache, but the API server still reports a holder: advancing here
	// would leave the restore pods unschedulable.
	for _, pod := range f.holderPods(corev1.PodRunning) {
		if _, err := f.kubeClient.CoreV1().Pods(pod.Namespace).Create(f.ctx, pod, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	f.kubeClient.ClearActions()

	f.assertStepFailureContract(f.deleteDistributionPods, stepFailureExpectation{
		conditionType: hcprecoveryv1alpha1.ConditionEtcdSnapshotDistributed,
		reason:        DistributionPodsRemainingReason,
		messageParts:  []string{"waiting for etcd holder pods"},
		retryError: func(err error) bool {
			return err != nil && strings.Contains(err.Error(), "waiting for etcd holder pods")
		},
	})
}

func newRestoringTest(t *testing.T, configure func(*recoveryTest)) *recoveryTest {
	t.Helper()
	return newRecoveryTest(t, func(f *recoveryTest) {
		f.recovery.Status.Phase = hcprecoveryv1alpha1.RestoreStateRestoring
		f.recovery.Status.EtcdSnapshot = &hcprecoveryv1alpha1.EtcdSnapshotDetails{
			SourceMember: "etcd-0", SHA256: testSnapshotSHA, Revision: 4242,
		}
		if configure != nil {
			configure(f)
		}
	})
}

func (f *recoveryTest) restorePods(phase corev1.PodPhase) []*corev1.Pod {
	pods := make([]*corev1.Pod, 0, testEtcdReplicas)
	for i := int32(0); i < testEtcdReplicas; i++ {
		pods = append(pods, f.workerPod(etcdRestorePodName(i), etcdRestoreApp,
			f.recovery.Spec.ControlPlaneNamespace, phase, ""))
	}
	return pods
}

func TestCreateEtcdRestorePodsOnePerReconcile(t *testing.T) {
	f := newRestoringTest(t, nil)

	for i := int32(0); i < testEtcdReplicas; i++ {
		result, err := f.runStep(f.createEtcdRestorePods, f.recovery)
		if result.outcome != stepStop || err != nil {
			t.Fatalf("expected restore pod %d to be created, got outcome=%v err=%v", i, result.outcome, err)
		}
		created := f.createdPods()
		if len(created) != 1 || created[0].Name != etcdRestorePodName(i) {
			t.Fatalf("expected exactly one create for %s, got %v", etcdRestorePodName(i), created)
		}
		f.kubeClient.ClearActions()
		if err := f.informers["Pod"].GetStore().Add(created[0]); err != nil {
			t.Fatal(err)
		}
	}

	if result, err := f.runStep(f.createEtcdRestorePods, f.recovery); result.outcome != stepContinue || err != nil ||
		len(f.kubeClient.Actions()) != 0 {
		t.Fatalf("expected the step to fall through once every member has a pod, got outcome=%v err=%v", result.outcome, err)
	}
}

// Members only form one cluster if they are restored with the same membership list,
// the same token and the same revision bump; only --name and the peer URL may differ.
func TestEtcdRestorePodSpecIsIdenticalExceptMemberIdentity(t *testing.T) {
	f := newRestoringTest(t, nil)

	shared := map[string]string{}
	for i := int32(0); i < testEtcdReplicas; i++ {
		if _, err := f.runStep(f.createEtcdRestorePods, f.recovery); err != nil {
			t.Fatal(err)
		}
		created := f.createdPods()
		pod := created[len(created)-1]
		if err := f.informers["Pod"].GetStore().Add(pod); err != nil {
			t.Fatal(err)
		}

		env := map[string]string{}
		for _, e := range pod.Spec.Containers[0].Env {
			env[e.Name] = e.Value
		}
		if env["ETCD_MEMBER"] != etcdMemberName(i) {
			t.Fatalf("pod %d restores as %q", i, env["ETCD_MEMBER"])
		}
		wantPeer := "https://etcd-" + string(rune('0'+i)) + ".etcd-discovery.hosted-cluster-cluster.svc:2380"
		if env["INITIAL_ADVERTISE_PEER_URLS"] != wantPeer {
			t.Fatalf("pod %d advertises %q, want %q", i, env["INITIAL_ADVERTISE_PEER_URLS"], wantPeer)
		}
		if env["SNAPSHOT_PATH"] != backup.EtcdSnapshotPath {
			t.Fatalf("pod %d reads %q, but the snapshot was distributed to %q", i, env["SNAPSHOT_PATH"], backup.EtcdSnapshotPath)
		}
		claim := pod.Spec.Volumes[0].PersistentVolumeClaim
		if claim == nil || claim.ClaimName != etcdPVCPrefix+string(rune('0'+i)) {
			t.Fatalf("pod %d must rebuild its own member's volume: %#v", i, pod.Spec.Volumes)
		}

		for _, key := range []string{"INITIAL_CLUSTER", "INITIAL_CLUSTER_TOKEN", "BUMP_REVISION", "DATA_DIR"} {
			if i == 0 {
				shared[key] = env[key]
				continue
			}
			if env[key] != shared[key] {
				t.Fatalf("%s must be identical on every member: pod %d has %q, pod 0 had %q",
					key, i, env[key], shared[key])
			}
		}
		f.kubeClient.ClearActions()
	}

	want := "etcd-0=https://etcd-0.etcd-discovery.hosted-cluster-cluster.svc:2380," +
		"etcd-1=https://etcd-1.etcd-discovery.hosted-cluster-cluster.svc:2380," +
		"etcd-2=https://etcd-2.etcd-discovery.hosted-cluster-cluster.svc:2380"
	if shared["INITIAL_CLUSTER"] != want {
		t.Fatalf("unexpected --initial-cluster:\n got: %s\nwant: %s", shared["INITIAL_CLUSTER"], want)
	}
	if shared["BUMP_REVISION"] != etcdRevisionBump || shared["INITIAL_CLUSTER_TOKEN"] != etcdInitialClusterToken {
		t.Fatalf("unexpected restore parameters: %v", shared)
	}
}

func TestWaitForEtcdRestorePods(t *testing.T) {
	t.Run("records success once every member is rebuilt", func(t *testing.T) {
		f := newRestoringTest(t, nil)
		f.addPods(f.restorePods(corev1.PodSucceeded)...)

		update := f.requireStepStatusUpdate(f.runStep(f.waitForEtcdRestorePods, f.recovery))
		condition := meta.FindStatusCondition(update.Status.Conditions, hcprecoveryv1alpha1.ConditionEtcdSnapshotRestored)
		if condition == nil || condition.Status != metav1.ConditionTrue ||
			!strings.Contains(condition.Message, "All 3 etcd members") {
			t.Fatalf("unexpected condition: %#v", condition)
		}
	})

	t.Run("waits while members are rebuilt", func(t *testing.T) {
		f := newRestoringTest(t, nil)
		pods := f.restorePods(corev1.PodSucceeded)
		pods[2] = f.workerPod(etcdRestorePodName(2), etcdRestoreApp,
			f.recovery.Spec.ControlPlaneNamespace, corev1.PodRunning, "")
		f.addPods(pods...)

		update := f.requireStepStatusUpdate(f.runStep(f.waitForEtcdRestorePods, f.recovery))
		condition := meta.FindStatusCondition(update.Status.Conditions, hcprecoveryv1alpha1.ConditionEtcdSnapshotRestored)
		if condition == nil || condition.Status != metav1.ConditionFalse ||
			condition.Reason != EtcdRestorePodRunningReason || !strings.Contains(condition.Message, "1 of 3") {
			t.Fatalf("unexpected condition: %#v", condition)
		}
	})

	// The pod skips a volume it has already rebuilt, so a failure means etcdutl itself
	// rejected the snapshot and starting etcd on top of it would be unsafe.
	t.Run("a failed member ends the recovery", func(t *testing.T) {
		f := newRestoringTest(t, nil)
		pods := f.restorePods(corev1.PodSucceeded)
		pods[1] = f.workerPod(etcdRestorePodName(1), etcdRestoreApp,
			f.recovery.Spec.ControlPlaneNamespace, corev1.PodFailed, "etcdutl: snapshot file corrupted")
		f.addPods(pods...)

		f.assertStepFailureContract(f.waitForEtcdRestorePods, stepFailureExpectation{
			conditionType: hcprecoveryv1alpha1.ConditionEtcdSnapshotRestored,
			reason:        EtcdRestorePodFailedReason,
			messageParts:  []string{"etcd-1", "snapshot file corrupted"},
			permanent:     true,
		})
	})
}

func TestDeleteEtcdRestorePodsReleasesVolumesBeforeAdvancing(t *testing.T) {
	f := newRestoringTest(t, func(f *recoveryTest) {
		f.sts.Spec.Replicas = ptr.To[int32](0)
	})
	pods := f.restorePods(corev1.PodSucceeded)
	f.addPods(pods...)
	f.cacheVeleroRestore(velerov1.RestorePhaseCompleted)
	recovery := f.recovery.DeepCopy()
	for _, step := range []phaseStep{
		f.createVeleroRestoreStep,
		f.waitForVeleroRestoreCompletion,
		f.scaleDownEtcdAfterRestore,
		f.validateEtcdPodsGone,
		f.waitForSnapshotTransfer,
		f.waitForEtcdRestorePods,
	} {
		recovery.Status = f.requireStepStatusUpdate(f.runStep(step, recovery)).Status
	}

	for i := int32(0); i < testEtcdReplicas; i++ {
		if err := f.reconcile(f.ctx, recovery); err != nil {
			t.Fatal(err)
		}
		if len(f.updates) != 0 {
			t.Fatalf("must not advance while restore pod %d remains: %v", i, f.updates)
		}
		if deleted := f.deletedPodNames(); len(deleted) != 1 {
			t.Fatalf("expected one deletion per reconcile, got %v", deleted)
		}
		if err := f.informers["Pod"].GetStore().Delete(pods[i]); err != nil {
			t.Fatal(err)
		}
		f.kubeClient.ClearActions()
	}

	if result, err := f.runStep(f.deleteEtcdRestorePods, recovery); result.outcome != stepContinue || err != nil ||
		len(f.updates) != 0 {
		t.Fatalf("expected cleanup to fall through without a status update, got outcome=%v err=%v updates=%v",
			result.outcome, err, f.updates)
	}
	if err := f.reconcile(f.ctx, recovery); err != nil {
		t.Fatal(err)
	}
	if len(f.updates) != 1 || f.updates[0].Status.Phase != hcprecoveryv1alpha1.RestoreStateValidating {
		t.Fatalf("expected the reconciler to hand off to Validating, got %v", f.updates)
	}
}

func TestDeleteEtcdRestorePodsSurfacesDeleteFailures(t *testing.T) {
	deleteErr := errors.New("pod delete rejected")
	f := newRestoringTest(t, nil)
	f.addPods(f.restorePods(corev1.PodSucceeded)...)
	f.kubeClient.PrependReactor("delete", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, deleteErr
	})

	f.assertStepFailureContract(f.deleteEtcdRestorePods, stepFailureExpectation{
		conditionType: hcprecoveryv1alpha1.ConditionEtcdSnapshotRestored,
		reason:        EtcdRestorePodsDeleteFailedReason,
		messageParts:  []string{"failed to delete worker pod", deleteErr.Error()},
		retryError:    func(err error) bool { return errors.Is(err, deleteErr) },
	})
}
