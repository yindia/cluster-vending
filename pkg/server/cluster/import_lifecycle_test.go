package cluster

import (
	"context"
	"slices"
	"sync"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/kube"
	recon "github.com/yindia/cluster-vending/pkg/reconcile"
)

// Every kind an import or a takeover records is one the controller's
// informer cache holds (crossplaneaws.ManagedObjects and the Helm
// ProviderConfig, see management.CachedObjects): the cache refuses reads
// of any other kind, which would block the cluster's status and deletion.
func TestImportAndTakeoverRecordOnlyCachedKinds(t *testing.T) {
	w := importWorld(t, terraformCluster())
	c := takenOver(t, w)
	cached, err := kube.GVKs(w.kube.Scheme(), crossplaneaws.ManagedObjects())
	if err != nil {
		t.Fatal(err)
	}
	cached = append(cached, crossplaneaws.HelmProviderConfigGVK)
	records, err := w.store.ManagedResources().ListByOwner(t.Context(), model.OwnerCluster, c.GetId())
	if err != nil || len(records) == 0 {
		t.Fatalf("%d records, %v", len(records), err)
	}

	for _, r := range records {
		gvk := schema.FromAPIVersionAndKind(r.APIVersion, r.Kind)
		if !slices.Contains(cached, gvk) {
			t.Errorf("%s %s is recorded and its kind %s is not cached", r.Kind, r.Name, gvk)
		}
	}
}

// deleted records, for every object deleted from the management cluster,
// the management policies it had at that moment: what its provider then
// does with the cloud resource.
type deleted struct {
	mu       sync.Mutex
	policies map[string][]string
}

func (d *deleted) funcs() interceptor.Funcs {
	return interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
		live := &unstructured.Unstructured{}
		live.SetGroupVersionKind(obj.GetObjectKind().GroupVersionKind())
		if gvks, _, err := c.Scheme().ObjectKinds(obj); err == nil && len(gvks) > 0 {
			live.SetGroupVersionKind(gvks[0])
		}
		if err := c.Get(ctx, client.ObjectKeyFromObject(obj), live); err == nil {
			d.mu.Lock()
			d.policies[live.GetKind()+"/"+live.GetName()] = policies(live)
			d.mu.Unlock()
		}
		return c.Delete(ctx, obj, opts...)
	}}
}

// Deleting a cluster that was imported and never taken over removes the
// platform's objects and nothing in the cloud: every object is deleted
// while it only observes.
func TestDeletingAnImportedClusterLeavesTheCloudAlone(t *testing.T) {
	d := &deleted{policies: map[string][]string{}}
	w := newWorld(t, d.funcs(), nil, withVersions, func(deps *recon.Deps) {
		deps.AWSClusterDiscovery = fakeClusterDiscoverer{cluster: terraformCluster()}
	})
	ctx := t.Context()
	c := importTF(t, w)
	imported := objectsOf(t, w, c.GetId())
	if len(imported) == 0 {
		t.Fatal("the import created no objects")
	}

	if _, err := w.svc.DeleteCluster(ctx, &cloudv1.DeleteClusterRequest{TenantId: worldTenant, ClusterId: c.GetId()}); err != nil {
		t.Fatalf("DeleteCluster: %v", err)
	}
	for range 5 {
		reconcile(t, w, c.GetId())
	}

	if left := objectsOf(t, w, c.GetId()); len(left) != 0 {
		t.Errorf("%d objects left after the deletion", len(left))
	}
	for name := range imported {
		got, ok := d.policies[name]
		if !ok {
			t.Errorf("%s was not deleted", name)
			continue
		}
		// Observe and LateInitialize read; anything else (Create, Update,
		// Delete, "*", or no policy at all: full management) writes.
		if len(got) == 0 || slices.ContainsFunc(got, func(p string) bool { return p != "Observe" && p != "LateInitialize" }) {
			t.Errorf("%s was deleted with managementPolicies %v: its cloud resource would be deleted", name, got)
		}
	}
	if _, err := w.store.Clusters().Get(ctx, worldTenant, c.GetId()); err == nil {
		t.Error("the cluster's row is still there")
	}
}

// takenOver imports the Terraform cluster, takes it over and lets it
// converge: every object Ready, the cluster RUNNING.
func takenOver(t *testing.T, w world) *cloudv1.Cluster {
	t.Helper()
	ctx := t.Context()
	c := importTF(t, w)
	if _, err := w.svc.TakeOverCluster(ctx, &cloudv1.TakeOverClusterRequest{TenantId: worldTenant, ClusterId: c.GetId(),
		AcknowledgeNotImported: true}); err != nil {
		t.Fatalf("TakeOverCluster: %v", err)
	}
	reconcile(t, w, c.GetId())
	for _, u := range objectsOf(t, w, c.GetId()) {
		conds := []any{map[string]any{"type": "Ready", "status": "True"},
			map[string]any{"type": "Synced", "status": "True", "observedGeneration": u.GetGeneration()}}
		if err := unstructured.SetNestedSlice(u.Object, conds, "status", "conditions"); err != nil {
			t.Fatal(err)
		}
		if u.GetKind() == "Cluster" {
			_ = unstructured.SetNestedField(u.Object, "ACTIVE", "status", "atProvider", "status")
		}
		if err := w.kube.Update(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	for range 3 {
		reconcile(t, w, c.GetId())
	}
	got := live(t, w, c.GetId())
	if got.GetStatus().GetState() != cloudv1.ClusterState_CLUSTER_STATE_RUNNING {
		t.Fatalf("after the takeover: %s (%s)", got.GetStatus().GetState(), got.GetStatus().GetMessage())
	}
	return got
}

// A takeover creates no second cloud resource: what the import observed
// keeps its cloud identity, and what is new is either no cloud resource
// or of a kind the plan announced it would create.
func TestTakeoverCreatesNoSecondCloudResource(t *testing.T) {
	w := importWorld(t, terraformCluster())
	c := importTF(t, w)
	before := objectsOf(t, w, c.GetId())
	announced := map[string]bool{}
	for _, l := range c.GetStatus().GetImport().GetResources() {
		if l.GetDisposition() == cloudv1.Disposition_DISPOSITION_WILL_CREATE {
			announced[l.GetKind()] = true
		}
	}

	if _, err := w.svc.TakeOverCluster(t.Context(), &cloudv1.TakeOverClusterRequest{TenantId: worldTenant, ClusterId: c.GetId(),
		AcknowledgeNotImported: true}); err != nil {
		t.Fatalf("TakeOverCluster: %v", err)
	}
	reconcile(t, w, c.GetId())

	// Not cloud resources: they protect, or write the kubeconfig Secret.
	local := map[string]bool{"Usage": true, "ClusterAuth": true}
	for name, now := range objectsOf(t, w, c.GetId()) {
		was, existed := before[name]
		switch {
		case existed:
			a, b := was.GetAnnotations()[crossplaneaws.ExternalNameAnnotation], now.GetAnnotations()[crossplaneaws.ExternalNameAnnotation]
			if a != b {
				t.Errorf("%s: external name %q became %q: another cloud resource", name, a, b)
			}
		case local[now.GetKind()], announced[now.GetKind()]:
		default:
			t.Errorf("%s was created at the takeover and the plan did not say so", name)
		}
	}
}

// After a takeover a deleted object is created again with the cloud
// identity it adopted, so its provider finds the resource that is there.
// That also holds for an object whose ID only the cloud knows (the key):
// its identity is recorded, so it is created again with it, not as a
// second key.
func TestRepairAfterATakeoverKeepsTheAdoptedIdentity(t *testing.T) {
	w := importWorld(t, terraformCluster())
	ctx := t.Context()
	c := takenOver(t, w)
	groups := objectsOfKind(objectsOf(t, w, c.GetId()), "NodeGroup")
	if len(groups) != 1 {
		t.Fatalf("%d node groups, want the adopted one", len(groups))
	}
	group := groups[0]
	if got := group.GetAnnotations()[crossplaneaws.ExternalNameAnnotation]; got != "default-ng" {
		t.Fatalf("the adopted node group's external name = %q", got)
	}

	if err := w.kube.Delete(ctx, group); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		reconcile(t, w, c.GetId())
	}

	again := objectsOf(t, w, c.GetId())["NodeGroup/"+group.GetName()]
	if again == nil {
		t.Fatal("the deleted node group was not created again")
	}
	if got := again.GetAnnotations()[crossplaneaws.ExternalNameAnnotation]; got != "default-ng" {
		t.Errorf("recreated with external name %q, want the adopted default-ng: a second node group would be made", got)
	}

	// The key's ID is AWS's: it is created again with the recorded one.
	keys := objectsOfKind(objectsOf(t, w, c.GetId()), "Key")
	if len(keys) != 1 {
		t.Fatalf("%d keys, want the carried-over one", len(keys))
	}
	keyID := keys[0].GetAnnotations()[crossplaneaws.ExternalNameAnnotation]
	if keyID == "" {
		t.Fatal("the carried-over key has no external name")
	}
	if err := w.kube.Delete(ctx, keys[0]); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := w.svc.Reconcile(ctx, worldTenant, c.GetId()); err != nil {
			t.Fatal(err)
		}
	}
	key := objectsOf(t, w, c.GetId())["Key/"+keys[0].GetName()]
	if key == nil {
		t.Fatal("the deleted key was not created again")
	}
	if got := key.GetAnnotations()[crossplaneaws.ExternalNameAnnotation]; got != keyID {
		t.Errorf("key recreated with external name %q, want %q: a second key would be made", got, keyID)
	}
}

// An imported cluster that was not taken over gets a deleted object back,
// observing the same resource: with its cloud identity and only Observe
// policies, exactly as the import made it.
func TestRepairOfAnImportedClusterObservesTheSameResource(t *testing.T) {
	w := importWorld(t, terraformCluster())
	ctx := t.Context()
	c := importTF(t, w)
	reconcile(t, w, c.GetId())
	before := objectsOf(t, w, c.GetId())
	keys := objectsOfKind(before, "Key")
	if len(keys) != 1 {
		t.Fatalf("%d keys, want the imported one", len(keys))
	}
	was := keys[0]
	if err := w.kube.Delete(ctx, was); err != nil {
		t.Fatal(err)
	}

	for range 2 {
		reconcile(t, w, c.GetId())
	}

	now := objectsOf(t, w, c.GetId())["Key/"+was.GetName()]
	if now == nil {
		t.Fatal("the deleted object of the imported cluster was not created again")
	}
	if a, b := was.GetAnnotations()[crossplaneaws.ExternalNameAnnotation], now.GetAnnotations()[crossplaneaws.ExternalNameAnnotation]; a == "" || a != b {
		t.Errorf("external name %q became %q", a, b)
	}
	if got := policies(now); !slices.Equal(got, policies(was)) || slices.ContainsFunc(got, func(p string) bool { return p != "Observe" && p != "LateInitialize" }) {
		t.Errorf("recreated with managementPolicies %v, want the import's %v (read-only)", got, policies(was))
	}
	if st := storedCluster(t, w, c.GetId()).GetStatus(); st.GetState() != cloudv1.ClusterState_CLUSTER_STATE_IMPORTED {
		t.Errorf("state = %s, want IMPORTED still", st.GetState())
	}
}
