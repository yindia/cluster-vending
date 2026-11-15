package cluster

import (
	"context"
	"slices"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// A retried failed or timed-out upgrade is in progress again from now: it
// gets a new upgrade-timeout and stops counting against maxFailed. Any
// other record is left as it is.
func TestRetriedUpgradeRecord(t *testing.T) {
	at := time.Date(2026, 11, 15, 3, 0, 0, 0, time.UTC)
	failed := &cloudv1.ClusterRollout{Channel: "alpha", FromVersion: "1.36", ToVersion: "1.37",
		StartTime: timestamppb.New(at.Add(-2 * time.Hour)), FinishTime: timestamppb.New(at.Add(-time.Hour)),
		Result: cloudv1.ClusterRolloutResult_CLUSTER_ROLLOUT_RESULT_TIMED_OUT, Message: "not finished within 1h30m0s"}

	got, ok := retriedUpgrade(failed, at, "alice")

	if !ok || got.GetResult() != cloudv1.ClusterRolloutResult_CLUSTER_ROLLOUT_RESULT_IN_PROGRESS ||
		!got.GetStartTime().AsTime().Equal(at) || got.GetFinishTime() != nil || got.GetToVersion() != "1.37" {
		t.Fatalf("retried = %v, %v", got, ok)
	}
	if failed.GetResult() != cloudv1.ClusterRolloutResult_CLUSTER_ROLLOUT_RESULT_TIMED_OUT {
		t.Error("the input record was changed")
	}
	for _, r := range []cloudv1.ClusterRolloutResult{cloudv1.ClusterRolloutResult_CLUSTER_ROLLOUT_RESULT_IN_PROGRESS,
		cloudv1.ClusterRolloutResult_CLUSTER_ROLLOUT_RESULT_SUCCEEDED, cloudv1.ClusterRolloutResult_CLUSTER_ROLLOUT_RESULT_PENDING} {
		if _, ok := retriedUpgrade(&cloudv1.ClusterRollout{Result: r}, at, "alice"); ok {
			t.Errorf("%s retried", r)
		}
	}
	if _, ok := retriedUpgrade(nil, at, "alice"); ok {
		t.Error("no record retried")
	}
}

// A retry removes a stuck Helm revision at once: the stuckAfter wait and
// the recovery limit are for the automatic recovery, not for a person
// who asked.
func TestRetryRemovesAFreshStuckRevision(t *testing.T) {
	at := time.Date(2026, 11, 15, 3, 0, 0, 0, time.UTC)
	records := []reconcile.HelmRecord{
		{Name: "sh.helm.release.v1.kyverno.v1", Revision: 1, Status: "deployed", Changed: at.Add(-time.Hour)},
		{Name: "sh.helm.release.v1.kyverno.v2", Revision: 2, Status: "pending-upgrade", Changed: at.Add(-time.Minute)},
	}

	if _, ok := recordToRemove(records, at); ok {
		t.Fatal("the automatic recovery removed a revision younger than stuckAfter")
	}
	got, ok := stuckRecord(records, at, 0)

	if !ok || got.Revision != 2 {
		t.Fatalf("retry removes %+v (%v), want revision 2", got, ok)
	}
	records[1].Status = "failed"
	if _, ok := stuckRecord(records, at, 0); ok {
		t.Error("a failed revision is never removed")
	}
}

// The retry a pass carries: the components it names, nothing without one.
func TestRetryInContext(t *testing.T) {
	ctx := withRetry(context.Background(), &model.Retry{At: time.Unix(1, 0), Components: []string{"kyverno"}})

	if !retrying(ctx, "kyverno") || retrying(ctx, "istiod") {
		t.Fatal("retrying() does not follow the request")
	}
	if retrying(context.Background(), "kyverno") || retrying(withRetry(context.Background(), nil), "kyverno") {
		t.Fatal("retrying without a request")
	}
}

// A retried component is tried again at once, past what the automatic
// recovery allows (recovered 3 times already, a revision younger than
// stuckAfter): its stuck revision is removed, provider-helm is told to act
// (the Release object is annotated), it is INSTALLING, the counter starts
// over and the cluster's event says so.
func TestRetriedComponentIsTriedAgainNow(t *testing.T) {
	atStuckTime(t)
	fresh := stuckAt.Add(-time.Minute)
	workloads := &fakeWorkloads{records: []reconcile.HelmRecord{
		{Name: "sh.helm.release.v1.base.v1", Revision: 1, Status: "uninstalling", Changed: fresh}}}
	w, _ := platformWorld(t, func(d *reconcile.Deps) { d.Workloads, d.RecoverStuckReleases = workloads, true })
	w.helmConfig(t)
	w.install(t)
	w.failing(t, "base", "uninstalling", `"base" has no deployed releases`, fresh, 7)
	row := w.withStored(t, &cloudv1.PlatformComponent{Name: "base", Version: "1.2.0",
		Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED, Recoveries: 3})
	ctx := withRetry(reconcile.WithNotes(t.Context()), &model.Retry{At: stuckAt, By: "alice", Components: []string{"base"}})

	p, err := w.svc.installPlatform(ctx, row)

	if err != nil {
		t.Fatal(err)
	}
	base := p.components[0]
	if len(workloads.removed) != 1 || base.GetPhase() != cloudv1.ComponentPhase_COMPONENT_PHASE_INSTALLING || base.GetRecoveries() != 0 {
		t.Fatalf("base = %s, %d recoveries, %q; removed %v", base.GetPhase(), base.GetRecoveries(), base.GetMessage(), workloads.removed)
	}
	if u := w.release(t, "base"); u.GetAnnotations()[AnnotationRetriedAt] == "" {
		t.Error("provider-helm was not told to act: no retried-at annotation on the Release")
	}
	notes := reconcile.Notes(ctx)
	if len(notes) != 1 || notes[0].Reason != reconcile.ReasonComponentRetried {
		t.Errorf("notes = %+v, want one ComponentRetried", notes)
	}
}

// A pass acts on the cluster's retry once: a timed-out automatic upgrade
// is in progress again (with an UpgradeRetried event), and the request is
// removed. A request that names no component is done after any pass.
func TestReconcileRetriesTheUpgradeAndClearsTheRequest(t *testing.T) {
	events := &reconcile.RecordedEvents{}
	w := newWorld(t, interceptor.Funcs{}, nil, func(d *reconcile.Deps) { d.Events = events })
	ctx := t.Context()
	record := `{"channel":"alpha","fromVersion":"1.36","toVersion":"1.37","startTime":"2026-11-15T00:00:00Z",` +
		`"finishTime":"2026-11-15T01:30:00Z","result":"CLUSTER_ROLLOUT_RESULT_TIMED_OUT","message":"not finished within 1h30m0s"}`
	if err := w.store.Clusters().SetRollout(ctx, worldTenant, w.row.ID, model.JSON(record)); err != nil {
		t.Fatal(err)
	}
	if err := w.store.Clusters().RequestRetry(ctx, worldTenant, w.row.ID, model.Retry{At: time.Now().UTC(), By: "alice", Upgrade: true}); err != nil {
		t.Fatal(err)
	}

	if err := w.svc.Reconcile(ctx, worldTenant, w.row.ID); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	row := w.cluster(t)
	rec := &cloudv1.ClusterRollout{}
	if err := Lenient.Unmarshal(row.Rollout, rec); err != nil || rec.GetResult() != cloudv1.ClusterRolloutResult_CLUSTER_ROLLOUT_RESULT_IN_PROGRESS ||
		rec.GetFinishTime() != nil || rec.GetMessage() != "retried by alice" {
		t.Fatalf("record = %v, %v; want in progress again", rec, err)
	}
	if row.Retry != nil {
		t.Errorf("retry = %+v, want it cleared", row.Retry)
	}
	if !slices.Contains(events.Reasons(), reconcile.ReasonUpgradeRetried) {
		t.Errorf("events = %v, want UpgradeRetried", events.Reasons())
	}
}
