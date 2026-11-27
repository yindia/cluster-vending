package service

import (
	"testing"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

func TestNotApplicableNamesTheBlockingChanges(t *testing.T) {
	plan := &cloudv1.Plan{Changes: []*cloudv1.PlannedChange{
		{SpecPath: "spec.control_plane.kubernetes_version", Action: cloudv1.PlannedAction_PLANNED_ACTION_DISRUPTIVE},
		{SpecPath: "spec.region", Action: cloudv1.PlannedAction_PLANNED_ACTION_REJECTED_IMMUTABLE},
		{SpecPath: "spec.region", Kind: "Cluster", Action: cloudv1.PlannedAction_PLANNED_ACTION_REJECTED_IMMUTABLE},
		{Kind: "VPC", Name: "net", Action: cloudv1.PlannedAction_PLANNED_ACTION_REJECTED_IMMUTABLE},
	}}
	got := NotApplicable("it changes an immutable field", plan)
	if want := "the plan is not applicable: it changes an immutable field (spec.region, VPC net)"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got := NotApplicable("x", &cloudv1.Plan{}); got != "the plan is not applicable: x" {
		t.Fatalf("no blocking change: %q", got)
	}
	many := &cloudv1.Plan{}
	for _, p := range []string{"a", "b", "c", "d", "e", "f"} {
		many.Changes = append(many.Changes, &cloudv1.PlannedChange{SpecPath: p, Action: cloudv1.PlannedAction_PLANNED_ACTION_REJECTED_IMMUTABLE})
	}
	if got := BlockingChanges(many); got != "a, b, c, d, e, ..." {
		t.Fatalf("got %q", got)
	}
}
