package component

import (
	"fmt"
	"slices"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
)

// Workload is one Deployment or DaemonSet a definition runs, for the
// sizing rule: where its replica count and per-pod requests are in the
// definition's platformValues (dotted paths). A DaemonSet has no Replicas:
// it runs one pod per node.
type Workload struct {
	Name, Replicas, Requests string
}

// Sizing is what a definition runs (spec.sizing); empty: no workload.
type Sizing struct {
	Deployments []Workload
	DaemonSets  []Workload
}

// Empty reports whether the definition runs no workload.
func (s Sizing) Empty() bool { return len(s.Deployments) == 0 && len(s.DaemonSets) == 0 }

// SizingOf is spec.sizing as the sizing rule takes it.
func SizingOf(s *cloudv1alpha1.ComponentSizing) Sizing {
	if s == nil {
		return Sizing{}
	}
	var out Sizing
	for _, d := range s.Deployments {
		out.Deployments = append(out.Deployments, Workload{Name: d.Name, Replicas: d.Replicas, Requests: d.Requests})
	}
	for _, d := range s.DaemonSets {
		out.DaemonSets = append(out.DaemonSets, Workload{Name: d.Name, Requests: d.Requests})
	}
	return out
}

// sizingProblems are what is wrong with d's sizing alone: it is for a
// Platform component with an install of its own, and its workload names
// are unique.
func sizingProblems(d Definition) []string {
	if d.Sizing.Empty() {
		return nil
	}
	var out []string
	if !d.IsPlatform() {
		out = append(out, "sizing is for a Platform component: a tenant's components are not sized by the platform")
	}
	var names []string
	for _, w := range slices.Concat(d.Sizing.Deployments, d.Sizing.DaemonSets) {
		if slices.Contains(names, w.Name) {
			out = append(out, fmt.Sprintf("sizing names %s twice", w.Name))
		}
		names = append(names, w.Name)
	}
	return out
}
