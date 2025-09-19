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

package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config represents the image updater configuration
type Config struct {
	Images map[string]ImageConfig `yaml:"images"`
}

// ImageConfig defines a single image's source and target configuration
type ImageConfig struct {
	Source  Source   `yaml:"source"`
	Targets []Target `yaml:"targets"`
}

// Source defines where to fetch the latest image digest from
type Source struct {
	Registry   string `yaml:"registry"`
	Repository string `yaml:"repository"`
	TagPattern string `yaml:"tagPattern,omitempty"`
}

// Target defines where to update the image digest
type Target struct {
	JsonPath string `yaml:"jsonPath"`
	FilePath string `yaml:"filePath"`
}

// Load reads and parses the configuration file
func Load(configPath string) (*Config, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file %s: %w", configPath, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file %s: %w", configPath, err)
	}

	// Validate configuration
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	return &cfg, nil
}

// validate ensures the configuration is complete and valid
func (c *Config) validate() error {
	if len(c.Images) == 0 {
		return fmt.Errorf("no images configured")
	}

	for name, img := range c.Images {
		if img.Source.Registry == "" {
			return fmt.Errorf("image %s: source registry is required", name)
		}
		if img.Source.Repository == "" {
			return fmt.Errorf("image %s: source repository is required", name)
		}
		for _, target := range img.Targets {
			if target.JsonPath == "" {
				return fmt.Errorf("image %s: target jsonPath is required", name)
			}
			if target.FilePath == "" {
				return fmt.Errorf("image %s: target filePath is required", name)
			}
		}
	}

	return nil
}
