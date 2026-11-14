package component

import (
	"slices"
	"testing"
)

// What keeps a definition from being deleted.
func TestHeldBy(t *testing.T) {
	deleting := func(defs []Definition, name string) []Definition {
		out := slices.Clone(defs)
		for i := range out {
			if out[i].Name == name {
				out[i].Deleting = true
			}
		}
		return out
	}
	tests := []struct {
		name    string
		catalog []Definition
		def     string
		use     Use
		want    []string
	}{
		{name: "nothing uses it", catalog: mesh(), def: "gpu-operator"},
		{name: "a tenant component", catalog: mesh(), def: "gpu-operator",
			use:  Use{Components: []string{"acme/prod", "acme/dev"}},
			want: []string{"enabled on acme/dev", "enabled on acme/prod"}},
		{name: "a platform component still installed", catalog: mesh(), def: "kyverno",
			use:  Use{Clusters: []string{"acme/prod"}},
			want: []string{"installed on acme/prod", "required by kyverno-policies"}},
		{name: "a member of a bundle that exists", catalog: mesh(), def: "base",
			want: []string{"member of bundle istio", "required by istiod"}},
		{name: "a member of a bundle being deleted", catalog: deleting(deleting(mesh(), "istio"), "istiod"), def: "base"},
		{name: "required by one being deleted", catalog: deleting(mesh(), "kyverno-policies"), def: "kyverno"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := HeldBy(tt.def, tt.catalog, tt.use)

			if !slices.Equal(got, tt.want) {
				t.Errorf("HeldBy = %v, want %v", got, tt.want)
			}
		})
	}
}

// A definition being deleted is off: clusters remove a Platform one, so
// its delete does not wait for ever.
func TestADeletingDefinitionIsOff(t *testing.T) {
	catalog := mesh()
	for i := range catalog {
		if catalog[i].Name == "lb" {
			catalog[i].Deleting = true
		}
	}

	off := Off(catalog)
	got, err := PlatformComponents(catalog)

	if off["lb"] != "being deleted" || off["gateway"] != "it requires lb, which is off" {
		t.Errorf("Off = %v", off)
	}
	if err != nil || slices.Contains(names(got), "lb") {
		t.Errorf("platform components = %v, %v; want lb left out", names(got), err)
	}
}
