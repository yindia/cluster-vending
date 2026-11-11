package kube

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
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
// with GetAPIReader. Start the informers with StartInformers.
func NewManager(cfg *rest.Config, cached []schema.GroupVersionKind, election LeaderElection) (manager.Manager, error) {
	scheme, err := Scheme()
	if err != nil {
		return nil, err
	}
	byObject := make(map[client.Object]cache.ByObject, len(cached))
	for _, gvk := range cached {
		byObject[unstructuredOf(gvk)] = cache.ByObject{}
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
		LeaderElectionID:              leaseName,
		LeaderElectionNamespace:       election.Namespace,
		LeaderElectionReleaseOnCancel: true,
		Metrics:                       metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress:        "0",
	})
	if err != nil {
		return nil, fmt.Errorf("management cluster manager: %w", err)
	}
	return mgr, nil
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

// kindPoll is how often WaitForKinds checks the API server.
const kindPoll = 5 * time.Second

// WaitForKinds waits until the API server serves every kind of objects
// (their CRDs exist): informers on a missing kind fail. A fresh management
// cluster gets the providers' CRDs a minute or two after the providers
// are installed.
func WaitForKinds(ctx context.Context, mgr manager.Manager, objects []client.Object, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		var missing []string
		for _, obj := range objects {
			gvk, err := apiutil.GVKForObject(obj, mgr.GetScheme())
			if err != nil {
				return err
			}
			if _, err := mgr.GetRESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version); err != nil {
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
