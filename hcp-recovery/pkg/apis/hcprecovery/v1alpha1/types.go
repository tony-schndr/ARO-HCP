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

// Package v1alpha1 contains API types for the hcprecovery API group.
// The HCPRecovery resource manages disaster recovery operations for Hosted Control Planes,
// coordinating etcd backup restoration and cluster state reconciliation.
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RestoreState represents the overall state of the recovery operation.
type RestoreState string

const (
	RestoreStatePreparing  RestoreState = "Preparing"
	RestoreStateRestoring  RestoreState = "Restoring"
	RestoreStateValidating RestoreState = "Validating"
	RestoreStateCompleted  RestoreState = "Completed"
	RestoreStateFailed     RestoreState = "Failed"
)

// Condition types for HCPRecovery.
const (
	// ConditionRecoveryInputsValidated indicates whether the recovery specification,
	// backup, and target cluster resources are valid and consistent.
	ConditionRecoveryInputsValidated = "RecoveryInputsValidated"
	// ConditionHostedClusterPaused indicates whether the HostedCluster has been paused
	// to prevent the hypershift operator from interfering with the restore.
	ConditionHostedClusterPaused = "HostedClusterPaused"
	// ConditionHealthChecked indicates that the HostedCluster is healthy after a restore
	ConditionHealthChecked = "HealthChecked"
	// ConditionHostedClusterUnpaused indicates that the HostedCluster has been unpaused after a restore
	ConditionHostedClusterUnpaused = "HostedClusterUnpaused"
	// ConditionVeleroRestoreCreated indicates whether the Velero restore has been created.
	ConditionVeleroRestoreCreated = "VeleroRestoreCreated"
	// ConditionVeleroRestoreCompleted indicates the Velero restore has been completed.
	ConditionVeleroRestoreCompleted = "VeleroRestoreCompleted"
	// ConditionKubeAPIServerScaledToZero indicates that the kube api server has no replicas running to serve traffic
	ConditionKubeAPIServerScaledToZero = "ConditionKubeAPIServerScaledToZero"
	// ConditionKubeAPIServerLookupFailed indicates that the kube-apiserver deployment lookup failed
	ConditionKubeAPIServerLookupFailed = "ConditionKubeAPIServerLookupFailed"
	// ConditionEtcdScaledToZero indicates that the etcd statefulset has no replicas running.
	ConditionEtcdScaledToZero = "ConditionEtcdScaledToZero"
	// ConditionEtcdScaledToZeroAfterVeleroRestore tracks the second etcd scale-down before restoring etcd data.
	ConditionEtcdScaledToZeroAfterVeleroRestore = "EtcdScaledToZeroAfterVeleroRestore"
	// ConditionScaledDownValidated indicates that no kube-apiserver or etcd pods remain.
	ConditionScaledDownValidated = "ScaledDownValidated"
	// ConditionEtcdPVCsDeleted indicates that no etcd PVCs remain.
	ConditionEtcdPVCsDeleted = "EtcdPVCsDeleted"
	// ConditionCapturedEtcdRuntimeDetails indicates the etcd runtime information was collected for etcd restore jobs
	EtcdRuntimeDetailsCollectedCondition = "CapturedEtcdRuntimeDetails"
	// ConditionEtcdPodsGone indicates no etcd pods remain after the post-Velero scale-down,
	// so the restored etcd PVCs can be mounted by the recovery pods.
	ConditionEtcdPodsGone = "EtcdPodsGone"
	// ConditionEtcdHolderPodsReady indicates a holder pod is running for every etcd PVC,
	// making each PVC reachable for snapshot distribution.
	ConditionEtcdHolderPodsReady = "EtcdHolderPodsReady"
	// ConditionEtcdSnapshotDistributed indicates the canonical etcd snapshot has been copied
	// to every etcd PVC and verified identical on each of them.
	ConditionEtcdSnapshotDistributed = "EtcdSnapshotDistributed"
	// ConditionEtcdSnapshotRestored indicates every etcd member's data directory has been
	// rebuilt from the canonical snapshot.
	ConditionEtcdSnapshotRestored = "EtcdSnapshotRestored"
	// ConditionEtcdScaledUp indicates the etcd statefulset is back at its original replica
	// count with every member pod ready.
	ConditionEtcdScaledUp = "EtcdScaledUp"
	// ConditionEtcdHealthy indicates etcd reports a single in-sync cluster with one leader.
	ConditionEtcdHealthy = "EtcdHealthy"
)

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="ClusterID",type=string,JSONPath=`.spec.hcpClusterAzureResourceId`
// +kubebuilder:printcolumn:name="BackupID",type=string,JSONPath=`.spec.veleroBackupName`
// +kubebuilder:printcolumn:name="Started",type=string,format=date-time,JSONPath=`.status.startedAt`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Completed",type=string,format=date-time,JSONPath=`.status.completedAt`

// HCPRecovery represents a disaster recovery operation for a Hosted Control Plane.
// It orchestrates the restoration of an HCP from a Velero backup, coordinating
// the pause of the HostedCluster, deletion and recreation of the HCP namespace,
// Velero restore execution, CAPI machine reconciliation, and post-restore health checks.
// The spec is immutable after creation — to change recovery parameters, delete and recreate.
//
// +kubebuilder:validation:XValidation:rule="(!has(oldSelf.status) || !has(oldSelf.status.phase)) ? (!has(self.status) || !has(self.status.phase) || self.status.phase in ['Preparing', 'Failed']) : (has(self.status) && has(self.status.phase) && (self.status.phase == 'Failed' || self.status.phase == oldSelf.status.phase || (oldSelf.status.phase == 'Preparing' && self.status.phase == 'Restoring') || (oldSelf.status.phase == 'Restoring' && self.status.phase == 'Validating') || (oldSelf.status.phase == 'Validating' && self.status.phase == 'Completed')))",message="phase must advance Preparing -> Restoring -> Validating -> Completed, or to Failed from any phase"
type HCPRecovery struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// +required
	// spec defines the desired recovery operation, including the target cluster
	// and backup to restore from. This field is immutable after creation.
	Spec HCPRecoverySpec `json:"spec"`

	// +optional
	// status contains the observed state of the recovery operation, including
	// progress through each recovery step and timestamps.
	Status HCPRecoveryStatus `json:"status,omitempty,omitzero"`
}

// HCPRecoverySpec defines the desired state of a recovery operation.
// All fields are immutable after creation to ensure recovery operations
// are auditable and cannot be modified mid-flight.
//
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable"
type HCPRecoverySpec struct {
	// +kubebuilder:validation:Required
	// hcpClusterAzureResourceId identifies the hosted cluster to restore
	HcpClusterAzureResourceID string `json:"hcpClusterAzureResourceId"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// hostedClusterName is the name of the HostedCluster.
	HostedClusterName string `json:"hostedClusterName"`

	// +kubebuilder:validation:Required
	// hostedClusterNamespace is the namespace of the HostedCluster.
	HostedClusterNamespace string `json:"hostedClusterNamespace"`

	// +kubebuilder:validation:Required
	// controlPlaneNamespace is the namespace where the hosted clusters control plane resources live.
	ControlPlaneNamespace string `json:"controlPlaneNamespace"`

	// +kubebuilder:validation:Required
	// veleroBackupName is the identifier of the Velero backup to restore from.
	// The controller validates that this backup exists, belongs to the hosted
	// cluster attempting to be restored, and is in a completed state before
	// proceeding with the restore.
	VeleroBackupName string `json:"veleroBackupName"`
}

// HCPRecoveryStatus reports the observed state of the recovery operation.
type HCPRecoveryStatus struct {
	// +optional
	// +kubebuilder:validation:Enum=Preparing;Restoring;Validating;Completed;Failed
	// phase represents the overall state of the recovery operation.
	// When set to Failed or Completed, the controller will not reprocess the resource.
	Phase RestoreState `json:"phase,omitempty"`

	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchMergeKey=type
	// +patchStrategy=merge
	// conditions represent the current state of the recovery operation.
	// Each condition corresponds to a step in the recovery process.
	// The controller processes steps sequentially; a condition with status
	// True indicates that the step's requirements held when last observed.
	// Preparation requirements are revalidated on every reconciliation.
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// +optional
	// startedAt is the timestamp when the recovery operation began processing.
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// +optional
	// completedAt is the timestamp when the recovery operation finished,
	// either successfully or with a terminal failure.
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`

	// +optional
	// restoredToTimestamp is the point-in-time that the backup represents,
	// indicating the state the HCP has been restored to.
	RestoredToTimestamp *metav1.Time `json:"restoredToTimestamp,omitempty"`

	// +optional
	// restoreName is the name of the Velero Restore created for this recovery.
	// It is persisted to status before the restore is created to prevent
	// duplicate restores if the controller crashes between creation and
	// status update.
	RestoreName string `json:"restoreName,omitempty"`

	// +optional
	// etcdRuntimeDetails captures the etcd image and pod identity from the etcd
	// StatefulSet before restoration, so recovery jobs can use the same tools
	// and preserve ownership of restored files.
	EtcdRuntimeDetails *EtcdRuntimeDetails `json:"etcdRuntimeDetails,omitempty"`

	// +optional
	// etcdSnapshot records the canonical etcd snapshot that was distributed to every
	// etcd PVC. It is reported by the snapshot transfer pod once every copy has been
	// verified, and identifies exactly which point in time etcd was rebuilt from.
	EtcdSnapshot *EtcdSnapshotDetails `json:"etcdSnapshot,omitempty"`
}

// EtcdRuntimeDetails records the runtime configuration needed to restore etcd data.
type EtcdRuntimeDetails struct {
	// +required
	// image is the image of the container named etcd in the StatefulSet pod template.
	Image string `json:"image"`

	// +optional
	// +kubebuilder:validation:Minimum=0
	// runAsUser is the pod security context's runAsUser from the StatefulSet.
	// An unset value means the pod does not specify a user ID.
	RunAsUser *int64 `json:"runAsUser,omitempty"`

	// +optional
	// +kubebuilder:validation:Minimum=0
	// fsGroup is the pod security context's fsGroup from the StatefulSet.
	// An unset value means the pod does not specify a filesystem group ID.
	FSGroup *int64 `json:"fsGroup,omitempty"`
}

// EtcdSnapshotDetails identifies the canonical snapshot every etcd member was restored from.
type EtcdSnapshotDetails struct {
	// +required
	// +kubebuilder:validation:MinLength=1
	// sourceMember is the etcd member whose snapshot was chosen as canonical and
	// copied to every other member's volume.
	SourceMember string `json:"sourceMember"`

	// +required
	// +kubebuilder:validation:MinLength=64
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{64}$`
	// sha256 is the checksum of the canonical snapshot, verified identical on every
	// etcd volume before any member was restored.
	SHA256 string `json:"sha256"`

	// +required
	// +kubebuilder:validation:Minimum=0
	// revision is the etcd revision contained in the canonical snapshot, before the
	// restore applies its revision bump.
	Revision int64 `json:"revision"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true

// HCPRecoveryList is a list of HCPRecovery resources
type HCPRecoveryList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HCPRecovery `json:"items"`
}
