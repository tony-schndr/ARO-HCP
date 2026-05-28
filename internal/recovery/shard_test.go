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
	"math/rand"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAssignShard(t *testing.T) {
	tests := []struct {
		name      string
		clusterID string
		numShards int
		want      int
	}{
		{
			name:      "single shard always returns 0",
			clusterID: "abc123",
			numShards: 1,
			want:      0,
		},
		{
			name:      "zero shards returns 0",
			clusterID: "abc123",
			numShards: 0,
			want:      0,
		},
		{
			name:      "negative shards returns 0",
			clusterID: "abc123",
			numShards: -1,
			want:      0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, AssignShard(tt.clusterID, tt.numShards))
		})
	}
}

func TestAssignShard_Deterministic(t *testing.T) {
	clusterID := "11111111111111111111111111111111"
	first := AssignShard(clusterID, 4)
	for range 100 {
		assert.Equal(t, first, AssignShard(clusterID, 4))
	}
}

func TestAssignShard_Distribution(t *testing.T) {
	numShards := 4
	counts := make([]int, numShards)
	for i := range 1000 {
		shard := AssignShard(fmt.Sprintf("cluster-%d", i), numShards)
		assert.GreaterOrEqual(t, shard, 0)
		assert.Less(t, shard, numShards)
		counts[shard]++
	}
	for i, c := range counts {
		assert.GreaterOrEqual(t, c, 150, "shard %d got only %d/1000 assignments, expected >= 15%%", i, c)
	}
}

func TestAssignShard_DistributionWithRealisticIDs(t *testing.T) {
	const hexChars = "0123456789abcdef"
	r := rand.New(rand.NewSource(42))

	for _, numShards := range []int{2, 3, 4} {
		t.Run(fmt.Sprintf("%d_shards", numShards), func(t *testing.T) {
			const n = 10000
			counts := make([]int, numShards)
			for range n {
				id := make([]byte, 32)
				for j := range id {
					id[j] = hexChars[r.Intn(16)]
				}
				shard := AssignShard(string(id), numShards)
				assert.GreaterOrEqual(t, shard, 0)
				assert.Less(t, shard, numShards)
				counts[shard]++
			}
			expected := n / numShards
			expectedMin := expected * 70 / 100
			expectedMax := expected * 130 / 100
			for i, c := range counts {
				assert.GreaterOrEqual(t, c, expectedMin, "shard %d underrepresented: %d/%d", i, c, n)
				assert.LessOrEqual(t, c, expectedMax, "shard %d overrepresented: %d/%d", i, c, n)
			}
		})
	}
}

func TestAssignShard_SmallSampleVariance(t *testing.T) {
	const hexChars = "0123456789abcdef"
	r := rand.New(rand.NewSource(99))
	const numShards = 2
	const numClusters = 5
	const numTrials = 100

	allOnOneShard := 0
	for range numTrials {
		counts := make([]int, numShards)
		for range numClusters {
			id := make([]byte, 32)
			for j := range id {
				id[j] = hexChars[r.Intn(16)]
			}
			counts[AssignShard(string(id), numShards)]++
		}
		if counts[0] == numClusters || counts[1] == numClusters {
			allOnOneShard++
		}
	}
	// With 5 clusters and 2 shards, P(all on one shard) = 2*(1/2)^5 = 6.25%.
	// Over 100 trials we expect ~6 occurrences. Assert it happens at least once
	// to document that skew is normal at small scale.
	assert.Greater(t, allOnOneShard, 0,
		"expected at least one trial where all %d clusters land on the same shard", numClusters)
}

func TestVeleroShardNamespace(t *testing.T) {
	assert.Equal(t, "velero-0", VeleroShardNamespace(0))
	assert.Equal(t, "velero-3", VeleroShardNamespace(3))
}

func TestVeleroBackupPrefix(t *testing.T) {
	assert.Equal(t, "velero/0", VeleroBackupPrefix(0))
	assert.Equal(t, "velero/3", VeleroBackupPrefix(3))
}
