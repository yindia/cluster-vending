package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/helmrel"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// AnnotationRetriedAt is set on a component's Helm Release object when a
// person retries it (cluster retry): the write is an event provider-helm
// acts on at once, instead of at its next poll.
const AnnotationRetriedAt = "cloud.cluster-vending.io/retried-at"

type retryKey struct{}

// withRetry gives a pass the cluster's pending retry (model.Retry), for
// the components it installs.
func withRetry(ctx context.Context, r *model.Retry) context.Context {
	return context.WithValue(ctx, retryKey{}, r)
}

// retrying reports whether the pass is to retry the component.
func retrying(ctx context.Context, component string) bool {
	r, _ := ctx.Value(retryKey{}).(*model.Retry)
	return r.Has(component)
}

// recover is what a pass does about a component's Helm Release that is
// not deployed: a retry a person asked for (retryRelease), else the
// automatic recovery of a stuck release (recoverStuck).
func (s *Reconciler) recover(ctx context.Context, row *model.Cluster, namespace, object string, c *cloudv1.PlatformComponent, d *releaseDetail, at time.Time) {
	if !retrying(ctx, c.GetName()) {
		s.recoverStuck(ctx, row, namespace, c, d, at)
		return
	}
	c.Recoveries = 0 // a person retried: the automatic recovery may run again
	s.retryRelease(ctx, row, namespace, object, c, d, at)
}

// retryRelease tries a failed component's chart again now: a stuck Helm
// revision is removed at once (no stuckAfter wait, no recovery limit), as
// recoverStuck would; then provider-helm is told to act now. The
// component is INSTALLING again, and the cluster's event says what was
// done.
func (s *Reconciler) retryRelease(ctx context.Context, row *model.Cluster, namespace, object string, c *cloudv1.PlatformComponent, d *releaseDetail, at time.Time) {
	what := ""
	if stuckBy(d.HelmState, d.LastError) {
		what = s.removeStuckNow(ctx, row, namespace, d, at)
	}
	// A retry (or a recovery, which is one) gives the rollout a new install
	// timeout: a timed-out component is judged again from now, not failed
	// again at once on the old clock.
	if err := s.nudgeRelease(ctx, namespace, object, at, rolloutRunning(d)); err != nil {
		c.Message = fmt.Sprintf("retry: provider-helm could not be told to try again: %v", err)
		return
	}
	if what == "" {
		what = "provider-helm installs chart " + d.Chart + " again now"
	}
	c.Phase, c.Message = cloudv1.ComponentPhase_COMPONENT_PHASE_INSTALLING, "retried: "+what
	reconcile.AddNote(ctx, reconcile.Normal(reconcile.ReasonComponentRetried, fmt.Sprintf("retrying %s: %s", c.GetName(), what)))
	s.logger().InfoContext(ctx, "component retried", "cluster_id", row.ID, "cluster", row.Name, "tenant_id", row.TenantID,
		"component", c.GetName(), "chart", d.Chart, "helm_state", d.HelmState, "what", what)
}

// removeStuckNow removes the newest Helm revision when it is a stuck one,
// whatever its age; "" when there is none or the cluster cannot be
// reached (provider-helm is still told to try again).
func (s *Reconciler) removeStuckNow(ctx context.Context, row *model.Cluster, namespace string, d *releaseDetail, at time.Time) string {
	if s.deps.Workloads == nil {
		return ""
	}
	storage, err := s.deps.Workloads.HelmStorage(ctx, namespace, storedStatus(row).GetKubeconfigSecret())
	if err != nil {
		return ""
	}
	records, err := storage.Records(ctx, d.TargetNamespace, d.Chart)
	if err != nil {
		return ""
	}
	record, ok := stuckRecord(records, at, 0)
	if !ok || storage.Remove(ctx, d.TargetNamespace, record.Name) != nil {
		return ""
	}
	return fmt.Sprintf("removed its stuck Helm revision %d (%s); chart %s is installed again", record.Revision, record.Status, d.Chart)
}

// nudgeRelease annotates the component's Release object, an update
// provider-helm reconciles at once; restart also removes its rollout mark
// (helmrel.RolloutAnnotation), so the install timeout starts again.
func (s *Reconciler) nudgeRelease(ctx context.Context, namespace, object string, at time.Time, restart bool) error {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(crossplaneaws.HelmReleaseGVK)
	u.SetNamespace(namespace)
	u.SetName(object)
	annotations := map[string]any{AnnotationRetriedAt: at.UTC().Format(time.RFC3339)}
	if restart {
		annotations[helmrel.RolloutAnnotation] = nil
	}
	raw, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": annotations}})
	if err != nil {
		return err
	}
	return client.IgnoreNotFound(s.deps.Kube.Patch(ctx, u, client.RawPatch(client.Merge.Type(), raw)))
}

// retryUpgrade tries the cluster's failed automatic upgrade again: its
// rollout record is in progress from now (retriedUpgrade).
func (s *Reconciler) retryUpgrade(ctx context.Context, row *model.Cluster) error {
	if len(row.Rollout) == 0 {
		return nil // no automatic upgrade to retry
	}
	rec := &cloudv1.ClusterRollout{}
	if err := Lenient.Unmarshal(row.Rollout, rec); err != nil {
		s.logger().WarnContext(ctx, "retry: the cluster's rollout record cannot be read; nothing retried",
			"cluster_id", row.ID, "error", err)
		return nil // a record this build cannot read has no upgrade to retry; the pass goes on
	}
	next, ok := retriedUpgrade(rec, now(), row.Retry.By)
	if !ok {
		return nil
	}
	raw, err := protojson.Marshal(next)
	if err != nil {
		return err
	}
	if err := s.deps.Store.Clusters().SetRollout(ctx, row.TenantID, row.ID, raw); err != nil {
		return fmt.Errorf("record the retried upgrade: %w", err)
	}
	reconcile.AddNote(ctx, reconcile.Normal(reconcile.ReasonUpgradeRetried, fmt.Sprintf(
		"retrying the automatic upgrade to %s (channel %s), asked by %s", next.GetToVersion(), next.GetChannel(), row.Retry.By)))
	return nil
}

// retriedUpgrade is a failed or timed-out upgrade's record, in progress
// again from at: the rollout follows it again (Finish), with a new
// upgrade-timeout, and it no longer counts against maxFailed. false for
// any other record.
func retriedUpgrade(rec *cloudv1.ClusterRollout, at time.Time, by string) (*cloudv1.ClusterRollout, bool) {
	switch rec.GetResult() {
	case cloudv1.ClusterRolloutResult_CLUSTER_ROLLOUT_RESULT_FAILED, cloudv1.ClusterRolloutResult_CLUSTER_ROLLOUT_RESULT_TIMED_OUT:
	default:
		return nil, false
	}
	next := proto.CloneOf(rec)
	next.Result, next.StartTime, next.FinishTime = cloudv1.ClusterRolloutResult_CLUSTER_ROLLOUT_RESULT_IN_PROGRESS, timestamppb.New(at), nil
	next.Message = "retried by " + by
	return next, true
}
