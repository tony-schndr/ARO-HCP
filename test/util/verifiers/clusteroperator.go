// Copyright 2025 Microsoft Corporation
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

package verifiers

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/rest"

	configv1 "github.com/openshift/api/config/v1"
	configv1client "github.com/openshift/client-go/config/clientset/versioned/typed/config/v1"
)

type verifyAllClusterOperatorsAvailableImpl struct{}

func (v verifyAllClusterOperatorsAvailableImpl) Name() string {
	return "VerifyAllClusterOperatorsAvailable"
}

func (v verifyAllClusterOperatorsAvailableImpl) Verify(ctx context.Context, adminRESTConfig *rest.Config) error {
	configClient, err := configv1client.NewForConfig(adminRESTConfig)
	if err != nil {
		return fmt.Errorf("failed to create config client: %w", err)
	}

	start := time.Now()
	var lastErr error
	var lastErrMsg string
	verifyErr := wait.PollUntilContextTimeout(ctx, 20*time.Second, 10*time.Minute, true, func(ctx context.Context) (done bool, err error) {
		clusterOperators, err := configClient.ClusterOperators().List(ctx, metav1.ListOptions{})
		if err != nil {
			if isContextExpiration(ctx, err) {
				return false, nil
			}
			lastErr = fmt.Errorf("failed to list ClusterOperators: %w", err)
			if msg := lastErr.Error(); msg != lastErrMsg {
				ginkgo.GinkgoLogr.Info("Failed to list ClusterOperators, retrying.", "error", err)
				lastErrMsg = msg
			}
			return false, nil
		}

		if len(clusterOperators.Items) == 0 {
			lastErr = errors.New("no ClusterOperators found in the cluster")
			if msg := lastErr.Error(); msg != lastErrMsg {
				ginkgo.GinkgoLogr.Info("ClusterOperators not yet ready.", "error", lastErr)
				lastErrMsg = msg
			}
			return false, nil
		}

		unavailableOperators := []string{}
		for _, co := range clusterOperators.Items {
			availableCondition := getClusterOperatorCondition(co.Status.Conditions, configv1.OperatorAvailable)
			if availableCondition == nil {
				unavailableOperators = append(unavailableOperators, fmt.Sprintf("%s (no Available condition)", co.Name))
				continue
			}
			if availableCondition.Status != configv1.ConditionTrue {
				unavailableOperators = append(unavailableOperators, fmt.Sprintf("%s (%s)", co.Name, formatConditions(co.Status.Conditions)))
			}
		}

		if len(unavailableOperators) > 0 {
			slices.Sort(unavailableOperators)
			lastErr = fmt.Errorf("cluster operators not available: %s", strings.Join(unavailableOperators, ", "))
			if msg := lastErr.Error(); msg != lastErrMsg {
				ginkgo.GinkgoLogr.Info("ClusterOperators not yet ready.", "unavailable", unavailableOperators)
				lastErrMsg = msg
			}
			return false, nil
		}

		return true, nil
	})

	if verifyErr == nil {
		ginkgo.GinkgoLogr.Info("All cluster operators available", "duration", time.Since(start))
		return nil
	}

	return errors.Join(verifyErr, lastErr)
}

type verifyAllClusterOperatorsAtClusterVersionImpl struct{}

func (v verifyAllClusterOperatorsAtClusterVersionImpl) Name() string {
	return "VerifyAllClusterOperatorsAtClusterVersion"
}

// Verify reports whether every ClusterOperator has converged on the version the CVO is driving.
//
// ClusterVersion reaching Completed is not the same as the cluster having finished the upgrade:
// the CVO can record the payload as applied while individual operators are still rolling their
// operands out, which is visible as an operator that still reports the old version with
// Progressing=True (`oc get co` VERSION column). Waiting on this verifier waits for that churn
// to drain.
func (v verifyAllClusterOperatorsAtClusterVersionImpl) Verify(ctx context.Context, adminRESTConfig *rest.Config) error {
	configClient, err := configv1client.NewForConfig(adminRESTConfig)
	if err != nil {
		return fmt.Errorf("failed to create config client: %w", err)
	}

	clusterVersion, err := configClient.ClusterVersions().Get(ctx, "version", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get clusterversion %q: %w", "version", err)
	}
	desiredVersion := clusterVersion.Status.Desired.Version
	if len(desiredVersion) == 0 {
		return errors.New("clusterversion status.desired.version is empty")
	}

	clusterOperators, err := configClient.ClusterOperators().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list ClusterOperators: %w", err)
	}
	if len(clusterOperators.Items) == 0 {
		return errors.New("no ClusterOperators found in the cluster")
	}

	var unsettled []string
	for _, co := range clusterOperators.Items {
		// The "operator" entry is the operator's own version, the one `oc get co` prints in
		// the VERSION column; the other entries are operand versions.
		operatorVersion, found := clusterOperatorVersion(co.Status.Versions, "operator")
		switch {
		case !found:
			unsettled = append(unsettled, fmt.Sprintf("%s (no %q version reported, want %s)", co.Name, "operator", desiredVersion))
			continue
		case operatorVersion != desiredVersion:
			unsettled = append(unsettled, fmt.Sprintf("%s (version %s, want %s)", co.Name, operatorVersion, desiredVersion))
			continue
		}

		if condition := getClusterOperatorCondition(co.Status.Conditions, configv1.OperatorAvailable); condition == nil || condition.Status != configv1.ConditionTrue {
			unsettled = append(unsettled, fmt.Sprintf("%s (Available!=True: %s)", co.Name, formatConditions(co.Status.Conditions)))
			continue
		}
		if condition := getClusterOperatorCondition(co.Status.Conditions, configv1.OperatorProgressing); condition != nil && condition.Status != configv1.ConditionFalse {
			unsettled = append(unsettled, fmt.Sprintf("%s (Progressing=%s: %s)", co.Name, condition.Status, condition.Message))
		}
	}

	if len(unsettled) > 0 {
		slices.Sort(unsettled)
		return fmt.Errorf("cluster operators have not settled on %s: %s", desiredVersion, strings.Join(unsettled, ", "))
	}

	ginkgo.GinkgoLogr.Info("All cluster operators settled on the cluster version",
		"version", desiredVersion, "count", len(clusterOperators.Items))
	return nil
}

// VerifyAllClusterOperatorsAtClusterVersion returns a verifier that every ClusterOperator reports
// the ClusterVersion status.desired.version as its own "operator" version, is Available=True, and
// is no longer Progressing. Use it to wait out the operator rollout that continues after
// ClusterVersion itself reports the update Completed.
func VerifyAllClusterOperatorsAtClusterVersion() HostedClusterVerifier {
	return verifyAllClusterOperatorsAtClusterVersionImpl{}
}

func clusterOperatorVersion(versions []configv1.OperandVersion, name string) (string, bool) {
	for _, version := range versions {
		if version.Name == name {
			return version.Version, true
		}
	}
	return "", false
}

func getClusterOperatorCondition(conditions []configv1.ClusterOperatorStatusCondition, conditionType configv1.ClusterStatusConditionType) *configv1.ClusterOperatorStatusCondition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}
	return nil
}

func formatConditions(conditions []configv1.ClusterOperatorStatusCondition) string {
	var parts []string
	for _, cond := range conditions {
		parts = append(parts, fmt.Sprintf("%s=%s", cond.Type, cond.Status))
	}
	return strings.Join(parts, ", ")
}

func VerifyAllClusterOperatorsAvailable() HostedClusterVerifier {
	return verifyAllClusterOperatorsAvailableImpl{}
}
