package storetest

import (
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/kube"
)

// NewClient returns a fake API server that knows the platform's kinds,
// with their status subresources, for a store in a test. Each call is a
// new, empty one: a test's store is not the client its code under test
// writes Crossplane objects with, so interceptors on that one (a test that
// a handler writes nothing to the management cluster) do not see the
// store's writes.
func NewClient(t *testing.T) client.Client {
	t.Helper()
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&cloudv1alpha1.CloudAccount{}, &cloudv1alpha1.Network{}, &cloudv1alpha1.Cluster{},
			&cloudv1alpha1.Component{}).Build()
}

// NewStore returns the store on the platform's kinds (pkg/database/
// cloudstore) over a new fake API server (NewClient): what a test that
// needs a store uses.
func NewStore(t *testing.T) repository.Store { return cloudstore.New(NewClient(t)) }
