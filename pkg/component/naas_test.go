package component_test

import (
	"slices"
	"testing"

	"github.com/yindia/cluster-vending/pkg/component"
)

func TestNaaSSelectsTheNamespaceTenancyRoleAndItsDependentsWithoutChangingTheCatalog(t *testing.T) {
	entries := []component.PlatformEntry{
		{Definition: component.Definition{Name: "base"}},
		// The role, not the name or the chart, gates it.
		{Definition: component.Definition{Name: "tenancy-gcp", Role: component.RoleNamespaceTenancy}, Install: component.Install{Main: component.Chart{Name: "other"}}},
		{Definition: component.Definition{Name: "tenant-bootstrap", Requires: []string{"tenancy-gcp"}}},
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
	if len(entries) != 5 || entries[1].Name != "tenancy-gcp" {
		t.Fatal("cluster selection changed the shared catalog")
	}
	// A definition stored before spec.role (an edited shipped one, which a
	// build does not update) is still gated: by the shipped name, or by
	// the Capsule chart.
	for _, legacy := range []component.Definition{{Name: "capsule"}, {Name: "capsule-gcp", Chart: "oci://ghcr.io/projectcapsule/charts capsule"}} {
		if len(component.ForNaaS([]component.PlatformEntry{{Definition: legacy}}, false)) != 0 {
			t.Fatalf("legacy Capsule definition %s installed without NaaS", legacy.Name)
		}
	}
	// Once a definition declares the role, the legacy name is ordinary.
	declared := []component.PlatformEntry{
		{Definition: component.Definition{Name: "capsule"}},
		{Definition: component.Definition{Name: "tenancy", Role: component.RoleNamespaceTenancy}},
	}
	if got := names(component.ForNaaS(declared, false)); !slices.Equal(got, []string{"capsule"}) {
		t.Fatalf("with a declared NamespaceTenancy: %v", got)
	}
}
