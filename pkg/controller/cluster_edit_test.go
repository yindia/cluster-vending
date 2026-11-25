package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	platform "github.com/yindia/cluster-vending/pkg/reconcile"
	reconcilecluster "github.com/yindia/cluster-vending/pkg/reconcile/cluster"
)

// settledCluster is a hand-made cluster that is adopted and applied, at
// object generation 1 with that generation observed. (The fake API server
// does not count generations: the tests do what the API server does.)
func (w world) settledCluster(t *testing.T) (*ClusterReconciler, string) {
	t.Helper()
	w.readyNetwork(t)
	w.must(t, w.k.Create(t.Context(), handMadeCluster(t, "prod", "net")))
	r := NewClusterReconciler(w.deps)
	for range 2 {
		_, err := r.Reconcile(t.Context(), request("prod"))
		w.must(t, err)
	}
	obj := w.cluster(t, "prod")
	id := obj.Labels[cloudstore.LabelID]
	obj.Generation = 1
	w.must(t, w.k.Update(t.Context(), obj))
	obj = w.cluster(t, "prod")
	obj.Status.ObservedGeneration = 1
	w.must(t, w.k.Status().Update(t.Context(), obj))
	_, err := r.Reconcile(t.Context(), request("prod"))
	w.must(t, err)
	return r, id
}

// edit changes the Cluster object's spec as kubectl does, and counts the
// generation.
func (w world) edit(t *testing.T, generation int64, change func(*cloudv1alpha1.Cluster)) {
	t.Helper()
	obj := w.cluster(t, "prod")
	change(obj)
	obj.Generation = generation
	w.must(t, w.k.Update(t.Context(), obj))
}

func (w world) pinned(t *testing.T, id string) map[string]string {
	t.Helper()
	row, err := w.store.Clusters().Get(t.Context(), tenant, id)
	w.must(t, err)
	input := &cloudv1.Cluster{}
	w.must(t, reconcilecluster.Lenient.Unmarshal(row.Input, input))
	return reconcilecluster.PinnedAddons(input.GetSpec())
}

// editedCIDR is the public access CIDR editCIDRs sets.
const editedCIDR = "198.51.100.0/24"

// editCIDRs is a kubectl edit of the public endpoint's CIDRs.
func editCIDRs(o *cloudv1alpha1.Cluster) {
	o.Spec.ControlPlane.EndpointAccess.PublicAccessCIDRs = []string{editedCIDR}
}

// publicCIDR is the first public access CIDR of the stored spec.
func publicCIDR(t *testing.T, row *model.Cluster) string {
	t.Helper()
	spec, err := reconcilecluster.SpecOf(row)
	if err != nil {
		t.Fatal(err)
	}
	cidrs := spec.GetControlPlane().GetEndpointAccess().GetPublicAccessCidrs()
	if len(cidrs) == 0 {
		return ""
	}
	return cidrs[0]
}

func TestKubectlEditKeepsAddonPinsAndDropsRemovedOnes(t *testing.T) {
	w := newWorld(t)
	r, id := w.settledCluster(t)
	if len(w.pinned(t, id)) != 0 {
		t.Fatalf("pins before any edit: %v", w.pinned(t, id))
	}
	w.edit(t, 2, func(o *cloudv1alpha1.Cluster) {
		o.Spec.AWS.Addons = []cloudv1alpha1.AWSAddon{{Name: "coredns", Version: "v1.11.4-eksbuild.2"}, {Name: "kube-proxy"}}
	})
	_, err := r.Reconcile(t.Context(), request("prod"))
	w.must(t, err)
	if got := w.pinned(t, id); len(got) != 1 || got["coredns"] != "v1.11.4-eksbuild.2" {
		t.Fatalf("pins after pinning coredns by kubectl = %v", got)
	}
	// A removed pin falls back to the recommended version.
	w.edit(t, 3, func(o *cloudv1alpha1.Cluster) {
		o.Spec.AWS.Addons = []cloudv1alpha1.AWSAddon{{Name: "coredns"}, {Name: "kube-proxy"}}
	})
	_, err = r.Reconcile(t.Context(), request("prod"))
	w.must(t, err)
	if got := w.pinned(t, id); len(got) != 0 {
		t.Fatalf("pins after clearing the version = %v, want none", got)
	}
}

func TestKubectlEditIsBumpedAppliedAndObserved(t *testing.T) {
	w := newWorld(t)
	r, id := w.settledCluster(t)
	before, err := w.store.Clusters().Get(t.Context(), tenant, id)
	w.must(t, err)
	w.edit(t, 2, editCIDRs)
	_, err = r.Reconcile(t.Context(), request("prod"))
	w.must(t, err)
	row, err := w.store.Clusters().Get(t.Context(), tenant, id)
	w.must(t, err)
	if row.Generation != before.Generation+1 || publicCIDR(t, row) != editedCIDR {
		t.Fatalf("generation %d (was %d), public CIDR %q: the edit was not applied", row.Generation, before.Generation, publicCIDR(t, row))
	}
	if obj := w.cluster(t, "prod"); edited(obj.Generation, obj.Status.ObservedGeneration) {
		t.Fatalf("the edit still counts as not seen: generation %d, observed %d", obj.Generation, obj.Status.ObservedGeneration)
	}
}

func TestKubectlEditWhileFrozenIsAppliedAfterUnfreeze(t *testing.T) {
	w := newWorld(t)
	r, id := w.settledCluster(t)
	before, err := w.store.Clusters().Get(t.Context(), tenant, id)
	w.must(t, err)
	hold := &cloudv1alpha1.ReleaseHold{ObjectMeta: metav1.ObjectMeta{Name: platform.FreezeName},
		Spec: cloudv1alpha1.ReleaseHoldSpec{Platform: true, Reason: "maintenance", By: "operator"}}
	w.must(t, w.k.Create(t.Context(), hold))
	w.edit(t, 2, editCIDRs)
	for range 3 {
		_, err := r.Reconcile(t.Context(), request("prod"))
		w.must(t, err)
	}
	if obj := w.cluster(t, "prod"); !edited(obj.Generation, obj.Status.ObservedGeneration) {
		t.Fatalf("a frozen pass observed the edit: generation %d, observed %d", obj.Generation, obj.Status.ObservedGeneration)
	}
	row, err := w.store.Clusters().Get(t.Context(), tenant, id)
	w.must(t, err)
	if row.Generation != before.Generation || publicCIDR(t, row) != editedCIDR {
		t.Fatalf("frozen: generation %d (was %d), public CIDR %q", row.Generation, before.Generation, publicCIDR(t, row))
	}
	w.must(t, w.k.Delete(t.Context(), &cloudv1alpha1.ReleaseHold{ObjectMeta: metav1.ObjectMeta{Name: platform.FreezeName}}))
	for range 2 {
		_, err := r.Reconcile(t.Context(), request("prod"))
		w.must(t, err)
	}
	row, err = w.store.Clusters().Get(t.Context(), tenant, id)
	w.must(t, err)
	if row.Generation != before.Generation+1 || publicCIDR(t, row) != editedCIDR {
		t.Fatalf("after the unfreeze: generation %d (was %d), public CIDR %q: the edit was lost", row.Generation, before.Generation, publicCIDR(t, row))
	}
	if obj := w.cluster(t, "prod"); edited(obj.Generation, obj.Status.ObservedGeneration) {
		t.Fatalf("the edit is not observed after the unfreeze: generation %d, observed %d", obj.Generation, obj.Status.ObservedGeneration)
	}
}

var _ client.Object = (*cloudv1alpha1.Cluster)(nil)
