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
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic/dynamicinformer"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clientfeatures "k8s.io/client-go/features"
	clientfeaturestesting "k8s.io/client-go/features/testing"
	kubeinformers "k8s.io/client-go/informers"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"

	hypershiftv1beta1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	hypershiftfake "github.com/openshift/hypershift/client/clientset/clientset/fake"
	hypershiftinformers "github.com/openshift/hypershift/client/informers/externalversions"
	hypershiftlisters "github.com/openshift/hypershift/client/listers/hypershift/v1beta1"

	hcprecoveryv1alpha1 "github.com/Azure/ARO-HCP/hcp-recovery/pkg/apis/hcprecovery/v1alpha1"
	recoveryapply "github.com/Azure/ARO-HCP/hcp-recovery/pkg/generated/applyconfiguration/hcprecovery/v1alpha1"
	recoveryfake "github.com/Azure/ARO-HCP/hcp-recovery/pkg/generated/clientset/versioned/fake"
	recoveryinformers "github.com/Azure/ARO-HCP/hcp-recovery/pkg/generated/informers/externalversions"
	"github.com/Azure/ARO-HCP/internal/backup"
	"github.com/Azure/ARO-HCP/internal/controllerutils"
)

const (
	testEtcdImage            = "quay.io/openshift/etcd:test"
	testWorkerImage          = "mcr.microsoft.com/aks/command/runtime:test"
	testWorkerServiceAccount = "hcp-recovery-worker"
	testEtcdReplicas         = int32(3)
)

var testNow = time.Date(2026, time.September, 29, 12, 0, 0, 0, time.UTC)

type recoveryTest struct {
	*RecoveryController
	t                     *testing.T
	ctx                   context.Context
	cancel                context.CancelFunc
	recovery              *hcprecoveryv1alpha1.HCPRecovery
	backup                *unstructured.Unstructured
	hc                    *hypershiftv1beta1.HostedCluster
	hcp                   *hypershiftv1beta1.HostedControlPlane
	controlPlaneNamespace *corev1.Namespace
	sts                   *appsv1.StatefulSet
	deployment            *appsv1.Deployment
	client                *recoveryfake.Clientset
	hcClient              *hypershiftfake.Clientset
	kubeClient            *kubefake.Clientset
	dynamicClient         *dynamicfake.FakeDynamicClient
	informers             map[string]cache.SharedIndexInformer
	updates               []*hcprecoveryv1alpha1.HCPRecovery
	writeErr              error
}

func newRecoveryTest(t *testing.T, configure func(*recoveryTest)) *recoveryTest {
	t.Helper()
	const resourceID = "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.RedHatOpenShift/hcpOpenShiftClusters/cluster"
	startedAt := metav1.NewTime(testNow.Add(-time.Hour))
	f := &recoveryTest{t: t}
	f.recovery = &hcprecoveryv1alpha1.HCPRecovery{
		ObjectMeta: metav1.ObjectMeta{Name: "recovery", Namespace: "custom-recovery", UID: "recovery-uid", ResourceVersion: "1", Generation: 1},
		Spec: hcprecoveryv1alpha1.HCPRecoverySpec{
			HcpClusterAzureResourceID: resourceID,
			HostedClusterName:         "cluster",
			HostedClusterNamespace:    "hosted-cluster",
			ControlPlaneNamespace:     "hosted-cluster-cluster",
			VeleroBackupName:          "backup",
		},
		Status: hcprecoveryv1alpha1.HCPRecoveryStatus{
			Phase:       hcprecoveryv1alpha1.RestoreStatePreparing,
			StartedAt:   &startedAt,
			RestoreName: "preserve-restore",
			Conditions: []metav1.Condition{{
				Type: "Unrelated", Status: metav1.ConditionTrue,
				Reason: "Preserve", Message: "Preserve this condition",
				ObservedGeneration: 1, LastTransitionTime: startedAt,
			}},
			EtcdRuntimeDetails: &hcprecoveryv1alpha1.EtcdRuntimeDetails{
				Image: testEtcdImage, RunAsUser: ptr.To[int64](1000), FSGroup: ptr.To[int64](2000),
			},
		},
	}
	f.backup = &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "velero.io/v1",
		"kind":       "Backup",
		"metadata": map[string]interface{}{
			"name":      "backup",
			"namespace": backup.VeleroNamespace,
			"annotations": map[string]interface{}{
				controllerutils.HcpClusterAzureResourceIdAnnotation: resourceID,
			},
		},
		"spec": map[string]interface{}{
			"includedNamespaces": []interface{}{f.recovery.Spec.HostedClusterNamespace, f.recovery.Spec.ControlPlaneNamespace},
		},
		"status": map[string]interface{}{"phase": "Completed"},
	}}
	f.hc = &hypershiftv1beta1.HostedCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cluster", Namespace: f.recovery.Spec.HostedClusterNamespace, UID: "cluster-uid", ResourceVersion: "2",
			Annotations: map[string]string{controllerutils.HcpClusterAzureResourceIdAnnotation: resourceID},
		},
		Spec: hypershiftv1beta1.HostedClusterSpec{Release: hypershiftv1beta1.Release{Image: "quay.io/openshift/release:test"}},
	}
	f.controlPlaneNamespace = &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:        f.recovery.Spec.ControlPlaneNamespace,
			Annotations: map[string]string{controllerutils.HcpClusterAzureResourceIdAnnotation: resourceID},
		},
	}
	f.hcp = &hypershiftv1beta1.HostedControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name: f.hc.Name, Namespace: f.recovery.Spec.ControlPlaneNamespace, Generation: 1, ResourceVersion: "1",
		},
		Spec: hypershiftv1beta1.HostedControlPlaneSpec{PausedUntil: ptr.To("true")},
		Status: hypershiftv1beta1.HostedControlPlaneStatus{
			Conditions: []metav1.Condition{{
				Type: string(hypershiftv1beta1.ReconciliationActive), Status: metav1.ConditionFalse,
				Reason: hypershiftv1beta1.ReconciliationPausedConditionReason, ObservedGeneration: 1,
				LastTransitionTime: startedAt,
			}},
		},
	}
	f.sts = &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "etcd", Namespace: f.recovery.Spec.ControlPlaneNamespace},
		Spec: appsv1.StatefulSetSpec{
			Replicas: ptr.To(testEtcdReplicas),
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "etcd", Image: testEtcdImage}},
				SecurityContext: &corev1.PodSecurityContext{
					RunAsUser: ptr.To[int64](1000), FSGroup: ptr.To[int64](2000),
				},
				NodeSelector:      map[string]string{"workload": "control-plane"},
				Tolerations:       []corev1.Toleration{{Key: "control-plane", Operator: corev1.TolerationOpExists}},
				PriorityClassName: "hypershift-control-plane",
			}},
		},
	}
	f.deployment = &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "kube-apiserver", Namespace: f.recovery.Spec.ControlPlaneNamespace},
		Spec:       appsv1.DeploymentSpec{Replicas: ptr.To[int32](3)},
		Status:     appsv1.DeploymentStatus{Replicas: 3},
	}
	if configure != nil {
		configure(f)
	}
	f.ctx, f.cancel = context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(f.cancel)
	f.client = recoveryfake.NewSimpleClientset(f.recovery)
	f.hcClient = hypershiftfake.NewSimpleClientset(f.hc, f.hcp)
	backupResource := schema.GroupVersionResource{Group: "velero.io", Version: "v1", Resource: "backups"}
	restoreResource := schema.GroupVersionResource{Group: "velero.io", Version: "v1", Resource: "restores"}
	f.dynamicClient = dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{backupResource: "BackupList", restoreResource: "RestoreList"}, f.backup)
	f.kubeClient = kubefake.NewSimpleClientset(f.sts, f.deployment, f.controlPlaneNamespace)
	kubeFactory := kubeinformers.NewSharedInformerFactory(f.kubeClient, 0)
	recoveryFactory := recoveryinformers.NewSharedInformerFactory(f.client, 0)
	hypershiftFactory := hypershiftinformers.NewSharedInformerFactory(f.hcClient, 0)
	dynamicFactory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(f.dynamicClient, 0, backup.VeleroNamespace, nil)
	hcInformer := hypershiftFactory.Hypershift().V1beta1().HostedClusters()
	hcpInformer := hypershiftFactory.Hypershift().V1beta1().HostedControlPlanes()
	recoveryInformer := recoveryFactory.Hcprecovery().V1alpha1().HCPRecoveries()
	backupInformer := dynamicFactory.ForResource(backupResource)
	stsInformer := kubeFactory.Apps().V1().StatefulSets()
	deploymentInformer := kubeFactory.Apps().V1().Deployments()
	podInformer := kubeFactory.Core().V1().Pods()
	pvcInformer := kubeFactory.Core().V1().PersistentVolumeClaims()
	restoreInformer := dynamicFactory.ForResource(restoreResource)
	var err error
	f.RecoveryController, err = NewRecoveryController(
		f.client, f.hcClient, f.kubeClient, f.dynamicClient.Resource(restoreResource).Namespace(backup.VeleroNamespace),
		stsInformer, deploymentInformer, podInformer,
		pvcInformer, hcInformer, hcpInformer, recoveryInformer, restoreInformer, backupInformer,
		testWorkerImage, testWorkerServiceAccount, 1,
	)
	if err != nil {
		t.Fatal(err)
	}
	f.TimeNow = func() time.Time { return testNow }
	t.Cleanup(f.workqueue.ShutDown)
	f.informers = map[string]cache.SharedIndexInformer{
		"HostedCluster": hcInformer.Informer(), "HCPRecovery": recoveryInformer.Informer(),
		"HostedControlPlane": hcpInformer.Informer(),
		"Backup":             backupInformer.Informer(),
		"StatefulSet":        stsInformer.Informer(), "Restore": restoreInformer.Informer(),
		"Deployment":            deploymentInformer.Informer(),
		"Pod":                   podInformer.Informer(),
		"PersistentVolumeClaim": pvcInformer.Informer(),
	}
	for kind, obj := range map[string]runtime.Object{
		"HCPRecovery": f.recovery, "Backup": f.backup, "HostedCluster": f.hc, "HostedControlPlane": f.hcp, "StatefulSet": f.sts,
		"Deployment": f.deployment,
	} {
		if err := f.informers[kind].GetStore().Add(obj); err != nil {
			t.Fatal(err)
		}
		original := obj.DeepCopyObject()
		t.Cleanup(func() {
			if !reflect.DeepEqual(obj, original) {
				t.Errorf("reconciliation mutated cached %s:\n got: %#v\nwant: %#v", kind, obj, original)
			}
		})
	}
	f.client.PrependReactor("patch", "hcprecoveries", func(action clienttesting.Action) (bool, runtime.Object, error) {
		assertStatusPatch(t, action)
		var update hcprecoveryv1alpha1.HCPRecovery
		if err := json.Unmarshal(action.(clienttesting.PatchAction).GetPatch(), &update); err != nil {
			t.Fatal(err)
		}
		f.updates = append(f.updates, &update)
		return f.writeErr != nil, nil, f.writeErr
	})
	return f
}

func (f *recoveryTest) configureAzureCMK() {
	activeKey := hypershiftv1beta1.AzureKMSKey{
		KeyVaultName: "test-vault", KeyName: "test-key", KeyVersion: "test-version",
	}
	f.hc.Spec.SecretEncryption = &hypershiftv1beta1.SecretEncryptionSpec{
		Type: hypershiftv1beta1.KMS,
		KMS: &hypershiftv1beta1.KMSSpec{
			Provider: hypershiftv1beta1.AZURE,
			Azure:    &hypershiftv1beta1.AzureKMSSpec{ActiveKey: activeKey},
		},
	}
	f.hc.Status.SecretEncryption.ActiveKey = hypershiftv1beta1.SecretEncryptionKeyStatus{
		Provider: hypershiftv1beta1.SecretEncryptionProviderAzure,
		Azure:    activeKey,
	}
	annotations := f.backup.GetAnnotations()
	annotations[controllerutils.HcpClusterKmsKeyFingerprintAnnotation] = backup.ActiveKMSKeyFingerprint(f.hc)
	f.backup.SetAnnotations(annotations)
}

func (f *recoveryTest) requireStatusUpdate(acted bool, err error) *hcprecoveryv1alpha1.HCPRecovery {
	f.t.Helper()
	if err != nil || !acted || len(f.updates) != 1 {
		f.t.Fatalf("expected one status update, got acted=%v updates=%v err=%v", acted, f.updates, err)
	}
	update := f.updates[0]
	f.updates = nil
	return update
}

// runStep executes a converted step and performs the status write runPhase would, so the
// assertions here can keep looking at client actions and recorded updates.
func (f *recoveryTest) runStep(step phaseStep, recovery *hcprecoveryv1alpha1.HCPRecovery) (stepResult, error) {
	f.t.Helper()
	result, err := step(f.ctx, recovery)
	if err != nil {
		return result, err
	}
	if result.statusUpdate != nil {
		return result, f.applyStatus(f.ctx, recovery, result.statusUpdate)
	}
	return result, result.err
}

// requireStepStatusUpdate asserts a converted step stopped the pipeline and wrote status
// exactly once.
func (f *recoveryTest) requireStepStatusUpdate(result stepResult, err error) *hcprecoveryv1alpha1.HCPRecovery {
	f.t.Helper()
	if result.outcome == stepContinue {
		f.t.Fatalf("expected the step to stop the pipeline, got %v", result.outcome)
	}
	return f.requireStatusUpdate(true, err)
}

func (f *recoveryTest) startInformers(names ...string) {
	f.t.Helper()
	// The generated HyperShift fake does not support watch-list initialization.
	clientfeaturestesting.SetFeatureDuringTest(f.t, clientfeatures.WatchListClient, false)
	if len(names) == 0 {
		for name := range f.informers {
			names = append(names, name)
		}
	}
	ctx, cancel := context.WithCancel(f.ctx)
	var wg sync.WaitGroup
	var synced []cache.InformerSynced
	for _, name := range names {
		informer := f.informers[name]
		// Let the initial list populate the cache and exercise add handlers.
		if err := informer.GetStore().Replace(nil, ""); err != nil {
			f.t.Fatal(err)
		}
		synced = append(synced, informer.HasSynced)
		wg.Add(1)
		go func() {
			defer wg.Done()
			informer.Run(ctx.Done())
		}()
	}
	f.t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
	if !cache.WaitForCacheSync(ctx.Done(), synced...) {
		f.t.Fatalf("failed to sync informer caches: %v", names)
	}
}

func (f *recoveryTest) waitFor(message string, condition func() bool) {
	f.t.Helper()
	if err := wait.PollUntilContextCancel(f.ctx, time.Millisecond, true, func(context.Context) (bool, error) {
		return condition(), nil
	}); err != nil {
		f.t.Fatalf("%s: %v", message, err)
	}
}

func assertStatusPatch(t *testing.T, action clienttesting.Action) {
	t.Helper()
	patch, ok := action.(clienttesting.PatchActionImpl)
	if !ok || patch.GetPatchType() != types.ApplyPatchType || patch.GetSubresource() != "status" {
		t.Fatalf("expected status apply patch, got %#v", action)
	}
	opts := patch.GetPatchOptions()
	if opts.FieldManager != fieldManager || (opts.Force != nil && *opts.Force) {
		t.Fatalf("expected named field manager without forcing ownership: %#v", opts)
	}
	var update recoveryapply.HCPRecoveryApplyConfiguration
	if err := json.Unmarshal(patch.GetPatch(), &update); err != nil {
		t.Fatal(err)
	}
	if update.UID == nil || *update.UID == "" || update.ResourceVersion == nil || *update.ResourceVersion == "" {
		t.Fatalf("missing concurrency preconditions: %#v", update.ObjectMetaApplyConfiguration)
	}
	if update.Spec != nil || update.Status == nil {
		t.Fatalf("apply must only include status fields: %s", patch.GetPatch())
	}
}

type stepFailureExpectation struct {
	conditionType string
	reason        string
	messageParts  []string
	permanent     bool
	retryError    func(error) bool
}

func (f *recoveryTest) assertStepFailureContract(step phaseStep, want stepFailureExpectation) {
	f.t.Helper()
	recovery := f.recovery
	original := recovery.DeepCopy()
	result, err := f.runStep(step, recovery)
	update := f.requireStepStatusUpdate(result, err)
	wantOutcome := stepRetry
	if want.permanent {
		wantOutcome = stepStop
	}
	if result.outcome != wantOutcome {
		f.t.Fatalf("expected outcome %v, got %v", wantOutcome, result.outcome)
	}
	if len(f.client.Actions()) != 1 {
		f.t.Fatalf("expected one failure status patch, got %v", f.client.Actions())
	}
	condition := meta.FindStatusCondition(update.Status.Conditions, want.conditionType)
	if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != want.reason ||
		condition.ObservedGeneration != recovery.Generation || !condition.LastTransitionTime.Time.Equal(testNow) {
		f.t.Fatalf("unexpected failure condition: %#v", condition)
	}
	for _, part := range want.messageParts {
		if !strings.Contains(condition.Message, part) {
			f.t.Fatalf("expected failure message to contain %q, got %q", part, condition.Message)
		}
	}
	wantStatus := original.DeepCopy().Status
	meta.RemoveStatusCondition(&wantStatus.Conditions, want.conditionType)
	wantStatus.Conditions = append([]metav1.Condition{*condition}, wantStatus.Conditions...)
	if want.permanent {
		// The spec is immutable, so a permanent failure cannot be corrected on the
		// object: it ends the recovery from whatever phase it is in.
		wantStatus.Phase = hcprecoveryv1alpha1.RestoreStateFailed
		completedAt := metav1.NewTime(testNow)
		wantStatus.CompletedAt = &completedAt
	}
	if !apiequality.Semantic.DeepEqual(update.Status, wantStatus) {
		f.t.Fatalf("unexpected failure status:\n got: %#v\nwant: %#v", update.Status, wantStatus)
	}
	if !reflect.DeepEqual(recovery, original) {
		f.t.Fatal("failure mutated cached recovery")
	}
	if !want.permanent {
		// A retryable failure whose condition is already recorded has nothing to write.
		// It must still stop the pipeline and surface its error for a rate-limited
		// retry: reporting stepContinue here would let the next step run on an unmet
		// precondition, and for the last step in a phase that means advancing.
		recovery.Status = update.Status
		f.client.ClearActions()
		result, err := f.runStep(step, recovery)
		if result.outcome != stepRetry || len(f.client.Actions()) != 0 || !want.retryError(err) {
			f.t.Fatalf("expected unchanged failure to retry without a status write, got outcome=%v err=%v actions=%v",
				result.outcome, err, f.client.Actions())
		}
	}
	recovery.Status = original.Status
	f.writeErr = apierrors.NewConflict(hcprecoveryv1alpha1.Resource("hcprecoveries"), recovery.Name, errors.New("status conflict"))
	if result, err := f.runStep(step, recovery); result.outcome == stepContinue || !errors.Is(err, f.writeErr) {
		f.t.Fatalf("expected status error to propagate, got outcome=%v err=%v", result.outcome, err)
	}
}

func (f *recoveryTest) cacheVeleroRestore(phase velerov1.RestorePhase) {
	f.t.Helper()
	restore := &unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{"name": f.recovery.Status.RestoreName, "namespace": backup.VeleroNamespace},
		"status":   map[string]interface{}{"phase": string(phase)},
	}}
	if err := f.informers["Restore"].GetStore().Add(restore); err != nil {
		f.t.Fatal(err)
	}
}

// addPods puts pods in the informer cache and the fake API server, so steps that read
// from the lister and then confirm against the API server see a consistent world.
func (f *recoveryTest) addPods(pods ...*corev1.Pod) {
	f.t.Helper()
	for _, pod := range pods {
		if err := f.informers["Pod"].GetStore().Add(pod); err != nil {
			f.t.Fatal(err)
		}
		if _, err := f.kubeClient.CoreV1().Pods(pod.Namespace).Create(f.ctx, pod, metav1.CreateOptions{}); err != nil {
			f.t.Fatal(err)
		}
	}
	f.kubeClient.ClearActions()
}

// workerPod builds one of the recovery's worker pods in a given phase. terminated is
// the container's termination message: the script's own result when it succeeded, or
// the tail of its logs when it failed.
func (f *recoveryTest) workerPod(name, app, namespace string, phase corev1.PodPhase, terminated string) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"app":                             app,
				recoveryNameLabel:                 f.recovery.Name,
				restoreControlPlaneNamespaceLabel: f.recovery.Spec.ControlPlaneNamespace,
			},
		},
		Status: corev1.PodStatus{Phase: phase},
	}
	switch phase {
	case corev1.PodRunning:
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	case corev1.PodSucceeded, corev1.PodFailed:
		exitCode := int32(0)
		if phase == corev1.PodFailed {
			exitCode = 1
		}
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name: workerContainerName,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: exitCode, Message: terminated,
			}},
		}}
	}
	return pod
}

// etcdPod builds a member pod of the real etcd StatefulSet.
func (f *recoveryTest) etcdPod(index int32, ready bool) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      etcdMemberName(index),
			Namespace: f.recovery.Spec.ControlPlaneNamespace,
			Labels:    map[string]string{"app": etcd},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}
	return pod
}

// createdPods returns the pods the step under test asked the API server to create.
func (f *recoveryTest) createdPods() []*corev1.Pod {
	f.t.Helper()
	var created []*corev1.Pod
	for _, action := range f.kubeClient.Actions() {
		create, ok := action.(clienttesting.CreateAction)
		if !ok || action.GetResource().Resource != "pods" || action.GetSubresource() != "" {
			continue
		}
		pod, ok := create.GetObject().(*corev1.Pod)
		if !ok {
			f.t.Fatalf("expected a *corev1.Pod create, got %T", create.GetObject())
		}
		created = append(created, pod)
	}
	return created
}

// deletedPodNames returns the pods the step under test asked the API server to delete.
func (f *recoveryTest) deletedPodNames() []string {
	f.t.Helper()
	var deleted []string
	for _, action := range f.kubeClient.Actions() {
		del, ok := action.(clienttesting.DeleteAction)
		if !ok || action.GetResource().Resource != "pods" {
			continue
		}
		deleted = append(deleted, del.GetName())
	}
	return deleted
}

type failingHostedClusterLister struct {
	hypershiftlisters.HostedClusterNamespaceLister
	err error
}

func (l failingHostedClusterLister) HostedClusters(string) hypershiftlisters.HostedClusterNamespaceLister {
	return l
}

func (l failingHostedClusterLister) Get(string) (*hypershiftv1beta1.HostedCluster, error) {
	return nil, l.err
}
