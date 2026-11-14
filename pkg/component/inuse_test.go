package component

import (
	"slices"
	"strings"
	"testing"
	"time"
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
			use:  Use{Clusters: []ClusterHold{{Cluster: "acme/prod"}}},
			want: []string{"installed on acme/prod (its objects are still in the management cluster)", "required by kyverno-policies"}},
		{name: "on a paused cluster", catalog: mesh(), def: "gpu-operator",
			use: Use{Clusters: []ClusterHold{{Cluster: "acme/prod", PausedBy: "alice",
				PausedSince: time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)}}},
			want: []string{"installed on acme/prod, paused by alice since 2026-11-14T09:00:00Z (removal resumes when it is resumed)"}},
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

// A cluster holds a definition while objects of the component are recorded
// for it, not while its status says so: a cluster that stops reporting
// cannot keep a definition for ever, and one that is gone or being deleted
// holds nothing.
func TestHoldingClusters(t *testing.T) {
	since := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	clusters := []ClusterState{
		{ID: "c-prod", Where: "acme/prod"},
		{ID: "c-dev", Where: "acme/dev", Deleting: true},
		{ID: "c-stage", Where: "acme/stage", PausedBy: "alice", PausedSince: since},
	}
	tests := []struct {
		name      string
		installed []string
		objects   []Recorded
		want      []ClusterHold
	}{
		{name: "nothing recorded", installed: []string{"lb"}},
		{name: "objects of another component", installed: []string{"lb"},
			objects: []Recorded{{Component: "kyverno", ClusterID: "c-prod"}}},
		{name: "objects on a running cluster, counted once", installed: []string{"lb"},
			objects: []Recorded{{Component: "lb", ClusterID: "c-prod"}, {Component: "lb", ClusterID: "c-prod"}},
			want:    []ClusterHold{{Cluster: "acme/prod"}}},
		{name: "a bundle member's objects", installed: []string{"istio", "istiod"},
			objects: []Recorded{{Component: "istiod", ClusterID: "c-prod"}},
			want:    []ClusterHold{{Cluster: "acme/prod"}}},
		{name: "a cluster being deleted", installed: []string{"lb"},
			objects: []Recorded{{Component: "lb", ClusterID: "c-dev"}}},
		{name: "a cluster that is gone", installed: []string{"lb"},
			objects: []Recorded{{Component: "lb", ClusterID: "c-gone"}}},
		{name: "objects without a cluster", installed: []string{"lb"},
			objects: []Recorded{{Component: "lb"}}},
		{name: "a paused cluster holds", installed: []string{"lb"},
			objects: []Recorded{{Component: "lb", ClusterID: "c-stage"}},
			want:    []ClusterHold{{Cluster: "acme/stage", PausedBy: "alice", PausedSince: since}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := HoldingClusters(tt.installed, tt.objects, clusters)

			if !slices.Equal(got, tt.want) {
				t.Errorf("HoldingClusters = %v, want %v", got, tt.want)
			}
		})
	}
}

// Only the exact value "true" forces a delete; anything else is reported.
func TestForceDelete(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		force       bool
		invalid     string
	}{
		{name: "none"},
		{name: "true", annotations: map[string]string{AnnotationForceDelete: "true"}, force: true},
		{name: "True", annotations: map[string]string{AnnotationForceDelete: "True"}, invalid: `"True"`},
		{name: "yes", annotations: map[string]string{AnnotationForceDelete: "yes"}, invalid: `"yes"`},
		{name: "empty", annotations: map[string]string{AnnotationForceDelete: ""}, invalid: `""`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			force, invalid := ForceDelete(tt.annotations)

			if force != tt.force || !strings.Contains(invalid, tt.invalid) || (tt.invalid == "") != (invalid == "") {
				t.Errorf("ForceDelete = %v, %q; want %v and a message naming %s", force, invalid, tt.force, tt.invalid)
			}
		})
	}
}
