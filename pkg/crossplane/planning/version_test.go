package planning

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

// A plan binds to what a controller does not change while it works: status
// writes bump the resourceVersion, not the generation or the spec.
func TestObjectVersionIgnoresResourceVersionAndStatus(t *testing.T) {
	live := resource("eks.aws.m.upbound.io/v1beta1", "tenant-acme", "prod")
	live.SetUID(types.UID("u-1"))
	live.SetGeneration(3)
	live.SetResourceVersion("100")
	before := ObjectVersion(live)

	touched := live.DeepCopy()
	touched.SetResourceVersion("101")
	if err := unstructured.SetNestedField(touched.Object, "UPDATING", "status", "atProvider", "status"); err != nil {
		t.Fatal(err)
	}
	if got := ObjectVersion(touched); got != before {
		t.Errorf("a status write changed the version: %s, was %s", got, before)
	}

	respec := live.DeepCopy()
	if err := unstructured.SetNestedField(respec.Object, int64(5), "spec", "forProvider", "initialNodeCount"); err != nil {
		t.Fatal(err)
	}
	if ObjectVersion(respec) == before {
		t.Error("a spec change did not change the version")
	}
	regen := live.DeepCopy()
	regen.SetGeneration(4)
	if ObjectVersion(regen) == before {
		t.Error("a new generation did not change the version")
	}
}
