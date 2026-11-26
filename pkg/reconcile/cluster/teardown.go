package cluster

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// leftBehind is what a skipped or timed-out teardown may leave in the
// cloud, and what deletes it then: the cloud sweep (delete_cloud_sweep.go)
// finds it by the cluster's tags.
const leftBehind = "load balancers and their security groups (of Services of type LoadBalancer and Ingresses), " +
	"Karpenter's EC2 instances and launch templates are deleted by the cloud sweep (stage 5) by the cluster's tags; " +
	"what carries none of them (a Service of the in-tree cloud provider, a load balancer made by hand) may be left in the account"

// loadBalancerLeftBehind is what a skipped or timed-out load balancer
// stage may leave, and what deletes it then.
const loadBalancerLeftBehind = "the AWS Load Balancer Controller's load balancers, target groups and security groups " +
	"are deleted by the cloud sweep (stage 5) by the cluster's tags; others may be left in the account"

// teardown is the first stage of a cluster's deletion: in the workload
// cluster, the load balancers its controllers made in the cloud are deleted
// while the AWS Load Balancer Controller still runs (planTeardown), before
// any platform component is uninstalled. (Karpenter's nodes are the fourth
// stage, deleteNodeGroups.) It reports whether the deletion may go on. It is
// skipped (with a warning naming why) when the deletion is forced, the
// cluster has no kubeconfig or its EKS cluster does not answer (it is not
// ACTIVE or UPDATING), and ends after the
// teardown timeout with a warning listing what is left: a deletion never
// waits on it for ever. Its state is the status's teardown (kept across
// passes: the timeout counts from its start).
func (s *Reconciler) teardown(ctx context.Context, row *model.Cluster, records []model.ManagedResource) (bool, error) {
	prev := storedStatus(row).GetTeardown()
	if prev.GetFinishTime() != nil {
		return true, nil
	}
	if s.deps.Teardowns == nil {
		return true, nil
	}
	skip, err := s.skipCause(ctx, row, records)
	if err != nil {
		return false, err
	}
	return s.runTeardown(ctx, row, prev, skip), nil
}

// runTeardown is teardown once it knows whether the cluster's API is to be
// used: skip says why it is not (skipCause), empty to run the stage. An API
// that is to be used and does not answer is retried until the timeout. It
// records the step's state (withTeardown) and its events.
func (s *Reconciler) runTeardown(ctx context.Context, row *model.Cluster, prev *cloudv1.ClusterTeardown, skip string) bool {
	at := now()
	t := &cloudv1.ClusterTeardown{StartTime: prev.GetStartTime()}
	if t.StartTime == nil {
		t.StartTime = timestamppb.New(at)
	}
	defer setTeardown(ctx, t)
	if skip != "" {
		t.FinishTime, t.Result = timestamppb.New(at), cloudv1.ClusterTeardownResult_CLUSTER_TEARDOWN_RESULT_SKIPPED
		t.Message = "skipped: the cluster's API is not used (" + skip + "); " + leftBehind
		reconcile.AddNote(ctx, reconcile.Warning(reconcile.ReasonTeardownSkipped, t.Message))
		return true
	}
	if prev == nil {
		reconcile.AddNote(ctx, reconcile.Normal(reconcile.ReasonTeardownStarted,
			"deleting the cluster in reverse order of its creation, starting with its load balancers"))
	}
	step, err := s.stepTeardown(ctx, row, loadBalancerStages)
	expired := at.Sub(t.StartTime.AsTime()) >= s.teardownTimeout()
	switch {
	case err != nil && !expired:
		t.Message = "cannot reach the cluster's API: " + err.Error() + "; trying again"
		setDeleteStage(ctx, stageLoadBalancers, t.Message)
		s.logger().WarnContext(ctx, "teardown", "cluster_id", row.ID, "error", err)
		return false
	case err != nil:
		return s.timedOut(ctx, t, at, "the cluster's API could not be reached: "+err.Error(), nil)
	case step.finished():
		t.FinishTime, t.Result = timestamppb.New(at), cloudv1.ClusterTeardownResult_CLUSTER_TEARDOWN_RESULT_FINISHED
		reconcile.AddNote(ctx, reconcile.Normal(reconcile.ReasonTeardownFinished,
			"the load balancers are gone; removing the tenant's components, then the platform's"))
		return true
	case expired:
		return s.timedOut(ctx, t, at, step.message, step.waiting)
	}
	t.Message, t.Waiting = step.message, refs(step.waiting)
	setDeleteStage(ctx, stageLoadBalancers, step.message)
	return false
}

// stepTeardown lists the workload cluster's objects of the stages' kinds
// and asks to delete those of the current stage.
func (s *Reconciler) stepTeardown(ctx context.Context, row *model.Cluster, stages []teardownStage) (teardownStep, error) {
	td, err := s.deps.Teardowns.Open(ctx, kube.TenantNamespace(row.TenantID), storedStatus(row).GetKubeconfigSecret())
	if err != nil {
		return teardownStep{}, err
	}
	objs, err := td.List(ctx, stageKinds(stages)...)
	if err != nil {
		return teardownStep{}, err
	}
	step := planTeardown(stages, objs)
	for _, o := range step.delete {
		if err := td.Delete(ctx, o); err != nil {
			return teardownStep{}, fmt.Errorf("delete %s: %w", o.Ref(), err)
		}
		s.logger().InfoContext(ctx, "teardown: deleting", "cluster_id", row.ID, "object", o.Ref())
	}
	return step, nil
}

// timedOut ends the teardown after its timeout: the deletion goes on, and
// a warning says what is still there.
func (s *Reconciler) timedOut(ctx context.Context, t *cloudv1.ClusterTeardown, at time.Time, what string,
	left []reconcile.WorkloadObject) bool {
	t.FinishTime, t.Result = timestamppb.New(at), cloudv1.ClusterTeardownResult_CLUSTER_TEARDOWN_RESULT_TIMED_OUT
	t.Waiting = refs(left)
	t.Message = fmt.Sprintf("timed out after %s (%s); going on: %s", s.teardownTimeout(), what, loadBalancerLeftBehind)
	reconcile.AddNote(ctx, reconcile.Warning(reconcile.ReasonTeardownTimedOut, t.Message))
	return true
}

func (s *Reconciler) teardownTimeout() time.Duration {
	if s.deps.TeardownTimeout > 0 {
		return s.deps.TeardownTimeout
	}
	return reconcile.DefaultTeardownTimeout
}

func refs(objs []reconcile.WorkloadObject) []string {
	out := make([]string, 0, len(objs))
	for _, o := range objs {
		out = append(out, o.Ref())
	}
	return out
}

// teardownKey holds the teardown state one pass computed, for the status
// the pass records (withTeardownStatus).
type teardownKey struct{}

type teardownHolder struct {
	t *cloudv1.ClusterTeardown
	// nodes and sweep are the node groups' stage and the cloud sweep
	// (delete_cloud_status.go); set says the pass computed them (nil:
	// cleared).
	nodes, sweep       *cloudv1.ClusterTeardown
	nodesSet, sweepSet bool
}

// withTeardownHolder returns a context a pass's teardown leaves its state
// in.
func withTeardownHolder(ctx context.Context) context.Context {
	return context.WithValue(ctx, teardownKey{}, &teardownHolder{})
}

func setTeardown(ctx context.Context, t *cloudv1.ClusterTeardown) {
	if h, ok := ctx.Value(teardownKey{}).(*teardownHolder); ok {
		h.t = t
	}
}

// withTeardownStatus puts the teardown into a status being built: the
// one this pass computed, else the one recorded.
func withTeardownStatus(ctx context.Context, st, stored *cloudv1.ClusterStatus) {
	withDeleteClocksStatus(ctx, st, stored)
	t := stored.GetTeardown()
	if h, ok := ctx.Value(teardownKey{}).(*teardownHolder); ok && h.t != nil {
		t = h.t
	}
	if t == nil {
		return
	}
	st.Teardown = t
}
