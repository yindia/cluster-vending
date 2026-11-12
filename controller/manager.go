// Package controller is the platform's controller: a kubebuilder project
// (PROJECT, api/, internal/controller/, config/) inside the repository's
// one Go module. Its API is cloud.cluster-vending.io/v1alpha1
// (CloudAccount owns Network owns Cluster); its reconcilers are not
// implemented yet, so the manager it runs is idle.
//
// kubebuilder scaffolds a cmd/main.go and a Makefile next to PROJECT; this
// project has neither. The binary is the repository's (`cluster-vending
// controller`, cmd/controller.go, which calls Run), and the generators are
// the root Makefile's `controller-apis` target.
package controller

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	cloudv1alpha1 "github.com/yindia/cluster-vending/controller/api/v1alpha1"
	reconcilers "github.com/yindia/cluster-vending/controller/internal/controller"
)

// leaseName is the leader election Lease of the controller.
const leaseName = "controller.cloud.cluster-vending.io"

// Options are what the controller manager runs with.
type Options struct {
	// Config is how the management cluster is reached.
	Config *rest.Config
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
	// Logger receives the manager's and the controllers' logs. Nil: they
	// are discarded.
	Logger *slog.Logger
}

// Scheme returns the types the controller manager knows: the built-in
// Kubernetes ones and the platform's API.
func Scheme() (*runtime.Scheme, error) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("controller scheme: %w", err)
	}
	if err := cloudv1alpha1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("controller scheme: %w", err)
	}
	return scheme, nil
}

// NewManager builds the controller manager and registers the three
// reconcilers and the probes. It does not contact the management cluster.
func NewManager(o Options) (manager.Manager, error) {
	scheme, err := Scheme()
	if err != nil {
		return nil, err
	}
	logger := logr.Discard()
	if o.Logger != nil {
		logger = logr.FromSlogHandler(o.Logger.Handler())
	}
	mgr, err := ctrl.NewManager(o.Config, ctrl.Options{
		Scheme:                        scheme,
		Logger:                        logger,
		Metrics:                       metricsserver.Options{BindAddress: o.MetricsAddr},
		HealthProbeBindAddress:        o.ProbeAddr,
		LeaderElection:                o.LeaderElect,
		LeaderElectionID:              leaseName,
		LeaderElectionNamespace:       o.LeaseNamespace,
		LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		return nil, fmt.Errorf("controller manager: %w", err)
	}
	if err := reconcilers.SetupAll(mgr); err != nil {
		return nil, err
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return nil, fmt.Errorf("controller health check: %w", err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return nil, fmt.Errorf("controller ready check: %w", err)
	}
	return mgr, nil
}

// Run runs the controller manager until ctx ends. The reconcilers are not
// implemented: the manager watches the platform's kinds and does nothing
// with them.
func Run(ctx context.Context, o Options) error {
	mgr, err := NewManager(o)
	if err != nil {
		return err
	}
	if err := mgr.Start(ctx); err != nil {
		return fmt.Errorf("controller manager: %w", err)
	}
	return nil
}
