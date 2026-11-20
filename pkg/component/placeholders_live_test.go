package component

import (
	"reflect"
	"testing"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
)

// WithPlacement replaces the four placement placeholders and leaves the
// rest and its input alone.
func TestWithPlacementSetsTheLiveValues(t *testing.T) {
	in := Vars{VarRegion: "us-east-1", VarSystemNodeSelector: `{"x":"y"}`}
	system := Placement{Labels: map[string]string{}}
	platform := Placement{Labels: crossplaneaws.PoolLabels(crossplaneaws.PoolPlatform),
		Taints: []crossplaneaws.TaintPlan{crossplaneaws.PoolTaint(crossplaneaws.PoolPlatform)}}

	got, err := Expand(map[string]any{
		"a": "${system.nodeSelector}", "b": "${system.tolerations}",
		"c": "${platform.nodeSelector}", "d": "${platform.tolerations}", "r": "${region}"}, WithPlacement(in, system, platform))

	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"a": map[string]any{}, "b": []any{}, "c": platformSelector, "d": platformTaints, "r": "us-east-1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
	if in[VarSystemNodeSelector] != `{"x":"y"}` {
		t.Error("WithPlacement changed its input")
	}
}
