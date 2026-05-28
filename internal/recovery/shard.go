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

package recovery

import (
	"fmt"
	"hash/fnv"
)

func AssignShard(clusterID string, numShards int) int {
	if numShards <= 1 {
		return 0
	}
	h := fnv.New32a()
	h.Write([]byte(clusterID))
	return int(h.Sum32() % uint32(numShards))
}

func VeleroShardNamespace(shardIndex int) string {
	return fmt.Sprintf("velero-%d", shardIndex)
}

func VeleroBackupPrefix(shardIndex int) string {
	return fmt.Sprintf("velero/%d", shardIndex)
}
