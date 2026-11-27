// Package management prepares the management cluster for the two commands
// that talk to it: the API server (`serve`), which reads it and writes the
// platform's own kinds, and the controller, which also installs the
// Crossplane providers and reconciles. It builds the manager whose cache
// both read from, and the store that keeps its rows as objects.
package management

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	awsv1beta1 "github.com/upbound/provider-aws/v2/apis/namespaced/v1beta1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/yindia/cluster-vending/pkg/config"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/telemetry"
)

// kindsTimeout bounds the wait for the providers' CRDs on a fresh
// management cluster.
const kindsTimeout = 5 * time.Minute

// Options say how to reach and prepare the management cluster.
type Options struct {
	Kubeconfig, KubeContext string
	// InstallProviders brings the Crossplane providers, provider-helm,
	// their runtime configs and the activation policy to this build
	// (crossplaneaws.InstallProviders); ProviderRuntime is how they run. Only the controller installs them; the API server
	// waits for their kinds.
	InstallProviders bool
	// InstallKinds installs the CustomResourceDefinitions of the platform's
	// own kinds (ConnectStore). The same rule: what reconciles installs
	// them, the API server waits for them.
	InstallKinds    bool
	ProviderRuntime crossplaneaws.ProviderRuntime
	// LeaderElection of the controllers; the API server needs none.
	LeaderElection kube.LeaderElection
	// RateLimits of the management cluster's clients; zero: kube's
	// defaults.
	RateLimits kube.RateLimits
	Logger     *slog.Logger
	// WatchPlatformKinds also caches the platform's own kinds (CloudAccount,
	// Network, Cluster), whatever their labels: the controller manager
	// watches them. They must be installed (ConnectStore with InstallKinds).
	WatchPlatformKinds bool
	// LeaseName, MetricsAddr and ProbeAddr are the manager's leader
	// election Lease and listen addresses; empty for the server's Lease
	// and for off.
	LeaseName, MetricsAddr, ProbeAddr string
}

// Connect prepares the management cluster and returns a manager,
// not started: it installs the providers (when asked), waits until the API
// server serves every cached kind (the manager's cache and its informers
// fail on a missing one), and builds the manager whose client reads from
// the informer cache. Register controllers on it, then Start.
func Connect(ctx context.Context, o Options) (manager.Manager, error) {
	ctrllog.SetLogger(logr.FromSlogHandler(o.Logger.Handler()))
	cfg, err := kube.RESTConfigWith(o.Kubeconfig, o.KubeContext, o.RateLimits)
	if err != nil {
		return nil, err
	}
	if o.InstallProviders {
		direct, err := kube.New(cfg)
		if err != nil {
			return nil, err
		}
		res, err := crossplaneaws.InstallProviders(ctx, direct, o.ProviderRuntime)
		if err != nil {
			return nil, fmt.Errorf("install the Crossplane providers (is Crossplane installed?): %w", err)
		}
		o.Logger.Info("crossplane providers", "created", res.Created, "existing", res.Existing, "updated", res.Updated, "deleted", res.Deleted)
		if len(res.Kept) > 0 {
			o.Logger.Info("provider fields set by another field manager (an operator) are kept as they are; remove that manager's change to take this build's",
				"kept", res.Kept)
		}
	}
	scheme, err := kube.Scheme()
	if err != nil {
		return nil, err
	}
	cached, err := kube.GVKs(scheme, CachedObjects())
	if err != nil {
		return nil, err
	}
	// Before the manager: its cache looks every kind up when it is built,
	// and the CRDs of providers installed just now are not there yet.
	mapper, err := kube.NewRESTMapper(cfg)
	if err != nil {
		return nil, err
	}
	o.Logger.Info("waiting for the managed resource kinds")
	if err := kube.WaitForKinds(ctx, mapper, cached, kindsTimeout); err != nil {
		return nil, err
	}
	opts := kube.ManagerOptions{Cached: cached, Election: o.LeaderElection, LeaseName: o.LeaseName,
		MetricsAddr: o.MetricsAddr, ProbeAddr: o.ProbeAddr}
	if o.WatchPlatformKinds {
		opts.Unfiltered = append(cloudstore.Kinds(), corev1.SchemeGroupVersion.WithKind("Namespace"))
	}
	return kube.NewManagerWith(cfg, opts)
}

// ConnectStore returns the store that keeps its rows as CloudAccount,
// Network and Cluster objects in the tenants' namespaces of the
// management cluster (pkg/database/cloudstore): it installs the
// CustomResourceDefinitions of the cloud.cluster-vending.io kinds when
// asked (Options.InstallKinds) and waits until they are served. The store's client reads from the API
// server, not from the manager's cache: a row is read right after it was
// written.
func ConnectStore(ctx context.Context, o Options) (repository.Store, error) {
	cfg, err := kube.RESTConfigWith(o.Kubeconfig, o.KubeContext, o.RateLimits)
	if err != nil {
		return nil, err
	}
	direct, err := kube.New(cfg)
	if err != nil {
		return nil, err
	}
	if o.InstallKinds {
		if err := cloudstore.Install(ctx, direct); err != nil {
			return nil, err
		}
	}
	mapper, err := kube.NewRESTMapper(cfg)
	if err != nil {
		return nil, err
	}
	o.Logger.Info("waiting for the platform's kinds")
	if err := kube.WaitForKinds(ctx, mapper, cloudstore.Kinds(), kindsTimeout); err != nil {
		return nil, err
	}
	return cloudstore.New(storeMetrics.Wrap(direct)), nil
}

// storeMetrics measure the store's calls (cluster_vending_store_*), in the
// process's metrics registry: the API server's /metrics and the
// controller's --metrics-addr.
var storeMetrics = telemetry.NewStore(ctrlmetrics.Registry)

// CachedObjects are the kinds the informer cache holds: the managed
// resources and Usages of networks and clusters, the accounts'
// ProviderConfigs, each cluster's Helm ProviderConfig, the Helm Releases
// of its platform components and the tenant namespaces. The cache refuses
// reads of any other kind, so every kind a network or cluster records
// must be here.
func CachedObjects() []client.Object {
	helm := &unstructured.Unstructured{}
	helm.SetGroupVersionKind(crossplaneaws.HelmProviderConfigGVK)
	release := &unstructured.Unstructured{}
	release.SetGroupVersionKind(crossplaneaws.HelmReleaseGVK)
	return append(crossplaneaws.ManagedObjects(), &awsv1beta1.ProviderConfig{}, helm, release, &corev1.Namespace{})
}

// Start starts mgr (its informers and controllers) in the
// background and returns once the cache has synced; ready then reports
// true (the readiness probe). The channel receives mgr's error when it
// stops (nil when ctx ends).
func Start(ctx context.Context, mgr manager.Manager, ready *atomic.Bool) (<-chan error, error) {
	cached, err := kube.GVKs(mgr.GetScheme(), CachedObjects())
	if err != nil {
		return nil, err
	}
	if err := kube.StartInformers(ctx, mgr, cached); err != nil {
		return nil, err
	}
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
	ready.Store(true)
	return done, nil
}

// CredentialsSecret is the provider credentials Secret of the settings
// (config.AWS.ProviderCredentials) as the renderer takes it; nil for none.
func CredentialsSecret(ref *config.SecretKeyRef) *crossplaneaws.CredentialsSecret {
	if ref == nil {
		return nil
	}
	return &crossplaneaws.CredentialsSecret{Namespace: ref.Namespace, Name: ref.Name, Key: ref.Key, Direct: ref.Direct}
}
