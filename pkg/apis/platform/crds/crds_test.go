package crds_test

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/yindia/cluster-vending/pkg/apis/platform/crds"
	platformv1alpha1 "github.com/yindia/cluster-vending/pkg/apis/platform/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/kubestore"
)

// Every kind the store uses has an embedded definition, namespaced, in
// the platform's group and version, without a status subresource (a row
// is written as a whole).
func TestEveryStoredKindHasADefinition(t *testing.T) {
	definitions, err := crds.All()
	if err != nil {
		t.Fatal(err)
	}
	byKind := map[string]*unstructured.Unstructured{}
	for _, crd := range definitions {
		kind, _, _ := unstructured.NestedString(crd.Object, "spec", "names", "kind")
		byKind[kind] = crd
	}
	if len(byKind) != len(kubestore.Kinds()) {
		t.Errorf("%d definitions for %d kinds", len(byKind), len(kubestore.Kinds()))
	}
	for _, gvk := range kubestore.Kinds() {
		crd := byKind[gvk.Kind]
		if crd == nil {
			t.Errorf("no definition of %s: run make apis", gvk.Kind)
			continue
		}
		group, _, _ := unstructured.NestedString(crd.Object, "spec", "group")
		scope, _, _ := unstructured.NestedString(crd.Object, "spec", "scope")
		versions, _, _ := unstructured.NestedSlice(crd.Object, "spec", "versions")
		if group != platformv1alpha1.Group || scope != "Namespaced" || len(versions) != 1 {
			t.Errorf("%s: group %s, scope %s, %d versions", gvk.Kind, group, scope, len(versions))
			continue
		}
		version := versions[0].(map[string]any)
		_, hasStatus, _ := unstructured.NestedFieldNoCopy(version, "subresources", "status")
		if version["name"] != platformv1alpha1.Version || hasStatus {
			t.Errorf("%s: version %v, status subresource %t", gvk.Kind, version["name"], hasStatus)
		}
		if !strings.HasPrefix(crd.GetName(), strings.ToLower(gvk.Kind)) {
			t.Errorf("%s is defined as %s", gvk.Kind, crd.GetName())
		}
	}
}

// The protojson documents of a row are stored as they are: their fields
// are not in the schema and must not be pruned.
func TestDocumentsAreNotPruned(t *testing.T) {
	definitions, err := crds.All()
	if err != nil {
		t.Fatal(err)
	}
	documents := map[string][]string{
		"Network":  {"spec", "status", "input"},
		"Cluster":  {"spec", "status", "input", "adoption"},
		"NodePool": {"spec", "status"},
	}
	for _, crd := range definitions {
		kind, _, _ := unstructured.NestedString(crd.Object, "spec", "names", "kind")
		versions, _, _ := unstructured.NestedSlice(crd.Object, "spec", "versions")
		for _, field := range documents[kind] {
			keep, found, _ := unstructured.NestedBool(versions[0].(map[string]any),
				"schema", "openAPIV3Schema", "properties", "spec", "properties", field, "x-kubernetes-preserve-unknown-fields")
			if !found || !keep {
				t.Errorf("%s spec.%s would be pruned", kind, field)
			}
		}
	}
}
