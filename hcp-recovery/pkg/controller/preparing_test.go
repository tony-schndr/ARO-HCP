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
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/managedfields"
	appslisters "k8s.io/client-go/listers/apps/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	hypershiftv1beta1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	hypershiftapply "github.com/openshift/hypershift/client/applyconfiguration/hypershift/v1beta1"
	hypershiftscheme "github.com/openshift/hypershift/client/clientset/clientset/scheme"

	hcprecoveryv1alpha1 "github.com/Azure/ARO-HCP/hcp-recovery/pkg/apis/hcprecovery/v1alpha1"
	"github.com/Azure/ARO-HCP/internal/backup"
	"github.com/Azure/ARO-HCP/internal/controllerutils"
)

func TestRecoveryInputsValidationPreservesUnchangedConditionTimestamp(t *testing.T) {
	previous := metav1.NewTime(testNow.Add(-time.Hour))
	f := newRecoveryTest(t, func(f *recoveryTest) {
		f.recovery.Status.Phase = hcprecoveryv1alpha1.RestoreStateRestoring
		f.recovery.Spec.HcpClusterAzureResourceID = ""
		f.recovery.Status.Conditions = []metav1.Condition{{
			Type: hcprecoveryv1alpha1.ConditionRecoveryInputsValidated, Status: metav1.ConditionFalse,
			Reason: BackupMissingResourceIDReason, Message: "The target cluster resource ID is required",
			ObservedGeneration: f.recovery.Generation, LastTransitionTime: previous,
		}}
	})
	update := f.requireStepStatusUpdate(f.runStep(f.validateRecoveryInputs, f.recovery))
	if !update.Status.Conditions[0].LastTransitionTime.Equal(&previous) {
		t.Fatalf("unchanged condition lost its transition time: %#v", update.Status)
	}
}

func TestRecoveryInputsValidation(t *testing.T) {
	type testCase struct {
		name   string
		mutate func(*recoveryTest)
		reason string
	}
	tests := []testCase{
		{name: "completed matching backup", reason: RecoveryInputsValidatedReason},
		{name: "case insensitive resource IDs", reason: RecoveryInputsValidatedReason, mutate: func(f *recoveryTest) {
			f.recovery.Spec.HcpClusterAzureResourceID = strings.ToUpper(f.recovery.Spec.HcpClusterAzureResourceID)
			f.hc.Annotations[controllerutils.HcpClusterAzureResourceIdAnnotation] = strings.ToLower(f.recovery.Spec.HcpClusterAzureResourceID)
			f.controlPlaneNamespace.Annotations[controllerutils.HcpClusterAzureResourceIdAnnotation] = f.recovery.Spec.HcpClusterAzureResourceID
		}},
		{name: "missing target ID", reason: BackupMissingResourceIDReason, mutate: func(f *recoveryTest) {
			f.recovery.Spec.HcpClusterAzureResourceID = ""
			f.backup.SetAnnotations(nil)
			f.hc.Annotations = nil
		}},
		{name: "missing hosted cluster namespace", reason: BackupMissingHostedClusterNamespaceReason, mutate: func(f *recoveryTest) {
			f.recovery.Spec.HostedClusterNamespace = ""
		}},
		{name: "missing hosted cluster name", reason: BackupMissingHostedClusterNameReason, mutate: func(f *recoveryTest) {
			f.recovery.Spec.HostedClusterName = ""
		}},
		{name: "wrong hosted cluster name", reason: BackupHostedClusterLookupFailedReason, mutate: func(f *recoveryTest) {
			f.recovery.Spec.HostedClusterName = "absent"
		}},
		{name: "missing backup", reason: BackupNotFoundReason, mutate: func(f *recoveryTest) {
			f.recovery.Spec.VeleroBackupName = "absent"
		}},
		{name: "backup in wrong namespace", reason: BackupNotFoundReason, mutate: func(f *recoveryTest) {
			f.backup.SetNamespace(f.recovery.Namespace)
		}},
		{name: "missing backup annotation", reason: BackupResourceIDMismatchReason, mutate: func(f *recoveryTest) {
			f.backup.SetAnnotations(nil)
		}},
		{name: "wrong backup ID", reason: BackupResourceIDMismatchReason, mutate: func(f *recoveryTest) {
			f.backup.SetAnnotations(map[string]string{controllerutils.HcpClusterAzureResourceIdAnnotation: "other"})
		}},
		{name: "deleting backup", reason: BackupDeletingReason, mutate: func(f *recoveryTest) {
			now := metav1.NewTime(testNow)
			f.backup.SetDeletionTimestamp(&now)
		}},
		{name: "missing phase", reason: BackupNotCompletedReason, mutate: func(f *recoveryTest) {
			delete(f.backup.Object, "status")
		}},
		{name: "malformed phase", reason: BackupInvalidPhaseReason, mutate: func(f *recoveryTest) {
			f.backup.Object["status"] = map[string]interface{}{"phase": int64(42)}
		}},
		{name: "missing hosted cluster annotation", reason: BackupHostedClusterResourceIDMismatchReason, mutate: func(f *recoveryTest) {
			f.hc.Annotations = nil
		}},
		{name: "wrong hosted cluster ID", reason: BackupHostedClusterResourceIDMismatchReason, mutate: func(f *recoveryTest) {
			f.hc.Annotations[controllerutils.HcpClusterAzureResourceIdAnnotation] = "other"
		}},
		{name: "hosted cluster in wrong namespace", reason: BackupHostedClusterLookupFailedReason, mutate: func(f *recoveryTest) {
			f.hc.Namespace = "other"
		}},
		{name: "missing control plane namespace", reason: BackupMissingControlPlaneNamespaceReason, mutate: func(f *recoveryTest) {
			f.recovery.Spec.ControlPlaneNamespace = ""
		}},
		{name: "nonexistent control plane namespace", reason: BackupControlPlaneNamespaceLookupFailedReason, mutate: func(f *recoveryTest) {
			f.recovery.Spec.ControlPlaneNamespace = "absent"
		}},
		{name: "missing control plane namespace annotation", reason: BackupControlPlaneNamespaceResourceIDMismatchReason, mutate: func(f *recoveryTest) {
			f.controlPlaneNamespace.Annotations = nil
		}},
		{name: "matching namespace name but wrong resource ID", reason: BackupControlPlaneNamespaceResourceIDMismatchReason, mutate: func(f *recoveryTest) {
			f.controlPlaneNamespace.Annotations[controllerutils.HcpClusterAzureResourceIdAnnotation] = "other"
		}},
		{name: "another cluster's control plane namespace", reason: BackupControlPlaneNamespaceResourceIDMismatchReason, mutate: func(f *recoveryTest) {
			f.recovery.Spec.ControlPlaneNamespace = "other-cluster"
			f.controlPlaneNamespace.Name = f.recovery.Spec.ControlPlaneNamespace
			f.controlPlaneNamespace.Annotations[controllerutils.HcpClusterAzureResourceIdAnnotation] = "other"
		}},
		{name: "backup and request agree on wrong control plane namespace", reason: BackupControlPlaneNamespaceResourceIDMismatchReason, mutate: func(f *recoveryTest) {
			f.recovery.Spec.ControlPlaneNamespace = "other-cluster"
			f.controlPlaneNamespace.Name = f.recovery.Spec.ControlPlaneNamespace
			f.controlPlaneNamespace.Annotations[controllerutils.HcpClusterAzureResourceIdAnnotation] = "other"
			f.backup.Object["spec"] = map[string]interface{}{
				"includedNamespaces": []interface{}{f.hc.Namespace, f.recovery.Spec.ControlPlaneNamespace},
			}
		}},
		{name: "nonstandard namespace name with matching resource ID", reason: RecoveryInputsValidatedReason, mutate: func(f *recoveryTest) {
			f.recovery.Spec.ControlPlaneNamespace = "custom-control-plane"
			f.controlPlaneNamespace.Name = f.recovery.Spec.ControlPlaneNamespace
			f.backup.Object["spec"] = map[string]interface{}{
				"includedNamespaces": []interface{}{f.hc.Namespace, f.recovery.Spec.ControlPlaneNamespace},
			}
		}},
	}
	for _, phase := range []string{"", "New", "InProgress", "WaitingForPluginOperations", "Finalizing", "Failed", "FailedValidation", "PartiallyFailed", "Deleting", "completed"} {
		tests = append(tests, testCase{
			name: "reject phase " + phase, reason: BackupNotCompletedReason,
			mutate: func(f *recoveryTest) {
				f.backup.Object["status"] = map[string]interface{}{"phase": phase}
			},
		})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newRecoveryTest(t, func(f *recoveryTest) {
				if tt.mutate != nil {
					tt.mutate(f)
				}
			})
			recovery := f.recovery
			update := f.requireStepStatusUpdate(f.runStep(f.validateRecoveryInputs, recovery))
			if update.Status.RestoreName != recovery.Status.RestoreName || update.Status.RestoredToTimestamp != nil || len(update.Status.Conditions) != 2 {
				t.Fatalf("validation must preserve unrelated status fields: %#v", update)
			}
			condition := update.Status.Conditions[0]
			if condition.Type != hcprecoveryv1alpha1.ConditionRecoveryInputsValidated || condition.Reason != tt.reason || condition.Message == "" || !condition.LastTransitionTime.Time.Equal(testNow) || condition.ObservedGeneration != recovery.Generation {
				t.Fatalf("unexpected validation condition: %#v", condition)
			}
			if tt.reason == RecoveryInputsValidatedReason {
				if condition.Status != metav1.ConditionTrue || update.Status.Phase != hcprecoveryv1alpha1.RestoreStatePreparing || update.Status.CompletedAt != nil {
					t.Fatalf("unexpected successful status: %#v", update.Status)
				}
			} else if condition.Status != metav1.ConditionFalse ||
				// The spec is immutable, so none of these rejections can be corrected
				// on the object: they end the recovery rather than blocking it.
				update.Status.Phase != hcprecoveryv1alpha1.RestoreStateFailed ||
				update.Status.CompletedAt == nil || !update.Status.CompletedAt.Time.Equal(testNow) {
				t.Fatalf("unexpected failure status: %#v", update.Status)
			}
			if !update.Status.StartedAt.Equal(recovery.Status.StartedAt) || update.UID != recovery.UID || update.ResourceVersion != recovery.ResourceVersion {
				t.Fatal("validation did not preserve the start time or concurrency preconditions")
			}
			repeated := f.requireStepStatusUpdate(f.runStep(f.validateRecoveryInputs, recovery))
			if !reflect.DeepEqual(update, repeated) {
				t.Fatal("validation should be deterministic")
			}
		})
	}
}

func TestRecoveryInputsValidationCMK(t *testing.T) {
	type testCase struct {
		name   string
		mutate func(*recoveryTest)
		reason string
	}
	tests := []testCase{
		{name: "matching active key without rotation history"},
		{name: "desired key does not override observed active key", mutate: func(f *recoveryTest) {
			f.hc.Spec.SecretEncryption.KMS.Azure.ActiveKey.KeyVersion = "desired-version"
		}},
		{name: "backup matches only desired key", reason: BackupKMSKeyFingerprintMismatchReason, mutate: func(f *recoveryTest) {
			f.hc.Status.SecretEncryption.ActiveKey.Azure.KeyVersion = "observed-version"
		}},
		{name: "backup matches only a historical key", reason: BackupKMSKeyFingerprintMismatchReason, mutate: func(f *recoveryTest) {
			oldFingerprint := backup.ActiveKMSKeyFingerprint(f.hc)
			f.hc.Status.SecretEncryption.ActiveKey.Azure.KeyVersion = "rotated-version"
			f.hc.Status.SecretEncryption.History = []hypershiftv1beta1.EncryptionMigrationHistory{{
				From: hypershiftv1beta1.EncryptionKeyReference{
					Provider: hypershiftv1beta1.SecretEncryptionProviderAzure, Fingerprint: oldFingerprint,
				},
				To: hypershiftv1beta1.EncryptionKeyReference{
					Provider: hypershiftv1beta1.SecretEncryptionProviderAzure, Fingerprint: backup.ActiveKMSKeyFingerprint(f.hc),
				},
				State: hypershiftv1beta1.EncryptionMigrationStateCompleted,
			}}
		}},
		{name: "fingerprint comparison is exact", reason: BackupKMSKeyFingerprintMismatchReason, mutate: func(f *recoveryTest) {
			annotations := f.backup.GetAnnotations()
			annotations[controllerutils.HcpClusterKmsKeyFingerprintAnnotation] = strings.ToUpper(backup.ActiveKMSKeyFingerprint(f.hc))
			f.backup.SetAnnotations(annotations)
		}},
		{name: "missing backup fingerprint", reason: BackupKMSKeyFingerprintMismatchReason, mutate: func(f *recoveryTest) {
			annotations := f.backup.GetAnnotations()
			delete(annotations, controllerutils.HcpClusterKmsKeyFingerprintAnnotation)
			f.backup.SetAnnotations(annotations)
		}},
		{name: "missing observed active key", reason: BackupInvalidKMSEncryptionStateReason, mutate: func(f *recoveryTest) {
			f.hc.Status.SecretEncryption.ActiveKey = hypershiftv1beta1.SecretEncryptionKeyStatus{}
		}},
		{name: "missing observed key and fingerprint cannot bypass validation", reason: BackupInvalidKMSEncryptionStateReason, mutate: func(f *recoveryTest) {
			f.hc.Status.SecretEncryption.ActiveKey = hypershiftv1beta1.SecretEncryptionKeyStatus{}
			annotations := f.backup.GetAnnotations()
			delete(annotations, controllerutils.HcpClusterKmsKeyFingerprintAnnotation)
			f.backup.SetAnnotations(annotations)
		}},
		{name: "missing observed vault", reason: BackupInvalidKMSEncryptionStateReason, mutate: func(f *recoveryTest) {
			f.hc.Status.SecretEncryption.ActiveKey.Azure.KeyVaultName = ""
		}},
		{name: "missing observed key name", reason: BackupInvalidKMSEncryptionStateReason, mutate: func(f *recoveryTest) {
			f.hc.Status.SecretEncryption.ActiveKey.Azure.KeyName = ""
		}},
		{name: "missing observed key version", reason: BackupInvalidKMSEncryptionStateReason, mutate: func(f *recoveryTest) {
			f.hc.Status.SecretEncryption.ActiveKey.Azure.KeyVersion = ""
		}},
		{name: "inconsistent observed key provider", reason: BackupInvalidKMSEncryptionStateReason, mutate: func(f *recoveryTest) {
			f.hc.Status.SecretEncryption.ActiveKey.Provider = hypershiftv1beta1.SecretEncryptionProviderAESCBC
		}},
		{name: "CMK backup with non-CMK target", reason: BackupInvalidKMSEncryptionStateReason, mutate: func(f *recoveryTest) {
			f.hc.Spec.SecretEncryption = nil
			f.hc.Status.SecretEncryption = hypershiftv1beta1.SecretEncryptionStatus{}
		}},
		{name: "observed CMK cannot bypass validation without desired spec", reason: BackupKMSKeyFingerprintMismatchReason, mutate: func(f *recoveryTest) {
			f.hc.Spec.SecretEncryption = nil
			f.hc.Status.SecretEncryption.ActiveKey.Azure.KeyVersion = "rotated-version"
			annotations := f.backup.GetAnnotations()
			delete(annotations, controllerutils.HcpClusterKmsKeyFingerprintAnnotation)
			f.backup.SetAnnotations(annotations)
		}},
		{name: "matching key with rotation target", reason: BackupKMSKeyRotationInProgressReason, mutate: func(f *recoveryTest) {
			f.hc.Status.SecretEncryption.TargetKey = f.hc.Status.SecretEncryption.ActiveKey
			f.hc.Status.SecretEncryption.TargetKey.Azure.KeyVersion = "target-version"
		}},
		{name: "partial rotation target", reason: BackupKMSKeyRotationInProgressReason, mutate: func(f *recoveryTest) {
			f.hc.Status.SecretEncryption.TargetKey.Azure.KeyVaultName = "target-vault"
		}},
		{name: "rotation target without observed active key", reason: BackupKMSKeyRotationInProgressReason, mutate: func(f *recoveryTest) {
			f.hc.Spec.SecretEncryption = nil
			f.hc.Status.SecretEncryption.TargetKey = f.hc.Status.SecretEncryption.ActiveKey
			f.hc.Status.SecretEncryption.ActiveKey = hypershiftv1beta1.SecretEncryptionKeyStatus{}
			annotations := f.backup.GetAnnotations()
			delete(annotations, controllerutils.HcpClusterKmsKeyFingerprintAnnotation)
			f.backup.SetAnnotations(annotations)
		}},
	}
	for _, rotation := range []struct {
		state  hypershiftv1beta1.EncryptionMigrationState
		reason string
	}{
		{state: hypershiftv1beta1.EncryptionMigrationStateCompleted},
		{state: hypershiftv1beta1.EncryptionMigrationStateInterrupted},
		{state: hypershiftv1beta1.EncryptionMigrationStateReadOnlyDeploy, reason: BackupKMSKeyRotationInProgressReason},
		{state: hypershiftv1beta1.EncryptionMigrationStateWritePromote, reason: BackupKMSKeyRotationInProgressReason},
		{state: hypershiftv1beta1.EncryptionMigrationStateMigrating, reason: BackupKMSKeyRotationInProgressReason},
		{state: "", reason: BackupInvalidKMSEncryptionStateReason},
		{state: "Unknown", reason: BackupInvalidKMSEncryptionStateReason},
	} {
		tests = append(tests, testCase{
			name: "cleared target with latest rotation state " + string(rotation.state), reason: rotation.reason,
			mutate: func(f *recoveryTest) {
				f.hc.Status.SecretEncryption.History = []hypershiftv1beta1.EncryptionMigrationHistory{{State: rotation.state}}
			},
		})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newRecoveryTest(t, func(f *recoveryTest) {
				f.configureAzureCMK()
				if tt.mutate != nil {
					tt.mutate(f)
				}
			})
			if tt.reason != "" {
				f.assertStepFailureContract(f.validateRecoveryInputs, stepFailureExpectation{
					conditionType: hcprecoveryv1alpha1.ConditionRecoveryInputsValidated,
					reason:        tt.reason,
					messageParts:  []string{f.hc.Namespace + "/" + f.hc.Name},
					permanent:     true,
				})
				return
			}
			update := f.requireStepStatusUpdate(f.runStep(f.validateRecoveryInputs, f.recovery))
			if !meta.IsStatusConditionTrue(update.Status.Conditions, hcprecoveryv1alpha1.ConditionRecoveryInputsValidated) ||
				update.Status.Phase != hcprecoveryv1alpha1.RestoreStatePreparing || update.Status.CompletedAt != nil {
				t.Fatalf("matching stable CMK must allow preparation: %#v", update.Status)
			}
		})
	}
}

func TestRecoveryInputsValidationNamespaceScope(t *testing.T) {
	both := []interface{}{"hosted-cluster", "hosted-cluster-cluster"}
	for _, tt := range []struct {
		name     string
		included interface{}
		excluded interface{}
		reason   string
	}{
		{name: "excluded target", included: both, excluded: []interface{}{"hosted-cluster-cluster"}, reason: BackupNamespaceMismatchReason},
		{name: "excluded wildcard", included: both, excluded: []interface{}{"*"}, reason: BackupNamespaceMismatchReason},
		{name: "malformed exclusions", included: both, excluded: "hosted-cluster", reason: BackupInvalidNamespacesReason},
		{name: "reversed namespace order", included: []interface{}{"hosted-cluster-cluster", "hosted-cluster"}, reason: RecoveryInputsValidatedReason},
		{name: "missing namespaces", reason: BackupNamespaceMismatchReason},
		{name: "empty namespaces", included: []interface{}{}, reason: BackupNamespaceMismatchReason},
		{name: "wildcard namespaces", included: []interface{}{"*"}, reason: BackupNamespaceMismatchReason},
		{name: "missing hosted cluster namespace", included: []interface{}{"hosted-cluster-cluster"}, reason: BackupNamespaceMismatchReason},
		{name: "missing control plane namespace", included: []interface{}{"hosted-cluster"}, reason: BackupNamespaceMismatchReason},
		{name: "wrong control plane namespace", included: []interface{}{"hosted-cluster", "other-cluster"}, reason: BackupNamespaceMismatchReason},
		{name: "extra namespace", included: []interface{}{"hosted-cluster", "hosted-cluster-cluster", "other-cluster"}, reason: BackupNamespaceMismatchReason},
		{name: "malformed namespaces", included: "hosted-cluster-cluster", reason: BackupInvalidNamespacesReason},
		{name: "malformed namespace entry", included: []interface{}{"hosted-cluster", int64(42)}, reason: BackupInvalidNamespacesReason},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newRecoveryTest(t, func(f *recoveryTest) {
				spec := map[string]interface{}{}
				if tt.included != nil {
					spec["includedNamespaces"] = tt.included
				}
				if tt.excluded != nil {
					spec["excludedNamespaces"] = tt.excluded
				}
				f.backup.Object["spec"] = spec
			})
			if tt.reason == RecoveryInputsValidatedReason {
				update := f.requireStepStatusUpdate(f.runStep(f.validateRecoveryInputs, f.recovery))
				if !meta.IsStatusConditionTrue(update.Status.Conditions, hcprecoveryv1alpha1.ConditionRecoveryInputsValidated) {
					t.Fatalf("expected valid namespace scope: %#v", update.Status)
				}
				return
			}
			f.assertStepFailureContract(f.validateRecoveryInputs, stepFailureExpectation{
				conditionType: hcprecoveryv1alpha1.ConditionRecoveryInputsValidated,
				reason:        tt.reason,
				permanent:     true,
			})
		})
	}
}

// Recovery inputs recorded as validated are re-checked on every pass. If the backup has since been
// deleted the recovery cannot proceed and the spec cannot be pointed at another backup,
// so the recovery ends rather than waiting for a backup that will never return.
func TestRecoveryInputsValidationRechecksValidatedBackup(t *testing.T) {
	f := newRecoveryTest(t, func(f *recoveryTest) {
		f.recovery.Status.Conditions = []metav1.Condition{{Type: hcprecoveryv1alpha1.ConditionRecoveryInputsValidated, Status: metav1.ConditionTrue}}
	})
	if err := f.informers["Backup"].GetStore().Delete(f.backup); err != nil {
		t.Fatal(err)
	}
	update := f.requireStepStatusUpdate(f.runStep(f.validateRecoveryInputs, f.recovery))
	if update.Status.Conditions[0].Status != metav1.ConditionFalse || update.Status.Phase != hcprecoveryv1alpha1.RestoreStateFailed {
		t.Fatalf("expected the recovery to end after the backup disappeared: %#v", update.Status)
	}
}

func TestRecoveryInputsValidationUsesNamedHostedCluster(t *testing.T) {
	f := newRecoveryTest(t, nil)
	second := f.hc.DeepCopy()
	second.Name = "second"
	if err := f.informers["HostedCluster"].GetStore().Add(second); err != nil {
		t.Fatal(err)
	}
	update := f.requireStepStatusUpdate(f.runStep(f.validateRecoveryInputs, f.recovery))
	if update.Status.Conditions[0].Reason != RecoveryInputsValidatedReason {
		t.Fatalf("another HostedCluster must not block validation of the named target: %#v", update.Status)
	}
	target := f.hc.DeepCopy()
	target.Annotations[controllerutils.HcpClusterAzureResourceIdAnnotation] = "other"
	if err := f.informers["HostedCluster"].GetStore().Update(target); err != nil {
		t.Fatal(err)
	}
	update = f.requireStepStatusUpdate(f.runStep(f.validateRecoveryInputs, f.recovery))
	if update.Status.Conditions[0].Reason != BackupHostedClusterResourceIDMismatchReason {
		t.Fatalf("another matching HostedCluster must not bypass the target's resource ID check: %#v", update.Status)
	}
}

func TestRecoveryInputsValidationHostedClusterLookupError(t *testing.T) {
	f := newRecoveryTest(t, nil)
	lookupErr := errors.New("cache lookup failed")
	f.hypershiftLister = failingHostedClusterLister{err: lookupErr}
	f.assertStepFailureContract(f.validateRecoveryInputs, stepFailureExpectation{
		conditionType: hcprecoveryv1alpha1.ConditionRecoveryInputsValidated,
		reason:        BackupHostedClusterLookupFailedReason,
		retryError:    func(err error) bool { return errors.Is(err, lookupErr) },
	})
}

func TestRecoveryInputsValidationControlPlaneNamespaceLookupError(t *testing.T) {
	f := newRecoveryTest(t, nil)
	lookupErr := errors.New("namespace lookup failed")
	f.kubeClient.PrependReactor("get", "namespaces", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, lookupErr
	})
	f.assertStepFailureContract(f.validateRecoveryInputs, stepFailureExpectation{
		conditionType: hcprecoveryv1alpha1.ConditionRecoveryInputsValidated,
		reason:        BackupControlPlaneNamespaceLookupFailedReason,
		messageParts:  []string{f.recovery.Spec.ControlPlaneNamespace, lookupErr.Error()},
		retryError:    func(err error) bool { return errors.Is(err, lookupErr) },
	})
}

func TestRecoveryInputsValidationRechecksControlPlaneNamespaceAnnotation(t *testing.T) {
	f := newRecoveryTest(t, nil)
	recovery := f.recovery.DeepCopy()
	recovery.Status = f.requireStepStatusUpdate(f.runStep(f.validateRecoveryInputs, recovery)).Status
	namespace := f.controlPlaneNamespace.DeepCopy()
	namespace.Annotations[controllerutils.HcpClusterAzureResourceIdAnnotation] = "other"
	if _, err := f.kubeClient.CoreV1().Namespaces().Update(f.ctx, namespace, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	update := f.requireStepStatusUpdate(f.runStep(f.validateRecoveryInputs, recovery))
	condition := meta.FindStatusCondition(update.Status.Conditions, hcprecoveryv1alpha1.ConditionRecoveryInputsValidated)
	if condition == nil || condition.Status != metav1.ConditionFalse ||
		condition.Reason != BackupControlPlaneNamespaceResourceIDMismatchReason ||
		update.Status.Phase != hcprecoveryv1alpha1.RestoreStateFailed {
		t.Fatalf("changed namespace annotation must invalidate the recovery: %#v", update.Status)
	}
}

func TestRecoveryInputsValidationStatusWriteError(t *testing.T) {
	for _, valid := range []bool{true, false} {
		t.Run(fmt.Sprint(valid), func(t *testing.T) {
			f := newRecoveryTest(t, func(f *recoveryTest) {
				if !valid {
					f.recovery.Spec.VeleroBackupName = "absent"
				}
			})
			writeErr := errors.New("status write failed")
			f.writeErr = writeErr
			if result, err := f.runStep(f.validateRecoveryInputs, f.recovery); result.outcome == stepContinue || !errors.Is(err, writeErr) {
				t.Fatalf("expected status write error, got outcome=%v err=%v", result.outcome, err)
			}
		})
	}
}

func TestCollectEtcdInformation(t *testing.T) {
	writeErr := errors.New("status write failed")
	tests := []struct {
		name        string
		mutate      func(*recoveryTest)
		wantDetails *hcprecoveryv1alpha1.EtcdRuntimeDetails
		writeErr    error
	}{
		{
			name: "collect image and pod identity",
		},
		{
			name: "select etcd container among sidecars",
			mutate: func(f *recoveryTest) {
				f.sts.Spec.Template.Spec.Containers = []corev1.Container{
					{Name: "metrics", Image: "metrics:before"},
					{Name: "etcd", Image: testEtcdImage},
					{Name: "proxy", Image: "proxy:after"},
				}
			},
		},
		{
			name: "complete details are refreshed from the StatefulSet",
			mutate: func(f *recoveryTest) {
				f.recovery.Status.EtcdRuntimeDetails = &hcprecoveryv1alpha1.EtcdRuntimeDetails{
					Image: "previous-image", RunAsUser: ptr.To[int64](0), FSGroup: ptr.To[int64](0),
				}
			},
		},
		{
			name: "empty details are collected",
			mutate: func(f *recoveryTest) {
				f.recovery.Status.EtcdRuntimeDetails = &hcprecoveryv1alpha1.EtcdRuntimeDetails{}
			},
		},
		{
			name: "successful collection replaces previous failure",
			mutate: func(f *recoveryTest) {
				f.recovery.Status.Conditions = append(f.recovery.Status.Conditions, metav1.Condition{
					Type: hcprecoveryv1alpha1.EtcdRuntimeDetailsCollectedCondition, Status: metav1.ConditionFalse,
					Reason: EtcdStatefulSetNotFoundReason, Message: "Previously missing",
					ObservedGeneration: f.recovery.Generation, LastTransitionTime: metav1.NewTime(testNow.Add(-time.Hour)),
				})
			},
		},
		{
			name: "missing image is collected",
			mutate: func(f *recoveryTest) {
				f.recovery.Status.EtcdRuntimeDetails = &hcprecoveryv1alpha1.EtcdRuntimeDetails{
					RunAsUser: ptr.To[int64](1), FSGroup: ptr.To[int64](2),
				}
			},
		},
		{
			name: "missing runAsUser is collected",
			mutate: func(f *recoveryTest) {
				f.recovery.Status.EtcdRuntimeDetails = &hcprecoveryv1alpha1.EtcdRuntimeDetails{
					Image: "previous-image", FSGroup: ptr.To[int64](2),
				}
			},
		},
		{
			name: "missing fsGroup is collected",
			mutate: func(f *recoveryTest) {
				f.recovery.Status.EtcdRuntimeDetails = &hcprecoveryv1alpha1.EtcdRuntimeDetails{
					Image: "previous-image", RunAsUser: ptr.To[int64](1),
				}
			},
		},
		{
			name: "no pod security context",
			mutate: func(f *recoveryTest) {
				f.sts.Spec.Template.Spec.SecurityContext = nil
				f.sts.Spec.Template.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{
					RunAsUser: ptr.To[int64](42),
				}
			},
			wantDetails: &hcprecoveryv1alpha1.EtcdRuntimeDetails{Image: testEtcdImage},
		},
		{
			name: "empty pod security context",
			mutate: func(f *recoveryTest) {
				f.sts.Spec.Template.Spec.SecurityContext = &corev1.PodSecurityContext{}
			},
			wantDetails: &hcprecoveryv1alpha1.EtcdRuntimeDetails{Image: testEtcdImage},
		},
		{
			name: "only runAsUser is set",
			mutate: func(f *recoveryTest) {
				f.sts.Spec.Template.Spec.SecurityContext.FSGroup = nil
			},
			wantDetails: &hcprecoveryv1alpha1.EtcdRuntimeDetails{Image: testEtcdImage, RunAsUser: ptr.To[int64](1000)},
		},
		{
			name: "only fsGroup is set",
			mutate: func(f *recoveryTest) {
				f.sts.Spec.Template.Spec.SecurityContext.RunAsUser = nil
			},
			wantDetails: &hcprecoveryv1alpha1.EtcdRuntimeDetails{Image: testEtcdImage, FSGroup: ptr.To[int64](2000)},
		},
		{
			name: "zero identity values are preserved",
			mutate: func(f *recoveryTest) {
				f.sts.Spec.Template.Spec.SecurityContext = &corev1.PodSecurityContext{
					RunAsUser: ptr.To[int64](0), FSGroup: ptr.To[int64](0),
				}
			},
			wantDetails: &hcprecoveryv1alpha1.EtcdRuntimeDetails{
				Image: testEtcdImage, RunAsUser: ptr.To[int64](0), FSGroup: ptr.To[int64](0),
			},
		},
		{
			name:     "status write error is propagated",
			writeErr: writeErr,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newRecoveryTest(t, func(f *recoveryTest) {
				restoredTo := metav1.NewTime(testNow.Add(-2 * time.Hour))
				f.recovery.Status.EtcdRuntimeDetails = nil
				f.recovery.Status.RestoredToTimestamp = &restoredTo
				if tt.mutate != nil {
					tt.mutate(f)
				}
			})
			recovery := f.recovery
			f.writeErr = tt.writeErr
			wrongNamespace := f.sts.DeepCopy()
			wrongNamespace.Namespace = recovery.Namespace
			wrongNamespace.Spec.Template.Spec.Containers = []corev1.Container{{Name: "etcd", Image: "wrong-namespace"}}
			if err := f.informers["StatefulSet"].GetStore().Add(wrongNamespace); err != nil {
				t.Fatal(err)
			}

			result, err := f.runStep(f.collectEtcdInformation, recovery)
			if result.outcome == stepContinue || !errors.Is(err, tt.writeErr) {
				t.Fatalf("expected the step to stop with err=%v, got outcome=%v err=%v", tt.writeErr, result.outcome, err)
			}
			if len(f.client.Actions()) != 1 || len(f.updates) != 1 {
				t.Fatalf("expected exactly one status patch, got actions=%v updates=%v", f.client.Actions(), f.updates)
			}
			update := f.updates[0]
			if update.Name != recovery.Name || update.Namespace != recovery.Namespace ||
				update.UID != recovery.UID || update.ResourceVersion != recovery.ResourceVersion {
				t.Fatalf("status patch lost identity or concurrency preconditions: %#v", update.ObjectMeta)
			}
			wantStatus := recovery.DeepCopy().Status
			meta.RemoveStatusCondition(&wantStatus.Conditions, hcprecoveryv1alpha1.EtcdRuntimeDetailsCollectedCondition)
			wantStatus.Conditions = append([]metav1.Condition{{
				Type: hcprecoveryv1alpha1.EtcdRuntimeDetailsCollectedCondition, Status: metav1.ConditionTrue,
				Reason:             EtcdRuntimeDetailsCollectedReason,
				Message:            "Collected etcd runtime details from StatefulSet " + recovery.Spec.ControlPlaneNamespace + "/etcd",
				ObservedGeneration: recovery.Generation, LastTransitionTime: metav1.NewTime(testNow),
			}}, wantStatus.Conditions...)
			wantStatus.EtcdRuntimeDetails = tt.wantDetails
			if wantStatus.EtcdRuntimeDetails == nil {
				wantStatus.EtcdRuntimeDetails = &hcprecoveryv1alpha1.EtcdRuntimeDetails{
					Image: testEtcdImage, RunAsUser: ptr.To[int64](1000), FSGroup: ptr.To[int64](2000),
				}
			}
			if !apiequality.Semantic.DeepEqual(update.Status, wantStatus) {
				t.Fatalf("unexpected status patch:\n got: %#v\nwant: %#v", update.Status, wantStatus)
			}
		})
	}
}

// This step runs on every Preparing reconcile. Once the details and the condition are
// recorded, re-running must not keep rewriting status — that would requeue the phase
// forever instead of letting Preparing advance.
func TestCollectEtcdInformationIsIdempotent(t *testing.T) {
	f := newRecoveryTest(t, nil)
	update := f.requireStepStatusUpdate(f.runStep(f.collectEtcdInformation, f.recovery))

	settled := f.recovery.DeepCopy()
	settled.Status = update.Status
	f.client.ClearActions()
	if result, err := f.runStep(f.collectEtcdInformation, settled); result.outcome != stepContinue || err != nil || len(f.client.Actions()) != 0 {
		t.Fatalf("recorded details must not be rewritten: outcome=%v err=%v actions=%v", result.outcome, err, f.client.Actions())
	}
}

type failingStatefulSetLister struct {
	appslisters.StatefulSetLister
	err error
}

func (l failingStatefulSetLister) StatefulSets(string) appslisters.StatefulSetNamespaceLister {
	return l
}

func (l failingStatefulSetLister) Get(string) (*appsv1.StatefulSet, error) {
	return nil, l.err
}

func TestCollectEtcdInformationFailures(t *testing.T) {
	lookupErr := errors.New("cache lookup failed")
	tests := []struct {
		name       string
		containers []corev1.Container
		err        error
		reason     string
		message    string
	}{
		{
			name: "missing StatefulSet", reason: EtcdStatefulSetNotFoundReason,
			message: "does not exist",
		},
		{
			name: "lookup failure", err: lookupErr, reason: EtcdStatefulSetLookupFailedReason,
			message: lookupErr.Error(),
		},
		{
			name: "missing etcd container", reason: EtcdImageMissingReason,
			containers: []corev1.Container{{Name: "sidecar", Image: "sidecar:test"}},
			message:    "has no etcd container with an image",
		},
		{
			name: "empty etcd image", reason: EtcdImageMissingReason,
			containers: []corev1.Container{{Name: "etcd"}},
			message:    "has no etcd container with an image",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newRecoveryTest(t, func(f *recoveryTest) {
				f.recovery.Status.EtcdRuntimeDetails = nil
				f.sts.Spec.Template.Spec.Containers = tt.containers
			})
			if tt.containers == nil {
				if err := f.informers["StatefulSet"].GetStore().Delete(f.sts); err != nil {
					t.Fatal(err)
				}
			}
			if tt.err != nil {
				f.statefulsetLister = failingStatefulSetLister{err: tt.err}
			}
			f.assertStepFailureContract(f.collectEtcdInformation, stepFailureExpectation{
				conditionType: hcprecoveryv1alpha1.EtcdRuntimeDetailsCollectedCondition,
				reason:        tt.reason,
				messageParts:  []string{tt.message, f.recovery.Spec.ControlPlaneNamespace + "/etcd"},
				permanent:     tt.reason == EtcdImageMissingReason,
				retryError: func(err error) bool {
					if tt.err != nil {
						return errors.Is(err, tt.err)
					}
					return apierrors.IsNotFound(err)
				},
			})
		})
	}
}

func TestPauseHostedCluster(t *testing.T) {
	tests := []struct {
		name        string
		pausedUntil *string
		wantPause   bool
	}{
		{
			name:      "unset pause is set indefinitely",
			wantPause: true,
		},
		{
			name:        "empty pause is set indefinitely",
			pausedUntil: ptr.To(""),
			wantPause:   true,
		},
		{
			name:        "false pause is set indefinitely",
			pausedUntil: ptr.To("false"),
			wantPause:   true,
		},
		{
			name:        "uppercase false pause is set indefinitely",
			pausedUntil: ptr.To("FALSE"),
			wantPause:   true,
		},
		{
			name:        "future pause is replaced with indefinite pause",
			pausedUntil: ptr.To(testNow.Add(time.Hour).Format(time.RFC3339)),
			wantPause:   true,
		},
		{
			name:        "expired pause is replaced with indefinite pause",
			pausedUntil: ptr.To(testNow.Add(-time.Hour).Format(time.RFC3339)),
			wantPause:   true,
		},
		{
			name:        "indefinite pause only records success",
			pausedUntil: ptr.To("true"),
			wantPause:   false,
		},
		{
			name:        "uppercase indefinite pause only records success",
			pausedUntil: ptr.To("TRUE"),
			wantPause:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newRecoveryTest(t, func(f *recoveryTest) {
				f.hc.Spec.PausedUntil = tt.pausedUntil
				f.recovery.Status.Conditions = append(f.recovery.Status.Conditions, metav1.Condition{
					Type: hcprecoveryv1alpha1.ConditionHostedClusterPaused, Status: metav1.ConditionFalse,
					Reason: HostedClusterPauseFailedReason, LastTransitionTime: metav1.NewTime(testNow.Add(-time.Hour)),
				})
			})
			recovery, hc := f.recovery, f.hc
			other := hc.DeepCopy()
			other.Name = "other"
			if err := f.informers["HostedCluster"].GetStore().Add(other); err != nil {
				t.Fatal(err)
			}
			// The generated HyperShift apply schema lacks the HostedCluster type.
			tracker := clienttesting.NewFieldManagedObjectTracker(hypershiftscheme.Scheme,
				hypershiftscheme.Codecs.UniversalDecoder(), managedfields.NewDeducedTypeConverter())
			if err := tracker.Add(hc); err != nil {
				t.Fatal(err)
			}
			f.hcClient.PrependReactor("*", "*", clienttesting.ObjectReaction(tracker))
			if tt.pausedUntil != nil {
				cfg := hypershiftapply.HostedCluster(hc.Name, hc.Namespace).
					WithSpec(hypershiftapply.HostedClusterSpec().WithPausedUntil(*tt.pausedUntil))
				if _, err := f.hcClient.HypershiftV1beta1().HostedClusters(hc.Namespace).
					Apply(f.ctx, cfg, metav1.ApplyOptions{FieldManager: "original-owner"}); err != nil {
					t.Fatal(err)
				}
				f.hcClient.ClearActions()
			}
			result, err := f.runStep(f.pauseHostedCluster, recovery)
			if result.outcome != stepStop || err != nil {
				t.Fatalf("expected one successful action, got outcome=%v err=%v", result.outcome, err)
			}
			if tt.wantPause {
				if len(f.hcClient.Actions()) != 1 || len(f.client.Actions()) != 0 {
					t.Fatalf("must pause before recording success: hc=%v recovery=%v", f.hcClient.Actions(), f.client.Actions())
				}
				patch := f.hcClient.Actions()[0].(clienttesting.PatchAction)
				if patch.GetPatchType() != types.MergePatchType {
					t.Fatalf("expected merge patch, got %v", patch.GetPatchType())
				}
				var update hypershiftapply.HostedClusterApplyConfiguration
				if err := json.Unmarshal(patch.GetPatch(), &update); err != nil {
					t.Fatal(err)
				}
				if update.UID == nil || *update.UID != hc.UID ||
					update.ResourceVersion == nil || *update.ResourceVersion != hc.ResourceVersion {
					t.Fatalf("pause patch lost concurrency preconditions: %#v", update.ObjectMetaApplyConfiguration)
				}
				updated, err := f.hcClient.HypershiftV1beta1().HostedClusters(hc.Namespace).Get(f.ctx, hc.Name, metav1.GetOptions{})
				want := hc.DeepCopy()
				want.Spec.PausedUntil = ptr.To("true")
				if err != nil || !apiequality.Semantic.DeepEqual(updated.Spec, want.Spec) {
					t.Fatalf("expected only an indefinite pause, got cluster=%#v err=%v", updated, err)
				}
				return
			}
			if len(f.hcClient.Actions()) != 0 || len(f.client.Actions()) != 1 {
				t.Fatalf("expected only a status patch: hc=%v recovery=%v", f.hcClient.Actions(), f.client.Actions())
			}
			updated := f.requireStepStatusUpdate(result, err)
			want := recovery.DeepCopy()
			meta.RemoveStatusCondition(&want.Status.Conditions, hcprecoveryv1alpha1.ConditionHostedClusterPaused)
			want.Status.Conditions = append([]metav1.Condition{{
				Type: hcprecoveryv1alpha1.ConditionHostedClusterPaused, Status: metav1.ConditionTrue,
				Reason: "ClusterPaused", Message: "HostedCluster has been paused",
				ObservedGeneration: recovery.Generation, LastTransitionTime: metav1.NewTime(testNow),
			}}, want.Status.Conditions...)
			if !apiequality.Semantic.DeepEqual(updated.Status, want.Status) {
				t.Fatalf("unexpected recovery status: recovery=%#v", updated)
			}
		})
	}
}

func TestPauseHostedClusterUsesOneWaitingConditionForEitherActiveReconciler(t *testing.T) {
	f := newRecoveryTest(t, func(f *recoveryTest) {
		f.hc.Spec.PausedUntil = ptr.To("true")
		f.hc.Status.Conditions = []metav1.Condition{{
			Type: string(hypershiftv1beta1.ReconciliationActive), Status: metav1.ConditionTrue,
		}}
	})
	update := f.requireStepStatusUpdate(f.runStep(f.pauseHostedCluster, f.recovery))
	condition := meta.FindStatusCondition(update.Status.Conditions, hcprecoveryv1alpha1.ConditionHostedClusterPaused)
	if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != HostedClusterPausePendingReason {
		t.Fatalf("expected HostedCluster pause pending condition, got %#v", condition)
	}
	settled := f.recovery.DeepCopy()
	settled.Status = update.Status
	result, err := f.runStep(f.pauseHostedCluster, settled)
	if err != nil || result.outcome != stepStop || result.statusUpdate != nil {
		t.Fatalf("active HostedCluster reconciliation must keep waiting without rewriting status: outcome=%v status=%v err=%v",
			result.outcome, result.statusUpdate, err)
	}
	hc := f.hc.DeepCopy()
	hc.Status.Conditions[0].Status = metav1.ConditionFalse
	if err := f.informers["HostedCluster"].GetStore().Update(hc); err != nil {
		t.Fatal(err)
	}
	hcp := f.hcp.DeepCopy()
	hcp.Status.Conditions[0].Status = metav1.ConditionTrue
	if err := f.informers["HostedControlPlane"].GetStore().Update(hcp); err != nil {
		t.Fatal(err)
	}
	result, err = f.runStep(f.pauseHostedCluster, settled)
	if err != nil || result.outcome != stepStop || result.statusUpdate != nil {
		t.Fatalf("switching active reconcilers must keep the same waiting status: outcome=%v status=%v err=%v",
			result.outcome, result.statusUpdate, err)
	}
	if len(f.client.Actions()) != 1 || len(f.hcClient.Actions()) != 0 || len(f.kubeClient.Actions()) != 0 {
		t.Fatal("waiting for HostedCluster reconciliation to pause must only write the pending condition once")
	}
}

func TestPreparationRequiresHostedControlPlanePauseAcknowledgement(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mutate func(*hypershiftv1beta1.HostedControlPlane)
		ready  bool
	}{
		{name: "acknowledged", ready: true},
		{name: "operator active", mutate: func(hcp *hypershiftv1beta1.HostedControlPlane) {
			hcp.Status.Conditions[0].Status = metav1.ConditionTrue
		}},
	} {
		for _, live := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/live=%t", tt.name, live), func(t *testing.T) {
				f := newScaledDownRecoveryTest(t)
				hcp := f.hcp.DeepCopy()
				if tt.mutate != nil {
					tt.mutate(hcp)
				}
				step := f.pauseHostedCluster
				conditionType := hcprecoveryv1alpha1.ConditionHostedClusterPaused
				if live {
					// Leave the acknowledged cache untouched to exercise the live safety check.
					if err := f.hcClient.Tracker().Update(hypershiftv1beta1.SchemeGroupVersion.WithResource("hostedcontrolplanes"), hcp, hcp.Namespace); err != nil {
						t.Fatal(err)
					}
					step = f.validateScaledDown
					conditionType = hcprecoveryv1alpha1.ConditionScaledDownValidated
				} else if err := f.informers["HostedControlPlane"].GetStore().Update(hcp); err != nil {
					t.Fatal(err)
				}
				update := f.requireStepStatusUpdate(f.runStep(step, f.recovery))
				condition := meta.FindStatusCondition(update.Status.Conditions, conditionType)
				if condition == nil || (condition.Status == metav1.ConditionTrue) != tt.ready {
					t.Fatalf("pause readiness=%t, got condition %#v", tt.ready, condition)
				}
				if !tt.ready {
					reason := HostedClusterPausePendingReason
					if live {
						reason = HostedControlPlanePausePendingReason
					}
					if condition.Reason != reason {
						t.Fatalf("unexpected waiting condition: %#v", condition)
					}
					settled := f.recovery.DeepCopy()
					settled.Status = update.Status
					result, err := f.runStep(step, settled)
					if result.outcome == stepContinue || (!live && err != nil) || (live && err == nil) {
						t.Fatalf("unacknowledged pause must keep blocking: outcome=%v err=%v", result.outcome, err)
					}
				}
				for _, action := range append(f.kubeClient.Actions(), f.hcClient.Actions()...) {
					if action.GetVerb() != "get" && action.GetVerb() != "list" {
						t.Fatalf("pause validation must not mutate cluster resources: %v", action)
					}
				}
			})
		}
	}
}

func TestPreparationHostedControlPlaneLookupFailures(t *testing.T) {
	for _, live := range []bool{false, true} {
		t.Run(fmt.Sprintf("live=%t", live), func(t *testing.T) {
			f := newScaledDownRecoveryTest(t)
			step := f.pauseHostedCluster
			conditionType := hcprecoveryv1alpha1.ConditionHostedClusterPaused
			if live {
				if err := f.hcClient.Tracker().Delete(hypershiftv1beta1.SchemeGroupVersion.WithResource("hostedcontrolplanes"), f.hcp.Namespace, f.hcp.Name); err != nil {
					t.Fatal(err)
				}
				step = f.validateScaledDown
				conditionType = hcprecoveryv1alpha1.ConditionScaledDownValidated
			} else if err := f.informers["HostedControlPlane"].GetStore().Delete(f.hcp); err != nil {
				t.Fatal(err)
			}
			f.assertStepFailureContract(step, stepFailureExpectation{
				conditionType: conditionType,
				reason:        HostedControlPlaneLookupFailedReason,
				retryError:    apierrors.IsNotFound,
			})
		})
	}
}

func TestPauseHostedClusterFailures(t *testing.T) {
	lookupErr := errors.New("cache lookup failed")
	pauseErr := errors.New("pause patch failed")
	for _, tt := range []struct {
		name      string
		lookupErr error
		pauseErr  error
		reason    string
	}{
		{name: "lookup error", lookupErr: lookupErr, reason: HostedClusterLookupFailedReason},
		{name: "lookup not found", lookupErr: apierrors.NewNotFound(hypershiftv1beta1.Resource("hostedclusters"), "cluster"), reason: HostedClusterLookupFailedReason},
		{name: "pause error", pauseErr: pauseErr, reason: HostedClusterPauseFailedReason},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newRecoveryTest(t, nil)
			f.hcClient.PrependReactor("patch", "hostedclusters", func(clienttesting.Action) (bool, runtime.Object, error) {
				return true, nil, tt.pauseErr
			})
			if tt.lookupErr != nil {
				f.hypershiftLister = failingHostedClusterLister{err: tt.lookupErr}
			}
			wantErr := tt.lookupErr
			if wantErr == nil {
				wantErr = tt.pauseErr
			}
			f.assertStepFailureContract(f.pauseHostedCluster, stepFailureExpectation{
				conditionType: hcprecoveryv1alpha1.ConditionHostedClusterPaused,
				reason:        tt.reason,
				messageParts:  []string{f.recovery.Spec.HostedClusterNamespace, f.recovery.Spec.HostedClusterName},
				retryError:    func(err error) bool { return errors.Is(err, wantErr) },
			})
		})
	}
}

func TestPauseHostedClusterStatusFailure(t *testing.T) {
	f := newRecoveryTest(t, func(f *recoveryTest) { f.hc.Spec.PausedUntil = ptr.To("true") })
	writeErr := errors.New("status write failed")
	f.writeErr = writeErr
	if result, err := f.runStep(f.pauseHostedCluster, f.recovery); result.outcome == stepContinue || !errors.Is(err, writeErr) {
		t.Fatalf("expected status failure: outcome=%v err=%v", result.outcome, err)
	}
}

func TestScaleDownKubeAPIServerStep(t *testing.T) {
	for _, tt := range []struct {
		name           string
		replicas       *int32
		statusReplicas int32
		wantPatch      bool
		wantSuccess    bool
	}{
		{name: "running deployment is scaled down", replicas: ptr.To[int32](3), statusReplicas: 3, wantPatch: true},
		{name: "unset desired replicas is scaled down", statusReplicas: 3, wantPatch: true},
		{name: "waiting for observed replicas to reach zero", replicas: ptr.To[int32](0), statusReplicas: 3},
		{name: "both desired and observed zero replicas records success", replicas: ptr.To[int32](0), wantSuccess: true},
		{name: "observed zero with nonzero desired replicas still scales down", replicas: ptr.To[int32](3), wantPatch: true},
		{name: "observed zero with unset desired replicas still scales down", wantPatch: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newRecoveryTest(t, func(f *recoveryTest) {
				f.deployment.Spec.Replicas = tt.replicas
				f.deployment.Status.Replicas = tt.statusReplicas
				f.recovery.Status.Conditions = append(f.recovery.Status.Conditions, metav1.Condition{
					Type: hcprecoveryv1alpha1.ConditionKubeAPIServerScaledToZero, Status: metav1.ConditionFalse,
					Reason:             KubeAPIServerPatchToZeroReplicasFailedReason,
					LastTransitionTime: metav1.NewTime(testNow.Add(-time.Hour)),
				})
			})
			f.kubeClient.PrependReactor("patch", "deployments", func(clienttesting.Action) (bool, runtime.Object, error) {
				return true, f.deployment.DeepCopy(), nil
			})

			// Every case here either patches, waits for the patch to take effect, or
			// records success — all of which stop the pass.
			result, err := f.runStep(f.scaleDownKubeAPIServer, f.recovery)
			if err != nil || result.outcome != stepStop {
				t.Fatalf("expected the step to stop the pass, got outcome=%v err=%v", result.outcome, err)
			}
			wantKubeActions := 0
			if tt.wantPatch {
				wantKubeActions = 1
			}
			if len(f.kubeClient.Actions()) != wantKubeActions {
				t.Fatalf("expected %d deployment writes, got %v", wantKubeActions, f.kubeClient.Actions())
			}
			if !tt.wantSuccess {
				if len(f.client.Actions()) != 0 {
					t.Fatalf("must not record success unless desired and observed replicas are both zero: %v", f.client.Actions())
				}
				return
			}
			updated := f.requireStepStatusUpdate(result, err)
			if len(f.client.Actions()) != 1 {
				t.Fatalf("expected only a completion status write, got %v", f.client.Actions())
			}
			want := f.recovery.DeepCopy()
			meta.RemoveStatusCondition(&want.Status.Conditions, hcprecoveryv1alpha1.ConditionKubeAPIServerScaledToZero)
			want.Status.Conditions = append([]metav1.Condition{{
				Type: hcprecoveryv1alpha1.ConditionKubeAPIServerScaledToZero, Status: metav1.ConditionTrue,
				Reason: "KubeAPIServerScaledToZeroReplicas", Message: "The kube-apiserver deployment is scaled to zero replicas",
				ObservedGeneration: f.recovery.Generation, LastTransitionTime: metav1.NewTime(testNow),
			}}, want.Status.Conditions...)
			if !apiequality.Semantic.DeepEqual(updated.Status, want.Status) {
				t.Fatalf("unexpected completion status:\n got: %#v\nwant: %#v", updated.Status, want.Status)
			}
		})
	}
}

func TestScaleDownKubeAPIServerStepRechecksCompletedCondition(t *testing.T) {
	f := newRecoveryTest(t, func(f *recoveryTest) {
		f.recovery.Status.Conditions = append(f.recovery.Status.Conditions, metav1.Condition{
			Type: hcprecoveryv1alpha1.ConditionKubeAPIServerScaledToZero, Status: metav1.ConditionTrue,
			Reason: "KubeAPIServerScaledToZeroReplicas", LastTransitionTime: metav1.NewTime(testNow),
		})
	})
	f.kubeClient.PrependReactor("patch", "deployments", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, f.deployment.DeepCopy(), nil
	})
	result, err := f.runStep(f.scaleDownKubeAPIServer, f.recovery)
	if result.outcome != stepStop || err != nil || len(f.client.Actions()) != 0 || len(f.kubeClient.Actions()) != 1 {
		t.Fatalf("stale success must not prevent scaling down, got outcome=%v err=%v recovery=%v kube=%v",
			result.outcome, err, f.client.Actions(), f.kubeClient.Actions())
	}
}

func TestScaleDownKubeAPIServerStepFailures(t *testing.T) {
	lookupErr := errors.New("deployment cache lookup failed")
	patchErr := errors.New("scale patch failed")
	for _, tt := range []struct {
		name          string
		missing       bool
		lookupErr     error
		patchErr      error
		conditionType string
		reason        string
	}{
		{
			name: "deployment not found", missing: true,
			conditionType: hcprecoveryv1alpha1.ConditionKubeAPIServerLookupFailed,
			reason:        KubeAPIServerDeploymentLookupFailedReason,
		},
		{
			name: "lookup error", lookupErr: lookupErr,
			conditionType: hcprecoveryv1alpha1.ConditionKubeAPIServerLookupFailed,
			reason:        KubeAPIServerDeploymentLookupFailedReason,
		},
		{
			name: "scale patch error", patchErr: patchErr,
			conditionType: hcprecoveryv1alpha1.ConditionKubeAPIServerScaledToZero,
			reason:        KubeAPIServerPatchToZeroReplicasFailedReason,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newRecoveryTest(t, nil)
			if tt.missing {
				if err := f.informers["Deployment"].GetStore().Delete(f.deployment); err != nil {
					t.Fatal(err)
				}
			}
			if tt.lookupErr != nil {
				f.deploymentLister = failingDeploymentLister{err: tt.lookupErr}
			}
			f.kubeClient.PrependReactor("patch", "deployments", func(clienttesting.Action) (bool, runtime.Object, error) {
				return true, nil, tt.patchErr
			})
			wantErr := tt.lookupErr
			messageParts := []string{f.recovery.Spec.ControlPlaneNamespace, "kube-apiserver"}
			if tt.patchErr != nil {
				wantErr = tt.patchErr
				messageParts = []string{tt.patchErr.Error()}
			} else if wantErr != nil {
				messageParts = append(messageParts, wantErr.Error())
			}
			f.assertStepFailureContract(f.scaleDownKubeAPIServer, stepFailureExpectation{
				conditionType: tt.conditionType,
				reason:        tt.reason,
				messageParts:  messageParts,
				retryError: func(err error) bool {
					if tt.missing {
						return apierrors.IsNotFound(err)
					}
					return errors.Is(err, wantErr)
				},
			})
			if tt.patchErr == nil && len(f.kubeClient.Actions()) != 0 {
				t.Fatalf("lookup failure must not attempt scaling: %v", f.kubeClient.Actions())
			}
		})
	}
}

func TestScaleDownKubeAPIServerStepCompletionStatusError(t *testing.T) {
	f := newRecoveryTest(t, func(f *recoveryTest) {
		f.deployment.Spec.Replicas = ptr.To[int32](0)
		f.deployment.Status.Replicas = 0
	})
	f.writeErr = apierrors.NewConflict(hcprecoveryv1alpha1.Resource("hcprecoveries"),
		f.recovery.Name, errors.New("status conflict"))
	result, err := f.runStep(f.scaleDownKubeAPIServer, f.recovery)
	if result.outcome == stepContinue || !errors.Is(err, f.writeErr) {
		t.Fatalf("expected completion status error to propagate, got outcome=%v err=%v", result.outcome, err)
	}
	if len(f.client.Actions()) != 1 || len(f.kubeClient.Actions()) != 0 {
		t.Fatalf("expected only a completion status attempt, got recovery=%v kube=%v",
			f.client.Actions(), f.kubeClient.Actions())
	}
}

type failingDeploymentLister struct {
	appslisters.DeploymentNamespaceLister
	err error
}

func (l failingDeploymentLister) Deployments(string) appslisters.DeploymentNamespaceLister {
	return l
}

func (l failingDeploymentLister) Get(string) (*appsv1.Deployment, error) {
	return nil, l.err
}

func TestDeleteEtcdPVCsListFailures(t *testing.T) {
	listErr := errors.New("PVC list failed")
	for _, tt := range []struct {
		name    string
		reason  string
		message string
	}{
		{"cache lookup", EtcdCachedPVCListFailedReason, "failed to list PVCs from cache"},
		{"live lookup", EtcdLivePVCListFailedReason, "failed to list etcd PVCs from API server"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newRecoveryTest(t, nil)
			if tt.reason == EtcdCachedPVCListFailedReason {
				f.pvcLister = failingPVCListLister{err: listErr}
			} else {
				f.kubeClient.PrependReactor("list", "persistentvolumeclaims", func(action clienttesting.Action) (bool, runtime.Object, error) {
					if action.GetNamespace() != f.recovery.Spec.ControlPlaneNamespace {
						t.Fatalf("unexpected list namespace: %q", action.GetNamespace())
					}
					if selector := action.(clienttesting.ListAction).GetListRestrictions().Labels.String(); selector != "app=etcd" {
						t.Fatalf("unexpected PVC selector: %q", selector)
					}
					return true, nil, listErr
				})
			}
			f.assertStepFailureContract(f.deleteEtcdPVCs, stepFailureExpectation{
				conditionType: hcprecoveryv1alpha1.ConditionEtcdPVCsDeleted,
				reason:        tt.reason,
				messageParts:  []string{tt.message, f.recovery.Spec.ControlPlaneNamespace, listErr.Error()},
				retryError:    func(err error) bool { return errors.Is(err, listErr) },
			})
			if tt.reason == EtcdCachedPVCListFailedReason && len(f.kubeClient.Actions()) != 0 {
				t.Fatalf("cache failure must not query the API server: %v", f.kubeClient.Actions())
			}
		})
	}
}

func TestDeleteEtcdPVCsDeletesOneClaim(t *testing.T) {
	deleteErr := errors.New("PVC delete failed")
	for _, wantErr := range []error{nil, deleteErr} {
		name := "success"
		if wantErr != nil {
			name = "delete failure"
		}
		t.Run(name, func(t *testing.T) {
			f := newRecoveryTest(t, nil)
			for _, name := range []string{"data-etcd-0", "data-etcd-1"} {
				pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
					Name: name, Namespace: f.recovery.Spec.ControlPlaneNamespace,
					Labels: map[string]string{"app": "etcd"},
				}}
				if err := f.informers["PersistentVolumeClaim"].GetStore().Add(pvc); err != nil {
					t.Fatal(err)
				}
			}
			f.kubeClient.PrependReactor("delete", "persistentvolumeclaims", func(action clienttesting.Action) (bool, runtime.Object, error) {
				deleteAction := action.(clienttesting.DeleteAction)
				if deleteAction.GetNamespace() != f.recovery.Spec.ControlPlaneNamespace ||
					(deleteAction.GetName() != "data-etcd-0" && deleteAction.GetName() != "data-etcd-1") {
					t.Fatalf("unexpected PVC deletion: %#v", action)
				}
				return true, nil, wantErr
			})
			if result, err := f.runStep(f.deleteEtcdPVCs, f.recovery); result.outcome == stepContinue || !errors.Is(err, wantErr) {
				t.Fatalf("expected one deletion attempt with err=%v, got outcome=%v err=%v", wantErr, result.outcome, err)
			}
			if len(f.kubeClient.Actions()) != 1 || len(f.client.Actions()) != 0 {
				t.Fatalf("deletion must be the only action: kube=%v recovery=%v", f.kubeClient.Actions(), f.client.Actions())
			}
		})
	}
}

func TestDeleteEtcdPVCsWaitsWithoutRetry(t *testing.T) {
	for _, source := range []string{"terminating cached PVC", "live PVC"} {
		t.Run(source, func(t *testing.T) {
			f := newRecoveryTest(t, nil)
			pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
				Name: "data-etcd-0", Namespace: f.recovery.Spec.ControlPlaneNamespace,
				Labels: map[string]string{"app": "etcd"},
			}}
			switch source {
			case "terminating cached PVC":
				deletionTime := metav1.NewTime(testNow)
				pvc.DeletionTimestamp = &deletionTime
			case "live PVC":
				if err := f.kubeClient.Tracker().Add(pvc); err != nil {
					t.Fatal(err)
				}
			}
			if source != "live PVC" {
				if err := f.informers["PersistentVolumeClaim"].GetStore().Add(pvc); err != nil {
					t.Fatal(err)
				}
			}
			original := pvc.DeepCopy()
			// The PVCs are already terminating, so this step has nothing left to do;
			// validateScaledDown is the gate that waits for them to go away.
			if result, err := f.runStep(f.deleteEtcdPVCs, f.recovery); result.outcome != stepContinue || err != nil {
				t.Fatalf("wait must not act or request a retry: outcome=%v err=%v", result.outcome, err)
			}
			wantKubeActions := 0
			if source == "live PVC" {
				wantKubeActions = 1
			}
			if len(f.kubeClient.Actions()) != wantKubeActions || len(f.client.Actions()) != 0 ||
				f.workqueue.Len() != 0 || !reflect.DeepEqual(pvc, original) {
				t.Fatalf("wait must only observe PVCs: kube=%v recovery=%v", f.kubeClient.Actions(), f.client.Actions())
			}
		})
	}
}

func TestDeleteEtcdPVCsRecordsDeletionWithoutTransitioning(t *testing.T) {
	f := newRecoveryTest(t, nil)
	// Only PVCs labeled app=etcd in the control plane namespace block recovery.
	for _, pvc := range []*corev1.PersistentVolumeClaim{
		{ObjectMeta: metav1.ObjectMeta{
			Name: "data-etcd-0", Namespace: "other", Labels: map[string]string{"app": "etcd"},
		}},
		{ObjectMeta: metav1.ObjectMeta{
			Name: "unlabeled", Namespace: f.recovery.Spec.ControlPlaneNamespace,
		}},
		{ObjectMeta: metav1.ObjectMeta{
			Name: "other-app", Namespace: f.recovery.Spec.ControlPlaneNamespace, Labels: map[string]string{"app": "other"},
		}},
	} {
		if err := f.informers["PersistentVolumeClaim"].GetStore().Add(pvc); err != nil {
			t.Fatal(err)
		}
		if err := f.kubeClient.Tracker().Add(pvc); err != nil {
			t.Fatal(err)
		}
	}
	updated := f.requireStepStatusUpdate(f.runStep(f.deleteEtcdPVCs, f.recovery))
	condition := meta.FindStatusCondition(updated.Status.Conditions, hcprecoveryv1alpha1.ConditionEtcdPVCsDeleted)
	if updated.Status.Phase != hcprecoveryv1alpha1.RestoreStatePreparing ||
		condition == nil || condition.Status != metav1.ConditionTrue || condition.Reason != "EtcdPVCsDeleted" ||
		condition.Message != "No etcd PVCs remain in the cache or API server" ||
		condition.ObservedGeneration != f.recovery.Generation || !condition.LastTransitionTime.Time.Equal(testNow) {
		t.Fatalf("expected PVC deletion condition without a phase transition, got %#v", updated.Status)
	}
	wantStatus := f.recovery.DeepCopy().Status
	wantStatus.Conditions = append([]metav1.Condition{*condition}, wantStatus.Conditions...)
	if !apiequality.Semantic.DeepEqual(updated.Status, wantStatus) {
		t.Fatalf("unrelated status must be preserved:\n got: %#v\nwant: %#v", updated.Status, wantStatus)
	}
	f.client.ClearActions()
	recovery := f.recovery.DeepCopy()
	recovery.Status = updated.Status
	if result, err := f.runStep(f.deleteEtcdPVCs, recovery); result.outcome != stepContinue || err != nil || len(f.client.Actions()) != 0 {
		t.Fatalf("unchanged status must not be written again: outcome=%v err=%v actions=%v", result.outcome, err, f.client.Actions())
	}
	f.writeErr = errors.New("status update failed")
	if result, err := f.runStep(f.deleteEtcdPVCs, f.recovery); result.outcome == stepContinue || !errors.Is(err, f.writeErr) {
		t.Fatalf("expected status error to propagate: outcome=%v err=%v", result.outcome, err)
	}
}

type failingPVCListLister struct {
	corelisters.PersistentVolumeClaimNamespaceLister
	err error
}

func (l failingPVCListLister) PersistentVolumeClaims(string) corelisters.PersistentVolumeClaimNamespaceLister {
	return l
}

func (l failingPVCListLister) List(labels.Selector) ([]*corev1.PersistentVolumeClaim, error) {
	return nil, l.err
}

func TestValidateScaledDownFailures(t *testing.T) {
	listErr := errors.New("pod list failed")
	for _, tt := range []struct {
		name    string
		reason  string
		message string
	}{
		{"cache lookup", ScaledDownCachedPodListFailedReason, "failed to fetch pods from cache"},
		{"cached pods", ScaledDownCachedPodsRemainingReason, "cache check shows"},
		{"live lookup", ScaledDownLivePodListFailedReason, "failed to fetch pods from api server"},
		{"live pods", ScaledDownLivePodsRemainingReason, "live check shows"},
		{"cached PVC lookup", EtcdCachedPVCListFailedReason, "failed to list etcd PVCs from cache"},
		{"cached PVCs", ScaledDownCachedPVCsRemainingReason, "cache check shows"},
		{"live PVC lookup", EtcdLivePVCListFailedReason, "failed to list etcd PVCs from API server"},
		{"live PVCs", ScaledDownLivePVCsRemainingReason, "live check shows"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newScaledDownRecoveryTest(t)
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Name: "etcd-0", Namespace: f.recovery.Spec.ControlPlaneNamespace,
				Labels: map[string]string{"app": etcd},
			}}
			pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
				Name: "data-etcd-0", Namespace: f.recovery.Spec.ControlPlaneNamespace,
				Labels: map[string]string{"app": etcd}, DeletionTimestamp: ptr.To(metav1.NewTime(testNow)),
			}}
			switch tt.reason {
			case EtcdCachedPVCListFailedReason:
				f.pvcLister = failingPVCListLister{err: listErr}
			case ScaledDownCachedPVCsRemainingReason:
				if err := f.informers["PersistentVolumeClaim"].GetStore().Add(pvc); err != nil {
					t.Fatal(err)
				}
			case EtcdLivePVCListFailedReason:
				f.kubeClient.PrependReactor("list", "persistentvolumeclaims", func(clienttesting.Action) (bool, runtime.Object, error) {
					return true, nil, listErr
				})
			case ScaledDownLivePVCsRemainingReason:
				if err := f.kubeClient.Tracker().Add(pvc); err != nil {
					t.Fatal(err)
				}
			case ScaledDownCachedPodListFailedReason:
				f.podLister = failingScaledDownPodLister{err: listErr}
			case ScaledDownCachedPodsRemainingReason:
				if err := f.informers["Pod"].GetStore().Add(pod); err != nil {
					t.Fatal(err)
				}
			case ScaledDownLivePodListFailedReason:
				f.kubeClient.PrependReactor("list", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
					return true, nil, listErr
				})
			case ScaledDownLivePodsRemainingReason:
				if err := f.kubeClient.Tracker().Add(pod); err != nil {
					t.Fatal(err)
				}
			}
			f.assertStepFailureContract(f.validateScaledDown, stepFailureExpectation{
				conditionType: hcprecoveryv1alpha1.ConditionScaledDownValidated,
				reason:        tt.reason,
				messageParts:  []string{tt.message},
				retryError: func(err error) bool {
					if tt.reason == ScaledDownCachedPodListFailedReason || tt.reason == ScaledDownLivePodListFailedReason ||
						tt.reason == EtcdCachedPVCListFailedReason || tt.reason == EtcdLivePVCListFailedReason {
						return errors.Is(err, listErr)
					}
					return err != nil && strings.Contains(err.Error(), tt.message)
				},
			})
			if tt.reason == ScaledDownCachedPodListFailedReason || tt.reason == ScaledDownCachedPodsRemainingReason {
				if len(f.kubeClient.Actions()) != 0 {
					t.Fatalf("cache failure must not query live pods: %v", f.kubeClient.Actions())
				}
			}
		})
	}
}

func TestValidateScaledDownRecordsSuccessWithoutAdvancing(t *testing.T) {
	f := newScaledDownRecoveryTest(t)
	other := f.hc.DeepCopy()
	other.Name = "other"
	other.Spec.PausedUntil = nil
	if err := f.hcClient.Tracker().Add(other); err != nil {
		t.Fatal(err)
	}
	f.hcClient.PrependReactor("list", "hostedclusters", func(clienttesting.Action) (bool, runtime.Object, error) {
		t.Fatal("validation must get the named HostedCluster instead of listing")
		return true, nil, nil
	})
	// Unrelated pods and matching pods in other namespaces must not block recovery.
	for _, pod := range []*corev1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: f.recovery.Spec.ControlPlaneNamespace, Labels: map[string]string{"app": "other"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "etcd-0", Namespace: "other", Labels: map[string]string{"app": etcd}}},
	} {
		if err := f.informers["Pod"].GetStore().Add(pod); err != nil {
			t.Fatal(err)
		}
		if err := f.kubeClient.Tracker().Add(pod); err != nil {
			t.Fatal(err)
		}
	}
	for _, pvc := range []*corev1.PersistentVolumeClaim{
		{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: f.recovery.Spec.ControlPlaneNamespace}},
		{ObjectMeta: metav1.ObjectMeta{Name: "data-etcd-0", Namespace: "other", Labels: map[string]string{"app": etcd}}},
	} {
		if err := f.informers["PersistentVolumeClaim"].GetStore().Add(pvc); err != nil {
			t.Fatal(err)
		}
		if err := f.kubeClient.Tracker().Add(pvc); err != nil {
			t.Fatal(err)
		}
	}
	updated := f.requireStepStatusUpdate(f.runStep(f.validateScaledDown, f.recovery))
	condition := meta.FindStatusCondition(updated.Status.Conditions, hcprecoveryv1alpha1.ConditionScaledDownValidated)
	if updated.Status.Phase != f.recovery.Status.Phase ||
		condition == nil || condition.Status != metav1.ConditionTrue || condition.Reason != "PodsCleanedUp" ||
		condition.ObservedGeneration != f.recovery.Generation || !condition.LastTransitionTime.Time.Equal(testNow) {
		t.Fatalf("expected successful validation without a phase change, got %#v", updated.Status)
	}
	f.client.ClearActions()
	recovery := f.recovery.DeepCopy()
	recovery.Status = updated.Status
	if result, err := f.runStep(f.validateScaledDown, recovery); result.outcome != stepContinue || err != nil || len(f.client.Actions()) != 0 {
		t.Fatalf("unchanged status must not be written again: outcome=%v err=%v actions=%v", result.outcome, err, f.client.Actions())
	}
	f.writeErr = errors.New("status update failed")
	if result, err := f.runStep(f.validateScaledDown, f.recovery); result.outcome == stepContinue || !errors.Is(err, f.writeErr) {
		t.Fatalf("expected status error to propagate: outcome=%v err=%v", result.outcome, err)
	}
}

func TestValidateScaledDownChecksLivePauseAndReplicas(t *testing.T) {
	lookupErr := errors.New("live lookup failed")
	for _, tt := range []struct {
		name   string
		reason string
		setup  func(*recoveryTest) error
	}{
		{"pause unset", ScaledDownHostedClusterNotPausedReason, func(f *recoveryTest) error {
			hc := f.hc.DeepCopy()
			hc.Spec.PausedUntil = nil
			return f.hcClient.Tracker().Update(hypershiftv1beta1.SchemeGroupVersion.WithResource("hostedclusters"), hc, hc.Namespace)
		}},
		{"pause false", ScaledDownHostedClusterNotPausedReason, func(f *recoveryTest) error {
			hc := f.hc.DeepCopy()
			hc.Spec.PausedUntil = ptr.To("false")
			return f.hcClient.Tracker().Update(hypershiftv1beta1.SchemeGroupVersion.WithResource("hostedclusters"), hc, hc.Namespace)
		}},
		{"HostedCluster lookup", HostedClusterLookupFailedReason, func(f *recoveryTest) error {
			f.hcClient.PrependReactor("get", "hostedclusters", func(clienttesting.Action) (bool, runtime.Object, error) {
				return true, nil, lookupErr
			})
			return nil
		}},
		{"HostedCluster missing", HostedClusterLookupFailedReason, func(f *recoveryTest) error {
			return f.hcClient.Tracker().Delete(hypershiftv1beta1.SchemeGroupVersion.WithResource("hostedclusters"), f.hc.Namespace, f.hc.Name)
		}},
		{"deployment desired replicas", ScaledDownDeploymentReplicasRemainingReason, func(f *recoveryTest) error {
			deployment := f.deployment.DeepCopy()
			deployment.Spec.Replicas = ptr.To[int32](1)
			return f.kubeClient.Tracker().Update(appsv1.SchemeGroupVersion.WithResource("deployments"), deployment, deployment.Namespace)
		}},
		{"deployment status replicas", ScaledDownDeploymentReplicasRemainingReason, func(f *recoveryTest) error {
			deployment := f.deployment.DeepCopy()
			deployment.Status.Replicas = 1
			return f.kubeClient.Tracker().Update(appsv1.SchemeGroupVersion.WithResource("deployments"), deployment, deployment.Namespace)
		}},
		{"deployment lookup", KubeAPIServerDeploymentLookupFailedReason, func(f *recoveryTest) error {
			f.kubeClient.PrependReactor("get", "deployments", func(clienttesting.Action) (bool, runtime.Object, error) {
				return true, nil, lookupErr
			})
			return nil
		}},
		{"StatefulSet desired replicas", ScaledDownStatefulSetReplicasRemainingReason, func(f *recoveryTest) error {
			sts := f.sts.DeepCopy()
			sts.Spec.Replicas = ptr.To[int32](1)
			return f.kubeClient.Tracker().Update(appsv1.SchemeGroupVersion.WithResource("statefulsets"), sts, sts.Namespace)
		}},
		{"StatefulSet status replicas", ScaledDownStatefulSetReplicasRemainingReason, func(f *recoveryTest) error {
			sts := f.sts.DeepCopy()
			sts.Status.Replicas = 1
			return f.kubeClient.Tracker().Update(appsv1.SchemeGroupVersion.WithResource("statefulsets"), sts, sts.Namespace)
		}},
		{"StatefulSet lookup", EtcdStatefulSetLookupFailedReason, func(f *recoveryTest) error {
			f.kubeClient.PrependReactor("get", "statefulsets", func(clienttesting.Action) (bool, runtime.Object, error) {
				return true, nil, lookupErr
			})
			return nil
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newScaledDownRecoveryTest(t)
			if err := tt.setup(f); err != nil {
				t.Fatal(err)
			}
			f.assertStepFailureContract(f.validateScaledDown, stepFailureExpectation{
				conditionType: hcprecoveryv1alpha1.ConditionScaledDownValidated,
				reason:        tt.reason,
				retryError: func(err error) bool {
					if tt.name == "HostedCluster missing" {
						return apierrors.IsNotFound(err)
					}
					switch tt.reason {
					case HostedClusterLookupFailedReason, KubeAPIServerDeploymentLookupFailedReason, EtcdStatefulSetLookupFailedReason:
						return errors.Is(err, lookupErr)
					default:
						return err != nil
					}
				},
			})
		})
	}
}

func newScaledDownRecoveryTest(t *testing.T) *recoveryTest {
	t.Helper()
	return newRecoveryTest(t, func(f *recoveryTest) {
		f.hc.Spec.PausedUntil = ptr.To("true")
		f.deployment.Spec.Replicas = ptr.To[int32](0)
		f.deployment.Status.Replicas = 0
		f.sts.Spec.Replicas = ptr.To[int32](0)
	})
}

func TestPreparationGatesReapOnlyTerminalPods(t *testing.T) {
	for _, gate := range []struct {
		name, app, condition string
		postRestore          bool
	}{
		{"preparing kube-apiserver", apiServer, hcprecoveryv1alpha1.ConditionScaledDownValidated, false},
		{"preparing etcd", etcd, hcprecoveryv1alpha1.ConditionScaledDownValidated, false},
		{"post-restore etcd", etcd, hcprecoveryv1alpha1.ConditionEtcdPodsGone, true},
	} {
		for _, cached := range []bool{true, false} {
			for _, phase := range []corev1.PodPhase{
				corev1.PodFailed, corev1.PodSucceeded, corev1.PodUnknown, corev1.PodPending, corev1.PodRunning, "",
			} {
				for _, terminating := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/cached=%t/phase=%s/terminating=%t", gate.name, cached, phase, terminating), func(t *testing.T) {
						f := newScaledDownRecoveryTest(t)
						recovery := f.recovery.DeepCopy()
						step := f.validateScaledDown
						if gate.postRestore {
							recovery.Status.Phase = hcprecoveryv1alpha1.RestoreStateRestoring
							step = f.validateEtcdPodsGone
						}
						pod := &corev1.Pod{
							ObjectMeta: metav1.ObjectMeta{
								Name: gate.app + "-old", Namespace: f.recovery.Spec.ControlPlaneNamespace,
								UID: "old-pod-uid", Labels: map[string]string{"app": gate.app},
							},
							Status: corev1.PodStatus{Phase: phase, ContainerStatuses: []corev1.ContainerStatus{{
								Name: gate.app,
								State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
									Reason: "ContainerStatusUnknown", ExitCode: 137,
								}},
							}}},
						}
						if terminating {
							pod.DeletionTimestamp = ptr.To(metav1.NewTime(testNow))
						}
						if cached {
							f.addPods(pod)
						} else if err := f.kubeClient.Tracker().Add(pod); err != nil {
							t.Fatal(err)
						}
						deletes := 0
						f.kubeClient.PrependReactor("delete", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
							deletes++
							opts := action.(clienttesting.DeleteAction).GetDeleteOptions()
							if opts.Preconditions == nil || opts.Preconditions.UID == nil || *opts.Preconditions.UID != pod.UID ||
								opts.GracePeriodSeconds != nil || opts.PropagationPolicy != nil {
								t.Fatalf("expected normal deletion with the observed UID, got %#v", opts)
							}
							return false, nil, nil
						})
						update := f.requireStepStatusUpdate(f.runStep(step, recovery))
						condition := meta.FindStatusCondition(update.Status.Conditions, gate.condition)
						if condition == nil || condition.Status != metav1.ConditionFalse ||
							!strings.Contains(condition.Message, fmt.Sprintf("%s (phase=%q, terminating=%t)", pod.Name, phase, terminating)) {
							t.Fatalf("remaining pod must block with name and phase: %#v", condition)
						}
						shouldDelete := !terminating && (phase == corev1.PodFailed || phase == corev1.PodSucceeded)
						if (deletes == 1) != shouldDelete || deletes > 1 {
							t.Fatalf("terminal=%t terminating=%t: got %d deletions", shouldDelete, terminating, deletes)
						}
						recovery.Status = update.Status
						if !shouldDelete {
							if result, err := f.runStep(step, recovery); result.outcome != stepRetry || err == nil || deletes != 0 {
								t.Fatalf("nonterminal or terminating pod must keep blocking without deletion: outcome=%v err=%v deletes=%d", result.outcome, err, deletes)
							}
							return
						}
						if cached {
							// A successful API deletion is not enough while the cache still has the pod.
							if result, err := f.runStep(step, recovery); result.outcome != stepRetry || err == nil {
								t.Fatalf("stale cached pod must still block: outcome=%v err=%v", result.outcome, err)
							}
							if err := f.informers["Pod"].GetStore().Delete(pod); err != nil {
								t.Fatal(err)
							}
						}
						update = f.requireStepStatusUpdate(f.runStep(step, recovery))
						if !meta.IsStatusConditionTrue(update.Status.Conditions, gate.condition) {
							t.Fatalf("expected validation only after cache and API disappearance: %#v", update.Status)
						}
					})
				}
			}
		}
	}
}

func TestPreparationGatesSurfaceTerminalPodDeleteErrors(t *testing.T) {
	for _, postRestore := range []bool{false, true} {
		for _, cached := range []bool{false, true} {
			for _, conflict := range []bool{false, true} {
				t.Run(fmt.Sprintf("postRestore=%t/cached=%t/conflict=%t", postRestore, cached, conflict), func(t *testing.T) {
					f := newScaledDownRecoveryTest(t)
					f.recovery = f.recovery.DeepCopy()
					step := f.validateScaledDown
					conditionType := hcprecoveryv1alpha1.ConditionScaledDownValidated
					if postRestore {
						f.recovery.Status.Phase = hcprecoveryv1alpha1.RestoreStateRestoring
						step = f.validateEtcdPodsGone
						conditionType = hcprecoveryv1alpha1.ConditionEtcdPodsGone
					}
					pod := f.etcdPod(0, false)
					pod.UID = "observed-uid"
					pod.Status.Phase = corev1.PodFailed
					if cached {
						f.addPods(pod)
					} else if err := f.kubeClient.Tracker().Add(pod); err != nil {
						t.Fatal(err)
					}
					deleteErr := apierrors.NewForbidden(corev1.Resource("pods"), pod.Name, errors.New("delete denied"))
					if conflict {
						// A new pod with the same name must not be deleted using a stale UID.
						deleteErr = apierrors.NewConflict(corev1.Resource("pods"), pod.Name, errors.New("UID precondition failed"))
					}
					f.kubeClient.PrependReactor("delete", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
						return true, nil, deleteErr
					})
					f.assertStepFailureContract(step, stepFailureExpectation{
						conditionType: conditionType,
						reason:        TerminalPodDeleteFailedReason,
						messageParts:  []string{pod.Name, "phase=Failed", deleteErr.Error()},
						retryError:    func(err error) bool { return errors.Is(err, deleteErr) },
					})
				})
			}
		}
	}
}

type failingScaledDownPodLister struct {
	corelisters.PodNamespaceLister
	err error
}

func (l failingScaledDownPodLister) Pods(string) corelisters.PodNamespaceLister {
	return l
}

func (l failingScaledDownPodLister) List(labels.Selector) ([]*corev1.Pod, error) {
	return nil, l.err
}

func TestScaleDownEtcdStepConditions(t *testing.T) {
	for _, conditionType := range []string{
		hcprecoveryv1alpha1.ConditionEtcdScaledToZero,
		hcprecoveryv1alpha1.ConditionEtcdScaledToZeroAfterVeleroRestore,
	} {
		t.Run(conditionType, func(t *testing.T) {
			testScaleDownEtcdStepConditions(t, conditionType)
		})
	}
}

func testScaleDownEtcdStepConditions(t *testing.T, conditionType string) {
	t.Helper()
	for _, tt := range []struct {
		name     string
		missing  bool
		patchErr error
		status   metav1.ConditionStatus
		reason   string
		message  string
	}{
		{
			name: "lookup failure", missing: true,
			status:  metav1.ConditionFalse,
			reason:  EtcdStatefulSetLookupFailedReason,
			message: `failed to get etcd statefulset in namespace "hosted-cluster-cluster": statefulset.apps "etcd" not found`,
		},
		{
			name: "patch failure", patchErr: errors.New("etcd scale patch failed"),
			status:  metav1.ConditionFalse,
			reason:  EtcdStatefulSetPatchToZeroReplicasFailedReason,
			message: "etcd scale patch failed",
		},
		{
			name:    "scaled to zero",
			status:  metav1.ConditionTrue,
			reason:  "EtcdScaledToZeroReplicas",
			message: "The etcd statefulset is scaled to zero replicas",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newRecoveryTest(t, func(f *recoveryTest) {
				f.sts.Spec.Replicas = ptr.To[int32](0)
				if tt.patchErr != nil {
					f.sts.Spec.Replicas = ptr.To[int32](3)
				}
			})
			if tt.missing {
				if err := f.informers["StatefulSet"].GetStore().Delete(f.sts); err != nil {
					t.Fatal(err)
				}
			}
			f.kubeClient.PrependReactor("patch", "statefulsets", func(clienttesting.Action) (bool, runtime.Object, error) {
				return true, nil, tt.patchErr
			})

			result, err := f.scaleDownEtcd(f.ctx, f.recovery, conditionType)
			if result.statusUpdate != nil {
				err = f.applyStatus(f.ctx, f.recovery, result.statusUpdate)
			}
			updated := f.requireStepStatusUpdate(result, err)
			condition := meta.FindStatusCondition(updated.Status.Conditions, conditionType)
			if condition == nil || condition.Status != tt.status || condition.Reason != tt.reason ||
				condition.Message != tt.message || condition.ObservedGeneration != f.recovery.Generation ||
				!condition.LastTransitionTime.Equal(&metav1.Time{Time: testNow}) {
				t.Fatalf("unexpected etcd condition: %#v", condition)
			}
			if meta.FindStatusCondition(updated.Status.Conditions, hcprecoveryv1alpha1.ConditionKubeAPIServerLookupFailed) != nil ||
				meta.FindStatusCondition(updated.Status.Conditions, hcprecoveryv1alpha1.ConditionKubeAPIServerScaledToZero) != nil {
				t.Fatalf("etcd step must not write kube-apiserver conditions: %v", updated.Status.Conditions)
			}
		})
	}
}
