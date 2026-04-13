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

package patchbackupprofile

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	"github.com/spf13/cobra"

	"github.com/Azure/ARO-HCP/admin/client/cmd/base"
	adminClient "github.com/Azure/ARO-HCP/admin/client/pkg/client"
)

func NewPatchBackupProfileCommand() (*cobra.Command, error) {
	opts := base.DefaultAuthOptions()
	var subscriptionID, resourceGroup, clusterName, state string

	cmd := &cobra.Command{
		Use:           "patch-backup-profile",
		Short:         "Update backup profile state for an HCP cluster (Active or Paused)",
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return execute(cmd.Context(), opts, subscriptionID, resourceGroup, clusterName, state)
		},
	}

	cmd.Flags().StringVar(&subscriptionID, "subscription-id", "", "Azure subscription ID")
	cmd.Flags().StringVar(&resourceGroup, "resource-group", "", "Azure resource group name")
	cmd.Flags().StringVar(&clusterName, "cluster-name", "", "HCP cluster name")
	cmd.Flags().StringVar(&state, "state", "", "Backup profile state: Active or Paused")

	for _, flag := range []string{"subscription-id", "resource-group", "cluster-name", "state"} {
		if err := cmd.MarkFlagRequired(flag); err != nil {
			return nil, fmt.Errorf("failed to mark flag %q as required: %w", flag, err)
		}
	}

	if err := opts.BindFlags(cmd); err != nil {
		return nil, err
	}

	return cmd, nil
}

func execute(ctx context.Context, opts *base.RawAuthOptions, subscriptionID, resourceGroup, clusterName, state string) error {
	validated, err := opts.Validate(ctx)
	if err != nil {
		return err
	}

	completed, err := validated.Complete(ctx)
	if err != nil {
		return err
	}

	logger, err := logr.FromContext(ctx)
	if err != nil {
		return fmt.Errorf("failed to get logger from context: %w", err)
	}
	logger.Info("Patching backup profile", "subscriptionID", subscriptionID, "resourceGroup", resourceGroup, "clusterName", clusterName, "state", state)

	client := adminClient.NewClient(completed.Endpoint, completed.Host, completed.Token, completed.Insecure, false)
	err = client.PatchBackupProfile(ctx, subscriptionID, resourceGroup, clusterName, state)
	if err != nil {
		return fmt.Errorf("failed to send request: %w", err)
	}

	logger.Info("Request successful")

	return nil
}
