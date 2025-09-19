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

package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	"github.com/go-logr/logr"
	"github.com/spf13/cobra"
	"sigs.k8s.io/prow/cmd/generic-autobumper/bumper"
	"sigs.k8s.io/yaml"

	"github.com/Azure/ARO-HCP/tooling/image-updater/internal/options"
	"github.com/Azure/ARO-HCP/tooling/image-updater/internal/updater"
)

// autobumpClient implements bumper.PRHandler interface
type autobumpClient struct {
	updateOpts *options.RawUpdateOptions
	updater    *updater.Updater
	logger     logr.Logger
}

var _ bumper.PRHandler = (*autobumpClient)(nil)

// Changes returns a slice of functions, each one does some stuff, and
// returns commit message for the changes
func (c *autobumpClient) Changes() []func(context.Context) (string, error) {
	return []func(context.Context) (string, error){
		func(ctx context.Context) (string, error) {
			c.logger.Info("Running image updates...")

			// Validate and complete the update options
			validated, err := c.updateOpts.Validate(ctx)
			if err != nil {
				return "", fmt.Errorf("failed to validate options: %w", err)
			}

			completed, err := validated.Complete(ctx)
			if err != nil {
				return "", fmt.Errorf("failed to complete options: %w", err)
			}

			// Store the updater for later use in PRTitleBody
			c.updater = completed

			// Perform the image updates
			if err := c.updater.UpdateImages(ctx); err != nil {
				return "", fmt.Errorf("failed to update images: %w", err)
			}

			// // Run yamlfmt
			// c.logger.Info("Running yamlfmt...")
			// if err := runMake("../..", "yamlfmt"); err != nil {
			// 	return "", fmt.Errorf("failed to run yamlfmt: %w", err)
			// }

			// // Run config materialization
			// c.logger.Info("Running config materialization...")
			// if err := runMake("../../config", "materialize"); err != nil {
			// 	return "", fmt.Errorf("failed to materialize config: %w", err)
			// }

			// Generate commit message
			commitMsg := c.updater.GenerateCommitMessage()
			if commitMsg == "" {
				return "", fmt.Errorf("no images were updated")
			}

			c.logger.Info("Image updates complete", "updatedCount", len(c.updater.Updates))
			return commitMsg, nil
		},
	}
}

// PRTitleBody returns the title and body of the PR
func (c *autobumpClient) PRTitleBody() (string, string) {
	if c.updater == nil || len(c.updater.Updates) == 0 {
		return "Update image digests", "No images were updated"
	}

	title := "updated image components for dev/int"

	// Group updates by environment
	envUpdates := make(map[string]map[string]string) // env -> name -> digest
	for _, update := range c.updater.Updates {
		if envUpdates[update.Environment] == nil {
			envUpdates[update.Environment] = make(map[string]string)
		}
		envUpdates[update.Environment][update.Name] = update.NewDigest
	}

	// Generate body with updates grouped by environment
	body := "This PR updates the following container image digests:\n\n"

	// Output dev environment first
	if updates, exists := envUpdates["dev"]; exists && len(updates) > 0 {
		body += "### Dev Environment\n"
		for name, digest := range updates {
			body += fmt.Sprintf("- **%s**: `%s`\n", name, digest)
		}
		body += "\n"
	}

	// Output int environment second
	if updates, exists := envUpdates["int"]; exists && len(updates) > 0 {
		body += "### Int Environment\n"
		for name, digest := range updates {
			body += fmt.Sprintf("- **%s**: `%s`\n", name, digest)
		}
		body += "\n"
	}

	return title, body
}

// autobumpOptions combines image-updater options with bumper options
type autobumpOptions struct {
	ConfigPath   string
	BumperConfig string
}

func NewAutobumpCommand() *cobra.Command {
	opts := &autobumpOptions{}

	cmd := &cobra.Command{
		Use:   "autobump",
		Short: "Update image digests and create a PR automatically",
		Long: `Autobump fetches the latest image digests from their source registries,
updates the target configuration files, commits the changes, and creates a pull request.

This command wraps the update functionality with the prow generic-autobumper to automate
the PR creation workflow including git operations, oncall assignment, and more.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAutobump(cmd, opts)
		},
	}

	cmd.Flags().StringVar(&opts.ConfigPath, "config", "", "Path to image-updater configuration file")
	cmd.Flags().StringVar(&opts.BumperConfig, "bumper-config", "", "Path to bumper configuration file")

	if err := cmd.MarkFlagRequired("config"); err != nil {
		return nil
	}
	if err := cmd.MarkFlagRequired("bumper-config"); err != nil {
		return nil
	}

	return cmd
}

func runAutobump(cmd *cobra.Command, opts *autobumpOptions) error {
	ctx := cmd.Context()
	logger := logr.FromContextOrDiscard(ctx)

	// Load bumper options from config file
	bumperOpts, err := loadBumperOptions(opts.BumperConfig)
	if err != nil {
		return fmt.Errorf("failed to load bumper config: %w", err)
	}

	// Create update options
	updateOpts := &options.RawUpdateOptions{
		ConfigPath: opts.ConfigPath,
		DryRun:     false, // Always false for autobump - we want to make changes
	}

	// Create autobump client
	client := &autobumpClient{
		updateOpts: updateOpts,
		logger:     logger,
	}

	// Run the bumper
	logger.Info("Starting autobump process...")
	if err := bumper.Run(ctx, bumperOpts, client); err != nil {
		return fmt.Errorf("autobump failed: %w", err)
	}

	logger.Info("Autobump completed successfully")
	return nil
}

func loadBumperOptions(configPath string) (*bumper.Options, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read bumper config file: %w", err)
	}

	var opts bumper.Options
	if err := yaml.Unmarshal(data, &opts); err != nil {
		return nil, fmt.Errorf("failed to parse bumper config: %w", err)
	}

	return &opts, nil
}

func runMake(dir string, target string) error {
	cmd := exec.Command("make", "-C", dir, target)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
