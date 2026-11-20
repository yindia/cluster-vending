package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/crossplane/managed"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/helmrel"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// helmJitter is the random share of a backoff delay; a test fixes it.
var helmJitter = rand.Float64

func (s *Reconciler) helmPolicy() helmrel.Policy {
	return helmrel.Policy{BackoffMax: s.deps.HelmBackoffMax, MaxFailures: s.deps.HelmMaxFailures}
}

// watchRelease is what a pass does about a component's Release that is
// not deployed and Ready: the backoff after failures (backoff), then a
// retry a person asked for or the recovery of a stuck release (recover).
// prev is the component as the last pass recorded it (nil: never).
func (s *Reconciler) watchRelease(ctx context.Context, row *model.Cluster, namespace, object string,
	prev, c *cloudv1.PlatformComponent, d *releaseDetail, at time.Time) {
	s.backoff(ctx, row, namespace, object, prev, c, d, at)
	s.recover(ctx, row, namespace, object, c, d, at)
}

// stateOf is what a component's recorded status remembers of the Release
// the phase is about.
func stateOf(prev *cloudv1.PlatformComponent, d *releaseDetail) helmrel.State {
	if prev == nil || prev.GetChart() != d.Chart {
		return helmrel.State{}
	}
	st := helmrel.State{Digest: prev.GetDigest(), Attempts: prev.GetAttempts(), Counted: prev.GetCountedFailures(),
		Failures: prev.GetFailures(), Class: helmrel.ErrorClass(prev.GetLastErrorClass()), LastError: prev.GetLastError()}
	if t := prev.GetNextRetryAt(); t != nil {
		st.NextRetry = t.AsTime()
	}
	return st
}

// backoff applies the policy of helmrel.Step to the component's Release
// (pause it after a failed attempt, lift the pause when the backoff has run
// out, give up after the limit) and reports the result on c: the Helm
// facts, the attempts, the next retry and the class of the last error.
// A person's retry (cluster retry --component) lifts our pause and starts
// the count again. Never on a Release someone else paused.
func (s *Reconciler) backoff(ctx context.Context, row *model.Cluster, namespace, object string,
	prev, c *cloudv1.PlatformComponent, d *releaseDetail, at time.Time) {
	c.Digest, c.Revision, c.ChartVersion = d.Digest, d.Revision, d.ChartVersion
	policy := s.helmPolicy()
	if retrying(ctx, c.GetName()) {
		if d.PausedByUs {
			s.setBackoff(ctx, row, namespace, object, time.Time{}, false)
		}
		return // a person retried: the count starts again
	}
	st := stateOf(prev, d)
	st.GaveUp = st.Counted >= policy.Limit()
	obs := helmrel.Observation{Digest: d.Digest, Ready: d.Ready && d.Synced && d.HelmState == crossplaneaws.HelmDeployed,
		HelmState: d.HelmState, Failures: d.Failures, Error: d.LastError, Paused: d.Paused || d.PausedByUs,
		PausedByUs: d.PausedByUs, BackoffUntil: d.BackoffUntil, Timeout: d.Timeout}
	dec := helmrel.Step(st, obs, policy, at, helmJitter())
	switch dec.Action {
	case helmrel.None:
	case helmrel.Pause:
		s.setBackoff(ctx, row, namespace, object, dec.Until, dec.State.GaveUp)
	case helmrel.Unpause:
		s.clearFailedHooks(ctx, row, namespace, d, dec.State)
		s.setBackoff(ctx, row, namespace, object, time.Time{}, false)
	}
	reportBackoff(c, dec.State, d, dec.Action == helmrel.Pause || d.PausedByUs && dec.Action != helmrel.Unpause)
	if dec.GaveUp {
		reconcile.AddNote(ctx, reconcile.Warning(reconcile.ReasonHelmReleaseFailed, fmt.Sprintf(
			"%s: chart %s failed %d times in a row (%s error), giving up; it is tried again every %s until its version or values change or it is retried: %s",
			c.GetName(), d.Chart, dec.State.Counted, dec.State.Class, policy.Cap(), dec.State.LastError)))
		s.logger().WarnContext(ctx, "helm release failed: giving up", "cluster_id", row.ID, "cluster", row.Name, "tenant_id", row.TenantID,
			"component", c.GetName(), "chart", d.Chart, "failures", dec.State.Counted, "class", string(dec.State.Class), "error", dec.State.LastError)
	}
}

// reportBackoff writes the remembered state on c, and what it means for
// the phase: a transient error is never a failure (INSTALLING, quietly
// retried); a release or chart error that is being retried stays the
// failure the provider reported, with when it is tried again; a component
// the platform gave up on is FAILED and says so.
func reportBackoff(c *cloudv1.PlatformComponent, st helmrel.State, d *releaseDetail, backingOff bool) {
	c.Attempts, c.CountedFailures, c.LastErrorClass = st.Attempts, st.Counted, st.Class.String()
	if !st.NextRetry.IsZero() {
		c.NextRetryAt = timestamppb.New(st.NextRetry)
	}
	if st.Digest != "" {
		c.Digest = st.Digest
	}
	if st.Attempts == 0 || d.LastError == "" {
		return
	}
	switch {
	case st.GaveUp:
		c.Phase = cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED
		c.Message = fmt.Sprintf("gave up after %d failed attempts (%s error), next try at %s: %s",
			st.Counted, st.Class, st.NextRetry.UTC().Format(time.RFC3339), d.LastError)
	case st.Class == helmrel.ClassTransient:
		c.Phase = cloudv1.ComponentPhase_COMPONENT_PHASE_INSTALLING
		c.Message = fmt.Sprintf("retrying after a transient error, next try at %s: %s", st.NextRetry.UTC().Format(time.RFC3339), d.LastError)
	case backingOff:
		c.Phase = cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED
		c.Message = fmt.Sprintf("chart %s: %s (%s error, attempt %d, next try at %s)", d.Chart, d.LastError, st.Class, st.Attempts,
			st.NextRetry.UTC().Format(time.RFC3339))
	}
}

// setBackoff pauses the Release until `until` with the platform's marker
// (zero `until` lifts it: pause and markers removed). A merge patch of
// the metadata only; the Release's spec is the sequence engine's.
func (s *Reconciler) setBackoff(ctx context.Context, row *model.Cluster, namespace, object string, until time.Time, gaveUp bool) {
	annotations := map[string]any{managed.PausedAnnotation: nil, helmrel.BackoffAnnotation: nil, helmrel.GaveUpAnnotation: nil}
	if !until.IsZero() {
		annotations[managed.PausedAnnotation] = "true"
		annotations[helmrel.BackoffAnnotation] = until.UTC().Format(time.RFC3339)
		if gaveUp {
			annotations[helmrel.GaveUpAnnotation] = "true"
		}
	}
	raw, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": annotations}})
	if err != nil {
		return
	}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(crossplaneaws.HelmReleaseGVK)
	u.SetNamespace(namespace)
	u.SetName(object)
	if err := client.IgnoreNotFound(s.deps.Kube.Patch(ctx, u, client.RawPatch(client.Merge.Type(), raw))); err != nil {
		s.logger().WarnContext(ctx, "helm release backoff not recorded on the Release", "cluster_id", row.ID, "release", object, "error", err)
	}
}

// clearFailedHooks deletes the failed hook Jobs a failed attempt left in
// the workload cluster before the release is tried again: a retry's hook
// of the same name would collide with them. Best effort; only after a
// release error that names hooks.
func (s *Reconciler) clearFailedHooks(ctx context.Context, row *model.Cluster, namespace string, d *releaseDetail, st helmrel.State) {
	if st.Class != helmrel.ClassRelease || !strings.Contains(strings.ToLower(st.LastError), "hook") || s.deps.Workloads == nil {
		return
	}
	storage, err := s.deps.Workloads.HelmStorage(ctx, namespace, storedStatus(row).GetKubeconfigSecret())
	if err != nil {
		return
	}
	ops, ok := storage.(reconcile.HelmOperator)
	if !ok {
		return
	}
	names, err := ops.DeleteFailedHookJobs(ctx, d.TargetNamespace, d.Chart)
	if err != nil {
		s.logger().WarnContext(ctx, "failed hook Jobs not deleted", "cluster_id", row.ID, "chart", d.Chart, "error", err)
		return
	}
	if len(names) > 0 {
		s.logger().InfoContext(ctx, "deleted failed hook Jobs before the retry", "cluster_id", row.ID, "chart", d.Chart, "jobs", names)
	}
}

// PlatformRetryAt is the earliest time a platform component's failed Helm
// release is tried again, as the row's recorded status has it: the cluster
// is looked at again then although it is RUNNING (the pause is lifted by a
// pass, and no event says when). false when no component is backing off.
// Tenant components are lifted by whichever pass comes next.
func PlatformRetryAt(row *model.Cluster) (time.Time, bool) {
	var first time.Time
	for _, c := range storedStatus(row).GetPlatformComponents() {
		t := c.GetNextRetryAt()
		if t == nil || c.GetAttempts() == 0 || c.GetPhase() == cloudv1.ComponentPhase_COMPONENT_PHASE_READY {
			continue
		}
		if at := t.AsTime(); first.IsZero() || at.Before(first) {
			first = at
		}
	}
	return first, !first.IsZero()
}
