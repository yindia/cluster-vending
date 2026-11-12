package crd_test

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/yindia/cluster-vending/controller/config/crd"
)

// The server installs what is embedded: the three kinds, each with its
// status subresource, and the contract status kept as it is written.
func TestEmbeddedDefinitions(t *testing.T) {
	definitions, err := crd.All()
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]*unstructured.Unstructured{}
	for _, d := range definitions {
		kind, _, _ := unstructured.NestedString(d.Object, "spec", "names", "kind")
		kinds[kind] = d
	}
	if len(definitions) != 3 || kinds["CloudAccount"] == nil || kinds["Network"] == nil || kinds["Cluster"] == nil {
		t.Fatalf("%d definitions of kinds %v, want CloudAccount, Network and Cluster", len(definitions), kinds)
	}
	for _, kind := range []string{"Network", "Cluster"} {
		versions, _, _ := unstructured.NestedSlice(kinds[kind].Object, "spec", "versions")
		if len(versions) != 1 {
			t.Fatalf("%s: %d versions", kind, len(versions))
		}
		version := versions[0].(map[string]any)
		if _, served, _ := unstructured.NestedMap(version, "subresources", "status"); !served {
			t.Errorf("%s has no status subresource", kind)
		}
		contract, found, _ := unstructured.NestedMap(version, "schema", "openAPIV3Schema", "properties", "status", "properties", "contractStatus")
		if !found || contract["x-kubernetes-preserve-unknown-fields"] != true {
			t.Errorf("%s status.contractStatus = %v, want a field that keeps what it is given", kind, contract)
		}
	}
}
