package cluster

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// teardownStage is a set of workload-cluster objects a deletion removes at
// one point: the objects of a stage are deleted only once the earlier
// stages' are gone.
type teardownStage struct {
	match   func(reconcile.WorkloadObject) bool
	order   []string // kinds, in the order the stage lists them
	message func(objs []reconcile.WorkloadObject) string
}

func ofKinds(kinds ...string) func(reconcile.WorkloadObject) bool {
	return func(o reconcile.WorkloadObject) bool { return slices.Contains(kinds, o.Kind) }
}

func isPlatformPool(o reconcile.WorkloadObject) bool {
	return o.Kind == reconcile.KindNodePool && o.Name == crossplaneaws.PoolPlatform
}

// loadBalancerStages are the first thing a deletion removes in the workload
// cluster: the load balancers, while the AWS Load Balancer Controller that
// removes them (a platform component) still runs.
var loadBalancerStages = []teardownStage{{
	match: ofKinds(reconcile.KindIngress, reconcile.KindService),
	order: []string{reconcile.KindIngress, reconcile.KindService},
	message: func(objs []reconcile.WorkloadObject) string {
		noun := "load balancers"
		if len(objs) == 1 {
			noun = "load balancer"
		}
		return fmt.Sprintf("deleting %d %s (%s)", len(objs), noun, listOf(objs))
	},
}}

// nodeGroupStages are the Karpenter node groups, removed after the platform
// components above Karpenter are gone: the user node pools, then the
// platform node pool, then whatever NodeClaims Karpenter has left to
// terminate, then the node classes (their instance profiles and launch
// templates), which Karpenter keeps while nodes use them. A pool is removed
// by Karpenter once its nodes are terminated, so a pool that is gone has no
// nodes.
var nodeGroupStages = []teardownStage{
	{match: func(o reconcile.WorkloadObject) bool { return o.Kind == reconcile.KindNodePool && !isPlatformPool(o) },
		order: []string{reconcile.KindNodePool},
		message: func(objs []reconcile.WorkloadObject) string {
			return "deleting the user node pools and waiting for Karpenter to terminate their nodes (" + listOf(objs) + ")"
		}},
	{match: isPlatformPool, order: []string{reconcile.KindNodePool},
		message: func(objs []reconcile.WorkloadObject) string {
			return "deleting the platform node pool and waiting for Karpenter to terminate its nodes (" + listOf(objs) + ")"
		}},
	{match: ofKinds(reconcile.KindNodeClaim), order: []string{reconcile.KindNodeClaim},
		message: func(objs []reconcile.WorkloadObject) string {
			return "waiting for Karpenter to terminate its remaining nodes (" + listOf(objs) + ")"
		}},
	{match: ofKinds(reconcile.KindEC2NodeClass), order: []string{reconcile.KindEC2NodeClass},
		message: func(objs []reconcile.WorkloadObject) string {
			return "deleting Karpenter's node classes and waiting for it to remove their instance profiles (" + listOf(objs) + ")"
		}},
}

// stageKinds are the kinds a list of stages reads.
func stageKinds(stages []teardownStage) []string {
	var kinds []string
	for _, st := range stages {
		for _, k := range st.order {
			if !slices.Contains(kinds, k) {
				kinds = append(kinds, k)
			}
		}
	}
	return kinds
}

// teardownStep is what one pass of a teardown does: the objects to ask to
// delete now, and the ones it waits for (the current stage's, deleted or
// not), with the message the cluster's status shows meanwhile. since is the
// oldest deletion request among the objects waited for: where a node
// stage's timeout counts from (zero: nothing was asked yet).
type teardownStep struct {
	delete, waiting []reconcile.WorkloadObject
	message         string
	since           time.Time
}

func (t teardownStep) finished() bool { return len(t.waiting) == 0 }

// planTeardown is the next step for the objects the workload cluster has:
// the first of the stages that still has objects.
func planTeardown(stages []teardownStage, objs []reconcile.WorkloadObject) teardownStep {
	for _, st := range stages {
		var stage []reconcile.WorkloadObject
		for _, o := range objs {
			if st.match(o) {
				stage = append(stage, o)
			}
		}
		if len(stage) == 0 {
			continue
		}
		slices.SortStableFunc(stage, func(a, b reconcile.WorkloadObject) int {
			return cmp.Or(cmp.Compare(slices.Index(st.order, a.Kind), slices.Index(st.order, b.Kind)),
				cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
		})
		step := teardownStep{waiting: stage, message: st.message(stage)}
		for _, o := range stage {
			if !o.Deleting {
				step.delete = append(step.delete, o)
				continue
			}
			if !o.DeletedAt.IsZero() && (step.since.IsZero() || o.DeletedAt.Before(step.since)) {
				step.since = o.DeletedAt
			}
		}
		return step
	}
	return teardownStep{}
}

// teardownShown is how many objects a message names.
const teardownShown = 5

func listOf(objs []reconcile.WorkloadObject) string {
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
	return list
}
