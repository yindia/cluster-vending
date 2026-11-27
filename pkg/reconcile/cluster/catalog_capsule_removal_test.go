package cluster

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/crossplane"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// capsuleClusterWorld is catalogClusterWorld with capsule in mesh's place,
// as a build with NaaS left a cluster that enabled it: the shipped Capsule
// definition (role NamespaceTenancy) next to base, both installed, Ready
// and fingerprinted.
func capsuleClusterWorld(t *testing.T) (world, *catalogPlatform, *model.Cluster) {
	t.Helper()
	w, src := platformWorld(t)
	capsule := platformDef("capsule")
	capsule.Spec.Role = "NamespaceTenancy"
	src.defs[0] = capsule // was mesh
	src.release.Spec.Content.Kubernetes[0].Components = []cloudv1alpha1.ComponentPin{
		{Name: "base", Version: "1.2.0"}, {Name: "capsule", Version: "0.15.2"}}
	cp := &catalogPlatform{fakePlatform: src, admitted: true}
	w.svc.deps.Platform = cp
	w.helmConfig(t)
	row := w.cluster(t)
	spec, err := SpecOf(row)
	if err != nil {
		t.Fatal(err)
	}
	spec.ReleaseChannel = cloudv1.ReleaseChannel_RELEASE_CHANNEL_STABLE
	if row.Spec, err = Encode(spec); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		row = w.pass(t, row)
		for _, name := range []string{"base", "capsule"} {
			if w.release(t, name) != nil {
				w.deployed(t, name)
			}
		}
	}
	row = w.pass(t, row)
	if got := phases(storedPlatform(row)); got != "base=READY,capsule=READY" {
		t.Fatalf("installed: %s", got)
	}
	if applied := catalogOf(row).GetApplied(); applied["capsule"] == "" {
		t.Fatalf("capsule not fingerprinted: %v", applied)
	}
	return w, cp, row
}

// componentReleases are the management cluster's Helm Releases labelled for
// component: what holds its definition (component.HoldingClusters).
func (w world) componentReleases(t *testing.T, component string) []unstructured.Unstructured {
	t.Helper()
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(crossplaneaws.HelmReleaseGVK.GroupVersion().WithKind(crossplaneaws.HelmReleaseGVK.Kind + "List"))
	if err := w.kube.List(t.Context(), list, client.MatchingLabels{crossplane.LabelComponent: component}); err != nil {
		t.Fatal(err)
	}
	return list.Items
}

// How Capsule leaves a running cluster that has it, once this build
// starts: catalog.Sync deletes the shipped definition (no longer in the
// build; the in-use finalizer holds it) and the migration clears its role
// (RetireNamespaceTenancy). The cluster then has a catalog removal pending:
// until the channel's code rollout admits it, Capsule keeps running; once
// admitted, its Helm release is uninstalled (the chart's post-delete hook
// removes Capsule's webhook configurations) and nothing labelled for it is
// left, so the definition's finalizer is released. base is untouched.
func TestCapsuleIsRemovedFromARunningClusterThroughTheGatedCatalogChange(t *testing.T) {
	w, cp, row := capsuleClusterWorld(t)
	now := metav1.Now()
	cp.defs[0].DeletionTimestamp = &now
	cp.defs[0].Spec.Role = ""
	cp.admitted = false

	row = w.pass(t, row)

	if c := catalogOf(row); c.GetPhase() != catalogPending || w.release(t, "capsule") == nil {
		t.Fatalf("not admitted: catalog change %v, capsule release kept %v; want pending and kept", c, w.release(t, "capsule") != nil)
	}
	if u := w.release(t, "capsule"); !u.GetDeletionTimestamp().IsZero() {
		t.Error("while pending: the capsule release is being deleted, want it kept")
	}

	cp.admitted = true
	for i := 0; i < 6 && catalogOf(row).GetPhase() != catalogDone; i++ {
		row = w.pass(t, row)
	}

	c := catalogOf(row)
	if c.GetPhase() != catalogDone || c.GetApplied()["capsule"] != "" || c.GetApplied()["base"] == "" {
		t.Fatalf("catalog change = %v", c)
	}
	if w.release(t, "capsule") != nil || w.release(t, "base") == nil {
		t.Fatalf("capsule release %v, base release %v: want capsule removed, base kept",
			w.release(t, "capsule") != nil, w.release(t, "base") != nil)
	}
	if left := w.componentReleases(t, "capsule"); len(left) != 0 {
		t.Errorf("%d Releases still labelled for capsule hold its definition", len(left))
	}
	if got := phases(storedPlatform(row)); got != "base=READY" {
		t.Errorf("components after the removal: %s", got)
	}
}
