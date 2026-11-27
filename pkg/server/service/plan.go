package service

import (
	"slices"
	"strings"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// maxBlockingPaths is how many refused changes a message names.
const maxBlockingPaths = 5

// BlockingChanges names the changes that make a plan not applicable (its
// REJECTED_IMMUTABLE ones): their API path, else the resource, e.g.
// "spec.region, AWS Cluster prod"; at most maxBlockingPaths, "..." for
// more. Empty when there is none.
func BlockingChanges(plan *cloudv1.Plan) string {
	var paths []string
	for _, c := range plan.GetChanges() {
		if c.GetAction() != cloudv1.PlannedAction_PLANNED_ACTION_REJECTED_IMMUTABLE {
			continue
		}
		path := c.GetSpecPath()
		if path == "" {
			path = strings.TrimSpace(c.GetKind() + " " + c.GetName())
		}
		if !slices.Contains(paths, path) {
			paths = append(paths, path)
		}
	}
	if len(paths) > maxBlockingPaths {
		paths = append(paths[:maxBlockingPaths], "...")
	}
	return strings.Join(paths, ", ")
}

// NotApplicable is the message of a plan that is not applicable: why, and
// the changes that block it.
func NotApplicable(why string, plan *cloudv1.Plan) string {
	msg := "the plan is not applicable: " + why
	if blocking := BlockingChanges(plan); blocking != "" {
		msg += " (" + blocking + ")"
	}
	return msg
}
