package component

import (
	"reflect"
	"testing"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
)

// WithPlacement replaces the four placement placeholders with the live
// system placement (${platform.*} is an alias of ${system.*}) and leaves the
// rest and its input alone.
func TestWithPlacementSetsTheLiveValues(t *testing.T) {
	in := Vars{VarRegion: "us-east-1", VarSystemNodeSelector: `{"x":"y"}`}
	live := Placement{Labels: map[string]string{"role": "old"},
		Taints: []crossplaneaws.TaintPlan{{Key: "dedicated", Value: "old", Effect: "NO_SCHEDULE"}}}

	got, err := Expand(map[string]any{
		"a": "${system.nodeSelector}", "b": "${system.tolerations}",
		"c": "${platform.nodeSelector}", "d": "${platform.tolerations}", "r": "${region}"}, WithPlacement(in, live))

	if err != nil {
		t.Fatal(err)
	}
	selector := map[string]any{"role": "old"}
	tolerations := []any{map[string]any{"key": "dedicated", "operator": "Equal", "value": "old", "effect": "NoSchedule"}}
	want := map[string]any{"a": selector, "b": tolerations, "c": selector, "d": tolerations, "r": "us-east-1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
	if in[VarSystemNodeSelector] != `{"x":"y"}` {
		t.Error("WithPlacement changed its input")
	}
}
