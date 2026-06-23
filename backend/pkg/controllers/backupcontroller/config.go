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

package backupcontroller

import (
	"fmt"
	"os"
	"time"

	"sigs.k8s.io/yaml"
)

type BackupCadence string

const (
	BackupCadenceProduction BackupCadence = "production"
	BackupCadenceTesting    BackupCadence = "testing"
)

type BackupConfig struct {
	Paused  bool          `json:"paused"`
	Cadence BackupCadence `json:"cadence"`
}

func (c *BackupConfig) Schedules() []BackupScheduleConfig {
	switch c.Cadence {
	case BackupCadenceTesting:
		return []BackupScheduleConfig{
			{Name: "hourly", Schedule: "*/5 * * * *", TTL: "1h"},
		}
	default:
		return []BackupScheduleConfig{
			{Name: "hourly", Schedule: "0 */1 * * *", TTL: "48h"},
			{Name: "daily", Schedule: "0 2 * * *", TTL: "336h"},
			{Name: "weekly", Schedule: "0 3 * * 0", TTL: "2160h"},
		}
	}
}

type BackupScheduleConfig struct {
	Name     string
	Schedule string
	TTL      string
}

func (s *BackupScheduleConfig) TTLDuration() time.Duration {
	d, _ := time.ParseDuration(s.TTL)
	return d
}

func LoadBackupConfig(path string) (*BackupConfig, error) {
	if len(path) == 0 {
		return nil, fmt.Errorf("backup configuration path is required")
	}

	rawBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("error reading file %s: %w", path, err)
	}

	var config BackupConfig
	err = yaml.Unmarshal(rawBytes, &config)
	if err != nil {
		return nil, fmt.Errorf("error unmarshaling file %s: %w", path, err)
	}

	if err := config.validate(); err != nil {
		return nil, fmt.Errorf("error validating backup config %s: %w", path, err)
	}

	return &config, nil
}

func (c *BackupConfig) validate() error {
	switch c.Cadence {
	case BackupCadenceProduction, BackupCadenceTesting:
	default:
		return fmt.Errorf("invalid backup cadence %q, must be %q or %q",
			c.Cadence, BackupCadenceProduction, BackupCadenceTesting)
	}
	return nil
}
