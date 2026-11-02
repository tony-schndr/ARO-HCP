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

package verifiers

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/blang/semver/v4"
	"github.com/onsi/ginkgo/v2"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	configv1 "github.com/openshift/api/config/v1"
	configv1client "github.com/openshift/client-go/config/clientset/versioned/typed/config/v1"

	"github.com/Azure/ARO-HCP/internal/api/metadataapi"
	"github.com/Azure/ARO-HCP/test/util/framework"
)

// shortImageDigest keeps release-image references readable while preserving enough of the digest
// to distinguish two payloads for the same OpenShift version.
func shortImageDigest(image string) string {
	if image == "" {
		return ""
	}
	if _, digest, found := strings.Cut(image, "@sha256:"); found {
		return "sha256:" + digest[:min(len(digest), 12)]
	}
	return image[strings.LastIndex(image, "/")+1:]
}

// renderClusterVersionHistory preserves the timestamps and release image needed to tell a stalled
// upgrade from a slow one.
func renderClusterVersionHistory(history []configv1.UpdateHistory) string {
	if len(history) == 0 {
		return "<empty>"
	}
	entries := make([]string, 0, len(history))
	for _, historyEntry := range history {
		entry := fmt.Sprintf("%s=%s", historyEntry.Version, historyEntry.State)
		if !historyEntry.StartedTime.IsZero() {
			entry += " started=" + historyEntry.StartedTime.UTC().Format(time.RFC3339)
		}
		if historyEntry.CompletionTime != nil && !historyEntry.CompletionTime.IsZero() {
			entry += " completed=" + historyEntry.CompletionTime.UTC().Format(time.RFC3339)
		}
		if image := shortImageDigest(historyEntry.Image); image != "" {
			entry += " image=" + image
		}
		entries = append(entries, entry)
	}
	return strings.Join(entries, "; ")
}

// renderClusterVersionProgress reports how far through the rollout the ClusterVersion Operator is.
func renderClusterVersionProgress(conditions []configv1.ClusterOperatorStatusCondition) string {
	for _, condition := range conditions {
		if condition.Type == configv1.OperatorProgressing {
			return fmt.Sprintf("Progressing=%s: %s", condition.Status, condition.Message)
		}
	}
	return ""
}

// clusterVersionSummary is the diagnostic line logged while polling and appended to failures.
func clusterVersionSummary(status configv1.ClusterVersionStatus) string {
	summary := "history is " + renderClusterVersionHistory(status.History)
	if progress := renderClusterVersionProgress(status.Conditions); progress != "" {
		summary += "; " + progress
	}
	return summary
}

// withClusterVersionSummary ensures every y- and z-stream verdict reports the state it judged.
func withClusterVersionSummary(status configv1.ClusterVersionStatus, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w; %s", err, clusterVersionSummary(status))
}

type verifyHostedControlPlaneZStreamUpgradeOnly struct {
	initialVersion string
}

func (v verifyHostedControlPlaneZStreamUpgradeOnly) Name() string {
	return fmt.Sprintf("VerifyHostedControlPlaneZStreamUpgradeOnly(initial=%s)", v.initialVersion)
}

func (v verifyHostedControlPlaneZStreamUpgradeOnly) Verify(ctx context.Context, adminRESTConfig *rest.Config) error {
	initialSemver, err := semver.ParseTolerant(v.initialVersion)
	if err != nil {
		return fmt.Errorf("parse initial version %q: %w", v.initialVersion, err)
	}

	configClient, err := configv1client.NewForConfig(adminRESTConfig)
	if err != nil {
		return fmt.Errorf("failed to create config client: %w", err)
	}

	clusterVersion, err := configClient.ClusterVersions().Get(ctx, "version", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get clusterversion %q: %w", "version", err)
	}

	ginkgo.GinkgoLogr.Info("Retrieved openshift cluster version history",
		"history", clusterVersionSummary(clusterVersion.Status))

	uniqueVersionSet := map[string]bool{}
	var uniqueVersions []string
	var sawInitial, sawUpgrade bool
	for _, history := range clusterVersion.Status.History {
		historyVersion, err := semver.ParseTolerant(history.Version)
		if err != nil {
			return withClusterVersionSummary(clusterVersion.Status,
				fmt.Errorf("parse version %q in history: %w", history.Version, err))
		}
		if historyVersion.Major != initialSemver.Major || historyVersion.Minor != initialSemver.Minor {
			return withClusterVersionSummary(clusterVersion.Status,
				fmt.Errorf("version %q in clusterversion history has different major.minor than initial %q (expected %d.%d.x)",
					historyVersion.String(), v.initialVersion, initialSemver.Major, initialSemver.Minor))
		}
		if historyVersion.LT(initialSemver) {
			return withClusterVersionSummary(clusterVersion.Status,
				fmt.Errorf("downgrade unexpected: version %q is less than initial %q", historyVersion.String(), initialSemver.String()))
		}
		if historyVersion.EQ(initialSemver) {
			sawInitial = true
		}
		if historyVersion.GT(initialSemver) {
			sawUpgrade = true
		}
		if key := historyVersion.String(); !uniqueVersionSet[key] {
			uniqueVersionSet[key] = true
			uniqueVersions = append(uniqueVersions, key)
		}
	}
	if !sawInitial {
		return withClusterVersionSummary(clusterVersion.Status,
			fmt.Errorf("install version %q not found in clusterversion/version status.history; cannot confirm the z-stream upgrade started from it", v.initialVersion))
	}
	if !sawUpgrade {
		return withClusterVersionSummary(clusterVersion.Status,
			fmt.Errorf("no version in clusterversion/version status.history is greater than initial %q", v.initialVersion))
	}
	// The automated z-stream upgrade must move the control plane off the pinned install version, so
	// the history must contain at least two unique versions: the install version and the latest
	// z-stream it was upgraded to.
	if len(uniqueVersions) < 2 {
		return withClusterVersionSummary(clusterVersion.Status,
			fmt.Errorf("expected at least 2 unique versions in clusterversion/version status.history (install %q plus the auto-upgraded z-stream), got %d: %v",
				v.initialVersion, len(uniqueVersions), uniqueVersions))
	}
	return nil
}

// VerifyHostedControlPlaneZStreamUpgradeOnly returns a verifier that the HCP control plane has
// performed only a z-stream upgrade from the initial version: every entry in ClusterVersion
// status.history has the same major.minor as initialVersion, the initial version itself appears in
// the history, at least one entry is greater than it, and the history holds at least two unique
// versions (the pinned install version plus the auto-upgraded latest z-stream).
func VerifyHostedControlPlaneZStreamUpgradeOnly(initialVersion string) HostedClusterVerifier {
	return verifyHostedControlPlaneZStreamUpgradeOnly{initialVersion: initialVersion}
}

type verifyHostedControlPlaneYStreamUpgrade struct {
	targetMinor   string
	previousMinor string
}

func (v verifyHostedControlPlaneYStreamUpgrade) Name() string {
	return fmt.Sprintf("VerifyHostedControlPlaneYStreamUpgrade(previousMinor=%s, targetMinor=%s)", v.previousMinor, v.targetMinor)
}

func (v verifyHostedControlPlaneYStreamUpgrade) Verify(ctx context.Context, adminRESTConfig *rest.Config) error {
	configClient, err := configv1client.NewForConfig(adminRESTConfig)
	if err != nil {
		return fmt.Errorf("failed to create config client: %w", err)
	}

	clusterVersion, err := configClient.ClusterVersions().Get(ctx, "version", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get clusterversion %q: %w", "version", err)
	}

	ginkgo.GinkgoLogr.Info("clusterversion status after y-stream upgrade",
		"history", clusterVersionSummary(clusterVersion.Status))

	parsedPreviousMinor := metadataapi.Must(semver.ParseTolerant(v.previousMinor))
	parsedTargetMinor := metadataapi.Must(semver.ParseTolerant(v.targetMinor))

	var previousMinorFound, targetMinorFound bool
	for _, historyEntry := range clusterVersion.Status.History {
		if len(historyEntry.Version) == 0 {
			continue
		}
		version, err := semver.ParseTolerant(historyEntry.Version)
		if err != nil {
			return withClusterVersionSummary(clusterVersion.Status,
				fmt.Errorf("parse clusterversion history version %q: %w", historyEntry.Version, err))
		}
		if version.Major == parsedPreviousMinor.Major && version.Minor == parsedPreviousMinor.Minor {
			previousMinorFound = true
		}
		if version.Major == parsedTargetMinor.Major && version.Minor == parsedTargetMinor.Minor {
			targetMinorFound = true
		}
	}
	if !previousMinorFound {
		return withClusterVersionSummary(clusterVersion.Status,
			fmt.Errorf("clusterversion status.history has no version in previous minor %q", v.previousMinor))
	}
	if !targetMinorFound {
		return withClusterVersionSummary(clusterVersion.Status,
			fmt.Errorf("clusterversion status.history has no version in target minor %q", v.targetMinor))
	}
	return nil
}

// VerifyHostedControlPlaneYStreamUpgrade returns a verifier that clusterversion status.history
// contains at least one parseable version in previousMinor and at least one in targetMinor.
func VerifyHostedControlPlaneYStreamUpgrade(previousMinor, targetMinor string) HostedClusterVerifier {
	return verifyHostedControlPlaneYStreamUpgrade{previousMinor: previousMinor, targetMinor: targetMinor}
}

type verifyKubeAPIServerServerVersionUpgraded struct {
	preUpgrade *version.Info
}

func (v verifyKubeAPIServerServerVersionUpgraded) Name() string {
	return "VerifyKubeAPIServerServerVersionUpgraded"
}

func (v verifyKubeAPIServerServerVersionUpgraded) Verify(ctx context.Context, adminRESTConfig *rest.Config) error {
	clientset, err := kubernetes.NewForConfig(adminRESTConfig)
	if err != nil {
		return fmt.Errorf("create kubernetes clientset: %w", err)
	}
	postUpgrade, err := clientset.Discovery().ServerVersion()
	if err != nil {
		return fmt.Errorf("get kube-apiserver ServerVersion: %w", err)
	}
	if reflect.DeepEqual(v.preUpgrade, postUpgrade) {
		return fmt.Errorf("expected the kube-apiserver ServerVersion to change from the pre-upgrade %q, still observing %q",
			v.preUpgrade.GitVersion, postUpgrade.GitVersion)
	}
	return nil
}

// VerifyKubeAPIServerServerVersionUpgraded fails if the kube-apiserver version is the same as before the upgrade.
// preUpgrade is the kubernetes discovery ServerVersion (/version) read from the cluster before upgrading.
func VerifyKubeAPIServerServerVersionUpgraded(preUpgrade *version.Info) HostedClusterVerifier {
	return verifyKubeAPIServerServerVersionUpgraded{preUpgrade: preUpgrade}
}

type verifyHostedControlPlaneVersionCompleted struct {
	targetVersion string
}

func (v verifyHostedControlPlaneVersionCompleted) Name() string {
	return fmt.Sprintf("VerifyHostedControlPlaneVersionCompleted(target=%s)", v.targetVersion)
}

func (v verifyHostedControlPlaneVersionCompleted) Verify(ctx context.Context, adminRESTConfig *rest.Config) error {
	configClient, err := configv1client.NewForConfig(adminRESTConfig)
	if err != nil {
		return fmt.Errorf("failed to create config client: %w", err)
	}

	clusterVersion, err := configClient.ClusterVersions().Get(ctx, "version", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get clusterversion %q: %w", "version", err)
	}

	target, err := semver.ParseTolerant(v.targetVersion)
	if err != nil {
		return fmt.Errorf("parse target version %q: %w", v.targetVersion, err)
	}
	desired, err := semver.ParseTolerant(clusterVersion.Status.Desired.Version)
	if err != nil {
		return fmt.Errorf("parse clusterversion status.desired.version %q: %w", clusterVersion.Status.Desired.Version, err)
	}
	if desired.Major != target.Major || desired.Minor != target.Minor {
		return fmt.Errorf("clusterversion status.desired.version %q is not in the target %d.%d line",
			clusterVersion.Status.Desired.Version, target.Major, target.Minor)
	}

	entry, err := framework.LatestClusterVersionHistoryEntryForMinor(clusterVersion.Status.History, v.targetVersion)
	if err != nil {
		return err
	}
	if entry == nil {
		return fmt.Errorf("clusterversion status.history has no entry in the %d.%d line: %v",
			target.Major, target.Minor, framework.SummarizeClusterVersionHistory(clusterVersion.Status.History))
	}
	if entry.State != configv1.CompletedUpdate {
		return fmt.Errorf("clusterversion history entry for %q is %q, expected %q", entry.Version, entry.State, configv1.CompletedUpdate)
	}

	// Desired and Completed on their own only say the CVO reached the payload. A control plane
	// still rolling operators out reports Progressing=True, and callers waiting on an upgrade
	// want the settled cluster, not the moment the payload was accepted.
	for _, condition := range clusterVersion.Status.Conditions {
		if condition.Type != configv1.OperatorProgressing {
			continue
		}
		if condition.Status != configv1.ConditionFalse {
			return fmt.Errorf("clusterversion Progressing is %s, expected False: %s", condition.Status, condition.Message)
		}
	}

	return nil
}

// VerifyHostedControlPlaneVersionCompleted returns a verifier that the hosted control plane has
// settled on targetVersion: clusterversion status.desired.version is in targetVersion's
// major.minor line, the newest history entry for that line is Completed, and the Progressing
// condition is False. Only major.minor is compared, so an exact nightly tag still matches the
// build the cluster landed on.
func VerifyHostedControlPlaneVersionCompleted(targetVersion string) HostedClusterVerifier {
	return verifyHostedControlPlaneVersionCompleted{targetVersion: targetVersion}
}

type verifyHostedControlPlaneUpgradeStartedAfter struct {
	targetVersion string
	notBefore     time.Time
}

func (v verifyHostedControlPlaneUpgradeStartedAfter) Name() string {
	return fmt.Sprintf("VerifyHostedControlPlaneUpgradeStartedAfter(target=%s, notBefore=%s)", v.targetVersion, v.notBefore.Format(time.RFC3339))
}

func (v verifyHostedControlPlaneUpgradeStartedAfter) Verify(ctx context.Context, adminRESTConfig *rest.Config) error {
	configClient, err := configv1client.NewForConfig(adminRESTConfig)
	if err != nil {
		return fmt.Errorf("failed to create config client: %w", err)
	}

	clusterVersion, err := configClient.ClusterVersions().Get(ctx, "version", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get clusterversion %q: %w", "version", err)
	}

	entry, err := framework.LatestClusterVersionHistoryEntryForMinor(clusterVersion.Status.History, v.targetVersion)
	if err != nil {
		return err
	}
	if entry == nil {
		return fmt.Errorf("clusterversion status.history has no entry for %q: %v",
			v.targetVersion, framework.SummarizeClusterVersionHistory(clusterVersion.Status.History))
	}
	if !entry.StartedTime.After(v.notBefore) {
		return fmt.Errorf("clusterversion history entry for %q started at %s, which is not after %s; the upgrade was not started again",
			entry.Version, entry.StartedTime.Format(time.RFC3339), v.notBefore.Format(time.RFC3339))
	}

	ginkgo.GinkgoLogr.Info("clusterversion upgrade was started again",
		"version", entry.Version, "startedTime", entry.StartedTime.Format(time.RFC3339), "notBefore", v.notBefore.Format(time.RFC3339))
	return nil
}

// VerifyHostedControlPlaneUpgradeStartedAfter returns a verifier that the newest clusterversion
// status.history entry in targetVersion's major.minor line was started strictly after notBefore.
// An entry that predates notBefore means the upgrade recorded before notBefore is still the one in
// the history, so no new attempt was made.
func VerifyHostedControlPlaneUpgradeStartedAfter(targetVersion string, notBefore time.Time) HostedClusterVerifier {
	return verifyHostedControlPlaneUpgradeStartedAfter{targetVersion: targetVersion, notBefore: notBefore}
}
