#!/bin/sh

# Copyright 2026 Microsoft Corporation
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# docs/etcd-restore.md step 7, run by the snapshot transfer Pod.
#
# Each etcd PVC carries its own snapshot, taken by the Velero pre-backup hook at a
# slightly different instant, so the three files differ. etcd can only be rebuilt as
# one cluster if every member restores from the SAME snapshot, so this copies the
# canonical member's snapshot over the others and refuses to finish unless all of them
# end up byte-identical.
#
# The PVCs live in different availability zones, so no single pod can mount them all.
# This pod mounts none of them: it reaches each volume through the holder Pod that does,
# staging the snapshot on its own emptyDir in between.
#
# Retries happen here rather than in the controller, so the controller only ever sees a
# pod that Succeeded or Failed and never has to block or re-create pods on a timer.

set -eu

: "${HCP_NS:?control-plane namespace is required}"
: "${CANONICAL_POD:?canonical holder pod name is required}"
: "${CANONICAL_MEMBER:?canonical etcd member name is required}"
: "${HOLDER_PODS:?space-separated holder pod names are required}"
: "${HOLDER_CONTAINER:?holder container name is required}"
: "${SNAPSHOT_PATH:?snapshot path is required}"
: "${STAGING_PATH:?staging path is required}"
: "${DEADLINE_SECONDS:?overall deadline in seconds is required}"

for tool in kubectl jq sha256sum; do
    if ! command -v "$tool" >/dev/null 2>&1; then
        echo "required tool $tool is not present in this image" >&2
        exit 1
    fi
done

deadline=$(( $(date +%s) + DEADLINE_SECONDS ))

# retry runs its arguments until they succeed or the overall deadline passes. Every
# step below is idempotent, so retrying is always safe.
retry() {
    attempt=1
    while true; do
        if "$@"; then
            return 0
        fi
        if [ "$(date +%s)" -ge "$deadline" ]; then
            echo "giving up on: $* (after $attempt attempts)" >&2
            return 1
        fi
        echo "attempt $attempt failed, retrying: $*" >&2
        attempt=$(( attempt + 1 ))
        sleep 10
    done
}

in_holder() {
    pod=$1
    shift
    kubectl -n "$HCP_NS" exec "$pod" -c "$HOLDER_CONTAINER" -- "$@"
}

snapshot_checksum() {
    # `sha256sum` prints "<digest>  <path>"; keep only the digest.
    in_holder "$1" sha256sum "$SNAPSHOT_PATH" | cut -d' ' -f1
}

snapshot_revision() {
    in_holder "$1" etcdutl snapshot status "$SNAPSHOT_PATH" -w json |
        jq -er '.revision | if type == "number" and . >= 0 and . == floor then tostring
                else error("snapshot reports a non-integer revision") end'
}

wait_for_holders() {
    # shellcheck disable=SC2086 # HOLDER_PODS is intentionally word-split.
    set -- $HOLDER_PODS
    for pod in "$@"; do
        kubectl -n "$HCP_NS" wait --for=condition=ready "pod/$pod" --timeout=60s || return 1
    done
}

retry wait_for_holders

if ! in_holder "$CANONICAL_POD" test -f "$SNAPSHOT_PATH"; then
    echo "$CANONICAL_MEMBER has no snapshot at $SNAPSHOT_PATH; the Velero backup hook did not produce one for this member" >&2
    exit 1
fi

expected_sha=$(retry snapshot_checksum "$CANONICAL_POD")
expected_revision=$(retry snapshot_revision "$CANONICAL_POD")
echo "canonical snapshot: member=$CANONICAL_MEMBER revision=$expected_revision sha256=$expected_sha"

fetch_canonical() {
    rm -f "$STAGING_PATH"
    # No TTY, so the stream stays binary-clean.
    kubectl -n "$HCP_NS" exec "$CANONICAL_POD" -c "$HOLDER_CONTAINER" \
        -- cat "$SNAPSHOT_PATH" > "$STAGING_PATH" || return 1
    actual=$(sha256sum "$STAGING_PATH" | cut -d' ' -f1)
    if [ "$actual" != "$expected_sha" ]; then
        echo "staged copy is $actual, expected $expected_sha" >&2
        return 1
    fi
}

push_to_holder() {
    pod=$1
    # Write to a scratch name and rename, so an interrupted copy never leaves a
    # truncated file that looks like a complete snapshot to a later attempt.
    kubectl -n "$HCP_NS" exec -i "$pod" -c "$HOLDER_CONTAINER" \
        -- sh -c "cat > ${SNAPSHOT_PATH}.copy" < "$STAGING_PATH" || return 1
    in_holder "$pod" mv "${SNAPSHOT_PATH}.copy" "$SNAPSHOT_PATH" || return 1
    actual=$(snapshot_checksum "$pod") || return 1
    if [ "$actual" != "$expected_sha" ]; then
        echo "$pod holds $actual after the copy, expected $expected_sha" >&2
        return 1
    fi
}

staged=false
# shellcheck disable=SC2086 # HOLDER_PODS is intentionally word-split.
set -- $HOLDER_PODS
for pod in "$@"; do
    [ "$pod" = "$CANONICAL_POD" ] && continue
    if [ "$(snapshot_checksum "$pod" 2>/dev/null || true)" = "$expected_sha" ]; then
        echo "$pod already holds the canonical snapshot"
        continue
    fi
    if [ "$staged" = false ]; then
        retry fetch_canonical
        staged=true
    fi
    retry push_to_holder "$pod"
done

# GATE: nothing downstream may run unless every volume holds the same snapshot.
# A mismatch here means the members would form separate clusters rather than one.
# shellcheck disable=SC2086 # HOLDER_PODS is intentionally word-split.
set -- $HOLDER_PODS
for pod in "$@"; do
    actual_sha=$(retry snapshot_checksum "$pod")
    actual_revision=$(retry snapshot_revision "$pod")
    if [ "$actual_sha" != "$expected_sha" ] || [ "$actual_revision" != "$expected_revision" ]; then
        echo "$pod: revision=$actual_revision sha256=$actual_sha does not match the canonical snapshot" >&2
        exit 1
    fi
    echo "$pod: revision=$actual_revision sha256=$actual_sha"
done

rm -f "$STAGING_PATH"

# The controller reads this off the Pod's terminated container status, so it learns the
# result without streaming logs or making an extra API call.
jq -cn --arg member "$CANONICAL_MEMBER" --arg sha "$expected_sha" \
    --argjson revision "$expected_revision" \
    '{sourceMember: $member, sha256: $sha, revision: $revision}' > /dev/termination-log
