package component

import (
	"slices"
	"strings"
)

// DefinitionFinalizer keeps a ComponentDefinition while something uses it
// (HeldBy).
const DefinitionFinalizer = "cloud.cluster-vending.io/definition-in-use"

// Use is where a definition is in use outside the catalog.
type Use struct {
	// Components are the tenant Components of the definition, as
	// "<tenant>/<cluster>".
	Components []string
	// Clusters have the definition installed as a platform component (it,
	// or for a bundle one of its members, is in their status), as
	// "<tenant>/<cluster>".
	Clusters []string
}

// HeldBy says what keeps the definition name from being deleted, sorted;
// none when it may go:
//   - a tenant Component of it (disable it first);
//   - a cluster that still has it installed: a definition being deleted is
//     off (Off), so the cluster removes it and the hold ends by itself;
//   - the bundle it is a member of, while the bundle is not being deleted;
//   - a definition that requires it, while that one is not being deleted.
func HeldBy(name string, catalog []Definition, use Use) []string {
	var out []string
	for _, c := range use.Components {
		out = append(out, "enabled on "+c)
	}
	for _, c := range use.Clusters {
		out = append(out, "installed on "+c)
	}
	for _, d := range catalog {
		if d.Name == name || d.Deleting {
			continue
		}
		if slices.Contains(d.Includes, name) {
			out = append(out, "member of bundle "+d.Name)
		}
		if slices.Contains(d.Requires, name) {
			out = append(out, "required by "+d.Name)
		}
	}
	slices.SortFunc(out, strings.Compare)
	return slices.Compact(out)
}
