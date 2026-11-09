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
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	applyv1 "k8s.io/client-go/applyconfigurations/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/informers"
	appsinformers "k8s.io/client-go/informers/apps/v1"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	appslisters "k8s.io/client-go/listers/apps/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	hypershiftclient "github.com/openshift/hypershift/client/clientset/clientset"
	"github.com/openshift/hypershift/client/informers/externalversions/hypershift/v1beta1"
	hypershiftlisters "github.com/openshift/hypershift/client/listers/hypershift/v1beta1"

	hcprecoveryv1alpha1 "github.com/Azure/ARO-HCP/hcp-recovery/pkg/apis/hcprecovery/v1alpha1"
	recoveryapply "github.com/Azure/ARO-HCP/hcp-recovery/pkg/generated/applyconfiguration/hcprecovery/v1alpha1"
	recoveryclient "github.com/Azure/ARO-HCP/hcp-recovery/pkg/generated/clientset/versioned"
	v1alpha1informers "github.com/Azure/ARO-HCP/hcp-recovery/pkg/generated/informers/externalversions/hcprecovery/v1alpha1"
	hcprecoverylisters "github.com/Azure/ARO-HCP/hcp-recovery/pkg/generated/listers/hcprecovery/v1alpha1"
	"github.com/Azure/ARO-HCP/internal/utils"
)

const (
	RecoveryControllerName = "HCPRecovery"
	fieldManager           = "hcp-recovery"
)

type RecoveryController struct {
	name              string
	hcpRecoveryClient recoveryclient.Interface
	hypershiftClient  hypershiftclient.Interface
	kubeClientset     kubernetes.Interface
	restoreClient     dynamic.ResourceInterface
	// workerImage runs the pods that drive work inside other pods, so it must carry
	// kubectl and jq. workerServiceAccount is deliberately separate from the
	// controller's own: it is the only identity granted pods/exec.
	workerImage          string
	workerServiceAccount string

	statefulsetLister        appslisters.StatefulSetLister
	deploymentLister         appslisters.DeploymentLister
	podLister                corelisters.PodLister
	pvcLister                corelisters.PersistentVolumeClaimLister
	hcpRecoveryIndexer       cache.Indexer
	hypershiftLister         hypershiftlisters.HostedClusterLister
	hostedControlPlaneLister hypershiftlisters.HostedControlPlaneLister
	restoreLister            cache.GenericLister
	backupLister             cache.GenericLister
	hcpRecoveryLister        hcprecoverylisters.HCPRecoveryLister
	workqueue                workqueue.TypedRateLimitingInterface[string]
	hasSynced                []cache.InformerSynced
	workers                  int
	TimeNow                  func() time.Time
}

func NewRecoveryController(
	hcpRecoveryClient recoveryclient.Interface,
	hypershiftClient hypershiftclient.Interface,
	kubeClientset kubernetes.Interface,
	restoreClient dynamic.ResourceInterface,
	statefulsetInformer appsinformers.StatefulSetInformer,
	deploymentInformer appsinformers.DeploymentInformer,
	podInformer coreinformers.PodInformer,
	pvcInformer coreinformers.PersistentVolumeClaimInformer,
	hypershiftInformer v1beta1.HostedClusterInformer,
	hostedControlPlaneInformer v1beta1.HostedControlPlaneInformer,
	hcpRecoveryInformer v1alpha1informers.HCPRecoveryInformer,
	restoreDynamicInformer informers.GenericInformer,
	backupDynamicInformer informers.GenericInformer,
	workerImage string,
	workerServiceAccount string,
	workers int,
) (*RecoveryController, error) {
	c := &RecoveryController{
		name:                 RecoveryControllerName,
		workers:              workers,
		hcpRecoveryClient:    hcpRecoveryClient,
		hypershiftClient:     hypershiftClient,
		kubeClientset:        kubeClientset,
		restoreClient:        restoreClient,
		workerImage:          workerImage,
		workerServiceAccount: workerServiceAccount,

		deploymentLister:         deploymentInformer.Lister(),
		podLister:                podInformer.Lister(),
		pvcLister:                pvcInformer.Lister(),
		hcpRecoveryIndexer:       hcpRecoveryInformer.Informer().GetIndexer(),
		statefulsetLister:        statefulsetInformer.Lister(),
		hypershiftLister:         hypershiftInformer.Lister(),
		hostedControlPlaneLister: hostedControlPlaneInformer.Lister(),
		hcpRecoveryLister:        hcpRecoveryInformer.Lister(),
		restoreLister:            restoreDynamicInformer.Lister(),
		backupLister:             backupDynamicInformer.Lister(),
		TimeNow:                  time.Now,
		workqueue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.DefaultTypedControllerRateLimiter[string](),
			workqueue.TypedRateLimitingQueueConfig[string]{
				Name: RecoveryControllerName,
			},
		),
	}
	if err := c.wireInformers(
		statefulsetInformer.Informer(),
		deploymentInformer.Informer(),
		podInformer.Informer(),
		pvcInformer.Informer(),
		hypershiftInformer.Informer(),
		hostedControlPlaneInformer.Informer(),
		hcpRecoveryInformer.Informer(),
		restoreDynamicInformer.Informer(),
		backupDynamicInformer.Informer(),
	); err != nil {
		c.workqueue.ShutDown()
		return nil, err
	}

	return c, nil
}

func (c *RecoveryController) runWorker(ctx context.Context) {
	for c.processNextWorkItem(ctx) {
	}
}

func (c *RecoveryController) processNextWorkItem(ctx context.Context) bool {
	key, shutdown := c.workqueue.Get()
	if shutdown {
		return false
	}
	defer c.workqueue.Done(key)
	err := c.syncHandler(ctx, key)
	if err == nil {
		c.workqueue.Forget(key)
		return true
	}

	utilruntime.HandleError(fmt.Errorf("error syncing HCPRecovery %q: %w", key, err))

	c.workqueue.AddRateLimited(key)
	return true
}

func (c *RecoveryController) syncHandler(ctx context.Context, key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return fmt.Errorf("invalid key %q: %w", key, err)
	}

	logger := klog.FromContext(ctx).WithValues("namespace", namespace, "name", name)
	ctx = klog.NewContext(ctx, logger)

	recovery, err := c.hcpRecoveryLister.HCPRecoveries(namespace).Get(name)
	if apierrors.IsNotFound(err) {
		logger.V(4).Info("HCPRecovery deleted")
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to get HCPRecovery %q: %w", key, err)
	}

	return c.reconcile(ctx, recovery)
}

// stepOutcome tells the phase runner what to do after a step returns. A bool cannot
// distinguish "stop" from "stop and retry": a retryable step whose condition was already
// recorded had nothing to write, so it reported false, and the runner read that as "this
// step passed, run the next one".
type stepOutcome int

const (
	// stepContinue means the step's goal already holds. Run the next step.
	stepContinue stepOutcome = iota
	// stepStop means the step acted on the cluster, or the recovery ended. Flush any
	// status and wait for the watch event that action produces.
	stepStop
	// stepRetry means the step's precondition is not met. Flush any status; if there was
	// none to flush, surface err so the workqueue retries with backoff.
	stepRetry
)

func (o stepOutcome) String() string {
	switch o {
	case stepContinue:
		return "Continue"
	case stepStop:
		return "Stop"
	case stepRetry:
		return "Retry"
	default:
		return fmt.Sprintf("stepOutcome(%d)", int(o))
	}
}

type stepResult struct {
	// statusUpdate is non-nil only when status actually changed. AsApplyConfiguration
	// always returns a configuration, so the constructors below drop it when nothing
	// changed — otherwise this field is never nil and cannot gate the write.
	statusUpdate *recoveryapply.HCPRecoveryApplyConfiguration
	outcome      stepOutcome
	// err is carried by stepRetry, for the case where no status write will produce a
	// watch event to requeue us.
	err error
}

// satisfied records that a step's goal is met. If that is news, write it and let the
// resulting watch event drive the next pass; if status already said so, fall through to
// the next step.
func satisfied(cfg *recoveryapply.HCPRecoveryApplyConfiguration, changed bool) stepResult {
	if !changed {
		return stepResult{outcome: stepContinue}
	}
	return stepResult{statusUpdate: cfg, outcome: stepStop}
}

// yield stops the pass without writing status: the step either changed the cluster (a
// patch, a delete) or is waiting on something that already has a watch. Either way an
// event will requeue us.
func yield() stepResult {
	return stepResult{outcome: stepStop}
}

// waiting is yield with an explanation: the step is parked on something a watch will
// report, and records why if that is news. Unlike retryableError it carries no error,
// because nothing needs a rate-limited retry to get us back here.
func waiting(cfg *recoveryapply.HCPRecoveryApplyConfiguration, changed bool) stepResult {
	if !changed {
		return yield()
	}
	return stepResult{statusUpdate: cfg, outcome: stepStop}
}

// proceed records that a step has nothing to do and nothing to say. Run the next step.
func proceed() stepResult {
	return stepResult{outcome: stepContinue}
}

type phaseStep func(ctx context.Context, recovery *hcprecoveryv1alpha1.HCPRecovery) (stepResult, error)

// runPhase runs steps in order until one stops the pipeline, then performs the single
// status write for the pass. When every step passes the phase advances to next: the
// advance belongs to the call site, never to a step, so reconcile reads as the whole
// restore.
func (c *RecoveryController) runPhase(ctx context.Context, recovery *hcprecoveryv1alpha1.HCPRecovery,
	next hcprecoveryv1alpha1.RestoreState, steps ...phaseStep) error {
	for _, step := range steps {
		result, err := step(ctx, recovery)
		if err != nil {
			return err
		}
		if result.outcome == stepContinue {
			continue
		}
		if result.statusUpdate != nil {
			// The write produces a watch event that requeues us, so a stepRetry does not
			// also need to return its error.
			return c.applyStatus(ctx, recovery, result.statusUpdate)
		}
		// Nothing was written, so no status change will wake us. stepRetry's error is
		// what requeues; stepStop carries a nil err and relies on the watch event from
		// whatever it did.
		return result.err
	}
	status := NewStatus(recovery.Status).WithPhase(next)
	if next == hcprecoveryv1alpha1.RestoreStateCompleted {
		// The recovery is over when the phase says so, so the timestamp goes with the
		// write that says it — as it does for the terminal failure in permanentError.
		status = status.WithCompletedAt(metav1.NewTime(c.TimeNow()))
	}
	statusUpdate, needsUpdate := status.AsApplyConfiguration(recovery)
	if !needsUpdate {
		return nil
	}
	return c.applyStatus(ctx, recovery, statusUpdate)
}

func (c *RecoveryController) reconcile(ctx context.Context, recovery *hcprecoveryv1alpha1.HCPRecovery) error {
	switch recovery.Status.Phase {
	case "":
		statusUpdate, _ := NewStatus(recovery.Status).
			WithPhase(hcprecoveryv1alpha1.RestoreStatePreparing).
			WithStartedAt(metav1.NewTime(c.TimeNow())).
			AsApplyConfiguration(recovery)
		return c.applyStatus(ctx, recovery, statusUpdate)
	case hcprecoveryv1alpha1.RestoreStatePreparing:
		return c.runPhase(ctx, recovery, hcprecoveryv1alpha1.RestoreStateRestoring,
			c.validateRecoveryInputs,
			c.collectEtcdInformation,
			c.pauseHostedCluster,
			c.scaleDownKubeAPIServer,
			c.scaleDownEtcdBeforeRestore,
			c.deleteEtcdPVCs,
			c.validateScaledDown,
		)
	case hcprecoveryv1alpha1.RestoreStateRestoring:
		return c.runPhase(ctx, recovery, hcprecoveryv1alpha1.RestoreStateValidating,
			c.createVeleroRestoreStep,
			c.waitForVeleroRestoreCompletion,
			c.scaleDownEtcdAfterRestore,
			c.validateEtcdPodsGone,
			c.createEtcdHolderPods,
			c.waitForHolderPodsReady,
			c.createSnapshotTransferPod,
			c.waitForSnapshotTransfer,
			c.deleteDistributionPods,
			c.createEtcdRestorePods,
			c.waitForEtcdRestorePods,
			c.deleteEtcdRestorePods,
		)
	case hcprecoveryv1alpha1.RestoreStateValidating:
		return c.runPhase(ctx, recovery, hcprecoveryv1alpha1.RestoreStateCompleted,
			c.scaleUpEtcd,
			c.waitForEtcdPodsReady,
			c.createEtcdHealthCheckPod,
			c.waitForEtcdHealthCheck,
			c.deleteEtcdHealthCheckPod,
			c.unpauseHostedCluster,
			c.validateHostedClusterHealthy,
		)
	case hcprecoveryv1alpha1.RestoreStateCompleted, hcprecoveryv1alpha1.RestoreStateFailed:
		klog.FromContext(ctx).V(4).Info("Skipping reconciliation, recovery is in terminal phase", "phase", recovery.Status.Phase)
		return nil
	default:
		return fmt.Errorf("unknown recovery phase %q", recovery.Status.Phase)
	}
}

func (c *RecoveryController) Run(ctx context.Context) error {
	defer utilruntime.HandleCrash()
	defer c.workqueue.ShutDown()

	ctx = utils.ContextWithControllerName(ctx, c.name)
	logger := klog.FromContext(ctx).WithValues(utils.LogValues{}.AddControllerName(c.name)...)
	ctx = klog.NewContext(ctx, logger)
	logger.Info("Starting HCP Recovery controller")

	logger.Info("Waiting for informer caches to sync")
	if ok := cache.WaitForCacheSync(ctx.Done(), c.hasSynced...); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	logger.Info("Starting workers", "count", c.workers)

	for i := 0; i < c.workers; i++ {
		go wait.UntilWithContext(ctx, c.runWorker, time.Second)
	}

	<-ctx.Done()
	return nil
}

func (c *RecoveryController) applyStatus(ctx context.Context, recovery *hcprecoveryv1alpha1.HCPRecovery, statusUpdate *recoveryapply.HCPRecoveryApplyConfiguration) error {
	if statusUpdate.Status != nil && statusUpdate.Status.Phase != nil {
		from, to := recovery.Status.Phase, *statusUpdate.Status.Phase
		if !validPhaseTransition(from, to) {
			return fmt.Errorf("invalid recovery phase transition %q -> %q", from, to)
		}
	}
	_, err := c.hcpRecoveryClient.HcprecoveryV1alpha1().HCPRecoveries(recovery.Namespace).ApplyStatus(ctx, statusUpdate, metav1.ApplyOptions{FieldManager: fieldManager})
	return err
}

// validPhaseTransition mirrors the CEL rule on the CRD. Both exist: the CEL rule is the
// enforcement, this catches a bad transition before it is sent and names the phases in
// the error.
func validPhaseTransition(from, to hcprecoveryv1alpha1.RestoreState) bool {
	if from == to {
		return true
	}
	if to == hcprecoveryv1alpha1.RestoreStateFailed {
		return true
	}
	switch from {
	case "":
		return to == hcprecoveryv1alpha1.RestoreStatePreparing
	case hcprecoveryv1alpha1.RestoreStatePreparing:
		return to == hcprecoveryv1alpha1.RestoreStateRestoring
	case hcprecoveryv1alpha1.RestoreStateRestoring:
		return to == hcprecoveryv1alpha1.RestoreStateValidating
	case hcprecoveryv1alpha1.RestoreStateValidating:
		return to == hcprecoveryv1alpha1.RestoreStateCompleted
	default:
		return false
	}
}

// permanentError ends the recovery from whatever phase it is in. During preparation the
// failure is a rejected request — the spec is immutable, so a backup that does not exist
// or a target that does not match cannot be corrected on the object. From the Velero
// restore onwards the recovery is rewriting etcd's volumes and cannot resume safely from
// a half-applied step. Neither is recoverable, so both report Failed.
//
// It stops the pipeline whether or not status changed: a recovery that has already
// recorded its failure must not keep running steps.
func (c *RecoveryController) permanentError(recovery *hcprecoveryv1alpha1.HCPRecovery, condition *applyv1.ConditionApplyConfiguration) stepResult {
	cfg, changed := NewStatus(recovery.Status).WithConditions(condition).
		WithPhase(hcprecoveryv1alpha1.RestoreStateFailed).
		WithCompletedAt(metav1.NewTime(c.TimeNow())).
		AsApplyConfiguration(recovery)

	result := stepResult{outcome: stepStop}
	if changed {
		result.statusUpdate = cfg
	}
	return result
}

// retryableError stops the pipeline and arranges a retry. When the condition is new,
// writing it produces the watch event that requeues us; when it is unchanged there is no
// write and so no event, and err goes back to the workqueue for a rate-limited retry.
// Either way the pipeline stops — a step whose precondition failed must never let the
// next step run.
func (c *RecoveryController) retryableError(recovery *hcprecoveryv1alpha1.HCPRecovery, condition *applyv1.ConditionApplyConfiguration, err error) stepResult {
	cfg, changed := NewStatus(recovery.Status).
		WithConditions(condition).AsApplyConfiguration(recovery)

	result := stepResult{outcome: stepRetry, err: err}
	if changed {
		result.statusUpdate = cfg
	}
	return result
}
