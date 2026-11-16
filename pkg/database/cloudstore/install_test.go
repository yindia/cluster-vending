package cloudstore

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/yindia/cluster-vending/config/crd"
)

// Startup readiness must wait for every embedded platform CRD to be served.
// Otherwise a newly added kind can fail requests after /readyz becomes healthy.
func TestStartupKindsCoverEmbeddedDefinitions(t *testing.T) {
	definitions, err := crd.All()
	if err != nil {
		t.Fatal(err)
	}
	wanted := make(map[string]bool, len(definitions))
	for _, definition := range definitions {
		kind, _, err := unstructured.NestedString(definition.Object, "spec", "names", "kind")
		if err != nil {
			t.Fatal(err)
		}
		wanted[kind] = true
	}
	for _, gvk := range Kinds() {
		if !wanted[gvk.Kind] {
			t.Errorf("startup waits for unknown or duplicate kind %s", gvk)
		}
		delete(wanted, gvk.Kind)
	}
	for kind := range wanted {
		t.Errorf("startup does not wait for embedded kind %s", kind)
	}
}
