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

# docs/etcd-restore.md step 8, run once per etcd PVC in a restore Pod that mounts it.
#
# Every member restores the same snapshot with the same --initial-cluster, the same
# cluster token and the same revision bump, differing only in --name and its own peer
# URL. That is what makes the members come up as one cluster instead of three.
#
# Requires that snapshot distribution has already verified every volume holds an
# identical snapshot, and that etcd and kube-apiserver are stopped.

set -eu

: "${ETCD_MEMBER:?etcd member name is required}"
: "${INITIAL_CLUSTER:?--initial-cluster value is required}"
: "${INITIAL_ADVERTISE_PEER_URLS:?--initial-advertise-peer-urls value is required}"
: "${INITIAL_CLUSTER_TOKEN:?--initial-cluster-token value is required}"
: "${BUMP_REVISION:?--bump-revision value is required}"
: "${SNAPSHOT_PATH:?snapshot path is required}"
: "${DATA_DIR:?data directory is required}"

# The snapshot file, not the data directory, decides whether this restore already ran.
#
# Velero restores the whole volume, so every member's volume arrives carrying the data
# directory that was live when the backup was taken. Keying on the data directory
# therefore matches on the very first attempt and skips the restore entirely, leaving
# etcd to boot three different crash-consistent copies of bbolt with no revision bump.
# The snapshot is removed only after a successful restore, so its absence next to an
# existing data directory is the one state that means the work is genuinely done.
if [ ! -f "$SNAPSHOT_PATH" ]; then
    if [ -d "${DATA_DIR}/member" ]; then
        echo "${ETCD_MEMBER}: snapshot already consumed and ${DATA_DIR}/member exists, nothing to restore"
        ls -la "${DATA_DIR}/member"
        exit 0
    fi
    echo "${ETCD_MEMBER}: no snapshot at ${SNAPSHOT_PATH} and no restored data directory" >&2
    exit 1
fi

etcdutl snapshot status "$SNAPSHOT_PATH" -w table

# Discard whatever the volume arrived with; docs/etcd-restore.md step 7d wipes these
# same stale directories by hand.
rm -rf "$DATA_DIR"

# --bump-revision moves the restored revision past anything clients have already seen,
# and --mark-compacted makes watches below it fail fast so informers re-list instead of
# hanging. Both values must be identical on every member; see docs/etcd-restore.md.
etcdutl snapshot restore "$SNAPSHOT_PATH" \
    --data-dir="$DATA_DIR" \
    --name "$ETCD_MEMBER" \
    --initial-cluster-token "$INITIAL_CLUSTER_TOKEN" \
    --initial-cluster "$INITIAL_CLUSTER" \
    --initial-advertise-peer-urls "$INITIAL_ADVERTISE_PEER_URLS" \
    --mark-compacted \
    --bump-revision "$BUMP_REVISION"

if [ ! -d "${DATA_DIR}/member" ]; then
    echo "${ETCD_MEMBER}: etcdutl reported success but ${DATA_DIR}/member does not exist" >&2
    exit 1
fi
ls -la "${DATA_DIR}/member"

# Only now is the snapshot expendable: a failure before this point leaves it in place
# so a replacement pod can retry.
rm -f "$SNAPSHOT_PATH"
