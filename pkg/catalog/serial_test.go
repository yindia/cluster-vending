package catalog_test

import (
	"slices"
	"strconv"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/buildserial"
	"github.com/yindia/cluster-vending/pkg/catalog"
)

// asNewerBuild makes d what a newer build shipped: stamped with a serial
// above this build's, with another spec.
func asNewerBuild(d *cloudv1alpha1.ComponentDefinition) *cloudv1alpha1.ComponentDefinition {
	d.Spec.Description = "from a newer build"
	d.Labels = map[string]string{cloudv1alpha1.LabelShipped: "true"}
	d.Annotations = map[string]string{catalog.ShippedHashAnnotation: catalog.SpecHash(d.Spec),
		buildserial.Annotation: strconv.FormatInt(buildserial.Serial+1, 10)}
	return d
}

// An older build (a rollback, an old pod) does not revert what a newer
// build shipped: a definition stamped with a newer serial is neither
// replaced with the older spec nor deleted when the older build does not
// have it. The refusal fails the ready check. What it creates or replaces
// is stamped with its serial.
func TestSyncRefusesToDowngradeTheCatalog(t *testing.T) {
	t.Cleanup(buildserial.ResetForTest)
	defs, err := catalog.Definitions()
	if err != nil {
		t.Fatal(err)
	}
	newer := asNewerBuild(defs[0].DeepCopy())
	onlyNewer := asNewerBuild(&cloudv1alpha1.ComponentDefinition{})
	onlyNewer.Name, onlyNewer.Spec = "added-by-a-newer-build", *defs[1].Spec.DeepCopy()
	onlyNewer.Annotations[catalog.ShippedHashAnnotation] = catalog.SpecHash(onlyNewer.Spec)
	c := newClient(t, newer, onlyNewer)

	res, err := catalog.Sync(t.Context(), c, defs)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(res.Newer, newer.Name) || !slices.Contains(res.Newer, onlyNewer.Name) || slices.Contains(res.Updated, newer.Name) ||
		slices.Contains(res.Deleted, onlyNewer.Name) {
		t.Fatalf("sync = %+v; want %s and %s left as newer", res, newer.Name, onlyNewer.Name)
	}
	got := &cloudv1alpha1.ComponentDefinition{}
	if err := c.Get(t.Context(), client.ObjectKey{Name: newer.Name}, got); err != nil || got.Spec.Description != "from a newer build" {
		t.Fatalf("%s = %q (%v); want the newer build's", newer.Name, got.Spec.Description, err)
	}
	if err := c.Get(t.Context(), client.ObjectKey{Name: onlyNewer.Name}, got); err != nil {
		t.Fatalf("%s deleted: %v", onlyNewer.Name, err)
	}
	if err := buildserial.Check(nil); err == nil {
		t.Fatal("the ready check passes after a refused downgrade")
	}
	if err := c.Get(t.Context(), client.ObjectKey{Name: defs[1].Name}, got); err != nil ||
		buildserial.Of(got.Annotations) != buildserial.Serial {
		t.Fatalf("created %s with serial %d (%v), want %d", defs[1].Name, buildserial.Of(got.Annotations), err, buildserial.Serial)
	}
}
