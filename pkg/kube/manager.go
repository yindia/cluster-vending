package kube

import (
	"context"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/labels"
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
// server. The cache holds only the platform's objects (the managed-by
// label), without managedFields, so 100+ clusters' ~5,000 objects stay in
// memory cheaply and reads cost no API calls. Read objects that must be
// current (or that carry no label) with GetAPIReader.
func NewManager(cfg *rest.Config) (manager.Manager, error) {
	scheme, err := Scheme()
	if err != nil {
		return nil, err
	}
	mgr, err := manager.New(cfg, manager.Options{
		Scheme: scheme,
		Cache: cache.Options{
			DefaultLabelSelector: labels.SelectorFromSet(crossplane.Labels("")),
			DefaultTransform:     cache.TransformStripManagedFields(),
		},
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
	})
	if err != nil {
		return nil, fmt.Errorf("management cluster manager: %w", err)
	}
	return mgr, nil
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
