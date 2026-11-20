package cluster

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/crossplane/managed"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/helmrel"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// The messages are the ones provider-helm reported on a live cluster.
const (
	capsuleErr  = `update failed: failed to upgrade release: could not get server version from Kubernetes: Get "https://ABC.gr7.eu-west-1.eks.amazonaws.com/version?timeout=32s": net/http: TLS handshake timeout`
	gpuErr      = `update failed: failed to upgrade release: pre-upgrade hooks failed: resource Job/gpu-operator/gpu-operator-upgrade-crd not ready, status: InProgress, message: Job in progress`
	kyvernoLock = `update failed: failed to upgrade release: another operation (install/upgrade/rollback) is in progress`
)

func noJitter(t *testing.T) {
	t.Helper()
	was := helmJitter
	helmJitter = func() float64 { return 0 }
	t.Cleanup(func() { helmJitter = was })
}

// again is a pass of the cluster after the one that produced p: its
// recorded status is what p reported.
func (w world) again(t *testing.T, p *platform) *model.Cluster {
	t.Helper()
	return w.withStored(t, p.components...)
}

func (w world) helmPass(t *testing.T, row *model.Cluster) (*platform, []reconcile.Note) {
	t.Helper()
	ctx := reconcile.WithNotes(t.Context())
	p, err := w.svc.installPlatform(ctx, row)
	if err != nil {
		t.Fatal(err)
	}
	return p, reconcile.Notes(ctx)
}

func annotation(u *unstructured.Unstructured, key string) string { return u.GetAnnotations()[key] }

func componentOf(p *platform, name string) *cloudv1.PlatformComponent {
	for _, c := range p.components {
		if c.GetName() == name {
			return c
		}
	}
	return nil
}

func hasReason(notes []reconcile.Note, reason string) int {
	n := 0
	for _, note := range notes {
		if note.Reason == reason {
			n++
		}
	}
	return n
}

// A failed release attempt pauses the Release for the first backoff step
// (one minute) with the platform's marker, counts once, and reports the
// classified error and when it is tried again.
func TestFailedReleaseIsPausedForBackoff(t *testing.T) {
	atStuckTime(t)
	noJitter(t)
	w, _ := platformWorld(t)
	w.helmConfig(t)
	w.install(t)
	w.failing(t, "base", "failed", gpuErr, stuckAt.Add(-time.Second), 1)

	p, notes := w.helmPass(t, w.cluster(t))

	u := w.release(t, "base")
	if !managed.IsPaused(u) || annotation(u, helmrel.BackoffAnnotation) != stuckAt.Add(time.Minute).Format(time.RFC3339) {
		t.Fatalf("annotations %v: want the platform's pause for one minute", u.GetAnnotations())
	}
	base := componentOf(p, "base")
	if base.GetAttempts() != 1 || base.GetCountedFailures() != 1 || base.GetLastErrorClass() != "release" ||
		!base.GetNextRetryAt().AsTime().Equal(stuckAt.Add(time.Minute)) {
		t.Fatalf("base = %+v", base)
	}
	if base.GetPhase() != cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED || !strings.Contains(base.GetMessage(), gpuErr) {
		t.Errorf("a release error that is being retried stays a failure: %s %q", base.GetPhase(), base.GetMessage())
	}
	if hasReason(notes, reconcile.ReasonHelmReleaseFailed) != 0 {
		t.Error("one failure is not a give-up")
	}
	if base.GetDigest() == "" || base.GetDigest() != annotation(u, helmrel.DigestAnnotation) {
		t.Errorf("digest %q", base.GetDigest())
	}
}

// The same failure seen on the next pass (the cache has not caught up with
// the pause) is not counted again.
func TestTheSameFailureIsCountedOnce(t *testing.T) {
	atStuckTime(t)
	noJitter(t)
	w, _ := platformWorld(t)
	w.helmConfig(t)
	w.install(t)
	w.failing(t, "base", "failed", gpuErr, stuckAt.Add(-time.Second), 1)
	first, _ := w.helmPass(t, w.cluster(t))
	// The pause is lost to a stale read: the Release looks unpaused again.
	u := w.release(t, "base")
	u.SetAnnotations(map[string]string{crossplaneaws.ExternalNameAnnotation: "base", helmrel.DigestAnnotation: annotation(u, helmrel.DigestAnnotation)})
	if err := w.kube.Update(t.Context(), u); err != nil {
		t.Fatal(err)
	}

	second, _ := w.helmPass(t, w.again(t, first))

	if base := componentOf(second, "base"); base.GetAttempts() != 1 || base.GetCountedFailures() != 1 {
		t.Fatalf("base = %+v, want one attempt", base)
	}
	if !managed.IsPaused(w.release(t, "base")) {
		t.Error("the pause is put back")
	}
}

// Capsule: the workload API was unreachable. Backed off, but never a
// failure: the component keeps INSTALLING and nothing is counted.
func TestTransientErrorIsNeverAFailure(t *testing.T) {
	atStuckTime(t)
	noJitter(t)
	w, _ := platformWorld(t)
	w.helmConfig(t)
	w.install(t)
	w.failing(t, "base", "deployed", capsuleErr, stuckAt.Add(-time.Second), 1)

	p, _ := w.helmPass(t, w.cluster(t))

	base := componentOf(p, "base")
	if base.GetCountedFailures() != 0 || base.GetAttempts() != 1 || base.GetLastErrorClass() != "transient" {
		t.Errorf("base = %+v", base)
	}
	if base.GetPhase() != cloudv1.ComponentPhase_COMPONENT_PHASE_INSTALLING {
		t.Errorf("phase %s: a transient error is not a failure", base.GetPhase())
	}
	if !managed.IsPaused(w.release(t, "base")) {
		t.Error("a transient error is still backed off, not retried every poll")
	}
}

// When the backoff has run out the pause is lifted, the attempts kept.
func TestBackoffEndsWithAnUnpause(t *testing.T) {
	atStuckTime(t)
	noJitter(t)
	w, _ := platformWorld(t)
	w.helmConfig(t)
	w.install(t)
	w.failing(t, "base", "failed", gpuErr, stuckAt.Add(-time.Second), 1)
	first, _ := w.helmPass(t, w.cluster(t))
	stuckAtLater := stuckAt.Add(61 * time.Second)
	now = func() time.Time { return stuckAtLater }

	before, _ := w.helmPass(t, w.again(t, first)) // due since 60s

	u := w.release(t, "base")
	if managed.IsPaused(u) || annotation(u, helmrel.BackoffAnnotation) != "" {
		t.Fatalf("annotations %v: the pause must be lifted", u.GetAnnotations())
	}
	if base := componentOf(before, "base"); base.GetAttempts() != 1 {
		t.Errorf("attempts %d", base.GetAttempts())
	}
}

func TestBackoffNotOverYet(t *testing.T) {
	atStuckTime(t)
	noJitter(t)
	w, _ := platformWorld(t)
	w.helmConfig(t)
	w.install(t)
	w.failing(t, "base", "failed", gpuErr, stuckAt.Add(-time.Second), 1)
	first, _ := w.helmPass(t, w.cluster(t))
	now = func() time.Time { return stuckAt.Add(30 * time.Second) }

	w.helmPass(t, w.again(t, first))

	if !managed.IsPaused(w.release(t, "base")) {
		t.Error("lifted before the backoff ran out")
	}
}

// After the limit of counted failures the component is FAILED with the
// classified error, the backoff is at its cap, and HelmReleaseFailed is
// emitted once.
func TestGivesUpAfterTheLimitAndSaysSoOnce(t *testing.T) {
	atStuckTime(t)
	noJitter(t)
	w, _ := platformWorld(t, func(d *reconcile.Deps) { d.HelmMaxFailures, d.HelmBackoffMax = 3, 20*time.Minute })
	w.helmConfig(t)
	w.install(t)
	digest := annotation(w.release(t, "base"), helmrel.DigestAnnotation)
	w.failing(t, "base", "failed", gpuErr, stuckAt.Add(-time.Second), 3)
	row := w.withStored(t, &cloudv1.PlatformComponent{Name: "base", Version: "1.2.0", Chart: "base", Digest: digest,
		Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED, Attempts: 2, CountedFailures: 2, Failures: 2,
		LastError: gpuErr, LastErrorClass: "release", NextRetryAt: nil})

	p, notes := w.helmPass(t, row)

	base := componentOf(p, "base")
	if base.GetPhase() != cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED || base.GetCountedFailures() != 3 ||
		!strings.Contains(base.GetMessage(), "gave up after 3 failed attempts") || !strings.Contains(base.GetMessage(), gpuErr) {
		t.Fatalf("base = %s %q %d", base.GetPhase(), base.GetMessage(), base.GetCountedFailures())
	}
	if !base.GetNextRetryAt().AsTime().Equal(stuckAt.Add(20 * time.Minute)) {
		t.Errorf("next retry %v, want the cap, 20m", base.GetNextRetryAt().AsTime())
	}
	if hasReason(notes, reconcile.ReasonHelmReleaseFailed) != 1 {
		t.Fatalf("notes %+v, want HelmReleaseFailed once", notes)
	}
	if u := w.release(t, "base"); annotation(u, helmrel.GaveUpAnnotation) != "true" || !managed.IsPaused(u) {
		t.Errorf("annotations %v", u.GetAnnotations())
	}
	// The next pass (paused, waiting) says nothing more.
	_, again := w.helmPass(t, w.again(t, p))
	if hasReason(again, reconcile.ReasonHelmReleaseFailed) != 0 {
		t.Error("HelmReleaseFailed is emitted once")
	}
}

// A transient error does not take the component any nearer to giving up.
func TestTransientErrorsNeverGiveUp(t *testing.T) {
	atStuckTime(t)
	noJitter(t)
	w, _ := platformWorld(t, func(d *reconcile.Deps) { d.HelmMaxFailures = 1 })
	w.helmConfig(t)
	w.install(t)
	w.failing(t, "base", "deployed", kyvernoLock, stuckAt.Add(-time.Second), 5)

	p, notes := w.helmPass(t, w.cluster(t))

	if base := componentOf(p, "base"); base.GetCountedFailures() != 0 || base.GetPhase() == cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED {
		t.Errorf("base = %+v", base)
	}
	if hasReason(notes, reconcile.ReasonHelmReleaseFailed) != 0 {
		t.Error("gave up on a transient error")
	}
}

// A new digest (another version or other values) ends the backoff at once
// and starts the count over.
func TestNewDigestStartsAgain(t *testing.T) {
	atStuckTime(t)
	noJitter(t)
	w, src := platformWorld(t)
	w.helmConfig(t)
	w.install(t)
	oldDigest := annotation(w.release(t, "base"), helmrel.DigestAnnotation)
	w.failing(t, "base", "failed", gpuErr, stuckAt.Add(-time.Second), 4)
	u := w.release(t, "base")
	u.SetAnnotations(map[string]string{crossplaneaws.ExternalNameAnnotation: "base", helmrel.DigestAnnotation: oldDigest,
		managed.PausedAnnotation: "true", helmrel.BackoffAnnotation: stuckAt.Add(time.Hour).Format(time.RFC3339)})
	if err := w.kube.Update(t.Context(), u); err != nil {
		t.Fatal(err)
	}
	row := w.withStored(t, &cloudv1.PlatformComponent{Name: "base", Version: "1.2.0", Chart: "base", Digest: oldDigest,
		Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED, Attempts: 4, CountedFailures: 4, Failures: 4, LastError: gpuErr})
	src.release.Spec.Content.Kubernetes[0].Components[0].Version = "1.3.0" // base moves

	p, _ := w.helmPass(t, row)

	live := w.release(t, "base")
	if annotation(live, helmrel.DigestAnnotation) == oldDigest {
		t.Fatal("the Release was not written with the new version")
	}
	if managed.IsPaused(live) {
		t.Errorf("annotations %v: the platform's pause must end with a new digest", live.GetAnnotations())
	}
	if base := componentOf(p, "base"); base.GetAttempts() != 0 || base.GetCountedFailures() != 0 {
		t.Errorf("base = %+v, want the count started over", base)
	}
}

// An operator's pause (or a freeze's) is never touched, never counted.
func TestAnOperatorsPauseWins(t *testing.T) {
	atStuckTime(t)
	noJitter(t)
	w, _ := platformWorld(t)
	w.helmConfig(t)
	w.install(t)
	w.failing(t, "base", "failed", gpuErr, stuckAt.Add(-time.Second), 1)
	u := w.release(t, "base")
	annotations := u.GetAnnotations()
	annotations[managed.PausedAnnotation] = "true"
	u.SetAnnotations(annotations)
	if err := w.kube.Update(t.Context(), u); err != nil {
		t.Fatal(err)
	}

	p, _ := w.helmPass(t, w.cluster(t))

	live := w.release(t, "base")
	if !managed.IsPaused(live) || annotation(live, helmrel.BackoffAnnotation) != "" {
		t.Errorf("annotations %v: only the operator's pause", live.GetAnnotations())
	}
	if base := componentOf(p, "base"); base.GetAttempts() != 0 || base.GetCountedFailures() != 0 {
		t.Errorf("base = %+v", base)
	}
}

// A cluster that is imported and not taken over has no Release objects:
// nothing is paused, nothing created.
func TestImportedClusterIsNeverPaused(t *testing.T) {
	w, _ := platformWorld(t)
	row := w.cluster(t)
	row.Imported, row.State = true, StateImported

	p, err := w.svc.installPlatform(t.Context(), row)

	if err != nil || len(p.components) != 0 {
		t.Fatalf("%+v %v", p, err)
	}
	if w.release(t, "base") != nil {
		t.Error("a Release exists for an imported cluster")
	}
}

// cluster retry --component lifts the platform's pause and starts the
// count again.
func TestRetryEndsTheBackoff(t *testing.T) {
	atStuckTime(t)
	noJitter(t)
	w, _ := platformWorld(t)
	w.helmConfig(t)
	w.install(t)
	w.failing(t, "base", "failed", gpuErr, stuckAt.Add(-time.Second), 3)
	first, _ := w.helmPass(t, w.cluster(t))
	row := w.again(t, first)
	ctx := withRetry(reconcile.WithNotes(t.Context()), &model.Retry{At: stuckAt, By: "alice", Components: []string{"base"}})

	p, err := w.svc.installPlatform(ctx, row)

	if err != nil {
		t.Fatal(err)
	}
	if u := w.release(t, "base"); managed.IsPaused(u) {
		t.Errorf("annotations %v", u.GetAnnotations())
	}
	if base := componentOf(p, "base"); base.GetAttempts() != 0 || base.GetCountedFailures() != 0 {
		t.Errorf("base = %+v", base)
	}
}

func TestPlatformRetryAt(t *testing.T) {
	soon, later := stuckAt.Add(time.Minute), stuckAt.Add(time.Hour)
	row := func(cs ...*cloudv1.PlatformComponent) *model.Cluster {
		raw, err := Encode(&cloudv1.ClusterStatus{PlatformComponents: cs})
		if err != nil {
			t.Fatal(err)
		}
		r := &model.Cluster{}
		r.Status = raw
		return r
	}
	backing := func(name string, at time.Time) *cloudv1.PlatformComponent {
		return &cloudv1.PlatformComponent{Name: name, Attempts: 1, Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED, NextRetryAt: timestamppb.New(at)}
	}

	got, ok := PlatformRetryAt(row(backing("a", later), backing("b", soon), &cloudv1.PlatformComponent{Name: "c"}))
	if !ok || !got.Equal(soon) {
		t.Errorf("%v %v, want the earliest retry", got, ok)
	}
	if _, ok := PlatformRetryAt(row(&cloudv1.PlatformComponent{Name: "ready", Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_READY})); ok {
		t.Error("nothing backing off")
	}
	if _, ok := PlatformRetryAt(row()); ok {
		t.Error("no components")
	}
}
