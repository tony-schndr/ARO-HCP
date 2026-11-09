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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"

	hcprecoveryv1alpha1 "github.com/Azure/ARO-HCP/hcp-recovery/pkg/apis/hcprecovery/v1alpha1"
)

const (
	namespaceIndex                    = "hcp-recovery-namespace-index"
	restoreControlPlaneNamespaceLabel = "hcprecovery.aro-hcp.azure.com/control-plane-namespace"
)

func (c *RecoveryController) wireInformers(
	statefulsetInformer cache.SharedIndexInformer,
	deploymentInformer cache.SharedIndexInformer,
	podInformer cache.SharedIndexInformer, // includes the controller's own namespace
	pvcInformer cache.SharedIndexInformer,
	hostedClusterInformer cache.SharedIndexInformer,
	hostedControlPlaneInformer cache.SharedIndexInformer,
	recoveryInformer cache.SharedIndexInformer,
	restoreInformer cache.SharedIndexInformer,
	backupInformer cache.SharedIndexInformer,
) error {
	c.hasSynced = []cache.InformerSynced{
		statefulsetInformer.HasSynced,
		hostedClusterInformer.HasSynced,
		hostedControlPlaneInformer.HasSynced,
		recoveryInformer.HasSynced,
		restoreInformer.HasSynced,
		backupInformer.HasSynced,
		deploymentInformer.HasSynced,
		podInformer.HasSynced,
		pvcInformer.HasSynced,
	}
	if err := recoveryInformer.AddIndexers(cache.Indexers{
		namespaceIndex: func(obj interface{}) ([]string, error) {
			recovery, ok := obj.(*hcprecoveryv1alpha1.HCPRecovery)
			if !ok {
				return nil, fmt.Errorf("expected *hcprecoveryv1alpha1.HCPRecovery got %T", obj)
			}
			return []string{
				recovery.Spec.ControlPlaneNamespace,
				recovery.Spec.HostedClusterNamespace,
			}, nil
		},
	}); err != nil {
		return fmt.Errorf("failed to add hcprecovery indexers: %w", err)
	}

	watches := []struct {
		name     string
		informer cache.SharedIndexInformer
		handler  cache.ResourceEventHandlerFuncs
	}{
		{"Deployment", deploymentInformer, cache.ResourceEventHandlerFuncs{
			AddFunc:    c.enqueueByNamespace,
			UpdateFunc: c.onDeploymentUpdate,
		}},
		{"StatefulSet", statefulsetInformer, cache.ResourceEventHandlerFuncs{
			UpdateFunc: c.onStatefulSetUpdate,
		}},
		{"Pod", podInformer, cache.ResourceEventHandlerFuncs{
			UpdateFunc: c.onPodUpdate,
			DeleteFunc: c.onPodDelete,
		}},
		{"PersistentVolumeClaim", pvcInformer, cache.ResourceEventHandlerFuncs{
			UpdateFunc: c.onPVCUpdate,
			DeleteFunc: c.onPVCDelete,
		}},
		{"HCPRecovery", recoveryInformer, cache.ResourceEventHandlerFuncs{
			AddFunc:    c.enqueueRecovery,
			UpdateFunc: func(_, obj interface{}) { c.enqueueRecovery(obj) },
			DeleteFunc: c.enqueueRecovery,
		}},
		{"HostedCluster", hostedClusterInformer, cache.ResourceEventHandlerFuncs{
			UpdateFunc: func(_, obj interface{}) { c.enqueueByNamespace(obj) },
		}},
		{"HostedControlPlane", hostedControlPlaneInformer, cache.ResourceEventHandlerFuncs{
			AddFunc:    c.enqueueByNamespace,
			UpdateFunc: func(_, obj interface{}) { c.enqueueByNamespace(obj) },
			DeleteFunc: c.onHostedControlPlaneDelete,
		}},
		{"Restore", restoreInformer, cache.ResourceEventHandlerFuncs{
			AddFunc:    c.enqueueRestore,
			UpdateFunc: func(_, obj interface{}) { c.enqueueRestore(obj) },
			DeleteFunc: c.enqueueRestore,
		}},
	}
	for _, watch := range watches {
		if _, err := watch.informer.AddEventHandler(watch.handler); err != nil {
			return fmt.Errorf("failed to add %s event handler: %w", watch.name, err)
		}
	}
	return nil
}

func (c *RecoveryController) enqueueByNamespace(obj interface{}) {
	objectMeta, err := meta.Accessor(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("failed to access event object metadata: %w", err))
		return
	}
	c.enqueueHCPRecoveryByNamespace(objectMeta.GetNamespace())
}

func (c *RecoveryController) onHostedControlPlaneDelete(obj interface{}) {
	if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tombstone.Obj
	}
	c.enqueueByNamespace(obj)
}

func (c *RecoveryController) enqueueRecovery(obj interface{}) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("failed to key HCPRecovery event: %w", err))
		return
	}
	c.workqueue.Add(key)
}

func (c *RecoveryController) enqueueRestore(obj interface{}) {
	if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tombstone.Obj
	}
	objectMeta, err := meta.Accessor(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("failed to access Restore event metadata: %w", err))
		return
	}
	namespace := objectMeta.GetLabels()[restoreControlPlaneNamespaceLabel]
	if namespace == "" {
		return
	}
	c.enqueueHCPRecoveryByNamespace(namespace)
}

func (c *RecoveryController) onDeploymentUpdate(oldObj, newObj interface{}) {
	oldDeployment, ok := oldObj.(*appsv1.Deployment)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("expected *appsv1.Deployment, got %T", oldObj))
		return
	}
	newDeployment, ok := newObj.(*appsv1.Deployment)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("expected *appsv1.Deployment, got %T", newObj))
		return
	}
	if ptr.Deref(oldDeployment.Spec.Replicas, 0) == ptr.Deref(newDeployment.Spec.Replicas, 0) {
		return
	}
	c.enqueueHCPRecoveryByNamespace(newDeployment.Namespace)
}

// onStatefulSetUpdate wakes the recovery when etcd's replica count or observed replicas
// move. Without it, scaling etcd back up would have no event to wait on, which is the
// only reason this informer carries a handler at all.
func (c *RecoveryController) onStatefulSetUpdate(oldObj, newObj interface{}) {
	oldStatefulSet, ok := oldObj.(*appsv1.StatefulSet)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("expected *appsv1.StatefulSet, got %T", oldObj))
		return
	}
	newStatefulSet, ok := newObj.(*appsv1.StatefulSet)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("expected *appsv1.StatefulSet, got %T", newObj))
		return
	}
	if newStatefulSet.Name != etcd {
		return
	}
	if ptr.Deref(oldStatefulSet.Spec.Replicas, 0) == ptr.Deref(newStatefulSet.Spec.Replicas, 0) &&
		oldStatefulSet.Status.Replicas == newStatefulSet.Status.Replicas &&
		oldStatefulSet.Status.ReadyReplicas == newStatefulSet.Status.ReadyReplicas {
		return
	}
	c.enqueueHCPRecoveryByNamespace(newStatefulSet.Namespace)
}

// onPodUpdate routes pod events to the recoveries that care about them. Control-plane
// pods are matched by namespace; the recovery's own worker pods live in the controller's
// namespace, which no recovery indexes, so they are matched by the recovery they name.
func (c *RecoveryController) onPodUpdate(_, newObj interface{}) {
	pod, ok := newObj.(*corev1.Pod)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("expected *corev1.Pod, got %T", newObj))
		return
	}
	switch pod.Labels["app"] {
	case etcd, apiServer, etcdHolderApp, etcdRestoreApp:
		c.enqueueHCPRecoveryByNamespace(pod.Namespace)
	case snapshotTransferApp, etcdHealthCheckApp:
		if name := pod.Labels[recoveryNameLabel]; name != "" {
			c.workqueue.Add(pod.Namespace + "/" + name)
		}
	}
}

func (c *RecoveryController) onPodDelete(obj interface{}) {
	if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tombstone.Obj
	}
	c.onPodUpdate(nil, obj)
}

func (c *RecoveryController) onPVCUpdate(_, newObj interface{}) {
	objMeta, err := meta.Accessor(newObj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("unable to access metadata of %T", newObj))
		return
	}
	c.enqueueHCPRecoveryByNamespace(objMeta.GetNamespace())
}

func (c *RecoveryController) onPVCDelete(obj interface{}) {
	if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tombstone.Obj
	}
	c.onPVCUpdate(nil, obj)
}

func (c *RecoveryController) enqueueHCPRecoveryByNamespace(namespace string) {
	recoveries, err := c.hcpRecoveryIndexer.ByIndex(namespaceIndex, namespace)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("unable to fetch hcprecoveries indexed by namespace %q: %w", namespace, err))
		return
	}

	for _, recovery := range recoveries {
		hcprecovery, ok := recovery.(*hcprecoveryv1alpha1.HCPRecovery)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("error fetching indexed hcprecovery, expected type *hcprecoveriesv1alpha1.HCPRecovery got %T", recovery))
			continue
		}
		c.workqueue.Add(hcprecovery.Namespace + "/" + hcprecovery.Name)
	}
}
