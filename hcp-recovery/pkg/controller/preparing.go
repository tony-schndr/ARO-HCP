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

	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"

	hypershiftv1beta1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	hypershiftapply "github.com/openshift/hypershift/client/applyconfiguration/hypershift/v1beta1"

	hcprecoveryv1alpha1 "github.com/Azure/ARO-HCP/hcp-recovery/pkg/apis/hcprecovery/v1alpha1"
	recoveryapply "github.com/Azure/ARO-HCP/hcp-recovery/pkg/generated/applyconfiguration/hcprecovery/v1alpha1"
	"github.com/Azure/ARO-HCP/internal/backup"
	"github.com/Azure/ARO-HCP/internal/controllerutils"
)

const (
	BackupMissingResourceIDReason                       = "MissingResourceID"
	BackupMissingHostedClusterNameReason                = "MissingHostedClusterName"
	BackupMissingHostedClusterNamespaceReason           = "MissingHostedClusterNamespace"
	BackupNotFoundReason                                = "BackupNotFound"
	BackupResourceIDMismatchReason                      = "BackupResourceIDMismatch"
	BackupDeletingReason                                = "BackupDeleting"
	BackupInvalidPhaseReason                            = "InvalidBackupPhase"
	BackupNotCompletedReason                            = "BackupNotCompleted"
	BackupHostedClusterResourceIDMismatchReason         = "HostedClusterResourceIDMismatch"
	RecoveryInputsValidatedReason                       = "RecoveryInputsValidated"
	BackupLookupFailedReason                            = "BackupLookupFailed"
	BackupInvalidObjectReason                           = "InvalidBackupObject"
	BackupHostedClusterLookupFailedReason               = "HostedClusterLookupFailed"
	BackupMissingControlPlaneNamespaceReason            = "MissingControlPlaneNamespace"
	BackupControlPlaneNamespaceLookupFailedReason       = "ControlPlaneNamespaceLookupFailed"
	BackupControlPlaneNamespaceResourceIDMismatchReason = "ControlPlaneNamespaceResourceIDMismatch"
	BackupNamespaceMismatchReason                       = "BackupNamespaceMismatch"
	BackupInvalidNamespacesReason                       = "InvalidBackupNamespaces"
	BackupKMSKeyFingerprintMismatchReason               = "BackupKMSKeyFingerprintMismatch"
	BackupKMSKeyRotationInProgressReason                = "KMSKeyRotationInProgress"
	BackupInvalidKMSEncryptionStateReason               = "InvalidKMSEncryptionState"
)

func (c *RecoveryController) validateRecoveryInputs(ctx context.Context, recovery *hcprecoveryv1alpha1.HCPRecovery) (stepResult, error) {
	if recovery.Spec.HcpClusterAzureResourceID == "" {
		return c.permanentError(recovery, RecoveryInputsNotValidatedCondition(
			BackupMissingResourceIDReason, "The target cluster resource ID is required", recovery.Generation, c.TimeNow())), nil
	}
	if recovery.Spec.HostedClusterNamespace == "" {
		return c.permanentError(recovery, RecoveryInputsNotValidatedCondition(
			BackupMissingHostedClusterNamespaceReason, "The HostedCluster namespace is required", recovery.Generation, c.TimeNow())), nil
	}
	if recovery.Spec.HostedClusterName == "" {
		return c.permanentError(recovery, RecoveryInputsNotValidatedCondition(
			BackupMissingHostedClusterNameReason, "The HostedCluster name is required", recovery.Generation, c.TimeNow())), nil
	}
	if recovery.Spec.ControlPlaneNamespace == "" {
		return c.permanentError(recovery, RecoveryInputsNotValidatedCondition(
			BackupMissingControlPlaneNamespaceReason, "The control plane namespace is required", recovery.Generation, c.TimeNow())), nil
	}

	obj, err := c.backupLister.ByNamespace(backup.VeleroNamespace).Get(recovery.Spec.VeleroBackupName)
	if apierrors.IsNotFound(err) {
		return c.permanentError(recovery, RecoveryInputsNotValidatedCondition(
			BackupNotFoundReason, fmt.Sprintf("Backup %s/%s does not exist", backup.VeleroNamespace, recovery.Spec.VeleroBackupName),
			recovery.Generation, c.TimeNow())), nil
	}
	if err != nil {
		err = fmt.Errorf("failed to get backup %q: %w", recovery.Spec.VeleroBackupName, err)
		return c.retryableError(recovery, RecoveryInputsNotValidatedCondition(BackupLookupFailedReason, err.Error(), recovery.Generation, c.TimeNow()), err), nil
	}
	veleroBackup, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return c.permanentError(recovery, RecoveryInputsNotValidatedCondition(
			BackupInvalidObjectReason, fmt.Sprintf("expected an unstructured Backup, got %T", obj), recovery.Generation, c.TimeNow())), nil
	}

	resourceID := veleroBackup.GetAnnotations()[controllerutils.HcpClusterAzureResourceIdAnnotation]
	if !strings.EqualFold(resourceID, recovery.Spec.HcpClusterAzureResourceID) {
		return c.permanentError(recovery, RecoveryInputsNotValidatedCondition(
			BackupResourceIDMismatchReason, "The backup resource ID annotation is missing or does not match the target cluster",
			recovery.Generation, c.TimeNow())), nil
	}
	if veleroBackup.GetDeletionTimestamp() != nil {
		return c.permanentError(recovery, RecoveryInputsNotValidatedCondition(
			BackupDeletingReason, "The backup is being deleted", recovery.Generation, c.TimeNow())), nil
	}
	phase, _, err := unstructured.NestedString(veleroBackup.Object, "status", "phase")
	if err != nil {
		return c.permanentError(recovery, RecoveryInputsNotValidatedCondition(
			BackupInvalidPhaseReason, err.Error(), recovery.Generation, c.TimeNow())), nil
	}
	if phase != string(velerov1.BackupPhaseCompleted) {
		return c.permanentError(recovery, RecoveryInputsNotValidatedCondition(
			BackupNotCompletedReason, fmt.Sprintf("Backup phase is %q; expected Completed", phase), recovery.Generation, c.TimeNow())), nil
	}

	hostedCluster, err := c.hypershiftLister.HostedClusters(recovery.Spec.HostedClusterNamespace).Get(recovery.Spec.HostedClusterName)
	if apierrors.IsNotFound(err) {
		return c.permanentError(recovery, RecoveryInputsNotValidatedCondition(
			BackupHostedClusterLookupFailedReason,
			fmt.Sprintf("HostedCluster %s/%s does not exist", recovery.Spec.HostedClusterNamespace, recovery.Spec.HostedClusterName),
			recovery.Generation, c.TimeNow())), nil
	}
	if err != nil {
		err = fmt.Errorf("failed to get HostedCluster %s/%s: %w", recovery.Spec.HostedClusterNamespace, recovery.Spec.HostedClusterName, err)
		return c.retryableError(recovery, RecoveryInputsNotValidatedCondition(BackupHostedClusterLookupFailedReason, err.Error(), recovery.Generation, c.TimeNow()), err), nil
	}
	if !strings.EqualFold(hostedCluster.Annotations[controllerutils.HcpClusterAzureResourceIdAnnotation], resourceID) {
		return c.permanentError(recovery, RecoveryInputsNotValidatedCondition(
			BackupHostedClusterResourceIDMismatchReason,
			fmt.Sprintf("HostedCluster %s/%s resource ID annotation is missing or does not match the backup resource ID",
				recovery.Spec.HostedClusterNamespace, recovery.Spec.HostedClusterName), recovery.Generation, c.TimeNow())), nil
	}

	backupFingerprint := veleroBackup.GetAnnotations()[controllerutils.HcpClusterKmsKeyFingerprintAnnotation]
	encryption := hostedCluster.Status.SecretEncryption
	azureKey := encryption.ActiveKey.Azure
	kmsConfigured := hostedCluster.Spec.SecretEncryption != nil && hostedCluster.Spec.SecretEncryption.Type == hypershiftv1beta1.KMS
	hasAzureKeyState := encryption.ActiveKey.Provider == hypershiftv1beta1.SecretEncryptionProviderAzure ||
		azureKey != (hypershiftv1beta1.AzureKMSKey{}) ||
		encryption.TargetKey.Provider == hypershiftv1beta1.SecretEncryptionProviderAzure ||
		encryption.TargetKey.Azure != (hypershiftv1beta1.AzureKMSKey{})
	if kmsConfigured || hasAzureKeyState || backupFingerprint != "" {
		if encryption.TargetKey.Provider != "" || encryption.TargetKey.Azure != (hypershiftv1beta1.AzureKMSKey{}) {
			return c.permanentError(recovery, RecoveryInputsNotValidatedCondition(
				BackupKMSKeyRotationInProgressReason,
				fmt.Sprintf("HostedCluster %s/%s has an in-progress key rotation; wait for it to finish and create a new recovery",
					hostedCluster.Namespace, hostedCluster.Name), recovery.Generation, c.TimeNow())), nil
		}
		if len(encryption.History) > 0 {
			switch encryption.History[0].State {
			case hypershiftv1beta1.EncryptionMigrationStateCompleted, hypershiftv1beta1.EncryptionMigrationStateInterrupted:
			case hypershiftv1beta1.EncryptionMigrationStateReadOnlyDeploy, hypershiftv1beta1.EncryptionMigrationStateWritePromote, hypershiftv1beta1.EncryptionMigrationStateMigrating:
				return c.permanentError(recovery, RecoveryInputsNotValidatedCondition(
					BackupKMSKeyRotationInProgressReason,
					fmt.Sprintf("HostedCluster %s/%s has an in-progress key rotation; wait for it to finish and create a new recovery",
						hostedCluster.Namespace, hostedCluster.Name), recovery.Generation, c.TimeNow())), nil
			default:
				return c.permanentError(recovery, RecoveryInputsNotValidatedCondition(
					BackupInvalidKMSEncryptionStateReason,
					fmt.Sprintf("HostedCluster %s/%s has an unknown key rotation state; verify its encryption state before creating a new recovery",
						hostedCluster.Namespace, hostedCluster.Name), recovery.Generation, c.TimeNow())), nil
			}
		}
		// Desired keys and rotation history do not establish which key can decrypt the backup.
		if encryption.ActiveKey.Provider != hypershiftv1beta1.SecretEncryptionProviderAzure ||
			azureKey.KeyVaultName == "" || azureKey.KeyName == "" || azureKey.KeyVersion == "" {
			return c.permanentError(recovery, RecoveryInputsNotValidatedCondition(
				BackupInvalidKMSEncryptionStateReason,
				fmt.Sprintf("HostedCluster %s/%s has no complete observed active Azure KMS key; verify its encryption state before creating a new recovery",
					hostedCluster.Namespace, hostedCluster.Name), recovery.Generation, c.TimeNow())), nil
		}
		if backupFingerprint != backup.ActiveKMSKeyFingerprint(hostedCluster) {
			return c.permanentError(recovery, RecoveryInputsNotValidatedCondition(
				BackupKMSKeyFingerprintMismatchReason,
				fmt.Sprintf("Backup %s/%s KMS key fingerprint is missing or does not match HostedCluster %s/%s's observed active key; select a compatible backup and create a new recovery",
					backup.VeleroNamespace, recovery.Spec.VeleroBackupName, hostedCluster.Namespace, hostedCluster.Name),
				recovery.Generation, c.TimeNow())), nil
		}
	}

	controlPlaneNamespace := recovery.Spec.ControlPlaneNamespace
	namespace, err := c.kubeClientset.CoreV1().Namespaces().Get(ctx, controlPlaneNamespace, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return c.permanentError(recovery, RecoveryInputsNotValidatedCondition(
			BackupControlPlaneNamespaceLookupFailedReason,
			fmt.Sprintf("Control plane namespace %q does not exist", controlPlaneNamespace),
			recovery.Generation, c.TimeNow())), nil
	}
	if err != nil {
		err = fmt.Errorf("failed to get control plane namespace %q: %w", controlPlaneNamespace, err)
		return c.retryableError(recovery, RecoveryInputsNotValidatedCondition(
			BackupControlPlaneNamespaceLookupFailedReason, err.Error(), recovery.Generation, c.TimeNow()), err), nil
	}
	if !strings.EqualFold(namespace.Annotations[controllerutils.HcpClusterAzureResourceIdAnnotation], resourceID) {
		return c.permanentError(recovery, RecoveryInputsNotValidatedCondition(
			BackupControlPlaneNamespaceResourceIDMismatchReason,
			fmt.Sprintf("Control plane namespace %q resource ID annotation is missing or does not match the backup resource ID", controlPlaneNamespace),
			recovery.Generation, c.TimeNow())), nil
	}

	includedNamespaces, _, err := unstructured.NestedStringSlice(veleroBackup.Object, "spec", "includedNamespaces")
	if err != nil {
		return c.permanentError(recovery, RecoveryInputsNotValidatedCondition(
			BackupInvalidNamespacesReason, err.Error(), recovery.Generation, c.TimeNow())), nil
	}
	// Velero restores into the backup's namespaces without remapping.
	if !sets.New(includedNamespaces...).Equal(sets.New(hostedCluster.Namespace, controlPlaneNamespace)) {
		return c.permanentError(recovery, RecoveryInputsNotValidatedCondition(
			BackupNamespaceMismatchReason,
			fmt.Sprintf("Backup included namespaces %q must contain exactly HostedCluster namespace %q and control plane namespace %q",
				includedNamespaces, hostedCluster.Namespace, controlPlaneNamespace),
			recovery.Generation, c.TimeNow())), nil
	}
	excludedNamespaces, _, err := unstructured.NestedStringSlice(veleroBackup.Object, "spec", "excludedNamespaces")
	if err != nil {
		return c.permanentError(recovery, RecoveryInputsNotValidatedCondition(
			BackupInvalidNamespacesReason, err.Error(), recovery.Generation, c.TimeNow())), nil
	}
	if sets.New(excludedNamespaces...).HasAny("*", hostedCluster.Namespace, controlPlaneNamespace) {
		return c.permanentError(recovery, RecoveryInputsNotValidatedCondition(
			BackupNamespaceMismatchReason, fmt.Sprintf("Backup excluded namespaces %q overlap the recovery namespaces", excludedNamespaces),
			recovery.Generation, c.TimeNow())), nil
	}
	statusUpdate, needsUpdate := NewStatus(recovery.Status).
		WithConditions(RecoveryInputsValidatedCondition(recovery.Generation, c.TimeNow()).
			WithMessage(fmt.Sprintf("Recovery inputs are valid: backup %s/%s is Completed, matches the target HostedCluster and control plane namespace, has the expected namespace scope, and is compatible with the observed active CMK when applicable",
				backup.VeleroNamespace, recovery.Spec.VeleroBackupName))).
		AsApplyConfiguration(recovery)
	return satisfied(statusUpdate, needsUpdate), nil
}

const (
	EtcdStatefulSetNotFoundReason     = "EtcdStatefulSetNotFound"
	EtcdStatefulSetLookupFailedReason = "EtcdStatefulSetLookupFailed"
	EtcdImageMissingReason            = "MissingEtcdImage"
	EtcdRuntimeDetailsCollectedReason = "EtcdRuntimeDetailsCollected"
)

func (c *RecoveryController) collectEtcdInformation(ctx context.Context, recovery *hcprecoveryv1alpha1.HCPRecovery) (stepResult, error) {
	etcdStatefulset, err := c.statefulsetLister.StatefulSets(recovery.Spec.ControlPlaneNamespace).Get("etcd")
	if apierrors.IsNotFound(err) {
		return c.retryableError(recovery, EtcdRuntimeDetailsNotCollectedCondition(
			EtcdStatefulSetNotFoundReason,
			fmt.Sprintf("Etcd StatefulSet %s/etcd does not exist", recovery.Spec.ControlPlaneNamespace),
			recovery.Generation, c.TimeNow()), err), nil
	}
	if err != nil {
		err = fmt.Errorf("failed to get etcd StatefulSet %s/etcd: %w", recovery.Spec.ControlPlaneNamespace, err)
		return c.retryableError(recovery, EtcdRuntimeDetailsNotCollectedCondition(
			EtcdStatefulSetLookupFailedReason, err.Error(), recovery.Generation, c.TimeNow()), err), nil
	}
	image := ""
	for _, container := range etcdStatefulset.Spec.Template.Spec.Containers {
		if container.Name == "etcd" {
			image = container.Image
		}
	}
	if image == "" {
		return c.permanentError(recovery, EtcdRuntimeDetailsNotCollectedCondition(
			EtcdImageMissingReason,
			fmt.Sprintf("StatefulSet %s/etcd has no etcd container with an image", recovery.Spec.ControlPlaneNamespace),
			recovery.Generation, c.TimeNow())), nil
	}

	etcdRuntimeDetails := recoveryapply.EtcdRuntimeDetails().WithImage(image)

	securityContext := etcdStatefulset.Spec.Template.Spec.SecurityContext

	if securityContext != nil {
		if securityContext.FSGroup != nil {
			etcdRuntimeDetails.WithFSGroup(*etcdStatefulset.Spec.Template.Spec.SecurityContext.FSGroup)
		}
		if securityContext.RunAsUser != nil {
			etcdRuntimeDetails.WithRunAsUser(*etcdStatefulset.Spec.Template.Spec.SecurityContext.RunAsUser)
		}
	}

	statusUpdate, needsUpdate := NewStatus(recovery.Status).
		WithConditions(EtcdRuntimeDetailsCollectedCondition(
			EtcdRuntimeDetailsCollectedReason,
			fmt.Sprintf("Collected etcd runtime details from StatefulSet %s/etcd", recovery.Spec.ControlPlaneNamespace),
			recovery.Generation, c.TimeNow())).
		AsApplyConfiguration(recovery)
	statusUpdate.Status.WithEtcdRuntimeDetails(etcdRuntimeDetails)

	details := &hcprecoveryv1alpha1.EtcdRuntimeDetails{
		Image: image, RunAsUser: etcdRuntimeDetails.RunAsUser, FSGroup: etcdRuntimeDetails.FSGroup,
	}
	if !needsUpdate && apiequality.Semantic.DeepEqual(details, recovery.Status.EtcdRuntimeDetails) {
		return proceed(), nil
	}
	return satisfied(statusUpdate, true), nil
}

const (
	HostedClusterLookupFailedReason      = "HostedClusterLookupFailed"
	HostedClusterPauseFailedReason       = "HostedClusterPauseFailed"
	HostedClusterPausePendingReason      = "HostedClusterPausePending"
	HostedControlPlaneLookupFailedReason = "HostedControlPlaneLookupFailed"
	HostedControlPlanePausePendingReason = "HostedControlPlanePausePending"
)

func (c *RecoveryController) pauseHostedCluster(ctx context.Context, recovery *hcprecoveryv1alpha1.HCPRecovery) (stepResult, error) {
	hostedCluster, err := c.hypershiftLister.HostedClusters(recovery.Spec.HostedClusterNamespace).Get(recovery.Spec.HostedClusterName)
	if err != nil {
		err = fmt.Errorf("failed to get HostedCluster %s/%s: %w", recovery.Spec.HostedClusterNamespace, recovery.Spec.HostedClusterName, err)
		return c.retryableError(recovery, HostedClusterNotPausedCondition(
			HostedClusterLookupFailedReason, err.Error(), recovery.Generation, c.TimeNow()), err), nil
	}

	if hostedCluster.Spec.PausedUntil == nil || !strings.EqualFold(*hostedCluster.Spec.PausedUntil, "true") {
		cfg := hypershiftapply.HostedCluster(hostedCluster.Name, hostedCluster.Namespace).
			WithUID(hostedCluster.UID).
			WithResourceVersion(hostedCluster.ResourceVersion).
			WithSpec(hypershiftapply.HostedClusterSpec().WithPausedUntil("true"))
		patch, err := json.Marshal(cfg)
		if err != nil {
			return proceed(), fmt.Errorf("failed to marshal HostedCluster pause patch: %w", err)
		}
		// A merge patch can replace a pause owned by another field manager.
		_, err = c.hypershiftClient.HypershiftV1beta1().HostedClusters(hostedCluster.Namespace).
			Patch(ctx, hostedCluster.Name, types.MergePatchType, patch, metav1.PatchOptions{FieldManager: fieldManager})
		if err != nil {
			err = fmt.Errorf("failed to pause HostedCluster %s/%s: %w", hostedCluster.Namespace, hostedCluster.Name, err)
			return c.retryableError(recovery, HostedClusterNotPausedCondition(
				HostedClusterPauseFailedReason, err.Error(), recovery.Generation, c.TimeNow()), err), nil
		}
		// The pause patch lands as a watch event on the HostedCluster.
		return yield(), nil
	}

	hostedControlPlane, err := c.hostedControlPlaneLister.HostedControlPlanes(recovery.Spec.ControlPlaneNamespace).Get(recovery.Spec.HostedClusterName)
	if err != nil {
		err = fmt.Errorf("failed to get HostedControlPlane %s/%s: %w", recovery.Spec.ControlPlaneNamespace, recovery.Spec.HostedClusterName, err)
		return c.retryableError(recovery, HostedClusterNotPausedCondition(
			HostedControlPlaneLookupFailedReason, err.Error(), recovery.Generation, c.TimeNow()), err), nil
	}
	if meta.IsStatusConditionTrue(hostedCluster.Status.Conditions, string(hypershiftv1beta1.ReconciliationActive)) ||
		meta.IsStatusConditionTrue(hostedControlPlane.Status.Conditions, string(hypershiftv1beta1.ReconciliationActive)) {
		statusUpdate, needsUpdate := NewStatus(recovery.Status).
			WithConditions(HostedClusterNotPausedCondition(
				HostedClusterPausePendingReason, "Waiting for HostedCluster and HostedControlPlane reconciliation to pause",
				recovery.Generation, c.TimeNow())).
			AsApplyConfiguration(recovery)
		return waiting(statusUpdate, needsUpdate), nil
	}

	statusUpdate, needsUpdate := NewStatus(recovery.Status).
		WithConditions(HostedClusterPausedCondition(recovery.Generation, c.TimeNow())).
		AsApplyConfiguration(recovery)
	return satisfied(statusUpdate, needsUpdate), nil
}

const (
	KubeAPIServerDeploymentLookupFailedReason    = "KubeAPIServerDeploymentLookupFailedReason"
	KubeAPIServerPatchToZeroReplicasFailedReason = "KubeAPIServerPatchToZeroReplicasFailedReason"
)

func (c *RecoveryController) scaleDownKubeAPIServer(ctx context.Context, recovery *hcprecoveryv1alpha1.HCPRecovery) (stepResult, error) {
	deployment, err := c.deploymentLister.Deployments(recovery.Spec.ControlPlaneNamespace).Get("kube-apiserver")
	if err != nil {
		err = fmt.Errorf("failed to get kube-apiserver deployment in namespace %q: %w", recovery.Spec.ControlPlaneNamespace, err)
		return c.retryableError(recovery, KubeAPIServerDeploymentLookUpFailedCondition(
			KubeAPIServerDeploymentLookupFailedReason, err.Error(), recovery.Generation,
			c.TimeNow()), err), nil
	}
	if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 0 {
		_, err = c.kubeClientset.AppsV1().
			Deployments(recovery.Spec.ControlPlaneNamespace).
			Patch(ctx, "kube-apiserver", types.MergePatchType,
				[]byte(`{"spec":{"replicas":0}}`),
				metav1.PatchOptions{FieldManager: fieldManager},
				"scale")

		if err != nil {
			return c.retryableError(recovery, KubeAPIServerNotScaledToZeroCondition(
				KubeAPIServerPatchToZeroReplicasFailedReason, err.Error(), recovery.Generation,
				c.TimeNow()), err), nil
		}
		// The patch lands as a watch event on the Deployment.
		return yield(), nil
	}
	if deployment.Status.Replicas != 0 {
		// Scale-down is in flight; the Deployment watch wakes us when it finishes.
		return yield(), nil
	}

	statusUpdate, needsUpdate := NewStatus(recovery.Status).
		WithConditions(KubeAPIServerScaledDownToZeroReplicasCondition(recovery.Generation, c.TimeNow())).AsApplyConfiguration(recovery)

	return satisfied(statusUpdate, needsUpdate), nil
}

const (
	EtcdStatefulSetPatchToZeroReplicasFailedReason = "EtcdStatefulSetPatchToZeroReplicasFailedReason"
)

func (c *RecoveryController) scaleDownEtcdBeforeRestore(ctx context.Context, recovery *hcprecoveryv1alpha1.HCPRecovery) (stepResult, error) {
	return c.scaleDownEtcd(ctx, recovery, hcprecoveryv1alpha1.ConditionEtcdScaledToZero)
}

// scaleDownEtcd is shared with the Restoring phase, which runs it again after the
// Velero restore brings the StatefulSet back up. See scaleDownEtcdAfterRestore.
func (c *RecoveryController) scaleDownEtcd(ctx context.Context, recovery *hcprecoveryv1alpha1.HCPRecovery, conditionType string) (stepResult, error) {
	etcdStatefulSet, err := c.statefulsetLister.StatefulSets(recovery.Spec.ControlPlaneNamespace).Get(etcd)
	if err != nil {
		err = fmt.Errorf("failed to get etcd statefulset in namespace %q: %w", recovery.Spec.ControlPlaneNamespace, err)
		return c.retryableError(recovery, EtcdNotScaledToZeroCondition(
			conditionType, EtcdStatefulSetLookupFailedReason, err.Error(), recovery.Generation,
			c.TimeNow()), err), nil
	}
	if etcdStatefulSet.Spec.Replicas == nil || *etcdStatefulSet.Spec.Replicas != 0 {
		_, err = c.kubeClientset.AppsV1().
			StatefulSets(recovery.Spec.ControlPlaneNamespace).
			Patch(ctx, etcd, types.MergePatchType,
				[]byte(`{"spec":{"replicas":0}}`),
				metav1.PatchOptions{FieldManager: fieldManager},
				"scale")

		if err != nil {
			return c.retryableError(recovery, EtcdNotScaledToZeroCondition(
				conditionType, EtcdStatefulSetPatchToZeroReplicasFailedReason, err.Error(), recovery.Generation,
				c.TimeNow()), err), nil
		}
		// The patch lands as a watch event on the StatefulSet.
		return yield(), nil
	}
	if etcdStatefulSet.Status.Replicas != 0 {
		// Scale-down is in flight; the StatefulSet watch wakes us when it finishes.
		return yield(), nil
	}

	statusUpdate, needsUpdate := NewStatus(recovery.Status).
		WithConditions(EtcdScaledDownToZeroReplicasCondition(conditionType, recovery.Generation, c.TimeNow())).AsApplyConfiguration(recovery)

	return satisfied(statusUpdate, needsUpdate), nil
}

const (
	EtcdCachedPVCListFailedReason = "CachedPVCListFailed"
	EtcdLivePVCListFailedReason   = "LivePVCListFailed"
)

func (c *RecoveryController) deleteEtcdPVCs(ctx context.Context, recovery *hcprecoveryv1alpha1.HCPRecovery) (stepResult, error) {
	requirement, err := labels.NewRequirement("app", selection.Equals, []string{etcd})
	if err != nil {
		err = fmt.Errorf("failed to build scaled-down pod selector: %w", err)
		return c.permanentError(recovery, ScaledDownNotValidatedCondition(
			ScaledDownInvalidPodSelectorReason, err.Error(), recovery.Generation, c.TimeNow())), nil
	}
	selector := labels.NewSelector().Add(*requirement)
	pvcs, err := c.pvcLister.PersistentVolumeClaims(recovery.Spec.ControlPlaneNamespace).List(selector)
	if err != nil {
		err = fmt.Errorf("failed to list PVCs from cache in namespace %q: %w", recovery.Spec.ControlPlaneNamespace, err)
		return c.retryableError(recovery, EtcdPVCsNotDeletedCondition(
			EtcdCachedPVCListFailedReason, err.Error(), recovery.Generation, c.TimeNow()), err), nil
	}
	if len(pvcs) > 0 {
		for _, pvc := range pvcs {
			if pvc.DeletionTimestamp == nil {
				return yield(), c.kubeClientset.CoreV1().PersistentVolumeClaims(recovery.Spec.ControlPlaneNamespace).Delete(ctx, pvc.Name, metav1.DeleteOptions{})
			}
		}
		// Every PVC is already terminating. validateScaledDown is the gate that waits
		// for them to actually go away, so let the pipeline reach it.
		return proceed(), nil
	}

	livePVCs, err := c.kubeClientset.CoreV1().PersistentVolumeClaims(recovery.Spec.ControlPlaneNamespace).List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		err = fmt.Errorf("failed to list etcd PVCs from API server in namespace %q: %w", recovery.Spec.ControlPlaneNamespace, err)
		return c.retryableError(recovery, EtcdPVCsNotDeletedCondition(
			EtcdLivePVCListFailedReason, err.Error(), recovery.Generation, c.TimeNow()), err), nil
	}
	if len(livePVCs.Items) > 0 {
		// The cache is behind the API server. validateScaledDown re-checks live, so
		// let the pipeline reach it rather than claiming the PVCs are gone.
		return proceed(), nil
	}

	statusUpdate, needsUpdate := NewStatus(recovery.Status).
		WithConditions(EtcdPVCsDeletedCondition(recovery.Generation, c.TimeNow())).
		AsApplyConfiguration(recovery)
	return satisfied(statusUpdate, needsUpdate), nil
}

const (
	apiServer = "kube-apiserver"
	etcd      = "etcd"

	ScaledDownInvalidPodSelectorReason           = "InvalidPodSelector"
	ScaledDownCachedPodListFailedReason          = "CachedPodListFailed"
	ScaledDownCachedPodsRemainingReason          = "CachedPodsRemaining"
	ScaledDownLivePodListFailedReason            = "LivePodListFailed"
	ScaledDownLivePodsRemainingReason            = "LivePodsRemaining"
	ScaledDownCachedPVCsRemainingReason          = "CachedPVCsRemaining"
	ScaledDownLivePVCsRemainingReason            = "LivePVCsRemaining"
	ScaledDownHostedClusterNotPausedReason       = "HostedClusterNotPaused"
	ScaledDownDeploymentReplicasRemainingReason  = "DeploymentReplicasRemaining"
	ScaledDownStatefulSetReplicasRemainingReason = "StatefulSetReplicasRemaining"
)

func (c *RecoveryController) validateScaledDown(ctx context.Context, recovery *hcprecoveryv1alpha1.HCPRecovery) (stepResult, error) {
	requirement, err := labels.NewRequirement("app", selection.In, []string{apiServer, etcd})
	if err != nil {
		err = fmt.Errorf("failed to build scaled-down pod selector: %w", err)
		return c.permanentError(recovery, ScaledDownNotValidatedCondition(
			ScaledDownInvalidPodSelectorReason, err.Error(), recovery.Generation, c.TimeNow())), nil
	}
	selector := labels.NewSelector().Add(*requirement)
	pods, err := c.podLister.Pods(recovery.Spec.ControlPlaneNamespace).List(selector)
	if err != nil {
		err = fmt.Errorf("failed to fetch pods from cache: %w", err)
		return c.retryableError(recovery, ScaledDownNotValidatedCondition(
			ScaledDownCachedPodListFailedReason, err.Error(), recovery.Generation, c.TimeNow()), err), nil
	}
	if len(pods) > 0 {
		if err := c.reapTerminalPod(ctx, pods); err != nil {
			return c.retryableError(recovery, ScaledDownNotValidatedCondition(
				TerminalPodDeleteFailedReason, err.Error(), recovery.Generation, c.TimeNow()), err), nil
		}
		err = fmt.Errorf("cache check shows we are still waiting for pods to be cleaned up: %s", blockingPods(pods))
		return c.retryableError(recovery, ScaledDownNotValidatedCondition(
			ScaledDownCachedPodsRemainingReason, err.Error(), recovery.Generation, c.TimeNow()), err), nil
	}

	pvcSelector := labels.SelectorFromSet(labels.Set{"app": etcd})
	pvcs, err := c.pvcLister.PersistentVolumeClaims(recovery.Spec.ControlPlaneNamespace).List(pvcSelector)
	if err != nil {
		err = fmt.Errorf("failed to list etcd PVCs from cache: %w", err)
		return c.retryableError(recovery, ScaledDownNotValidatedCondition(
			EtcdCachedPVCListFailedReason, err.Error(), recovery.Generation, c.TimeNow()), err), nil
	}
	if len(pvcs) > 0 {
		err = fmt.Errorf("cache check shows we are still waiting for etcd PVCs to be deleted")
		return c.retryableError(recovery, ScaledDownNotValidatedCondition(
			ScaledDownCachedPVCsRemainingReason, err.Error(), recovery.Generation, c.TimeNow()), err), nil
	}

	hostedCluster, err := c.hypershiftClient.HypershiftV1beta1().HostedClusters(recovery.Spec.HostedClusterNamespace).Get(ctx, recovery.Spec.HostedClusterName, metav1.GetOptions{})
	if err != nil {
		err = fmt.Errorf("failed to get HostedCluster %s/%s from API server: %w", recovery.Spec.HostedClusterNamespace, recovery.Spec.HostedClusterName, err)
		return c.retryableError(recovery, ScaledDownNotValidatedCondition(
			HostedClusterLookupFailedReason, err.Error(), recovery.Generation, c.TimeNow()), err), nil
	}
	if paused := hostedCluster.Spec.PausedUntil; paused == nil || !strings.EqualFold(*paused, "true") {
		err = fmt.Errorf("HostedCluster is not paused")
		return c.retryableError(recovery, ScaledDownNotValidatedCondition(
			ScaledDownHostedClusterNotPausedReason, err.Error(), recovery.Generation, c.TimeNow()), err), nil
	}

	hostedControlPlane, err := c.hypershiftClient.HypershiftV1beta1().HostedControlPlanes(recovery.Spec.ControlPlaneNamespace).Get(ctx, recovery.Spec.HostedClusterName, metav1.GetOptions{})
	if err != nil {
		err = fmt.Errorf("failed to get HostedControlPlane %s/%s from API server: %w", recovery.Spec.ControlPlaneNamespace, recovery.Spec.HostedClusterName, err)
		return c.retryableError(recovery, ScaledDownNotValidatedCondition(
			HostedControlPlaneLookupFailedReason, err.Error(), recovery.Generation, c.TimeNow()), err), nil
	}
	if meta.IsStatusConditionTrue(hostedControlPlane.Status.Conditions, string(hypershiftv1beta1.ReconciliationActive)) {
		err = fmt.Errorf("HostedControlPlane reconciliation is still active")
		return c.retryableError(recovery, ScaledDownNotValidatedCondition(
			HostedControlPlanePausePendingReason, err.Error(), recovery.Generation, c.TimeNow()), err), nil
	}

	deployment, err := c.kubeClientset.AppsV1().Deployments(recovery.Spec.ControlPlaneNamespace).Get(ctx, apiServer, metav1.GetOptions{})
	if err != nil {
		err = fmt.Errorf("failed to get kube-apiserver deployment from API server: %w", err)
		return c.retryableError(recovery, ScaledDownNotValidatedCondition(
			KubeAPIServerDeploymentLookupFailedReason, err.Error(), recovery.Generation, c.TimeNow()), err), nil
	}
	if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 0 || deployment.Status.Replicas != 0 {
		err = fmt.Errorf("kube-apiserver deployment has not scaled down to zero replicas")
		return c.retryableError(recovery, ScaledDownNotValidatedCondition(
			ScaledDownDeploymentReplicasRemainingReason, err.Error(), recovery.Generation, c.TimeNow()), err), nil
	}

	statefulSet, err := c.kubeClientset.AppsV1().StatefulSets(recovery.Spec.ControlPlaneNamespace).Get(ctx, etcd, metav1.GetOptions{})
	if err != nil {
		err = fmt.Errorf("failed to get etcd StatefulSet from API server: %w", err)
		return c.retryableError(recovery, ScaledDownNotValidatedCondition(
			EtcdStatefulSetLookupFailedReason, err.Error(), recovery.Generation, c.TimeNow()), err), nil
	}
	if statefulSet.Spec.Replicas == nil || *statefulSet.Spec.Replicas != 0 || statefulSet.Status.Replicas != 0 {
		err = fmt.Errorf("etcd StatefulSet has not scaled down to zero replicas")
		return c.retryableError(recovery, ScaledDownNotValidatedCondition(
			ScaledDownStatefulSetReplicasRemainingReason, err.Error(), recovery.Generation, c.TimeNow()), err), nil
	}

	livePods, err := c.kubeClientset.CoreV1().Pods(recovery.Spec.ControlPlaneNamespace).List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		err = fmt.Errorf("failed to fetch pods from api server: %w", err)
		return c.retryableError(recovery, ScaledDownNotValidatedCondition(
			ScaledDownLivePodListFailedReason, err.Error(), recovery.Generation, c.TimeNow()), err), nil
	}
	if len(livePods.Items) > 0 {
		for i := range livePods.Items {
			pods = append(pods, &livePods.Items[i])
		}
		if err := c.reapTerminalPod(ctx, pods); err != nil {
			return c.retryableError(recovery, ScaledDownNotValidatedCondition(
				TerminalPodDeleteFailedReason, err.Error(), recovery.Generation, c.TimeNow()), err), nil
		}
		err = fmt.Errorf("live check shows we are still waiting for pods to be cleaned up: %s", blockingPods(pods))
		return c.retryableError(recovery, ScaledDownNotValidatedCondition(
			ScaledDownLivePodsRemainingReason, err.Error(), recovery.Generation, c.TimeNow()), err), nil
	}

	livePVCs, err := c.kubeClientset.CoreV1().PersistentVolumeClaims(recovery.Spec.ControlPlaneNamespace).List(ctx, metav1.ListOptions{LabelSelector: pvcSelector.String()})
	if err != nil {
		err = fmt.Errorf("failed to list etcd PVCs from API server: %w", err)
		return c.retryableError(recovery, ScaledDownNotValidatedCondition(
			EtcdLivePVCListFailedReason, err.Error(), recovery.Generation, c.TimeNow()), err), nil
	}
	if len(livePVCs.Items) > 0 {
		err = fmt.Errorf("live check shows we are still waiting for etcd PVCs to be deleted")
		return c.retryableError(recovery, ScaledDownNotValidatedCondition(
			ScaledDownLivePVCsRemainingReason, err.Error(), recovery.Generation, c.TimeNow()), err), nil
	}

	statusUpdate, needsUpdate := NewStatus(recovery.Status).
		WithConditions(ScaledDownValidatedCondition(recovery.Generation, c.TimeNow())).
		AsApplyConfiguration(recovery)
	return satisfied(statusUpdate, needsUpdate), nil
}
