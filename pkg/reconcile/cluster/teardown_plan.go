package cluster

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// teardownStages are the workload-cluster kinds a deletion removes, by
// stage: a stage's objects are deleted only once the earlier stages' are
// gone. Load balancers first, while the controller that removes them
// still runs; then Karpenter's node pools and the nodes it launched; then
// its node classes (their instance profiles and launch templates), which
// it keeps while nodes use them.
var teardownStages = [][]string{
	{reconcile.KindIngress, reconcile.KindService},
	{reconcile.KindNodePool, reconcile.KindNodeClaim},
	{reconcile.KindEC2NodeClass},
}

// teardownStep is what one pass of the teardown does: the objects to ask
// to delete now, and the ones it waits for (the current stage's, deleted
// or not), with the message the cluster's status shows meanwhile.
type teardownStep struct {
	delete, waiting []reconcile.WorkloadObject
	message         string
}

func (t teardownStep) finished() bool { return len(t.waiting) == 0 }

// planTeardown is the next step for the objects the workload cluster has.
func planTeardown(objs []reconcile.WorkloadObject) teardownStep {
	for i, kinds := range teardownStages {
		var stage []reconcile.WorkloadObject
		for _, o := range objs {
			if slices.Contains(kinds, o.Kind) {
				stage = append(stage, o)
			}
		}
		if len(stage) == 0 {
			continue
		}
		slices.SortStableFunc(stage, func(a, b reconcile.WorkloadObject) int {
			return cmp.Or(cmp.Compare(slices.Index(kinds, a.Kind), slices.Index(kinds, b.Kind)),
				cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
		})
		step := teardownStep{waiting: stage, message: stageMessage(i, stage)}
		for _, o := range stage {
			if !o.Deleting {
				step.delete = append(step.delete, o)
			}
		}
		return step
	}
	return teardownStep{}
}

// teardownShown is how many objects a message names.
const teardownShown = 5

func stageMessage(stage int, objs []reconcile.WorkloadObject) string {
	refs := make([]string, 0, teardownShown)
	for i, o := range objs {
		if i == teardownShown {
			break
		}
		refs = append(refs, o.Ref())
	}
	list := strings.Join(refs, ", ")
	if more := len(objs) - teardownShown; more > 0 {
		list += fmt.Sprintf(" and %d more", more)
	}
	switch stage {
	case 0:
		noun := "load balancers"
		if len(objs) == 1 {
			noun = "load balancer"
		}
		return fmt.Sprintf("deleting %d %s (%s)", len(objs), noun, list)
	case 1:
		return "deleting Karpenter's node pools and nodes (" + list + ")"
	default:
		return "deleting Karpenter's node classes (" + list + ")"
	}
}
