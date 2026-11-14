package component

import (
	"fmt"
	"slices"
)

// knownProviders are the clouds a definition may name (the cluster's
// provider names).
var knownProviders = []string{"aws", "gcp", "azure"}

// Covers reports whether the component is installed on clusters of
// provider: every definition is for exactly one cloud.
func (d Definition) Covers(provider string) bool {
	return provider != "" && d.Provider == provider
}

// NotOnProvider says d is not installed on clusters of provider.
func NotOnProvider(d Definition, provider string) string {
	return fmt.Sprintf("not available on %s clusters: %s is for %s", provider, d.Name, d.Provider)
}

// providerProblems are what is wrong with d's provider.
func providerProblems(d Definition) []string {
	switch {
	case d.Provider == "":
		return []string{"provider: name the cloud the component is installed on (aws, gcp or azure)"}
	case !slices.Contains(knownProviders, d.Provider):
		return []string{fmt.Sprintf("provider: %q is not aws, gcp or azure", d.Provider)}
	}
	return nil
}

// checkProviders are the catalog-wide provider rules for d:
//   - a bundle's members are for the bundle's cloud;
//   - what d requires is for d's cloud;
//   - no other definition for the same cloud installs the same chart into
//     the same namespace, so a cluster never gets two variants of one
//     component.
func checkProviders(d Definition, byName map[string]Definition, defs []Definition) []string {
	var out []string
	for _, name := range d.Includes {
		if m, ok := byName[name]; ok && m.Provider != d.Provider {
			out = append(out, fmt.Sprintf("%s is for %s but its member %s is for %s", d.Name, d.Provider, name, m.Provider))
		}
	}
	for _, r := range d.Requires {
		if req, ok := byName[r]; ok && req.Provider != d.Provider {
			out = append(out, fmt.Sprintf("%s is for %s but requires %s, which is for %s", d.Name, d.Provider, r, req.Provider))
		}
	}
	if d.Chart == "" {
		return out
	}
	for _, other := range defs {
		if other.Name <= d.Name || other.Provider != d.Provider || other.Chart != d.Chart || other.Namespace != d.Namespace {
			continue
		}
		out = append(out, fmt.Sprintf("%s and %s are both for %s and install %s into namespace %s: a cluster would get both",
			d.Name, other.Name, d.Provider, d.Chart, d.Namespace))
	}
	return out
}

// ForProvider are the entries a cluster of provider gets: those for its
// cloud, plus any installed already (installed, by name) that are not.
// Those are kept, never removed by a catalog change, and named in
// keptElsewhere so the caller can say so. entries is not changed.
func ForProvider(entries []PlatformEntry, provider string, installed map[string]bool) (kept []PlatformEntry, keptElsewhere []string) {
	for _, e := range entries {
		switch {
		case e.Covers(provider):
			kept = append(kept, e)
		case installed[e.Name]:
			kept = append(kept, e)
			keptElsewhere = append(keptElsewhere, e.Name)
		}
	}
	return kept, keptElsewhere
}
