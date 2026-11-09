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
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/component-base/metrics/legacyregistry"
	"k8s.io/klog/v2"

	hypershiftclient "github.com/openshift/hypershift/client/clientset/clientset"
	hypershiftinformers "github.com/openshift/hypershift/client/informers/externalversions"

	"github.com/Azure/ARO-HCP/hcp-recovery/pkg/controller"
	"github.com/Azure/ARO-HCP/hcp-recovery/pkg/generated/clientset/versioned"
	"github.com/Azure/ARO-HCP/hcp-recovery/pkg/generated/informers/externalversions"
	"github.com/Azure/ARO-HCP/internal/backup"
	sharedleaderelection "github.com/Azure/ARO-HCP/internal/leaderelection"
)

const (
	LeaderElectionLockName = "hcp-recovery-controller-leader"
)

type RawControllerOptions struct {
	HealthAddress string
	Kubeconfig    string
	Namespace     string
	// WorkerImage and WorkerServiceAccount configure the short-lived pods the
	// controller uses to reach into the control-plane namespace. The controller never
	// execs into a pod itself, so this is how etcd volumes are read and written.
	WorkerImage          string
	WorkerServiceAccount string
	Workers              int
	LogVerbosity         int
}

func DefaultControllerOptions() *RawControllerOptions {
	return &RawControllerOptions{
		HealthAddress: ":8080",
		Workers:       2,
	}
}

func (o *RawControllerOptions) BindFlags(cmd *cobra.Command) error {
	cmd.Flags().StringVar(&o.HealthAddress, "health-address", o.HealthAddress, "The bind address for the health check server (e.g., ':8080')")
	cmd.Flags().StringVar(&o.Kubeconfig, "kubeconfig", "", "Path to a kubeconfig. Optional.")
	cmd.Flags().StringVar(&o.Namespace, "namespace", os.Getenv("POD_NAMESPACE"), "The namespace where the mgmt-agent controller is deployed.")
	cmd.Flags().StringVar(&o.WorkerImage, "worker-image", o.WorkerImage,
		"Image for the pods that drive recovery work in the control-plane namespace. Must provide kubectl and jq.")
	cmd.Flags().StringVar(&o.WorkerServiceAccount, "worker-service-account", o.WorkerServiceAccount,
		"ServiceAccount those pods run as. It is the only identity granted pods/exec, so it must not be the controller's own.")
	cmd.Flags().IntVar(&o.Workers, "workers", o.Workers, "Number of reconcile workers to run")
	cmd.Flags().IntVar(&o.LogVerbosity, "log-verbosity", o.LogVerbosity,
		"Log verbosity. 0 is the default verbosity level, equivalent to INFO. "+
			"It must be a value >= 0, where a higher value means more verbose output.")

	return nil // TODO: Why?
}

type validatedControllerOptions struct {
	*RawControllerOptions
}

type ValidatedControllerOptions struct {
	*validatedControllerOptions
}

type completedControllerOptions struct {
	ctrl                *controller.RecoveryController
	kubeInformers       kubeinformers.SharedInformerFactory
	hypershiftInformers hypershiftinformers.SharedInformerFactory
	dynamicInformers    dynamicinformer.DynamicSharedInformerFactory
	hcpRecoveryInformer externalversions.SharedInformerFactory
	healthAddress       string
	leaderElectionLock  resourcelock.Interface
}

type ControllerOptions struct {
	*completedControllerOptions
}

func (o *RawControllerOptions) Validate(ctx context.Context) (*ValidatedControllerOptions, error) {
	if o.Namespace == "" {
		return nil, fmt.Errorf("namespace is required")
	}
	if o.HealthAddress == "" {
		return nil, fmt.Errorf("health-address is required")
	}
	if o.LogVerbosity < 0 {
		return nil, fmt.Errorf("--log-verbosity must be a value >= 0")
	}
	if o.WorkerImage == "" {
		return nil, fmt.Errorf("worker-image is required")
	}
	if o.WorkerServiceAccount == "" {
		return nil, fmt.Errorf("worker-service-account is required")
	}
	return &ValidatedControllerOptions{
		validatedControllerOptions: &validatedControllerOptions{
			RawControllerOptions: o,
		},
	}, nil
}

func (o *ValidatedControllerOptions) Complete(ctx context.Context) (*ControllerOptions, error) {
	kubeConfig, err := o.buildKubeConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to build kubeconfig: %w", err)
	}

	kubeClientset, err := kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create kubernetes clientset: %w", err)
	}

	hcpRecoveryClientSet, err := versioned.NewForConfig(kubeConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create hcp recovery clientset: %w", err)
	}

	dynamicClientSet, err := dynamic.NewForConfig(kubeConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create dynamic clientset: %w", err)
	}

	hcpRecoveryInformer := externalversions.NewSharedInformerFactoryWithOptions(hcpRecoveryClientSet, 10*time.Minute, externalversions.WithNamespace(o.Namespace))
	restoreResource := schema.GroupVersionResource{Group: backup.VeleroGroup, Version: backup.VeleroVersion, Resource: "restores"}
	restoreClient := dynamicClientSet.Resource(restoreResource).Namespace(backup.VeleroNamespace)
	backupResource := schema.GroupVersionResource{Group: backup.VeleroGroup, Version: backup.VeleroVersion, Resource: backup.VeleroBackupResource}
	dynamicInformers := dynamicinformer.NewFilteredDynamicSharedInformerFactory(dynamicClientSet, 10*time.Minute, backup.VeleroNamespace, nil)
	kubeInformers := kubeinformers.NewSharedInformerFactory(kubeClientset, 10*time.Minute)
	hypershiftClient, err := hypershiftclient.NewForConfig(kubeConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create hypershift clientset: %w", err)
	}
	hypershiftInformers := hypershiftinformers.NewSharedInformerFactory(hypershiftClient, 10*time.Minute)

	ctrl, err := controller.NewRecoveryController(
		hcpRecoveryClientSet,
		hypershiftClient,
		kubeClientset,
		restoreClient,
		kubeInformers.Apps().V1().StatefulSets(),
		kubeInformers.Apps().V1().Deployments(),
		kubeInformers.Core().V1().Pods(),
		kubeInformers.Core().V1().PersistentVolumeClaims(),
		hypershiftInformers.Hypershift().V1beta1().HostedClusters(),
		hypershiftInformers.Hypershift().V1beta1().HostedControlPlanes(),
		hcpRecoveryInformer.Hcprecovery().V1alpha1().HCPRecoveries(),
		dynamicInformers.ForResource(restoreResource),
		dynamicInformers.ForResource(backupResource),
		o.WorkerImage,
		o.WorkerServiceAccount,
		o.Workers,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create RecoveryController: %w", err)
	}

	hostname, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("failed to get hostname for leader election: %w", err)
	}

	leaderElectionLock, err := sharedleaderelection.NewLeaderElectionLock(hostname, kubeConfig, o.Namespace, LeaderElectionLockName, sharedleaderelection.RecommendedRenewDeadline)
	if err != nil {
		return nil, fmt.Errorf("failed to create leader election lock: %w", err)
	}
	return &ControllerOptions{
		completedControllerOptions: &completedControllerOptions{
			ctrl:                ctrl,
			kubeInformers:       kubeInformers,
			hypershiftInformers: hypershiftInformers,
			hcpRecoveryInformer: hcpRecoveryInformer,
			dynamicInformers:    dynamicInformers,
			healthAddress:       o.HealthAddress,
			leaderElectionLock:  leaderElectionLock,
		},
	}, nil
}

func (o *ControllerOptions) Run(ctx context.Context) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(fmt.Errorf("Run returned"))

	logger := klog.FromContext(ctx)

	var (
		mu   sync.Mutex
		errs []error
		wg   sync.WaitGroup
	)

	// health/metrics server
	wg.Add(1)
	go func() {
		defer cancel(fmt.Errorf("health server exited"))
		defer wg.Done()
		defer utilruntime.HandleCrash()
		mux := http.NewServeMux()
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		mux.Handle("/metrics", legacyregistry.Handler())
		server := &http.Server{Addr: o.healthAddress, Handler: mux}
		if err := runHTTPServer(ctx, server, "health server"); err != nil {
			mu.Lock()
			errs = append(errs, err)
			mu.Unlock()
		}
	}()

	// leader election
	wg.Add(1)
	go func() {
		defer cancel(fmt.Errorf("leader election existed"))
		defer wg.Done()
		defer utilruntime.HandleCrash()
		if err := o.runControllerUnderLeaderElection(ctx); err != nil {
			mu.Lock()
			errs = append(errs, err)
			mu.Unlock()
		}
	}()

	wg.Wait()
	logger.Info("hcp-recovery stopped")
	return errors.Join(errs...)
}

func (o ControllerOptions) runControllerUnderLeaderElection(ctx context.Context) error {
	logger := klog.FromContext(ctx)

	leaderElectionConfig := leaderelection.LeaderElectionConfig{
		Lock:          o.leaderElectionLock,
		LeaseDuration: sharedleaderelection.RecommendedLeaseDuration,
		RenewDeadline: sharedleaderelection.RecommendedRenewDeadline,
		RetryPeriod:   sharedleaderelection.RecommendedRetryPeriod,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				logger.Info("acquired leader election lease; starting informers and controllers")

				o.kubeInformers.Start(ctx.Done())
				o.hypershiftInformers.Start(ctx.Done())
				o.hcpRecoveryInformer.Start(ctx.Done())
				o.dynamicInformers.Start(ctx.Done())

				go func() {
					defer utilruntime.HandleCrash()
					if err := o.ctrl.Run(ctx); err != nil {
						logger.Error(err, "hcp recovery controller failed")
					}
				}()
			},
			OnStoppedLeading: func() {
				logger.Info("lost leader election lease")
			},
		},
		ReleaseOnCancel: true,
		Name:            LeaderElectionLockName,
	}

	sharedleaderelection.LogLeaseProperties(logger, leaderElectionConfig)

	le, err := leaderelection.NewLeaderElector(leaderElectionConfig)
	if err != nil {
		return err
	}
	le.Run(ctx)
	return nil
}

// runHTTPServer runs the server and shuts it down when ctx is cancelled.
// It returns nil if the server was shut down cleanly (http.ErrServerClosed),
// or the underlying error if ListenAndServe failed for another reason.
func runHTTPServer(ctx context.Context, server *http.Server, name string) error {
	logger := klog.FromContext(ctx)

	done := make(chan struct{})
	defer close(done)
	go func() {
		defer utilruntime.HandleCrash()
		select {
		case <-ctx.Done():
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 31*time.Second)
			defer shutdownCancel()
			logger.Info(fmt.Sprintf("shutting down %s", name))
			if err := server.Shutdown(shutdownCtx); err != nil {
				logger.Error(err, fmt.Sprintf("failed to shut down %s", name))
			} else {
				logger.Info(fmt.Sprintf("%s shut down completed", name))
			}
		case <-done:
		}
	}()

	logger.Info(fmt.Sprintf("%s listening on %s", name, server.Addr))
	err := server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (o *ValidatedControllerOptions) buildKubeConfig() (*rest.Config, error) {
	config, err := rest.InClusterConfig()
	if err == nil {
		klog.V(6).Info("Using in-cluster kubeconfig")
		return config, nil
	}

	klog.V(6).Info("Not running in-cluster, using out-of-cluster kubeconfig")
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if o.Kubeconfig != "" {
		loadingRules.ExplicitPath = o.Kubeconfig
	}

	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, &clientcmd.ConfigOverrides{})
	config, err = kubeConfig.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to build kubeconfig from loading rules: %w", err)
	}

	return config, nil
}
