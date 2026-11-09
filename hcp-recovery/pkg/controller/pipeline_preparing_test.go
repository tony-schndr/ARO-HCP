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
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	hypershiftv1beta1 "github.com/openshift/hypershift/api/hypershift/v1beta1"

	hcprecoveryv1alpha1 "github.com/Azure/ARO-HCP/hcp-recovery/pkg/apis/hcprecovery/v1alpha1"
	"github.com/Azure/ARO-HCP/internal/controllerutils"
)

func TestPreparingRejectsNamespaceMismatchBeforeMutation(t *testing.T) {
	for _, previouslyValidated := range []bool{false, true} {
		for _, mismatch := range []string{"request", "backup"} {
			t.Run(fmt.Sprintf("%s/previouslyValidated=%t", mismatch, previouslyValidated), func(t *testing.T) {
				f := newRecoveryTest(t, func(f *recoveryTest) {
					if mismatch == "request" {
						f.recovery.Spec.ControlPlaneNamespace = "other-cluster"
						f.controlPlaneNamespace.Name = f.recovery.Spec.ControlPlaneNamespace
						f.controlPlaneNamespace.Annotations[controllerutils.HcpClusterAzureResourceIdAnnotation] = "other"
						f.sts.Namespace = "other-cluster"
						f.deployment.Namespace = "other-cluster"
					} else {
						f.backup.Object["spec"] = map[string]interface{}{
							"includedNamespaces": []interface{}{f.hc.Namespace, "other-cluster"},
						}
					}
					if previouslyValidated {
						f.recovery.Status.Conditions = append(f.recovery.Status.Conditions, metav1.Condition{
							Type: hcprecoveryv1alpha1.ConditionRecoveryInputsValidated, Status: metav1.ConditionTrue,
							Reason: RecoveryInputsValidatedReason, LastTransitionTime: metav1.NewTime(testNow),
						})
					}
				})
				if err := f.reconcile(f.ctx, f.recovery); err != nil {
					t.Fatal(err)
				}
				if len(f.updates) != 1 || f.updates[0].Status.Phase != hcprecoveryv1alpha1.RestoreStateFailed ||
					!meta.IsStatusConditionFalse(f.updates[0].Status.Conditions, hcprecoveryv1alpha1.ConditionRecoveryInputsValidated) {
					t.Fatalf("namespace mismatch must fail recovery: %v", f.updates)
				}
				kubeActions := f.kubeClient.Actions()
				if len(f.hcClient.Actions()) != 0 || len(kubeActions) != 1 ||
					!kubeActions[0].Matches("get", "namespaces") || len(f.dynamicClient.Actions()) != 0 {
					t.Fatalf("namespace mismatch must not pause, scale, delete or restore resources: hostedcluster=%v kube=%v velero=%v",
						f.hcClient.Actions(), kubeActions, f.dynamicClient.Actions())
				}
			})
		}
	}
}

func TestPreparingRejectsIncompatibleCMKBeforeMutation(t *testing.T) {
	for _, previouslyValidated := range []bool{false, true} {
		for _, rotation := range []bool{false, true} {
			t.Run(fmt.Sprintf("rotation=%t/previouslyValidated=%t", rotation, previouslyValidated), func(t *testing.T) {
				f := newRecoveryTest(t, func(f *recoveryTest) { f.configureAzureCMK() })
				recovery := f.recovery.DeepCopy()
				if previouslyValidated {
					recovery.Status = f.requireStepStatusUpdate(f.runStep(f.validateRecoveryInputs, recovery)).Status
					f.kubeClient.ClearActions()
				}
				target := f.hc.DeepCopy()
				reason := BackupKMSKeyFingerprintMismatchReason
				if rotation {
					target.Status.SecretEncryption.TargetKey = hypershiftv1beta1.SecretEncryptionKeyStatus{
						Provider: hypershiftv1beta1.SecretEncryptionProviderAzure,
						Azure:    target.Status.SecretEncryption.ActiveKey.Azure,
					}
					target.Status.SecretEncryption.TargetKey.Azure.KeyVersion = "target-version"
					reason = BackupKMSKeyRotationInProgressReason
				} else {
					target.Status.SecretEncryption.ActiveKey.Azure.KeyVersion = "rotated-version"
				}
				if err := f.informers["HostedCluster"].GetStore().Update(target); err != nil {
					t.Fatal(err)
				}
				if err := f.reconcile(f.ctx, recovery); err != nil {
					t.Fatal(err)
				}
				if len(f.updates) != 1 || f.updates[0].Status.Phase != hcprecoveryv1alpha1.RestoreStateFailed ||
					f.updates[0].Status.CompletedAt == nil {
					t.Fatalf("incompatible CMK must fail recovery: %v", f.updates)
				}
				condition := meta.FindStatusCondition(f.updates[0].Status.Conditions, hcprecoveryv1alpha1.ConditionRecoveryInputsValidated)
				if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != reason {
					t.Fatalf("unexpected CMK failure condition: %#v", condition)
				}
				if len(f.hcClient.Actions()) != 0 || len(f.kubeClient.Actions()) != 0 || len(f.dynamicClient.Actions()) != 0 {
					t.Fatalf("incompatible CMK must not pause, scale, delete or restore resources: hostedcluster=%v kube=%v velero=%v",
						f.hcClient.Actions(), f.kubeClient.Actions(), f.dynamicClient.Actions())
				}
			})
		}
	}
}

func preparedRecoveryTest(t *testing.T) (*recoveryTest, *hcprecoveryv1alpha1.HCPRecovery) {
	t.Helper()
	f := newRecoveryTest(t, func(f *recoveryTest) {
		f.hc.Spec.PausedUntil = ptr.To("true")
		f.deployment.Spec.Replicas = ptr.To[int32](0)
		f.deployment.Status.Replicas = 0
		f.sts.Spec.Replicas = ptr.To[int32](0)
	})
	recovery := f.recovery
	for range 10 {
		if err := f.reconcile(f.ctx, recovery); err != nil {
			t.Fatal(err)
		}
		var err error
		recovery, err = f.client.HcprecoveryV1alpha1().HCPRecoveries(recovery.Namespace).
			Get(f.ctx, recovery.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if recovery.Status.Phase != hcprecoveryv1alpha1.RestoreStatePreparing {
			t.Fatalf("advanced before all preparation conditions were recorded: %#v", recovery.Status)
		}
		if meta.IsStatusConditionTrue(recovery.Status.Conditions, hcprecoveryv1alpha1.ConditionEtcdPVCsDeleted) {
			f.client.ClearActions()
			f.kubeClient.ClearActions()
			f.hcClient.ClearActions()
			return f, recovery
		}
	}
	t.Fatal("preparation never converged")
	return nil, nil
}

func TestPreparingWaitsForPauseAcknowledgementBeforeDeletingStorage(t *testing.T) {
	f, recovery := preparedRecoveryTest(t)
	hcp := f.hcp.DeepCopy()
	hcp.Status.Conditions[0].Status = metav1.ConditionTrue
	if err := f.informers["HostedControlPlane"].GetStore().Update(hcp); err != nil {
		t.Fatal(err)
	}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: "data-etcd-0", Namespace: f.recovery.Spec.ControlPlaneNamespace,
		Labels: map[string]string{"app": etcd},
	}}
	if err := f.informers["PersistentVolumeClaim"].GetStore().Add(pvc); err != nil {
		t.Fatal(err)
	}
	if _, err := f.kubeClient.CoreV1().PersistentVolumeClaims(pvc.Namespace).Create(f.ctx, pvc, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	f.kubeClient.ClearActions()
	for range 2 {
		if err := f.reconcile(f.ctx, recovery); err != nil {
			t.Fatal(err)
		}
		for _, action := range f.kubeClient.Actions() {
			if action.GetVerb() != "get" && action.GetVerb() != "list" {
				t.Fatalf("must not scale or delete storage before pause acknowledgement: %v", action)
			}
		}
		var err error
		recovery, err = f.client.HcprecoveryV1alpha1().HCPRecoveries(recovery.Namespace).Get(f.ctx, recovery.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
	}
	hcp = hcp.DeepCopy()
	hcp.Status.Conditions[0].Status = metav1.ConditionFalse
	if err := f.informers["HostedControlPlane"].GetStore().Update(hcp); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := f.reconcile(f.ctx, recovery); err != nil {
			t.Fatal(err)
		}
		var err error
		recovery, err = f.client.HcprecoveryV1alpha1().HCPRecoveries(recovery.Namespace).Get(f.ctx, recovery.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, action := range f.kubeClient.Actions() {
		if action.Matches("delete", "persistentvolumeclaims") {
			return
		}
	}
	t.Fatal("storage deletion did not resume after pause acknowledgement")
}

func TestPreparingRepairsDriftDespiteSuccessfulConditions(t *testing.T) {
	for _, step := range []string{"pause", "kube-apiserver"} {
		t.Run(step, func(t *testing.T) {
			f, recovery := preparedRecoveryTest(t)
			resource, verb := "", "patch"
			switch step {
			case "pause":
				hc := f.hc.DeepCopy()
				hc.Spec.PausedUntil = ptr.To("false")
				if err := f.informers["HostedCluster"].GetStore().Update(hc); err != nil {
					t.Fatal(err)
				}
				resource = "hostedclusters"
			case "kube-apiserver":
				deployment := f.deployment.DeepCopy()
				deployment.Spec.Replicas = ptr.To[int32](3)
				if err := f.informers["Deployment"].GetStore().Update(deployment); err != nil {
					t.Fatal(err)
				}
				resource = "deployments"
			}
			// Scale subresource patches are not implemented by the simple fake.
			f.kubeClient.PrependReactor("patch", "*", func(clienttesting.Action) (bool, runtime.Object, error) {
				return true, nil, nil
			})
			if err := f.reconcile(f.ctx, recovery); err != nil {
				t.Fatal(err)
			}
			if len(f.client.Actions()) != 0 {
				t.Fatalf("drift must be corrected before advancing: %v", f.client.Actions())
			}
			actions := append(f.kubeClient.Actions(), f.hcClient.Actions()...)
			found := false
			for _, action := range actions {
				if action.Matches(verb, resource) {
					found = true
				}
			}
			if !found {
				t.Fatalf("expected %s %s to repair drift, got %v", verb, resource, actions)
			}
		})
	}
}

func TestPreparingWaitsForObservedScaleDown(t *testing.T) {
	f, recovery := preparedRecoveryTest(t)
	deployment := f.deployment.DeepCopy()
	deployment.Status.Replicas = 1
	if err := f.informers["Deployment"].GetStore().Update(deployment); err != nil {
		t.Fatal(err)
	}
	if err := f.reconcile(f.ctx, recovery); err != nil {
		t.Fatal(err)
	}
	kubeActions := f.kubeClient.Actions()
	if len(f.client.Actions()) != 0 || len(kubeActions) != 1 || !kubeActions[0].Matches("get", "namespaces") {
		t.Fatalf("must wait before transitioning: recovery=%v kube=%v",
			f.client.Actions(), f.kubeClient.Actions())
	}
}

func TestPreparingWaitsForPodsToBeCleanedUp(t *testing.T) {
	f, recovery := preparedRecoveryTest(t)
	f.updates = nil
	if err := f.informers["Pod"].GetStore().Add(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "kube-apiserver", Namespace: recovery.Spec.ControlPlaneNamespace,
			Labels: map[string]string{"app": apiServer},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.reconcile(f.ctx, recovery); err != nil {
		t.Fatal(err)
	}
	if len(f.updates) != 1 || f.updates[0].Status.Phase != hcprecoveryv1alpha1.RestoreStatePreparing ||
		!meta.IsStatusConditionFalse(f.updates[0].Status.Conditions, hcprecoveryv1alpha1.ConditionScaledDownValidated) {
		t.Fatalf("remaining pods must block the phase transition: %v", f.updates)
	}
}

func TestPreparingTransitionsOnceAllRequirementsHold(t *testing.T) {
	f, recovery := preparedRecoveryTest(t)
	if err := f.reconcile(f.ctx, recovery); err != nil {
		t.Fatal(err)
	}
	validated, err := f.client.HcprecoveryV1alpha1().HCPRecoveries(recovery.Namespace).
		Get(f.ctx, recovery.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if validated.Status.Phase != hcprecoveryv1alpha1.RestoreStatePreparing ||
		!meta.IsStatusConditionTrue(validated.Status.Conditions, hcprecoveryv1alpha1.ConditionScaledDownValidated) {
		t.Fatalf("expected scale-down validation before advancing, got %#v", validated.Status)
	}
	f.client.ClearActions()
	if err := f.reconcile(f.ctx, validated); err != nil {
		t.Fatal(err)
	}
	restoring, err := f.client.HcprecoveryV1alpha1().HCPRecoveries(recovery.Namespace).
		Get(f.ctx, recovery.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if restoring.Status.Phase != hcprecoveryv1alpha1.RestoreStateRestoring ||
		!meta.IsStatusConditionTrue(restoring.Status.Conditions, hcprecoveryv1alpha1.ConditionScaledDownValidated) {
		t.Fatalf("expected Restoring with scale-down validated, got %#v", restoring.Status)
	}
	f.client.ClearActions()
	f.kubeClient.ClearActions()
	f.hcClient.ClearActions()
	f.dynamicClient.ClearActions()
	if err := f.reconcile(f.ctx, restoring); err != nil {
		t.Fatal(err)
	}
	if len(f.client.Actions()) != 0 || len(f.kubeClient.Actions()) != 0 || len(f.hcClient.Actions()) != 0 {
		t.Fatal("Restoring must not execute preparation actions")
	}
	actions := f.dynamicClient.Actions()
	if len(actions) != 1 || !actions[0].Matches("create", "restores") {
		t.Fatalf("expected Velero restore creation after preparation, got %v", actions)
	}
}

func TestRecoveryInitializesPreparingBeforeExecutingSteps(t *testing.T) {
	f := newRecoveryTest(t, func(f *recoveryTest) {
		f.recovery.Status = hcprecoveryv1alpha1.HCPRecoveryStatus{}
	})
	if err := f.reconcile(f.ctx, f.recovery); err != nil {
		t.Fatal(err)
	}
	if len(f.updates) != 1 || f.updates[0].Status.Phase != hcprecoveryv1alpha1.RestoreStatePreparing ||
		f.updates[0].Status.StartedAt == nil || !f.updates[0].Status.StartedAt.Time.Equal(testNow) ||
		len(f.updates[0].Status.Conditions) != 0 || len(f.kubeClient.Actions()) != 0 ||
		len(f.hcClient.Actions()) != 0 || len(f.dynamicClient.Actions()) != 0 {
		t.Fatalf("expected only phase and start time initialization, got %v", f.updates)
	}
}

// validateScaledDown is the last gate before the Velero restore begins rewriting etcd's
// volumes. Its failures are retryable, and a retryable failure writes nothing once the
// condition is already recorded — so a step runner that keys "should I run the next
// step?" off "did status change?" treats the second pass as a pass. For the last step in
// a phase that means advancing: Velero would restore on top of a live kube-apiserver and
// live etcd PVCs. The gate has to hold for as long as the pods are there, not just on
// the pass that first noticed them.
func TestPreparingNeverAdvancesWhileEtcdPodsSurvive(t *testing.T) {
	f, recovery := preparedRecoveryTest(t)
	f.addPods(f.etcdPod(0, true))

	for pass := range 5 {
		f.client.ClearActions()
		syncErr := f.reconcile(f.ctx, recovery)
		// Every pass must leave something behind that brings us back: either a status
		// write, whose watch event requeues, or an error for a rate-limited retry.
		// Returning nil with neither would strand the recovery until the resync.
		if syncErr == nil && len(f.client.Actions()) == 0 {
			t.Fatalf("pass %d: returned no error and wrote nothing, so nothing will requeue", pass)
		}
		var err error
		recovery, err = f.client.HcprecoveryV1alpha1().HCPRecoveries(recovery.Namespace).
			Get(f.ctx, recovery.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if recovery.Status.Phase != hcprecoveryv1alpha1.RestoreStatePreparing {
			t.Fatalf("pass %d: advanced to %q with an etcd pod still running; "+
				"the Velero restore would rewrite volumes that are still mounted",
				pass, recovery.Status.Phase)
		}
		if meta.IsStatusConditionTrue(recovery.Status.Conditions, hcprecoveryv1alpha1.ConditionScaledDownValidated) {
			t.Fatalf("pass %d: recorded scale-down as validated with an etcd pod still running", pass)
		}
	}
}

// The allowed edges are restated here rather than derived from validPhaseTransition, so
// that changing the state machine has to be a deliberate edit in both places. They must
// also stay in step with the CEL rule on the CRD, which is the real enforcement.
func TestApplyStatusEnforcesForwardOnlyTransitions(t *testing.T) {
	advance := map[hcprecoveryv1alpha1.RestoreState]hcprecoveryv1alpha1.RestoreState{
		"": hcprecoveryv1alpha1.RestoreStatePreparing,
		hcprecoveryv1alpha1.RestoreStatePreparing:  hcprecoveryv1alpha1.RestoreStateRestoring,
		hcprecoveryv1alpha1.RestoreStateRestoring:  hcprecoveryv1alpha1.RestoreStateValidating,
		hcprecoveryv1alpha1.RestoreStateValidating: hcprecoveryv1alpha1.RestoreStateCompleted,
	}
	// Failure is reachable from every phase. Preparation reads and scales down only,
	// but the spec is immutable, so a rejected request there cannot be corrected on the
	// object either; from the Velero restore onwards the recovery is rewriting etcd and
	// cannot resume from a half-applied step.
	states := []hcprecoveryv1alpha1.RestoreState{"", hcprecoveryv1alpha1.RestoreStatePreparing,
		hcprecoveryv1alpha1.RestoreStateRestoring, hcprecoveryv1alpha1.RestoreStateValidating,
		hcprecoveryv1alpha1.RestoreStateCompleted, hcprecoveryv1alpha1.RestoreStateFailed}
	for _, from := range states {
		for _, to := range states {
			t.Run(string(from)+"->"+string(to), func(t *testing.T) {
				f := newRecoveryTest(t, func(f *recoveryTest) { f.recovery.Status.Phase = from })
				update, _ := NewStatus(f.recovery.Status).WithPhase(to).AsApplyConfiguration(f.recovery)
				err := f.applyStatus(f.ctx, f.recovery, update)
				next, hasNext := advance[from]
				allowed := from == to || (hasNext && next == to) ||
					to == hcprecoveryv1alpha1.RestoreStateFailed
				if allowed && err != nil || !allowed && (err == nil || len(f.client.Actions()) != 0) {
					t.Fatalf("transition allowed=%v, got err=%v actions=%v", allowed, err, f.client.Actions())
				}
			})
		}
	}
}
