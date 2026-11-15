package cluster

import (
	"context"
	"fmt"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// leftBehind is what a skipped or timed-out teardown may leave in the
// cloud, and what that blocks.
const leftBehind = "load balancers and their security groups (of Services of type LoadBalancer and Ingresses), " +
	"Karpenter's EC2 instances, launch templates and instance profiles may be left in the account; " +
	"network delete then waits until they are deleted"

// teardown is the first step of a cluster's deletion: in the workload
// cluster, what its controllers made in the cloud is deleted while those
// controllers still run (planTeardown), before the platform components
// that are those controllers are uninstalled. It reports whether the
// deletion may go on. It is skipped (with a warning) when the deletion is
// forced or the cluster's API cannot be reached, and ends after the
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
	reachable := !row.DeleteForce && storedStatus(row).GetKubeconfigSecret() != ""
	if reachable {
		active, err := s.eksActive(ctx, records)
		if err != nil {
			return false, err
		}
		reachable = active
	}
	return s.runTeardown(ctx, row, prev, reachable), nil
}

// runTeardown is teardown once it knows whether the cluster's API can be
// reached; it records the step's state (withTeardown) and its events.
func (s *Reconciler) runTeardown(ctx context.Context, row *model.Cluster, prev *cloudv1.ClusterTeardown, reachable bool) bool {
	at := now()
	t := &cloudv1.ClusterTeardown{StartTime: prev.GetStartTime()}
	if t.StartTime == nil {
		t.StartTime = timestamppb.New(at)
	}
	defer setTeardown(ctx, t)
	if !reachable {
		why := "the cluster's API cannot be reached (its EKS cluster is not ACTIVE)"
		if row.DeleteForce {
			why = "the deletion is forced"
		}
		t.FinishTime, t.Result = timestamppb.New(at), cloudv1.ClusterTeardownResult_CLUSTER_TEARDOWN_RESULT_SKIPPED
		t.Message = "skipped: " + why + "; " + leftBehind
		reconcile.AddNote(ctx, reconcile.Warning(reconcile.ReasonTeardownSkipped, t.Message))
		return true
	}
	if prev == nil {
		reconcile.AddNote(ctx, reconcile.Normal(reconcile.ReasonTeardownStarted,
			"deleting what the cluster's controllers made in the cloud (load balancers, Karpenter's nodes) before its platform components"))
	}
	step, err := s.stepTeardown(ctx, row)
	expired := at.Sub(t.StartTime.AsTime()) >= s.teardownTimeout()
	switch {
	case err != nil && !expired:
		t.Message = "cannot reach the cluster's API: " + err.Error() + "; trying again"
		s.logger().WarnContext(ctx, "teardown", "cluster_id", row.ID, "error", err)
		return false
	case err != nil:
		return s.timedOut(ctx, t, at, "the cluster's API could not be reached: "+err.Error(), nil)
	case step.finished():
		t.FinishTime, t.Result = timestamppb.New(at), cloudv1.ClusterTeardownResult_CLUSTER_TEARDOWN_RESULT_FINISHED
		reconcile.AddNote(ctx, reconcile.Normal(reconcile.ReasonTeardownFinished,
			"the load balancers and Karpenter's nodes are gone; uninstalling the platform components"))
		return true
	case expired:
		return s.timedOut(ctx, t, at, step.message, step.waiting)
	}
	t.Message, t.Waiting = step.message, refs(step.waiting)
	return false
}

// stepTeardown lists the workload cluster's objects and asks to delete
// those of the current stage.
func (s *Reconciler) stepTeardown(ctx context.Context, row *model.Cluster) (teardownStep, error) {
	td, err := s.deps.Teardowns.Open(ctx, kube.TenantNamespace(row.TenantID), storedStatus(row).GetKubeconfigSecret())
	if err != nil {
		return teardownStep{}, err
	}
	objs, err := td.List(ctx)
	if err != nil {
		return teardownStep{}, err
	}
	step := planTeardown(objs)
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
	t.Message = fmt.Sprintf("timed out after %s (%s); going on: %s", s.teardownTimeout(), what, leftBehind)
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

type teardownHolder struct{ t *cloudv1.ClusterTeardown }

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
// one this pass computed, else the one recorded. While it runs, it is the
// deleting cluster's message: what it waits for.
func withTeardownStatus(ctx context.Context, st, stored *cloudv1.ClusterStatus) {
	t := stored.GetTeardown()
	if h, ok := ctx.Value(teardownKey{}).(*teardownHolder); ok && h.t != nil {
		t = h.t
	}
	if t == nil {
		return
	}
	st.Teardown = t
	if st.GetState() == cloudv1.ClusterState_CLUSTER_STATE_DELETING && t.GetFinishTime() == nil && t.GetMessage() != "" &&
		!strings.HasPrefix(t.GetMessage(), "cannot reach") {
		st.Message = "deleting; " + t.GetMessage() + " before the platform components are uninstalled"
	}
}
