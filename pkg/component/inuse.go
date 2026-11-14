package component

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// DefinitionFinalizer keeps a ComponentDefinition while something uses it
// (HeldBy).
const DefinitionFinalizer = "cloud.cluster-vending.io/definition-in-use"

// AnnotationForceDelete on a ComponentDefinition lets its delete finish
// although clusters still have objects of it: the objects stay there,
// orphaned. Only the value "true" counts (ForceDelete).
const AnnotationForceDelete = "cloud.cluster-vending.io/force-delete"

// Use is where a definition is in use outside the catalog.
type Use struct {
	// Components are the tenant Components of the definition, as
	// "<tenant>/<cluster>".
	Components []string
	// Clusters still have objects of the definition (or, for a bundle, of
	// a member) in the management cluster (HoldingClusters).
	Clusters []ClusterHold
}

// ClusterHold is a cluster that still has objects of a component.
type ClusterHold struct {
	// Cluster is "<tenant>/<cluster>".
	Cluster string
	// PausedBy and PausedSince are the cluster's pause, if any: removal
	// resumes when it is resumed.
	PausedBy    string
	PausedSince time.Time
}

// Recorded is an object of a component in the management cluster: the
// component and the cluster ID its labels name.
type Recorded struct {
	Component, ClusterID string
}

// ClusterState is what a hold needs of a cluster.
type ClusterState struct {
	// ID is the cluster's ID; Where is "<tenant>/<cluster>".
	ID, Where string
	// Deleting: the cluster is being deleted, and removes its components
	// with it.
	Deleting bool
	// PausedBy and PausedSince are its pause, if any.
	PausedBy    string
	PausedSince time.Time
}

// HoldingClusters are the clusters that hold a definition: those with
// objects of one of the installed components (the definition, or a bundle
// and its members) still in the management cluster. The objects go exactly
// when the uninstall finishes, so the hold ends by itself; it does not
// depend on the cluster writing its status. A cluster that is being
// deleted, or that no longer exists, holds nothing: it can never release.
// A paused one holds, with its pause.
func HoldingClusters(installed []string, objects []Recorded, clusters []ClusterState) []ClusterHold {
	byID := make(map[string]ClusterState, len(clusters))
	for _, c := range clusters {
		byID[c.ID] = c
	}
	seen := map[string]bool{}
	var out []ClusterHold
	for _, o := range objects {
		if !slices.Contains(installed, o.Component) || seen[o.ClusterID] {
			continue
		}
		c, ok := byID[o.ClusterID]
		if !ok || c.Deleting {
			continue
		}
		seen[o.ClusterID] = true
		out = append(out, ClusterHold{Cluster: c.Where, PausedBy: c.PausedBy, PausedSince: c.PausedSince})
	}
	slices.SortFunc(out, func(a, b ClusterHold) int { return strings.Compare(a.Cluster, b.Cluster) })
	return out
}

// ForceDelete reads AnnotationForceDelete: force when it is exactly
// "true"; otherwise, when it is set, why it is ignored.
func ForceDelete(annotations map[string]string) (force bool, invalid string) {
	value, ok := annotations[AnnotationForceDelete]
	switch {
	case !ok:
		return false, ""
	case value == "true":
		return true, ""
	default:
		return false, fmt.Sprintf("%s is %q: only \"true\" forces the delete", AnnotationForceDelete, value)
	}
}

// HeldBy says what keeps the definition name from being deleted, sorted;
// none when it may go:
//   - a tenant Component of it (disable it first);
//   - a cluster that still has objects of it (HoldingClusters): a
//     definition being deleted is off (Off), so the cluster removes it and
//     the hold ends by itself;
//   - the bundle it is a member of, while the bundle is not being deleted;
//   - a definition that requires it, while that one is not being deleted.
func HeldBy(name string, catalog []Definition, use Use) []string {
	var out []string
	for _, c := range use.Components {
		out = append(out, "enabled on "+c)
	}
	for _, c := range use.Clusters {
		if c.PausedBy != "" {
			out = append(out, fmt.Sprintf("installed on %s, paused by %s since %s (removal resumes when it is resumed)",
				c.Cluster, c.PausedBy, c.PausedSince.UTC().Format(time.RFC3339)))
			continue
		}
		out = append(out, "installed on "+c.Cluster+" (its objects are still in the management cluster)")
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
