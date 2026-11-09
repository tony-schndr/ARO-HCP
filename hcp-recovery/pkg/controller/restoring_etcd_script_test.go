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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// etcdutlStub stands in for etcdutl. It records every invocation and, for a restore,
// creates the member directory the way the real tool would.
const etcdutlStub = `#!/bin/sh
echo "$@" >> "$ETCDUTL_LOG"
case "$2" in
status)
    exit 0
    ;;
restore)
    if [ -n "${ETCDUTL_FAIL:-}" ]; then
        echo "etcdutl: simulated failure" >&2
        exit 1
    fi
    for arg in "$@"; do
        case "$arg" in
            --data-dir=*) data_dir="${arg#--data-dir=}" ;;
        esac
    done
    mkdir -p "${data_dir}/member/snap"
    echo restored > "${data_dir}/member/snap/db"
    ;;
esac
`

type restoreScriptResult struct {
	exitCode  int
	stdout    string
	stderr    string
	etcdutl   []string
	volumeDir string
}

// runRestoreMemberScript executes the embedded restore script against a fake etcd
// volume, with etcdutl stubbed out. volume is populated by the caller to describe the
// state the volume arrived in.
func runRestoreMemberScript(t *testing.T, populate func(volumeDir string), failRestore bool) restoreScriptResult {
	t.Helper()
	root := t.TempDir()
	volumeDir := filepath.Join(root, "volume")
	binDir := filepath.Join(root, "bin")
	for _, dir := range []string{volumeDir, binDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	stubPath := filepath.Join(binDir, "etcdutl")
	if err := os.WriteFile(stubPath, []byte(etcdutlStub), 0o755); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(root, "restore-member.sh")
	if err := os.WriteFile(scriptPath, []byte(restoreMemberScript), 0o755); err != nil {
		t.Fatal(err)
	}
	if populate != nil {
		populate(volumeDir)
	}

	logPath := filepath.Join(root, "etcdutl.log")
	cmd := exec.Command("/bin/sh", scriptPath)
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"ETCDUTL_LOG="+logPath,
		"ETCD_MEMBER=etcd-0",
		"INITIAL_CLUSTER=etcd-0=https://etcd-0.etcd-discovery.ns.svc:2380",
		"INITIAL_ADVERTISE_PEER_URLS=https://etcd-0.etcd-discovery.ns.svc:2380",
		"INITIAL_CLUSTER_TOKEN="+etcdInitialClusterToken,
		"BUMP_REVISION="+etcdRevisionBump,
		"SNAPSHOT_PATH="+filepath.Join(volumeDir, "backup.db"),
		"DATA_DIR="+filepath.Join(volumeDir, "data"),
	)
	if failRestore {
		cmd.Env = append(cmd.Env, "ETCDUTL_FAIL=1")
	}
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	exitCode := 0
	if err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("failed to run the restore script: %v", err)
		}
		exitCode = exitErr.ExitCode()
	}

	var invocations []string
	if raw, err := os.ReadFile(logPath); err == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			if line != "" {
				invocations = append(invocations, line)
			}
		}
	}
	return restoreScriptResult{
		exitCode: exitCode, stdout: stdout.String(), stderr: stderr.String(),
		etcdutl: invocations, volumeDir: volumeDir,
	}
}

func writeVolumeSnapshot(t *testing.T, volumeDir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(volumeDir, "backup.db"), []byte("snapshot"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeStaleDataDir reproduces what Velero actually hands back. The CSI snapshot
// captures the whole volume, so a restored etcd volume always arrives carrying the
// data directory that was live when the backup was taken, right beside backup.db.
func writeStaleDataDir(t *testing.T, volumeDir string) {
	t.Helper()
	stale := filepath.Join(volumeDir, "data", "member", "snap")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "db"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (r restoreScriptResult) restoreInvocation(t *testing.T) string {
	t.Helper()
	for _, inv := range r.etcdutl {
		if strings.Contains(inv, "snapshot restore") {
			return inv
		}
	}
	return ""
}

// The volume Velero restores always contains a data directory, so treating one as
// proof the work is done skips the restore on the very first attempt. That shipped
// once: etcd came up on the volume's own data with no revision bump, every condition
// still went green, and the guest cluster's operators were left degraded.
func TestRestoreMemberScriptRestoresOntoAVeleroRestoredVolume(t *testing.T) {
	result := runRestoreMemberScript(t, func(volumeDir string) {
		writeStaleDataDir(t, volumeDir)
		writeVolumeSnapshot(t, volumeDir)
	}, false)

	if result.exitCode != 0 {
		t.Fatalf("expected success, got exit %d\nstdout: %s\nstderr: %s", result.exitCode, result.stdout, result.stderr)
	}
	invocation := result.restoreInvocation(t)
	if invocation == "" {
		t.Fatalf("etcdutl snapshot restore never ran; invocations: %v\nstdout: %s", result.etcdutl, result.stdout)
	}
	for _, want := range []string{
		"--mark-compacted",
		"--bump-revision " + etcdRevisionBump,
		"--initial-cluster-token " + etcdInitialClusterToken,
		"--name etcd-0",
	} {
		if !strings.Contains(invocation, want) {
			t.Errorf("restore invocation is missing %q: %s", want, invocation)
		}
	}
	// The stale directory must be gone, replaced by the one etcdutl built.
	if data, err := os.ReadFile(filepath.Join(result.volumeDir, "data", "member", "snap", "db")); err != nil {
		t.Fatalf("expected a restored data directory: %v", err)
	} else if strings.TrimSpace(string(data)) != "restored" {
		t.Fatalf("the stale data directory survived the restore: %q", data)
	}
	if _, err := os.Stat(filepath.Join(result.volumeDir, "backup.db")); !os.IsNotExist(err) {
		t.Fatalf("a consumed snapshot should be removed, stat err = %v", err)
	}
}

func TestRestoreMemberScriptIsIdempotent(t *testing.T) {
	t.Run("a consumed snapshot with a data directory is already done", func(t *testing.T) {
		result := runRestoreMemberScript(t, func(volumeDir string) {
			// What a volume looks like after this script has already succeeded.
			writeStaleDataDir(t, volumeDir)
		}, false)

		if result.exitCode != 0 {
			t.Fatalf("expected success, got exit %d\nstderr: %s", result.exitCode, result.stderr)
		}
		if invocation := result.restoreInvocation(t); invocation != "" {
			t.Fatalf("a completed restore must not run again: %s", invocation)
		}
	})

	t.Run("no snapshot and no data directory fails", func(t *testing.T) {
		result := runRestoreMemberScript(t, nil, false)

		if result.exitCode == 0 {
			t.Fatal("expected a failure when there is nothing to restore from")
		}
		if !strings.Contains(result.stderr, "no snapshot") {
			t.Fatalf("unexpected stderr: %s", result.stderr)
		}
	})

	// A replacement pod can only retry while the snapshot is still there, so a failed
	// restore must leave it in place.
	t.Run("a failed restore keeps the snapshot for the retry", func(t *testing.T) {
		result := runRestoreMemberScript(t, func(volumeDir string) {
			writeStaleDataDir(t, volumeDir)
			writeVolumeSnapshot(t, volumeDir)
		}, true)

		if result.exitCode == 0 {
			t.Fatal("expected the script to fail when etcdutl fails")
		}
		if _, err := os.Stat(filepath.Join(result.volumeDir, "backup.db")); err != nil {
			t.Fatalf("the snapshot must survive a failed restore: %v", err)
		}
	})
}
