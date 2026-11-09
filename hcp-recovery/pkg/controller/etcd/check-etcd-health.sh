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

# docs/etcd-restore.md step 10.
#
# Confirms the restored members formed ONE cluster rather than several. A restore that
# went wrong still produces running, individually healthy etcd pods, so "the pods are
# up" proves nothing — these checks are what distinguish a real recovery from a
# split-brain.
#
# etcd's internal health has no Kubernetes object to watch, so the polling lives here.
# The controller creates this pod once and waits for it to terminate.

set -eu

: "${HCP_NS:?control-plane namespace is required}"
: "${EXEC_POD:?etcd pod to run etcdctl in is required}"
: "${ETCD_CONTAINER:?etcd container name is required}"
: "${ETCD_ENDPOINTS:?comma-separated etcd endpoints are required}"
: "${EXPECTED_MEMBERS:?expected etcd member count is required}"
: "${ETCD_CACERT:?etcd CA certificate path is required}"
: "${ETCD_CERT:?etcd client certificate path is required}"
: "${ETCD_KEY:?etcd client key path is required}"
: "${MIN_REVISION:?post-restore revision floor is required}"
: "${DEADLINE_SECONDS:?overall deadline in seconds is required}"

for tool in kubectl jq; do
    if ! command -v "$tool" >/dev/null 2>&1; then
        echo "required tool $tool is not present in this image" >&2
        exit 1
    fi
done

etcdctl() {
    kubectl -n "$HCP_NS" exec "$EXEC_POD" -c "$ETCD_CONTAINER" -- \
        etcdctl --cacert="$ETCD_CACERT" --cert="$ETCD_CERT" --key="$ETCD_KEY" \
        --endpoints="$ETCD_ENDPOINTS" "$@"
}

check_once() {
    # Every member must be started and voting. A member with no name or no client URL is
    # the "phantom" entry left behind when a member was added but never brought up.
    etcdctl member list -w json | jq -er --argjson want "$EXPECTED_MEMBERS" '
        (.members // []) as $m
        | if ($m | length) != $want then
              error("expected \($want) members, found \($m | length)")
          elif ($m | map(select(((.name // "") == "")
                             or (((.peerURLs // []) | length) == 0)
                             or (((.clientURLs // []) | length) == 0))) | length) > 0 then
              error("some members are not fully started: \($m | map({name, peerURLs, clientURLs}) | tostring)")
          elif ($m | map(select(.isLearner // false)) | length) > 0 then
              error("some members are still learners: \($m | map(select(.isLearner // false) | .name) | tostring)")
          else
              "members: \($m | map(.name) | join(", "))"
          end' || return 1

    # One cluster ID and one raft term across every endpoint is what rules out a split
    # brain; exactly one leader rules out an election still in progress.
    #
    # Member and cluster IDs are 64-bit and older jq rounds them to a double. Both sides
    # of every comparison here are rounded the same way, so the checks still hold; the
    # IDs are only ever compared, never printed as an identifier to act on.
    # The revision floor is what proves the snapshot restore actually happened. A
    # member's own data directory also boots into a healthy, single-leader cluster, so
    # every other check here passes either way; only the revision separates "etcd is
    # healthy" from "etcd is healthy AND was rebuilt from the snapshot". etcdutl adds a
    # fixed offset to the snapshot's revision, so a store below that offset was never
    # restored, and its clients will resume against revisions they have already seen.
    #
    # This proves the restore ran. It cannot prove the offset was large enough to clear
    # the pre-outage high-water mark; that revision is not observable from here.
    etcdctl endpoint status -w json | jq -er --argjson want "$EXPECTED_MEMBERS" \
        --argjson floor "$MIN_REVISION" '
        if (length) != $want then
            error("expected \($want) endpoints, found \(length)")
        elif ([.[].Status.header.cluster_id] | unique | length) != 1 then
            error("endpoints report different cluster IDs: \([.[].Status.header.cluster_id] | unique | tostring)")
        elif ([.[].Status.header.raft_term] | unique | length) != 1 then
            error("endpoints report different raft terms: \([.[].Status.header.raft_term] | unique | tostring)")
        elif ([.[] | select(.Status.header.member_id == .Status.leader)] | length) != 1 then
            error("expected exactly one leader, found \([.[] | select(.Status.header.member_id == .Status.leader)] | length)")
        elif (.[0].Status.header.revision < $floor) then
            error("revision \(.[0].Status.header.revision) is below the post-restore floor \($floor), so the snapshot restore did not take effect")
        else
            "cluster: id=\(.[0].Status.header.cluster_id) raftTerm=\(.[0].Status.header.raft_term) revision=\(.[0].Status.header.revision)"
        end' || return 1

    etcdctl endpoint health -w json | jq -er --argjson want "$EXPECTED_MEMBERS" '
        if (length) != $want then
            error("expected \($want) endpoints, found \(length)")
        elif ([.[] | select((.health // false) != true)] | length) > 0 then
            error("unhealthy endpoints: \([.[] | select((.health // false) != true) | .endpoint] | tostring)")
        else
            "all \($want) endpoints healthy"
        end' || return 1
}

deadline=$(( $(date +%s) + DEADLINE_SECONDS ))
attempt=1
while true; do
    if summary=$(check_once 2>&1); then
        echo "$summary"
        printf '%s' "$(printf '%s' "$summary" | tr '\n' ';')" > /dev/termination-log
        exit 0
    fi
    if [ "$(date +%s)" -ge "$deadline" ]; then
        echo "etcd did not become one healthy cluster within ${DEADLINE_SECONDS}s" >&2
        echo "$summary" >&2
        exit 1
    fi
    echo "attempt $attempt: $summary" >&2
    attempt=$(( attempt + 1 ))
    sleep 10
done
