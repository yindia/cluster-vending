package kube

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/yindia/cluster-vending/pkg/crossplane"
)

// NewManager returns a controller-runtime manager for the management
// cluster. Its client reads from an informer cache and writes to the API
// server. The cache holds exactly the kinds in cached (unstructured: one
// informer per kind), only the platform's objects (the managed-by label),
// without managedFields, so 100+ clusters' ~5,000 objects stay in memory
// cheaply and reads cost no API calls. Reads of any other kind fail
// (cache.ErrResourceNotCached) instead of starting an informer, and
// Secrets are never cached: read those, and objects that must be current,
// with GetAPIReader. Every cached kind must be served already
// (WaitForKinds). Start the informers with StartInformers.
func NewManager(cfg *rest.Config, cached []schema.GroupVersionKind, election LeaderElection) (manager.Manager, error) {
	return NewManagerWith(cfg, ManagerOptions{Cached: cached, Election: election})
}

// ManagerOptions are what a manager of the management cluster is built
// with (NewManagerWith).
type ManagerOptions struct {
	// Cached are the kinds the cache holds, the platform's objects only
	// (the managed-by label): see NewManager.
	Cached []schema.GroupVersionKind
	// Unfiltered are kinds the cache holds whatever their labels: the
	// platform's own kinds, which a controller watches and which someone
	// may have created by hand.
	Unfiltered []schema.GroupVersionKind
	Election   LeaderElection
	// LeaseName is the leader election Lease; empty for the server's.
	LeaseName string
	// MetricsAddr and ProbeAddr are the listen addresses of the manager's
	// metrics and probes; empty or "0" turns them off.
	MetricsAddr, ProbeAddr string
}

// NewManagerWith is NewManager with every option.
func NewManagerWith(cfg *rest.Config, o ManagerOptions) (manager.Manager, error) {
	scheme, err := Scheme()
	if err != nil {
		return nil, err
	}
	election := o.Election
	byObject := make(map[client.Object]cache.ByObject, len(o.Cached)+len(o.Unfiltered))
	for _, gvk := range o.Cached {
		byObject[unstructuredOf(gvk)] = cache.ByObject{}
	}
	for _, gvk := range o.Unfiltered {
		byObject[unstructuredOf(gvk)] = cache.ByObject{Label: labels.Everything()}
	}
	lease := o.LeaseName
	if lease == "" {
		lease = leaseName
	}
	mgr, err := manager.New(cfg, manager.Options{
		Scheme: scheme,
		Cache: cache.Options{
			DefaultLabelSelector:        labels.SelectorFromSet(crossplane.Labels("")),
			DefaultTransform:            cache.TransformStripManagedFields(),
			ByObject:                    byObject,
			ReaderFailOnMissingInformer: true,
		},
		Client: client.Options{Cache: &client.CacheOptions{
			Unstructured: true,
			DisableFor:   []client.Object{&corev1.Secret{}},
		}},
		LeaderElection:                election.Enabled,
		LeaderElectionID:              lease,
		LeaderElectionNamespace:       election.Namespace,
		LeaderElectionReleaseOnCancel: true,
		Metrics:                       metricsserver.Options{BindAddress: off(o.MetricsAddr)},
		HealthProbeBindAddress:        off(o.ProbeAddr),
	})
	if err != nil {
		return nil, fmt.Errorf("management cluster manager: %w", err)
	}
	return mgr, nil
}

// off is a listen address, "0" (off) for none.
func off(addr string) string {
	if addr == "" {
		return "0"
	}
	return addr
}

// leaseName is the leader election Lease.
const leaseName = "cluster-vending"

// LeaderElection configures the manager's leader election: runnables that
// need it (the background loop) run on the replica holding the Lease
// leaseName in Namespace. Disabled, they run in every replica.
type LeaderElection struct {
	Enabled   bool
	Namespace string
}

// kindPoll is how often WaitForKinds checks the API server; tests shorten
// it.
var kindPoll = 5 * time.Second

// NewRESTMapper returns a REST mapper for cfg that looks kinds up in the
// API server and again when one is missing, so it sees CRDs installed
// after it was created.
func NewRESTMapper(cfg *rest.Config) (meta.RESTMapper, error) {
	httpClient, err := rest.HTTPClientFor(cfg)
	if err != nil {
		return nil, fmt.Errorf("management cluster client: %w", err)
	}
	mapper, err := apiutil.NewDynamicRESTMapper(cfg, httpClient)
	if err != nil {
		return nil, fmt.Errorf("management cluster API discovery: %w", err)
	}
	return mapper, nil
}

// WaitForKinds waits until the API server serves every kind (their CRDs
// exist). It must run before NewManager: the manager's cache looks every
// cached kind up when it is built and fails on a missing one. A fresh
// management cluster, or one whose server was upgraded to use more
// providers, gets the providers' CRDs a minute or two after the providers
// are installed.
func WaitForKinds(ctx context.Context, mapper meta.RESTMapper, kinds []schema.GroupVersionKind, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		var missing []string
		for _, gvk := range kinds {
			if _, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version); err != nil {
				missing = append(missing, gvk.Kind+"."+gvk.Group)
			}
		}
		if len(missing) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the management cluster does not serve %s (are the Crossplane providers installed and healthy?)",
				strings.Join(missing, ", "))
		case <-time.After(kindPoll):
		}
	}
}

// StartInformers registers an informer per kind (they run once the
// manager starts): with ReaderFailOnMissingInformer, reads only use
// informers registered here.
func StartInformers(ctx context.Context, mgr manager.Manager, cached []schema.GroupVersionKind) error {
	for _, gvk := range cached {
		if _, err := mgr.GetCache().GetInformer(ctx, unstructuredOf(gvk)); err != nil {
			return fmt.Errorf("informer for %s: %w", gvk.Kind, err)
		}
	}
	return nil
}

// GVKs are the kinds of objects, from the scheme.
func GVKs(scheme *runtime.Scheme, objects []client.Object) ([]schema.GroupVersionKind, error) {
	out := make([]schema.GroupVersionKind, 0, len(objects))
	for _, obj := range objects {
		gvk, err := apiutil.GVKForObject(obj, scheme)
		if err != nil {
			return nil, err
		}
		out = append(out, gvk)
	}
	return out, nil
}

func unstructuredOf(gvk schema.GroupVersionKind) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	return u
}
