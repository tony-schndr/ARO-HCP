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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	hypershiftv1beta1 "github.com/openshift/hypershift/api/hypershift/v1beta1"

	hcprecoveryv1alpha1 "github.com/Azure/ARO-HCP/hcp-recovery/pkg/apis/hcprecovery/v1alpha1"
	"github.com/Azure/ARO-HCP/internal/backup"
)

func newStartingEtcdTest(t *testing.T, configure func(*recoveryTest)) *recoveryTest {
	t.Helper()
	return newRecoveryTest(t, func(f *recoveryTest) {
		f.recovery.Status.Phase = hcprecoveryv1alpha1.RestoreStateValidating
		f.sts.Spec.Replicas = ptr.To[int32](0)
		if configure != nil {
			configure(f)
		}
	})
}

// Every member was restored with the full membership list, so the StatefulSet goes
// straight back to full size. Growing it one replica at a time would make the joining
// members reject the cluster.
func TestScaleUpEtcdRestoresTheCapturedMemberCount(t *testing.T) {
	f := newStartingEtcdTest(t, nil)

	result, err := f.runStep(f.scaleUpEtcd, f.recovery)
	if result.outcome != stepStop || err != nil {
		t.Fatalf("expected the scale patch, got outcome=%v err=%v", result.outcome, err)
	}
	actions := f.kubeClient.Actions()
	if len(actions) != 1 {
		t.Fatalf("expected exactly one action, got %v", actions)
	}
	patch, ok := actions[0].(clienttesting.PatchAction)
	if !ok || patch.GetName() != "etcd" || actions[0].GetSubresource() != "scale" {
		t.Fatalf("expected a scale subresource patch on etcd, got %#v", actions[0])
	}
	if string(patch.GetPatch()) != `{"spec":{"replicas":3}}` {
		t.Fatalf("unexpected scale patch: %s", patch.GetPatch())
	}

	scaled := f.sts.DeepCopy()
	scaled.Spec.Replicas = ptr.To[int32](3)
	if err := f.informers["StatefulSet"].GetStore().Update(scaled); err != nil {
		t.Fatal(err)
	}
	f.kubeClient.ClearActions()
	if result, err := f.runStep(f.scaleUpEtcd, f.recovery); result.outcome != stepContinue || err != nil ||
		len(f.kubeClient.Actions()) != 0 {
		t.Fatalf("expected the step to fall through once scaled, got outcome=%v err=%v", result.outcome, err)
	}
}

func TestScaleUpEtcdFailures(t *testing.T) {
	t.Run("patch failure retries", func(t *testing.T) {
		patchErr := errors.New("scale patch rejected")
		f := newStartingEtcdTest(t, nil)
		f.kubeClient.PrependReactor("patch", "statefulsets", func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, patchErr
		})
		f.assertStepFailureContract(f.scaleUpEtcd, stepFailureExpectation{
			conditionType: hcprecoveryv1alpha1.ConditionEtcdScaledUp,
			reason:        EtcdScaleUpPatchFailedReason,
			messageParts:  []string{"failed to scale etcd statefulset to 3 replicas", patchErr.Error()},
			retryError:    func(err error) bool { return errors.Is(err, patchErr) },
		})
	})
}

func TestWaitForEtcdPodsReady(t *testing.T) {
	t.Run("records readiness once every member is up", func(t *testing.T) {
		f := newStartingEtcdTest(t, nil)
		f.addPods(f.etcdPod(0, true), f.etcdPod(1, true), f.etcdPod(2, true))

		update := f.requireStepStatusUpdate(f.runStep(f.waitForEtcdPodsReady, f.recovery))
		condition := meta.FindStatusCondition(update.Status.Conditions, hcprecoveryv1alpha1.ConditionEtcdScaledUp)
		if condition == nil || condition.Status != metav1.ConditionTrue ||
			!strings.Contains(condition.Message, "All 3 etcd pods are ready") {
			t.Fatalf("unexpected condition: %#v", condition)
		}

		settled := f.recovery.DeepCopy()
		settled.Status = update.Status
		f.client.ClearActions()
		if result, err := f.runStep(f.waitForEtcdPodsReady, settled); result.outcome != stepContinue || err != nil ||
			len(f.client.Actions()) != 0 {
			t.Fatalf("expected an idempotent second pass, got outcome=%v err=%v", result.outcome, err)
		}
	})

	// Readiness changes raise pod events, so the step parks rather than erroring to get
	// itself re-queued.
	t.Run("waits without polling while members start", func(t *testing.T) {
		f := newStartingEtcdTest(t, nil)
		f.addPods(f.etcdPod(0, true), f.etcdPod(1, false), f.etcdPod(2, false))

		update := f.requireStepStatusUpdate(f.runStep(f.waitForEtcdPodsReady, f.recovery))
		condition := meta.FindStatusCondition(update.Status.Conditions, hcprecoveryv1alpha1.ConditionEtcdScaledUp)
		if condition == nil || condition.Status != metav1.ConditionFalse ||
			condition.Reason != EtcdPodsStartingReason || !strings.Contains(condition.Message, "1 of 3") {
			t.Fatalf("unexpected condition: %#v", condition)
		}

		settled := f.recovery.DeepCopy()
		settled.Status = update.Status
		f.client.ClearActions()
		if result, err := f.runStep(f.waitForEtcdPodsReady, settled); result.outcome != stepStop || err != nil ||
			len(f.client.Actions()) != 0 {
			t.Fatalf("expected a silent wait, got outcome=%v err=%v actions=%v", result.outcome, err, f.client.Actions())
		}
	})
}

func (f *recoveryTest) withEtcdHealthy() *hcprecoveryv1alpha1.HCPRecovery {
	recovery := f.recovery.DeepCopy()
	recovery.Status.Conditions = append(recovery.Status.Conditions, metav1.Condition{
		Type: hcprecoveryv1alpha1.ConditionEtcdHealthy, Status: metav1.ConditionTrue,
		Reason: "EtcdHealthy", Message: "all 3 endpoints healthy",
		ObservedGeneration: recovery.Generation, LastTransitionTime: metav1.NewTime(testNow),
	})
	return recovery
}

// Running etcd pods prove nothing: a botched restore produces members that are each
// individually healthy but belong to different clusters. The check interrogates etcd
// itself, from a pod so the controller never blocks on it.
func TestCreateEtcdHealthCheckPodSpec(t *testing.T) {
	f := newStartingEtcdTest(t, nil)

	if result, err := f.runStep(f.createEtcdHealthCheckPod, f.recovery); result.outcome != stepStop || err != nil {
		t.Fatalf("expected the health check pod to be created, got outcome=%v err=%v", result.outcome, err)
	}
	created := f.createdPods()
	if len(created) != 1 {
		t.Fatalf("expected one pod, got %v", created)
	}
	pod := created[0]

	if pod.Namespace != f.recovery.Namespace || pod.Spec.ServiceAccountName != testWorkerServiceAccount {
		t.Fatalf("health check pod must run as the worker identity in the controller namespace: %#v", pod.Spec)
	}
	if pod.Spec.Containers[0].TerminationMessagePolicy != corev1.TerminationMessageFallbackToLogsOnError {
		t.Fatal("the controller reads the verdict from this pod's termination message")
	}

	env := map[string]string{}
	for _, e := range pod.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	if env["EXPECTED_MEMBERS"] != "3" {
		t.Fatalf("the check must expect the captured member count, got %q", env["EXPECTED_MEMBERS"])
	}
	if env["EXEC_POD"] != "etcd-0" || env["ETCD_CONTAINER"] != backup.EtcdContainerName {
		t.Fatalf("unexpected etcdctl target: %v", env)
	}
	// Each member is addressed directly so their individual answers can be compared;
	// going through the client service would only ever reach one of them.
	want := "https://etcd-0.etcd-discovery.hosted-cluster-cluster.svc:2379," +
		"https://etcd-1.etcd-discovery.hosted-cluster-cluster.svc:2379," +
		"https://etcd-2.etcd-discovery.hosted-cluster-cluster.svc:2379"
	if env["ETCD_ENDPOINTS"] != want {
		t.Fatalf("unexpected endpoints:\n got: %s\nwant: %s", env["ETCD_ENDPOINTS"], want)
	}
	if env["ETCD_CACERT"] != backup.EtcdCACertPath || env["ETCD_CERT"] != backup.EtcdClientCert ||
		env["ETCD_KEY"] != backup.EtcdClientKey {
		t.Fatalf("the check must use the certificates mounted in the etcd container: %v", env)
	}
	// A store below the bump the restore pods applied was never rebuilt from the
	// snapshot, which is the one failure every other check in this phase passes.
	if env["MIN_REVISION"] != etcdRevisionBump {
		t.Fatalf("the revision floor must match the bump the restore pods apply: floor=%q bump=%q",
			env["MIN_REVISION"], etcdRevisionBump)
	}
}

func TestWaitForEtcdHealthCheck(t *testing.T) {
	t.Run("records the verdict", func(t *testing.T) {
		f := newStartingEtcdTest(t, nil)
		f.addPods(f.workerPod(etcdHealthCheckPodName(f.recovery), etcdHealthCheckApp,
			f.recovery.Namespace, corev1.PodSucceeded, "members: etcd-0, etcd-1, etcd-2;all 3 endpoints healthy"))

		update := f.requireStepStatusUpdate(f.runStep(f.waitForEtcdHealthCheck, f.recovery))
		condition := meta.FindStatusCondition(update.Status.Conditions, hcprecoveryv1alpha1.ConditionEtcdHealthy)
		if condition == nil || condition.Status != metav1.ConditionTrue ||
			!strings.Contains(condition.Message, "all 3 endpoints healthy") {
			t.Fatalf("unexpected condition: %#v", condition)
		}
	})

	t.Run("waits while the check polls", func(t *testing.T) {
		f := newStartingEtcdTest(t, nil)
		f.addPods(f.workerPod(etcdHealthCheckPodName(f.recovery), etcdHealthCheckApp,
			f.recovery.Namespace, corev1.PodRunning, ""))

		update := f.requireStepStatusUpdate(f.runStep(f.waitForEtcdHealthCheck, f.recovery))
		condition := meta.FindStatusCondition(update.Status.Conditions, hcprecoveryv1alpha1.ConditionEtcdHealthy)
		if condition == nil || condition.Status != metav1.ConditionFalse ||
			condition.Reason != EtcdHealthCheckRunningReason {
			t.Fatalf("unexpected condition: %#v", condition)
		}
	})

	// The pod already retried for as long as etcd is allowed to take, so a failure
	// means the HostedCluster must not be unpaused on top of this etcd.
	t.Run("a split cluster ends the recovery", func(t *testing.T) {
		f := newStartingEtcdTest(t, nil)
		f.addPods(f.workerPod(etcdHealthCheckPodName(f.recovery), etcdHealthCheckApp,
			f.recovery.Namespace, corev1.PodFailed, "endpoints report different cluster IDs"))

		f.assertStepFailureContract(f.waitForEtcdHealthCheck, stepFailureExpectation{
			conditionType: hcprecoveryv1alpha1.ConditionEtcdHealthy,
			reason:        EtcdHealthCheckFailedReason,
			messageParts:  []string{"did not converge into one healthy cluster", "different cluster IDs"},
			permanent:     true,
		})
	})
}

// Once etcd has been declared healthy the check is not repeated, so a pod deleted
// after the fact cannot drag the recovery backwards.
func TestEtcdHealthCheckIsNotRepeatedOnceRecorded(t *testing.T) {
	f := newStartingEtcdTest(t, nil)
	recovery := f.withEtcdHealthy()

	if result, err := f.runStep(f.createEtcdHealthCheckPod, recovery); result.outcome != stepContinue || err != nil ||
		len(f.kubeClient.Actions()) != 0 {
		t.Fatalf("expected no new check pod, got outcome=%v err=%v actions=%v", result.outcome, err, f.kubeClient.Actions())
	}
	if result, err := f.runStep(f.waitForEtcdHealthCheck, recovery); result.outcome != stepContinue || err != nil ||
		len(f.client.Actions()) != 0 {
		t.Fatalf("expected the step to fall through, got outcome=%v err=%v", result.outcome, err)
	}
}

// The step only reaps the pod; unpausing and control-plane validation follow in the
// same Validating phase.
func TestDeleteEtcdHealthCheckPodReapsThenFallsThrough(t *testing.T) {
	f := newStartingEtcdTest(t, nil)
	recovery := f.withEtcdHealthy()
	pod := f.workerPod(etcdHealthCheckPodName(f.recovery), etcdHealthCheckApp,
		f.recovery.Namespace, corev1.PodSucceeded, "all 3 endpoints healthy")
	f.addPods(pod)

	result, err := f.runStep(f.deleteEtcdHealthCheckPod, recovery)
	if result.outcome != stepStop || err != nil {
		t.Fatalf("expected the completed check pod to be reaped, got outcome=%v err=%v", result.outcome, err)
	}
	if deleted := f.deletedPodNames(); len(deleted) != 1 || deleted[0] != pod.Name {
		t.Fatalf("expected only the check pod to be deleted, got %v", deleted)
	}

	if err := f.informers["Pod"].GetStore().Delete(pod); err != nil {
		t.Fatal(err)
	}
	f.client.ClearActions()
	if result, err := f.runStep(f.deleteEtcdHealthCheckPod, recovery); result.outcome != stepContinue || err != nil ||
		len(f.client.Actions()) != 0 {
		t.Fatalf("expected the reaped pod to let the phase fall through, got outcome=%v err=%v actions=%v",
			result.outcome, err, f.client.Actions())
	}
}

func newResumingTest(t *testing.T, configure func(*recoveryTest)) *recoveryTest {
	t.Helper()
	return newRecoveryTest(t, func(f *recoveryTest) {
		f.recovery.Status.Phase = hcprecoveryv1alpha1.RestoreStateValidating
		f.hc.Spec.PausedUntil = ptr.To("true")
		if configure != nil {
			configure(f)
		}
	})
}

func hostedClusterConditions(statuses map[hypershiftv1beta1.ConditionType]metav1.ConditionStatus) []metav1.Condition {
	conditions := make([]metav1.Condition, 0, len(statuses))
	for conditionType, status := range statuses {
		conditions = append(conditions, metav1.Condition{
			Type: string(conditionType), Status: status, Reason: "Reported",
			LastTransitionTime: metav1.NewTime(testNow),
		})
	}
	return conditions
}

// An apply configuration cannot express removal, and any value left in pausedUntil
// keeps the HyperShift operator from reconciling, so the field is cleared with a null.
func TestUnpauseHostedClusterClearsPausedUntil(t *testing.T) {
	f := newResumingTest(t, nil)

	result, err := f.runStep(f.unpauseHostedCluster, f.recovery)
	if result.outcome != stepStop || err != nil {
		t.Fatalf("expected the unpause patch, got outcome=%v err=%v", result.outcome, err)
	}
	actions := f.hcClient.Actions()
	if len(actions) != 1 {
		t.Fatalf("expected exactly one action, got %v", actions)
	}
	patch, ok := actions[0].(clienttesting.PatchAction)
	if !ok || patch.GetPatchType() != types.MergePatchType || patch.GetName() != f.recovery.Spec.HostedClusterName {
		t.Fatalf("unexpected action: %#v", actions[0])
	}
	if string(patch.GetPatch()) != `{"spec":{"pausedUntil":null}}` {
		t.Fatalf("unexpected unpause patch: %s", patch.GetPatch())
	}
	if len(f.client.Actions()) != 0 {
		t.Fatalf("patching is the action for this loop, no status write expected: %v", f.client.Actions())
	}
}

func TestUnpauseHostedClusterRecordsSuccessOnceObserved(t *testing.T) {
	f := newResumingTest(t, func(f *recoveryTest) { f.hc.Spec.PausedUntil = nil })

	update := f.requireStepStatusUpdate(f.runStep(f.unpauseHostedCluster, f.recovery))
	condition := meta.FindStatusCondition(update.Status.Conditions, hcprecoveryv1alpha1.ConditionHostedClusterUnpaused)
	if condition == nil || condition.Status != metav1.ConditionTrue {
		t.Fatalf("unexpected condition: %#v", condition)
	}
	if len(f.hcClient.Actions()) != 0 {
		t.Fatalf("an already unpaused cluster must not be patched again: %v", f.hcClient.Actions())
	}

	settled := f.recovery.DeepCopy()
	settled.Status = update.Status
	f.client.ClearActions()
	if result, err := f.runStep(f.unpauseHostedCluster, settled); result.outcome != stepContinue || err != nil ||
		len(f.client.Actions()) != 0 {
		t.Fatalf("expected an idempotent second pass, got outcome=%v err=%v", result.outcome, err)
	}
}

func TestUnpauseHostedClusterPatchFailure(t *testing.T) {
	patchErr := errors.New("unpause patch rejected")
	f := newResumingTest(t, nil)
	f.hcClient.PrependReactor("patch", "hostedclusters", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, patchErr
	})

	f.assertStepFailureContract(f.unpauseHostedCluster, stepFailureExpectation{
		conditionType: hcprecoveryv1alpha1.ConditionHostedClusterUnpaused,
		reason:        HostedClusterUnpauseFailedReason,
		messageParts:  []string{"failed to unpause HostedCluster", patchErr.Error()},
		retryError:    func(err error) bool { return errors.Is(err, patchErr) },
	})
}

// A healthy etcd is not a recovered cluster: the HostedCluster only reports these
// conditions once the operator has resumed and rebuilt the control plane on top of it.
func TestValidateHostedClusterHealthyCompletesTheRecovery(t *testing.T) {
	f := newResumingTest(t, func(f *recoveryTest) {
		f.hc.Spec.PausedUntil = nil
		f.hc.Status.Conditions = hostedClusterConditions(map[hypershiftv1beta1.ConditionType]metav1.ConditionStatus{
			hypershiftv1beta1.EtcdAvailable:          metav1.ConditionTrue,
			hypershiftv1beta1.HostedClusterAvailable: metav1.ConditionTrue,
		})
	})
	f.addPods(f.etcdPod(0, true), f.etcdPod(1, true), f.etcdPod(2, true))
	recovery := f.withEtcdHealthy()
	recovery.Status = f.requireStepStatusUpdate(f.runStep(f.waitForEtcdPodsReady, recovery)).Status

	update := f.requireStepStatusUpdate(f.runStep(f.validateHostedClusterHealthy, recovery))
	if update.Status.Phase != hcprecoveryv1alpha1.RestoreStateValidating {
		t.Fatalf("completing the recovery belongs to runPhase, got %q", update.Status.Phase)
	}
	condition := meta.FindStatusCondition(update.Status.Conditions, hcprecoveryv1alpha1.ConditionHealthChecked)
	if condition == nil || condition.Status != metav1.ConditionTrue {
		t.Fatalf("unexpected condition: %#v", condition)
	}

	recovery.Status = update.Status
	f.updates = nil
	// The unpause step records its own condition first, so the handoff to Completed
	// takes a further pass.
	for range 2 {
		if err := f.reconcile(f.ctx, recovery); err != nil {
			t.Fatal(err)
		}
		if len(f.updates) == 0 {
			t.Fatalf("expected a status write on every pass, got %v", recovery.Status)
		}
		recovery.Status = f.updates[len(f.updates)-1].Status
	}
	if recovery.Status.Phase != hcprecoveryv1alpha1.RestoreStateCompleted {
		t.Fatalf("expected the recovery to complete, got %q", recovery.Status.Phase)
	}
	if recovery.Status.CompletedAt == nil || !recovery.Status.CompletedAt.Time.Equal(testNow) {
		t.Fatalf("expected a completion timestamp, got %v", recovery.Status.CompletedAt)
	}
}

func TestValidateHostedClusterHealthyWaitsForTheControlPlane(t *testing.T) {
	for _, tt := range []struct {
		name       string
		conditions []metav1.Condition
		wantParts  []string
	}{
		{
			name:       "nothing reported yet",
			conditions: nil,
			wantParts:  []string{"EtcdAvailable is not reported", "Available is not reported"},
		},
		{
			name: "etcd still unavailable",
			conditions: hostedClusterConditions(map[hypershiftv1beta1.ConditionType]metav1.ConditionStatus{
				hypershiftv1beta1.EtcdAvailable:          metav1.ConditionFalse,
				hypershiftv1beta1.HostedClusterAvailable: metav1.ConditionTrue,
			}),
			wantParts: []string{"EtcdAvailable is False"},
		},
		{
			name: "etcd back but the control plane is not",
			conditions: hostedClusterConditions(map[hypershiftv1beta1.ConditionType]metav1.ConditionStatus{
				hypershiftv1beta1.EtcdAvailable:          metav1.ConditionTrue,
				hypershiftv1beta1.HostedClusterAvailable: metav1.ConditionFalse,
			}),
			wantParts: []string{"Available is False"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newResumingTest(t, func(f *recoveryTest) {
				f.hc.Spec.PausedUntil = nil
				f.hc.Status.Conditions = tt.conditions
			})

			f.assertStepFailureContract(f.validateHostedClusterHealthy, stepFailureExpectation{
				conditionType: hcprecoveryv1alpha1.ConditionHealthChecked,
				reason:        HostedClusterNotReadyReason,
				messageParts:  append([]string{"has not recovered"}, tt.wantParts...),
				retryError: func(err error) bool {
					return err != nil && strings.Contains(err.Error(), "has not recovered")
				},
			})
		})
	}
}
