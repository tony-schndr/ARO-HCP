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
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

// Scaling etcd back up has no other wake-up: without a StatefulSet handler the phase
// would patch the scale and then wait for an event that never arrives.
func TestStatefulSetUpdateEnqueuesEtcdChanges(t *testing.T) {
	for _, tt := range []struct {
		name       string
		mutate     func(*appsv1.StatefulSet)
		objectName string
		wantQueued bool
	}{
		{
			name:       "scale up",
			mutate:     func(s *appsv1.StatefulSet) { s.Spec.Replicas = ptr.To[int32](3) },
			wantQueued: true,
		},
		{
			name:       "members become ready",
			mutate:     func(s *appsv1.StatefulSet) { s.Status.ReadyReplicas = 3 },
			wantQueued: true,
		},
		{
			name:       "members observed",
			mutate:     func(s *appsv1.StatefulSet) { s.Status.Replicas = 3 },
			wantQueued: true,
		},
		{
			name:   "unrelated change",
			mutate: func(s *appsv1.StatefulSet) { s.Labels = map[string]string{"churn": "yes"} },
		},
		{
			name:       "another statefulset in the namespace",
			objectName: "kube-apiserver",
			mutate:     func(s *appsv1.StatefulSet) { s.Spec.Replicas = ptr.To[int32](3) },
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newRecoveryTest(t, nil)
			name := tt.objectName
			if name == "" {
				name = etcd
			}
			oldStatefulSet := &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: f.recovery.Spec.ControlPlaneNamespace},
				Spec:       appsv1.StatefulSetSpec{Replicas: ptr.To[int32](0)},
			}
			newStatefulSet := oldStatefulSet.DeepCopy()
			tt.mutate(newStatefulSet)

			f.onStatefulSetUpdate(oldStatefulSet, newStatefulSet)
			if got := f.workqueue.Len() > 0; got != tt.wantQueued {
				t.Fatalf("queued recovery=%v, want %v", got, tt.wantQueued)
			}
		})
	}
}

// Worker pods in the control-plane namespace are routed by namespace like everything
// else there, but the kubectl pods run in the controller's own namespace, which no
// recovery indexes — those are routed by the recovery they name.
func TestWorkerPodEventsEnqueueTheirRecovery(t *testing.T) {
	for _, tt := range []struct {
		name       string
		app        string
		namespace  func(*recoveryTest) string
		labels     map[string]string
		wantQueued bool
	}{
		{
			name:       "holder pod",
			app:        etcdHolderApp,
			namespace:  func(f *recoveryTest) string { return f.recovery.Spec.ControlPlaneNamespace },
			wantQueued: true,
		},
		{
			name:       "restore pod",
			app:        etcdRestoreApp,
			namespace:  func(f *recoveryTest) string { return f.recovery.Spec.ControlPlaneNamespace },
			wantQueued: true,
		},
		{
			name:       "snapshot transfer pod",
			app:        snapshotTransferApp,
			namespace:  func(f *recoveryTest) string { return f.recovery.Namespace },
			wantQueued: true,
		},
		{
			name:       "health check pod",
			app:        etcdHealthCheckApp,
			namespace:  func(f *recoveryTest) string { return f.recovery.Namespace },
			wantQueued: true,
		},
		{
			name:      "transfer pod without a recovery label",
			app:       snapshotTransferApp,
			namespace: func(f *recoveryTest) string { return f.recovery.Namespace },
			labels:    map[string]string{"app": snapshotTransferApp},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newRecoveryTest(t, nil)
			namespace := tt.namespace(f)
			labels := tt.labels
			if labels == nil {
				labels = map[string]string{"app": tt.app, recoveryNameLabel: f.recovery.Name}
			}
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Name: "worker", Namespace: namespace, Labels: labels,
			}}

			f.onPodUpdate(nil, pod)
			if got := f.workqueue.Len() > 0; got != tt.wantQueued {
				t.Fatalf("queued recovery=%v, want %v", got, tt.wantQueued)
			}
			if !tt.wantQueued {
				return
			}
			key, _ := f.workqueue.Get()
			defer f.workqueue.Done(key)
			want := f.recovery.Namespace + "/" + f.recovery.Name
			if key != want {
				t.Fatalf("expected key %q, got %q", want, key)
			}
		})
	}
}

// The StatefulSet lister is read by steps that run before any other cache is
// consulted, so its sync has to be waited on like every other informer.
func TestStatefulSetInformerIsWaitedOn(t *testing.T) {
	f := newRecoveryTest(t, nil)
	if len(f.hasSynced) != len(f.informers) {
		t.Fatalf("expected every wired informer to be waited on, got %d of %d",
			len(f.hasSynced), len(f.informers))
	}
}
