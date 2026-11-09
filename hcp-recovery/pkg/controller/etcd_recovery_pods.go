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
	"slices"
	"strconv"
	"strings"

	_ "embed"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/utils/ptr"

	hcprecoveryv1alpha1 "github.com/Azure/ARO-HCP/hcp-recovery/pkg/apis/hcprecovery/v1alpha1"
	"github.com/Azure/ARO-HCP/internal/backup"
)

//go:embed etcd/distribute-snapshot.sh
var distributeSnapshotScript string

//go:embed etcd/restore-member.sh
var restoreMemberScript string

//go:embed etcd/check-etcd-health.sh
var checkEtcdHealthScript string

const (
	// recoveryNameLabel ties a worker pod back to the HCPRecovery that created it. Pods
	// in the control-plane namespace are found through the namespace index like every
	// other object there; pods in the controller's own namespace are found through this.
	recoveryNameLabel = "hcprecovery.aro-hcp.azure.com/recovery-name"

	// App labels for the worker pods. None of them may be "etcd": the preparation steps
	// select etcd pods and PVCs by app=etcd, and a worker pod answering that selector
	// would make them conclude the control plane is still running.
	etcdHolderApp       = "etcd-holder"
	etcdRestoreApp      = "etcd-restore"
	snapshotTransferApp = "etcd-snapshot-transfer"
	etcdHealthCheckApp  = "etcd-health-check"

	// holderContainerName is the container in a holder pod that has the etcd PVC mounted.
	holderContainerName = "holder"
	// workerContainerName is the single container in the kubectl-driven worker pods.
	workerContainerName = "worker"

	// etcdDataMountPath is where the etcd PVC is mounted, matching the etcd StatefulSet,
	// so backup.db and the data directory sit at the paths the backup hook used.
	etcdDataMountPath = "/var/lib"
	etcdDataDirPath   = etcdDataMountPath + "/data"
	// etcdPVCPrefix is the StatefulSet volume claim template name; data-etcd-0 belongs to
	// the etcd-0 pod.
	etcdPVCPrefix = "data-etcd-"
	// etcdPeerPort and etcdDiscoveryService form the peer URLs baked into the restored
	// data directory, so they must match what the etcd StatefulSet advertises.
	etcdPeerPort         = "2380"
	etcdDiscoveryService = "etcd-discovery"

	// etcdInitialClusterToken and etcdRevisionBump must be identical for every member or
	// they will not form one cluster. See docs/etcd-restore.md step 8.
	etcdInitialClusterToken = "etcd-cluster"
	etcdRevisionBump        = "1000000000"

	// snapshotStagingPath is on the transfer pod's own emptyDir, not on any etcd volume.
	snapshotStagingVolume = "staging"
	snapshotStagingDir    = "/work"
	snapshotStagingPath   = snapshotStagingDir + "/canonical-backup.db"

	// Deadlines the worker pods enforce on themselves. They are generous because the
	// controller treats a pod that gave up as a terminal failure rather than retrying.
	snapshotDistributionDeadline = 30 * 60
	etcdHealthCheckDeadline      = 10 * 60

	// etcdMembers is how many etcd members every recovery rebuilds. HyperShift sizes the
	// etcd StatefulSet from the HostedCluster's ControllerAvailabilityPolicy, and ARO-HCP
	// rejects any cluster that is not HighlyAvailable — both
	// internal/validation/validate_cluster.go and internal/admission/admit_cluster.go
	// forbid the single-replica tag on create — so the StatefulSet is always three. This
	// drives the recovery pod count, the --initial-cluster membership list, and the
	// replica count etcd is scaled back up to.
	etcdMembers = int32(3)
)

func etcdMemberName(index int32) string {
	return fmt.Sprintf("etcd-%d", index)
}

func etcdHolderPodName(index int32) string {
	return fmt.Sprintf("%s-%d", etcdHolderApp, index)
}

func etcdRestorePodName(index int32) string {
	return fmt.Sprintf("%s-%d", etcdRestoreApp, index)
}

func snapshotTransferPodName(recovery *hcprecoveryv1alpha1.HCPRecovery) string {
	return recovery.Name + "-snapshot-transfer"
}

func etcdHealthCheckPodName(recovery *hcprecoveryv1alpha1.HCPRecovery) string {
	return recovery.Name + "-etcd-health-check"
}

func etcdPeerURL(namespace string, index int32) string {
	return fmt.Sprintf("https://%s.%s.%s.svc:%s", etcdMemberName(index), etcdDiscoveryService, namespace, etcdPeerPort)
}

// etcdInitialCluster is the full membership every member is restored with. Restoring
// each member with the same list is what makes them boot as one cluster instead of
// trying to join an existing one.
func etcdInitialCluster(namespace string) string {
	entries := make([]string, 0, etcdMembers)
	for i := int32(0); i < etcdMembers; i++ {
		entries = append(entries, fmt.Sprintf("%s=%s", etcdMemberName(i), etcdPeerURL(namespace, i)))
	}
	return strings.Join(entries, ",")
}

// etcdClientEndpoints addresses every member directly rather than going through the
// client service, so the health check can compare what each member individually reports.
func etcdClientEndpoints(namespace string) string {
	entries := make([]string, 0, etcdMembers)
	for i := int32(0); i < etcdMembers; i++ {
		entries = append(entries, fmt.Sprintf("https://%s.%s.%s.svc:%s",
			etcdMemberName(i), etcdDiscoveryService, namespace, backup.EtcdClientPort))
	}
	return strings.Join(entries, ",")
}

func holderPodNames() []string {
	names := make([]string, 0, etcdMembers)
	for i := int32(0); i < etcdMembers; i++ {
		names = append(names, etcdHolderPodName(i))
	}
	return names
}

// workerPodLabels identify a worker pod both by what it does and by which recovery owns
// it, so the informer handlers can route its events back to that recovery.
func workerPodLabels(recovery *hcprecoveryv1alpha1.HCPRecovery, app string) map[string]string {
	return map[string]string{
		"app":                             app,
		recoveryNameLabel:                 recovery.Name,
		restoreControlPlaneNamespaceLabel: recovery.Spec.ControlPlaneNamespace,
	}
}

// etcdVolumePod builds a pod that mounts one etcd PVC. It reuses the etcd image and pod
// identity captured from the StatefulSet so the tools match and restored files keep the
// ownership etcd expects, and it copies the StatefulSet's scheduling constraints so the
// pod lands somewhere its zone-bound volume can follow. The StatefulSet's affinity is
// deliberately not copied: it spreads etcd members apart, which would leave these pods
// unschedulable against volumes that are already placed.
func etcdVolumePod(
	recovery *hcprecoveryv1alpha1.HCPRecovery,
	etcdStatefulSet *appsv1.StatefulSet,
	name, app string,
	index int32,
	command []string,
	env []corev1.EnvVar,
) *corev1.Pod {
	details := recovery.Status.EtcdRuntimeDetails
	template := etcdStatefulSet.Spec.Template.Spec
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: recovery.Spec.ControlPlaneNamespace,
			Labels:    workerPodLabels(recovery, app),
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsUser: details.RunAsUser,
				FSGroup:   details.FSGroup,
			},
			// These pods only touch their mounted volume, so they get no API credentials.
			AutomountServiceAccountToken: ptr.To(false),
			NodeSelector:                 template.NodeSelector,
			Tolerations:                  template.Tolerations,
			PriorityClassName:            template.PriorityClassName,
			Containers: []corev1.Container{{
				Name:                     containerNameFor(app),
				Image:                    details.Image,
				Command:                  command,
				Env:                      env,
				TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
				VolumeMounts: []corev1.VolumeMount{{
					Name:      "data",
					MountPath: etcdDataMountPath,
				}},
			}},
			Volumes: []corev1.Volume{{
				Name: "data",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: fmt.Sprintf("%s%d", etcdPVCPrefix, index),
					},
				},
			}},
		},
	}
}

func containerNameFor(app string) string {
	if app == etcdHolderApp {
		return holderContainerName
	}
	return workerContainerName
}

// etcdHolderPod keeps one etcd PVC mounted and does nothing else. The volumes are in
// different availability zones, so this is the only way to reach a volume's contents
// from somewhere that can also reach the others.
func etcdHolderPod(recovery *hcprecoveryv1alpha1.HCPRecovery, etcdStatefulSet *appsv1.StatefulSet, index int32) *corev1.Pod {
	return etcdVolumePod(recovery, etcdStatefulSet, etcdHolderPodName(index), etcdHolderApp, index,
		[]string{"/bin/sh", "-c", "sleep infinity"}, nil)
}

// etcdRestorePod rebuilds one member's data directory from the distributed snapshot.
func etcdRestorePod(recovery *hcprecoveryv1alpha1.HCPRecovery, etcdStatefulSet *appsv1.StatefulSet, index int32) *corev1.Pod {
	namespace := recovery.Spec.ControlPlaneNamespace
	return etcdVolumePod(recovery, etcdStatefulSet, etcdRestorePodName(index), etcdRestoreApp, index,
		[]string{"/bin/sh", "-c", restoreMemberScript},
		[]corev1.EnvVar{
			{Name: "ETCD_MEMBER", Value: etcdMemberName(index)},
			{Name: "INITIAL_CLUSTER", Value: etcdInitialCluster(namespace)},
			{Name: "INITIAL_ADVERTISE_PEER_URLS", Value: etcdPeerURL(namespace, index)},
			{Name: "INITIAL_CLUSTER_TOKEN", Value: etcdInitialClusterToken},
			{Name: "BUMP_REVISION", Value: etcdRevisionBump},
			{Name: "SNAPSHOT_PATH", Value: backup.EtcdSnapshotPath},
			{Name: "DATA_DIR", Value: etcdDataDirPath},
		})
}

// kubectlWorkerPod builds a pod that drives work in other pods over the Kubernetes API.
// It runs in the controller's namespace, which mounts no etcd volume and lets it carry
// an owner reference to the HCPRecovery, so it is garbage collected if the recovery is
// deleted while it is still running.
func kubectlWorkerPod(
	recovery *hcprecoveryv1alpha1.HCPRecovery,
	name, app, image, serviceAccount, script string,
	env []corev1.EnvVar,
	volumes []corev1.Volume,
	volumeMounts []corev1.VolumeMount,
) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: recovery.Namespace,
			Labels:    workerPodLabels(recovery, app),
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(recovery,
				hcprecoveryv1alpha1.SchemeGroupVersion.WithKind("HCPRecovery"))},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:      corev1.RestartPolicyNever,
			ServiceAccountName: serviceAccount,
			SecurityContext:    &corev1.PodSecurityContext{RunAsNonRoot: ptr.To(true)},
			Containers: []corev1.Container{{
				Name:    workerContainerName,
				Image:   image,
				Command: []string{"/bin/sh", "-c", script},
				Env:     env,
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: ptr.To(false),
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
					SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
				},
				// On success the script writes its result here; on failure the tail of its
				// logs lands here instead, so the controller can surface either in a
				// condition without streaming logs.
				TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
				VolumeMounts:             volumeMounts,
			}},
			Volumes: volumes,
		},
	}
}

// snapshotTransferPod copies the canonical snapshot onto every etcd volume via the
// holder pods, staging it on its own emptyDir because no pod can mount all the volumes.
func snapshotTransferPod(recovery *hcprecoveryv1alpha1.HCPRecovery, image, serviceAccount string) *corev1.Pod {
	return kubectlWorkerPod(recovery, snapshotTransferPodName(recovery), snapshotTransferApp,
		image, serviceAccount, distributeSnapshotScript,
		[]corev1.EnvVar{
			{Name: "HCP_NS", Value: recovery.Spec.ControlPlaneNamespace},
			{Name: "CANONICAL_POD", Value: etcdHolderPodName(0)},
			{Name: "CANONICAL_MEMBER", Value: etcdMemberName(0)},
			{Name: "HOLDER_PODS", Value: strings.Join(holderPodNames(), " ")},
			{Name: "HOLDER_CONTAINER", Value: holderContainerName},
			{Name: "SNAPSHOT_PATH", Value: backup.EtcdSnapshotPath},
			{Name: "STAGING_PATH", Value: snapshotStagingPath},
			{Name: "DEADLINE_SECONDS", Value: strconv.Itoa(snapshotDistributionDeadline)},
		},
		[]corev1.Volume{{
			Name:         snapshotStagingVolume,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		}},
		[]corev1.VolumeMount{{Name: snapshotStagingVolume, MountPath: snapshotStagingDir}},
	)
}

// etcdHealthCheckPod asks the restored etcd whether it is one in-sync cluster.
func etcdHealthCheckPod(recovery *hcprecoveryv1alpha1.HCPRecovery, image, serviceAccount string) *corev1.Pod {
	return kubectlWorkerPod(recovery, etcdHealthCheckPodName(recovery), etcdHealthCheckApp,
		image, serviceAccount, checkEtcdHealthScript,
		[]corev1.EnvVar{
			{Name: "HCP_NS", Value: recovery.Spec.ControlPlaneNamespace},
			{Name: "EXEC_POD", Value: etcdMemberName(0)},
			{Name: "ETCD_CONTAINER", Value: backup.EtcdContainerName},
			{Name: "ETCD_ENDPOINTS", Value: etcdClientEndpoints(recovery.Spec.ControlPlaneNamespace)},
			{Name: "EXPECTED_MEMBERS", Value: strconv.Itoa(int(etcdMembers))},
			{Name: "ETCD_CACERT", Value: backup.EtcdCACertPath},
			{Name: "ETCD_CERT", Value: backup.EtcdClientCert},
			{Name: "ETCD_KEY", Value: backup.EtcdClientKey},
			// Sourced from the same constant the restore pods bump by, so the floor
			// cannot drift away from what the restore actually applied.
			{Name: "MIN_REVISION", Value: etcdRevisionBump},
			{Name: "DEADLINE_SECONDS", Value: strconv.Itoa(etcdHealthCheckDeadline)},
		},
		nil, nil,
	)
}

// podResult reports what a worker pod has to say for itself. message is the container's
// termination message, which is the script's own result on success and the tail of its
// logs on failure, falling back to the pod-level status when the container never ran.
func podResult(pod *corev1.Pod) (phase corev1.PodPhase, message string) {
	message = pod.Status.Message
	for _, status := range pod.Status.ContainerStatuses {
		if terminated := status.State.Terminated; terminated != nil {
			if terminated.Message != "" {
				message = terminated.Message
			} else if terminated.Reason != "" {
				message = fmt.Sprintf("container %s terminated with exit code %d (%s)",
					status.Name, terminated.ExitCode, terminated.Reason)
			}
		}
	}
	return pod.Status.Phase, strings.TrimSpace(message)
}

// workerPodSelector matches this recovery's worker pods of the given kinds. Scoping by
// recovery name keeps two recoveries in the same namespace from deleting each other's
// pods, which also stops a stale pod from a previous attempt being mistaken for a
// current one.
func workerPodSelector(recovery *hcprecoveryv1alpha1.HCPRecovery, apps ...string) (labels.Selector, error) {
	app, err := labels.NewRequirement("app", selection.In, apps)
	if err != nil {
		return nil, fmt.Errorf("failed to build worker pod app selector: %w", err)
	}
	owner, err := labels.NewRequirement(recoveryNameLabel, selection.Equals, []string{recovery.Name})
	if err != nil {
		return nil, fmt.Errorf("failed to build worker pod owner selector: %w", err)
	}
	return labels.NewSelector().Add(*app, *owner), nil
}

// deleteWorkerPods deletes at most one matching pod per call, mirroring deleteEtcdPVCs
// so each reconcile performs a single mutating action. An empty cache is confirmed
// against the API server before reporting that none remain, because the next step
// either mounts the volumes these pods hold or starts etcd on top of them.
func (c *RecoveryController) deleteWorkerPods(
	ctx context.Context, namespace string, selector labels.Selector,
) (acted bool, remaining bool, err error) {
	pods, err := c.podLister.Pods(namespace).List(selector)
	if err != nil {
		return false, true, fmt.Errorf("failed to list worker pods from cache in namespace %q: %w", namespace, err)
	}
	// Listers return cache order, which is arbitrary. Deleting in a stable order keeps a
	// repeated failure reporting the same pod, so the condition settles instead of
	// flapping between names and rewriting status on every retry.
	slices.SortFunc(pods, func(a, b *corev1.Pod) int { return strings.Compare(a.Name, b.Name) })
	for _, pod := range pods {
		if pod.DeletionTimestamp != nil {
			continue
		}
		err := c.kubeClientset.CoreV1().Pods(namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return false, true, fmt.Errorf("failed to delete worker pod %s/%s: %w", namespace, pod.Name, err)
		}
		return true, true, nil
	}
	if len(pods) > 0 {
		// Everything left is already terminating; its deletion event brings us back.
		return false, true, nil
	}

	livePods, err := c.kubeClientset.CoreV1().Pods(namespace).
		List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		return false, true, fmt.Errorf("failed to list worker pods from API server in namespace %q: %w", namespace, err)
	}
	return false, len(livePods.Items) > 0, nil
}

// podReady reports whether every container in a running pod is ready.
func podReady(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}
