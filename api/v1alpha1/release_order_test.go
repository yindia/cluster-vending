package v1alpha1

import (
	"slices"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func rel(name string, serial int64, created time.Time) Release {
	r := Release{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if serial > 0 {
		r.Spec.Serial = &serial
	}
	if !created.IsZero() {
		r.CreationTimestamp = metav1.NewTime(created)
	}
	return r
}

func names(rs []Release) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.Name)
	}
	return out
}

func TestSortNewestFirst(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []Release
		want []string
	}{
		{"serials beat creation times", []Release{rel("a", 3, epoch), rel("b", 1, epoch.Add(time.Hour)), rel("c", 2, epoch.Add(2*time.Hour))}, []string{"a", "c", "b"}},
		{"re-created release keeps its place", []Release{rel("old", 1, epoch.Add(48*time.Hour)), rel("new", 2, epoch.Add(24*time.Hour))}, []string{"new", "old"}},
		{"legacy releases sort below serialed ones", []Release{rel("legacy", 0, epoch.Add(time.Hour)), rel("s", 1, epoch)}, []string{"s", "legacy"}},
		{"legacy releases by creation time", []Release{rel("a", 0, epoch), rel("b", 0, epoch.Add(time.Hour))}, []string{"b", "a"}},
		{"legacy ties by name, later name is newer", []Release{rel("a", 0, epoch), rel("b", 0, epoch)}, []string{"b", "a"}},
		{"legacy without time sort last", []Release{rel("none", 0, time.Time{}), rel("a", 0, epoch)}, []string{"a", "none"}},
		{"serial ties by name", []Release{rel("a", 5, epoch), rel("b", 5, epoch)}, []string{"b", "a"}},
		{"empty", nil, nil},
	} {
		in := slices.Clone(tc.in)
		SortReleasesNewestFirst(in)
		if got := names(in); !slices.Equal(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestReleaseNotNewer(t *testing.T) {
	for _, tc := range []struct {
		name            string
		current, target Release
		want            bool
	}{
		{"higher serial is newer", rel("c", 1, epoch), rel("t", 2, epoch.Add(-time.Hour)), false},
		{"same serial", rel("c", 2, epoch), rel("t", 2, epoch), true},
		{"lower serial is not newer, whatever the times", rel("c", 2, epoch), rel("t", 1, epoch.Add(time.Hour)), true},
		{"serial on current only: target is older", rel("c", 1, epoch), rel("t", 0, epoch.Add(time.Hour)), true},
		{"serial on target only: target is newer", rel("c", 0, epoch), rel("t", 1, epoch.Add(-time.Hour)), false},
		{"legacy: older by time", rel("c", 0, epoch), rel("t", 0, epoch.Add(-time.Hour)), true},
		{"legacy: newer by time", rel("c", 0, epoch), rel("t", 0, epoch.Add(time.Hour)), false},
		{"legacy: no time is not known", rel("c", 0, time.Time{}), rel("t", 0, epoch), false},
		{"legacy: no time on target is not known", rel("c", 0, epoch), rel("t", 0, time.Time{}), false},
	} {
		if got := ReleaseNotNewer(&tc.current, &tc.target); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestBackfillSerials(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []Release
		want map[string]int64
	}{
		{"creation order", []Release{rel("b", 0, epoch.Add(time.Hour)), rel("a", 0, epoch)}, map[string]int64{"a": 1, "b": 2}},
		{"ties by name", []Release{rel("z", 0, epoch), rel("m", 0, epoch), rel("a", 0, epoch)}, map[string]int64{"a": 1, "m": 2, "z": 3}},
		{"missing timestamps first, by name", []Release{rel("t", 0, epoch), rel("y", 0, time.Time{}), rel("x", 0, time.Time{})}, map[string]int64{"x": 1, "y": 2, "t": 3}},
		{"serials kept, new ones above them", []Release{rel("k", 7, epoch.Add(time.Hour)), rel("n", 0, epoch)}, map[string]int64{"k": 7, "n": 8}},
		{"nothing to do", []Release{rel("k", 4, epoch)}, map[string]int64{"k": 4}},
		{"empty", nil, map[string]int64{}},
	} {
		in := slices.Clone(tc.in)
		out := BackfillSerials(in)
		got := map[string]int64{}
		for _, r := range out {
			got[r.Name] = *r.Spec.Serial
		}
		if len(got) != len(tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
			continue
		}
		for k, v := range tc.want {
			if got[k] != v {
				t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
				break
			}
		}
		for i := range in { // the input is left as it was
			if (in[i].Spec.Serial == nil) != (tc.in[i].Spec.Serial == nil) {
				t.Errorf("%s: input %s was changed", tc.name, in[i].Name)
			}
		}
	}
}
