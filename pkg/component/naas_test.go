package component_test

import (
	"slices"
	"testing"

	"github.com/yindia/cluster-vending/pkg/component"
)

func TestNaaSSelectsCapsuleAndItsDependentsWithoutChangingTheCatalog(t *testing.T) {
	entries := []component.PlatformEntry{
		{Definition: component.Definition{Name: "base"}},
		{Definition: component.Definition{Name: "capsule-gcp"}, Install: component.Install{Main: component.Chart{Name: "capsule"}}},
		{Definition: component.Definition{Name: "tenant-bootstrap", Requires: []string{"capsule-gcp"}}},
		{Definition: component.Definition{Name: "tenant-policies", Requires: []string{"tenant-bootstrap"}}},
		{Definition: component.Definition{Name: "mesh", Requires: []string{"base"}}},
	}
	names := func(in []component.PlatformEntry) []string {
		var out []string
		for _, entry := range in {
			out = append(out, entry.Name)
		}
		return out
	}
	if got := names(component.ForNaaS(entries, false)); !slices.Equal(got, []string{"base", "mesh"}) {
		t.Fatalf("NaaS off: %v", got)
	}
	if got := names(component.ForNaaS(entries, true)); !slices.Equal(got, names(entries)) {
		t.Fatalf("NaaS on: %v", got)
	}
	if len(entries) != 5 || entries[1].Name != "capsule-gcp" {
		t.Fatal("cluster selection changed the shared catalog")
	}
	// A definition with the canonical name remains gated even when an operator
	// changes its chart source.
	canonical := []component.PlatformEntry{{Definition: component.Definition{Name: "capsule"}}}
	if len(component.ForNaaS(canonical, false)) != 0 {
		t.Fatal("canonical Capsule definition installed without NaaS")
	}
}
