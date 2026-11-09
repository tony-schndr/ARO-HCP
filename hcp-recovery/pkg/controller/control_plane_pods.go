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
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const TerminalPodDeleteFailedReason = "TerminalPodDeleteFailed"

// Reap at most one terminal pod per pass. Workload controllers can leave terminal
// pods behind even after reporting zero replicas; nonterminal pods must still drain.
func (c *RecoveryController) reapTerminalPod(ctx context.Context, pods []*corev1.Pod) error {
	pods = slices.Clone(pods)
	slices.SortFunc(pods, func(a, b *corev1.Pod) int { return strings.Compare(a.Name, b.Name) })
	for _, pod := range pods {
		if pod.DeletionTimestamp != nil || (pod.Status.Phase != corev1.PodFailed && pod.Status.Phase != corev1.PodSucceeded) {
			continue
		}
		err := c.kubeClientset.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{
			Preconditions: &metav1.Preconditions{UID: &pod.UID},
		})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to delete terminal pod %s/%s (phase=%s): %w", pod.Namespace, pod.Name, pod.Status.Phase, err)
		}
		return nil
	}
	return nil
}

func blockingPods(pods []*corev1.Pod) string {
	details := make([]string, 0, len(pods))
	for _, pod := range pods {
		details = append(details, fmt.Sprintf("%s (phase=%q, terminating=%t)", pod.Name, pod.Status.Phase, pod.DeletionTimestamp != nil))
	}
	slices.Sort(details)
	return strings.Join(details, ", ")
}
