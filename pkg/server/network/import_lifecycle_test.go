package network

import (
	"context"
	"slices"
	"sync"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// deletedPolicies records, for every object deleted from the management
// cluster, the management policies it had at that moment: what its
// provider then does with the cloud resource.
type deletedPolicies struct {
	mu       sync.Mutex
	policies map[string][]string
}

func (d *deletedPolicies) funcs() interceptor.Funcs {
	return interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
		live := &unstructured.Unstructured{}
		if gvks, _, err := c.Scheme().ObjectKinds(obj); err == nil && len(gvks) > 0 {
			live.SetGroupVersionKind(gvks[0])
		} else {
			live.SetGroupVersionKind(obj.GetObjectKind().GroupVersionKind())
		}
		if err := c.Get(ctx, client.ObjectKeyFromObject(obj), live); err == nil {
			p, _, _ := unstructured.NestedStringSlice(live.Object, "spec", "managementPolicies")
			d.mu.Lock()
			d.policies[live.GetKind()+"/"+live.GetName()] = p
			d.mu.Unlock()
		}
		return c.Delete(ctx, obj, opts...)
	}}
}

// readOnly reports whether policies only read the cloud resource.
func readOnly(policies []string) bool {
	return len(policies) > 0 && !slices.ContainsFunc(policies, func(p string) bool { return p != "Observe" && p != "LateInitialize" })
}

// Deleting a network that was imported and never taken over removes the
// platform's objects and nothing in the cloud; deleting one that was taken
// over deletes its resources, as the owner's.
func TestDeletingAnImportedNetwork(t *testing.T) {
	tests := []struct {
		name     string
		takeOver bool
	}{
		{"imported only: the cloud is left alone", false},
		{"taken over: the resources are the platform's to delete", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &deletedPolicies{policies: map[string][]string{}}
			f := newFixture(t, d.funcs())
			f.svc.deps.AWSDiscovery = fakeDiscoverer{network: existingVPC()}
			n := f.imported(t)
			if tt.takeOver {
				if _, err := f.svc.TakeOverNetwork(t.Context(), &cloudv1.TakeOverNetworkRequest{TenantId: tenant, NetworkId: n.GetId(),
					AcknowledgeNotImported: true}); err != nil {
					t.Fatalf("TakeOverNetwork: %v", err)
				}
				f.reconcile(t, n.GetId())
			}
			recorded := len(f.status(t, n.GetId()).GetStatus().GetManagedResources())
			if recorded == 0 {
				t.Fatal("the import recorded no objects")
			}

			if _, err := f.svc.DeleteNetwork(t.Context(), &cloudv1.DeleteNetworkRequest{TenantId: tenant, NetworkId: n.GetId()}); err != nil {
				t.Fatalf("DeleteNetwork: %v", err)
			}
			for range 12 {
				if err := f.svc.Reconcile(t.Context(), tenant, n.GetId()); err != nil {
					t.Fatalf("Reconcile: %v", err)
				}
			}

			if len(d.policies) != recorded {
				t.Errorf("%d objects deleted, %d were recorded", len(d.policies), recorded)
			}
			for name, got := range d.policies {
				if tt.takeOver == readOnly(got) {
					t.Errorf("%s was deleted with managementPolicies %v", name, got)
				}
			}
		})
	}
}
