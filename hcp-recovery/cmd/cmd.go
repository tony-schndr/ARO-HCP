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

package cmd

import (
	"context"
	"log/slog"
	"os"

	"github.com/go-logr/logr"
	"github.com/spf13/cobra"

	"k8s.io/klog/v2"
)

func NewRootCmd() (*cobra.Command, error) {
	cmd := &cobra.Command{
		Use:   "hcp-recovery",
		Short: "",
		Long:  ``,
	}
	subcommands := []func() (*cobra.Command, error){
		NewControllerCommand,
	}

	for _, newCmd := range subcommands {
		subCmd, err := newCmd()
		if err != nil {
			return nil, err
		}
		cmd.AddCommand(subCmd)
	}
	return cmd, nil
}

func NewControllerCommand() (*cobra.Command, error) {
	opts := DefaultControllerOptions()
	cmd := &cobra.Command{
		Use:   "controller",
		Short: "Start the hcp-recovery controller",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runController(cmd.Context(), opts)
		},
	}
	if err := opts.BindFlags(cmd); err != nil {
		return nil, err
	}
	return cmd, nil
}

func runController(ctx context.Context, opts *RawControllerOptions) error {
	// Create a logr.Logger backed by slog's JSON handler for structured JSON logging.
	// slog.Level(opts.LogVerbosity * -1) converts the verbosity to an slog.Level:
	// 0 = INFO, higher values = more verbose.
	handlerOptions := &slog.HandlerOptions{Level: slog.Level(opts.LogVerbosity * -1), AddSource: true}
	slogJSONHandler := slog.NewJSONHandler(os.Stdout, handlerOptions)
	logger := logr.FromSlogHandler(slogJSONHandler)
	ctx = logr.NewContext(ctx, logger)

	// Redirect klog (used by k8s client-go, etc.) through the same JSON handler.
	klog.SetLogger(logger)

	validated, err := opts.Validate(ctx)
	if err != nil {
		return err
	}
	completed, err := validated.Complete(ctx)
	if err != nil {
		return err
	}
	return completed.Run(ctx)
}
