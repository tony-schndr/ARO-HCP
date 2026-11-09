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
	"net"
	"path/filepath"
	"testing"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func TestHealthAddressOptions(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	kubeconfigPath := filepath.Join(t.TempDir(), "kubeconfig")
	if err := clientcmd.WriteToFile(clientcmdapi.Config{
		Clusters:       map[string]*clientcmdapi.Cluster{"test": {Server: "http://127.0.0.1:1"}},
		Contexts:       map[string]*clientcmdapi.Context{"test": {Cluster: "test"}},
		CurrentContext: "test",
	}, kubeconfigPath); err != nil {
		t.Fatal(err)
	}
	for _, address := range []string{":8080", "127.0.0.1:9090"} {
		t.Run(address, func(t *testing.T) {
			options := DefaultControllerOptions()
			if address != ":8080" {
				options.HealthAddress = address
			}
			if options.HealthAddress != address {
				t.Fatalf("health address = %q, want %q", options.HealthAddress, address)
			}
			if _, _, err := net.SplitHostPort(options.HealthAddress); err != nil {
				t.Fatalf("invalid listen address: %v", err)
			}
			options.Namespace = "hcp-recovery"
			options.Kubeconfig = kubeconfigPath
			// Both are required and have no default: the image is pinned by the chart,
			// and the worker identity must be the one holding pods/exec.
			options.WorkerImage = "mcr.microsoft.com/aks/command/runtime:test"
			options.WorkerServiceAccount = "hcp-recovery-worker"
			validated, err := options.Validate(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			completed, err := validated.Complete(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				if err := completed.ctrl.Run(ctx); err == nil {
					t.Error("expected cancelled cache sync to fail")
				}
			})
			if completed.healthAddress != address {
				t.Fatalf("completed health address = %q, want %q", completed.healthAddress, address)
			}
		})
	}
}
