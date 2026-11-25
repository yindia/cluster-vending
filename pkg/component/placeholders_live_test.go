package component

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
)

var (
	livePlacement = Placement{Labels: map[string]string{"role": "old"},
		Taints: []crossplaneaws.TaintPlan{{Key: "dedicated", Value: "old", Effect: "NO_SCHEDULE"}}}
	liveSelector    = map[string]any{"role": "old"}
	liveTolerations = []any{map[string]any{"key": "dedicated", "operator": "Equal", "value": "old", "effect": "NoSchedule"}}
)

var placementDoc = map[string]any{
	"a": "${system.nodeSelector}", "b": "${system.tolerations}",
	"c": "${platform.nodeSelector}", "d": "${platform.tolerations}", "r": "${region}"}

// WithPlacement replaces the system placement placeholders with the live
// system node group's; ${platform.*} (the platform pool) is not the system
// group's and stays as it was. Its input is not changed.
func TestWithPlacementSetsTheLiveSystemValues(t *testing.T) {
	in := VarsFor(testCluster(true), testNetwork(), testAccount(), IAMOutputs{})
	before := in[VarSystemNodeSelector]

	got, err := Expand(placementDoc, WithPlacement(in, livePlacement))

	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"a": liveSelector, "b": liveTolerations, "c": poolSelector, "d": poolTaints, "r": "us-east-1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
	if in[VarSystemNodeSelector] != before {
		t.Error("WithPlacement changed its input")
	}
}

// WithPlatformPlacement puts ${platform.*} on a given placement (a cluster
// without a platform pool keeps its components on the system group).
func TestWithPlatformPlacement(t *testing.T) {
	in := VarsFor(testCluster(true), testNetwork(), testAccount(), IAMOutputs{})

	got, err := Expand(placementDoc, WithPlatformPlacement(in, livePlacement))

	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"a": systemSelector, "b": systemTaints, "c": liveSelector, "d": liveTolerations, "r": "us-east-1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
	if in[VarPlatformNodeSelector] != mustJSON(crossplaneaws.PoolLabels(crossplaneaws.PoolPlatform)) {
		t.Error("WithPlatformPlacement changed its input")
	}
}

// Until the platform pool is Ready a component placed on it waits (phase 2
// of the install order), with the reason; the system placement does not.
func TestWaitForPlatformPool(t *testing.T) {
	in := VarsFor(testCluster(true), testNetwork(), testAccount(), IAMOutputs{})

	vars := WaitForPlatformPool(in, "the platform pool is PENDING")

	if _, err := Expand(map[string]any{"a": "${system.nodeSelector}", "b": "${system.tolerations}"}, vars); err != nil {
		t.Errorf("system placement waits too: %v", err)
	}
	_, err := Expand(map[string]any{"c": "${platform.nodeSelector}", "d": "${platform.tolerations}"}, vars)
	var notReady *NotReadyError
	if !errors.As(err, &notReady) || len(notReady.Missing) != 2 {
		t.Fatalf("platform placement = %v, want a wait for both", err)
	}
	if !strings.Contains(err.Error(), "the platform pool is PENDING") {
		t.Errorf("wait %q does not say why", err)
	}
	if in[VarPlatformNodeSelector] == "" {
		t.Error("WaitForPlatformPool changed its input")
	}
}
