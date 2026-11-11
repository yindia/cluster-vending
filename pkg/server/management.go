package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/go-logr/logr"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/kube"
)

// kindsTimeout bounds the wait for the providers' CRDs on a fresh
// management cluster.
const kindsTimeout = 5 * time.Minute

// ManagementOptions say how to reach and prepare the management cluster.
type ManagementOptions struct {
	Kubeconfig, KubeContext string
	// InstallProviders creates the Crossplane providers, provider-helm and
	// the activation policy when missing; ManagementRoleARN is the AWS
	// providers' IRSA role.
	InstallProviders  bool
	ManagementRoleARN string
	Logger            *slog.Logger
}

// ConnectManagement prepares the management cluster and returns a manager,
// not started: it installs the providers (when asked), waits until the API
// server serves every managed resource kind (informers on a missing kind
// fail), and builds the manager whose client reads from the informer
// cache. Register controllers on it, then StartManagement.
func ConnectManagement(ctx context.Context, o ManagementOptions) (manager.Manager, error) {
	ctrllog.SetLogger(logr.FromSlogHandler(o.Logger.Handler()))
	cfg, err := kube.RESTConfig(o.Kubeconfig, o.KubeContext)
	if err != nil {
		return nil, err
	}
	if o.InstallProviders {
		direct, err := kube.New(cfg)
		if err != nil {
			return nil, err
		}
		res, err := crossplaneaws.InstallProviders(ctx, direct, o.ManagementRoleARN)
		if err != nil {
			return nil, fmt.Errorf("install the Crossplane providers (is Crossplane installed?): %w", err)
		}
		o.Logger.Info("crossplane providers", "created", res.Created, "existing", res.Existing)
	}
	mgr, err := kube.NewManager(cfg)
	if err != nil {
		return nil, err
	}
	o.Logger.Info("waiting for the managed resource kinds")
	if err := kube.WaitForKinds(ctx, mgr, crossplaneaws.ManagedObjects(), kindsTimeout); err != nil {
		return nil, err
	}
	return mgr, nil
}

// StartManagement starts mgr (its informers and controllers) in the
// background and returns once the cache has synced. The channel receives
// mgr's error when it stops (nil when ctx ends).
func StartManagement(ctx context.Context, mgr manager.Manager) (<-chan error, error) {
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()
	if !mgr.GetCache().WaitForCacheSync(ctx) {
		select {
		case err := <-done:
			return nil, fmt.Errorf("management cluster cache: %w", err)
		default:
			return nil, errors.New("management cluster cache did not sync")
		}
	}
	return done, nil
}
