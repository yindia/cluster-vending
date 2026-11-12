// Package manager is the platform's controller: a kubebuilder project
// (PROJECT, api/, internal/controller/, config/) inside the repository's
// one Go module. Its API is cloud.cluster-vending.io/v1alpha1
// (CloudAccount owns Network owns Cluster).
//
// The controllers (internal/controller) reconcile those objects by
// calling the reconcilers the API server's background loop runs on the
// database store (pkg/reconcile/account, network, cluster), on the store
// that keeps its rows as these objects (pkg/database/cloudstore). The API
// server with --store=kubernetes writes the same objects and leaves
// reconciling to this controller.
//
// kubebuilder scaffolds a cmd/main.go and a Makefile next to PROJECT; this
// project has neither. The binary is the repository's (`cluster-vending
// controller`, cmd/controller.go, which calls Run), and the generators are
// the root Makefile's `controller-apis` target.
package manager

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"

	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/yindia/cluster-vending/pkg/config"
	reconcilers "github.com/yindia/cluster-vending/pkg/controller"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/management"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// leaseName is the leader election Lease of the controller: not the API
// server's, so both can run.
const leaseName = "controller.cloud.cluster-vending.io"

// Options are what the controller manager runs with.
type Options struct {
	// Kubeconfig and KubeContext are how the management cluster is
	// reached (empty: the default loading rules, then in-cluster).
	Kubeconfig, KubeContext string
	// MetricsAddr is the listen address of the Prometheus metrics; "0"
	// turns them off.
	MetricsAddr string
	// ProbeAddr is the listen address of /healthz and /readyz; "0" turns
	// them off.
	ProbeAddr string
	// LeaderElect runs the controllers behind a Lease in LeaseNamespace,
	// so one replica reconciles.
	LeaderElect    bool
	LeaseNamespace string
	// RenderTarget (0: this build's renderer version) and RenderRollout
	// (empty: all) drive the renderer rollout, as for the API server.
	RenderTarget  int64
	RenderRollout []string
	// InstallProviders creates the Crossplane providers, provider-helm and
	// the activation policy when missing; ProviderRuntime is how the AWS
	// providers run.
	InstallProviders bool
	ProviderRuntime  crossplaneaws.ProviderRuntime
	// AWS is the management account (the providers run as its role), the
	// optional role between the controller's own credentials and the
	// tenant roles, and whether keys were found in the environment (local
	// testing).
	AWS config.AWS
	// Logger receives the manager's and the controllers' logs. Nil: they
	// are discarded.
	Logger *slog.Logger
}

// Deps are what the controllers work with (reconcile.Deps on the store of
// the platform's kinds, and a client that reads those kinds at the API
// server).
type Deps = reconcilers.Deps

// Setup registers the three controllers and the probes with mgr. It does
// not contact the management cluster.
func Setup(mgr manager.Manager, d Deps) error {
	if err := reconcilers.SetupAll(mgr, d); err != nil {
		return err
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("controller health check: %w", err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("controller ready check: %w", err)
	}
	return nil
}

// Run runs the controller manager until ctx ends. It prepares the
// management cluster (pkg/management): the platform's
// CustomResourceDefinitions and, unless told not to, the Crossplane
// providers are installed, and it waits until their kinds are served. The
// API server installs neither providers nor anything of AWS; it waits for
// the controller. The controller's cache also holds the platform's own
// kinds, which it watches.
//
// The controller is what looks at the cloud. Its lookups start from its
// own credentials (the AWS SDK's default chain: IRSA in production, keys
// in the environment for local testing) and always assume the tenant role
// of the account they read (management.WithAWS).
func Run(ctx context.Context, o Options) error {
	logger := o.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	options := management.Options{
		Kubeconfig: o.Kubeconfig, KubeContext: o.KubeContext, Logger: logger,
		InstallProviders: o.InstallProviders, ProviderRuntime: o.ProviderRuntime, InstallKinds: true,
		LeaderElection:     kube.LeaderElection{Enabled: o.LeaderElect, Namespace: o.LeaseNamespace},
		WatchPlatformKinds: true, LeaseName: leaseName, MetricsAddr: o.MetricsAddr, ProbeAddr: o.ProbeAddr,
	}
	// The store first: it installs the kinds the manager's cache holds.
	store, err := management.ConnectStore(ctx, options)
	if err != nil {
		return err
	}
	mgr, err := management.Connect(ctx, options)
	if err != nil {
		return fmt.Errorf("%w (--install-providers=false skips installing the providers)", err)
	}
	cfg, err := kube.RESTConfig(o.Kubeconfig, o.KubeContext)
	if err != nil {
		return err
	}
	direct, err := kube.New(cfg)
	if err != nil {
		return err
	}
	if o.AWS.StaticKeys != nil {
		logger.Warn("AWS keys found in the environment: they are the controller's own credentials (local testing); every lookup still assumes the tenant role of the account it reads")
	}
	renderTarget := o.RenderTarget
	if renderTarget == 0 {
		renderTarget = crossplaneaws.RenderVersion
	}
	reconciling, err := management.WithAWS(ctx, reconcile.Deps{
		Store:        store,
		Kube:         mgr.GetClient(), // reads from the informer cache
		KubeReader:   mgr.GetAPIReader(),
		RenderTarget: renderTarget, RenderRollout: o.RenderRollout,
		Logger: logger,
	}, o.AWS.SourceRoleARN)
	if err != nil {
		return err
	}
	deps := Deps{Direct: direct, Deps: reconciling}
	if err := Setup(mgr, deps); err != nil {
		return err
	}
	var ready atomic.Bool
	done, err := management.Start(ctx, mgr, &ready)
	if err != nil {
		return err
	}
	logger.Info("controller manager started", "leader_elect", o.LeaderElect, "render_target", renderTarget)
	if err := <-done; err != nil {
		return fmt.Errorf("controller manager: %w", err)
	}
	return nil
}
