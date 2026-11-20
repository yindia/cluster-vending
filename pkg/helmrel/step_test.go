package helmrel_test

import (
	"testing"
	"time"

	"github.com/yindia/cluster-vending/pkg/helmrel"
)

var (
	t0     = time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	policy = helmrel.Policy{BackoffMax: 30 * time.Minute, MaxFailures: 5}
)

const (
	hookErr      = "update failed: failed to upgrade release: pre-upgrade hooks failed: resource Job/gpu-operator/gpu-operator-upgrade-crd not ready"
	transientErr = `update failed: failed to upgrade release: could not get server version from Kubernetes: Get "https://x/version?timeout=32s": net/http: TLS handshake timeout`
)

func failing(digest string, failures int32, msg string) helmrel.Observation {
	return helmrel.Observation{Digest: digest, Failures: failures, Error: msg, Timeout: 10 * time.Minute}
}

func TestFirstFailurePausesForOneMinute(t *testing.T) {
	d := helmrel.Step(helmrel.State{}, failing("d1", 1, hookErr), policy, t0, 0)

	if d.Action != helmrel.Pause || !d.Until.Equal(t0.Add(time.Minute)) {
		t.Fatalf("action %v until %v, want Pause for 1m", d.Action, d.Until)
	}
	if d.State.Attempts != 1 || d.State.Counted != 1 || d.State.Class != helmrel.ClassRelease {
		t.Errorf("state %+v", d.State)
	}
	if !d.State.NextRetry.Equal(d.Until) || d.State.Digest != "d1" || d.State.Failures != 1 || d.State.LastError != hookErr {
		t.Errorf("state %+v", d.State)
	}
	if d.GaveUp {
		t.Error("not given up yet")
	}
}

func TestTransientBacksOffButIsNeverCounted(t *testing.T) {
	st := helmrel.State{}
	at := t0
	for i := 1; i <= 8; i++ {
		d := helmrel.Step(st, failing("d1", int32(i), transientErr), policy, at, 0)
		if d.Action != helmrel.Pause {
			t.Fatalf("attempt %d: %v", i, d.Action)
		}
		if d.State.Counted != 0 || d.State.Class != helmrel.ClassTransient || d.GaveUp || d.State.GaveUp {
			t.Fatalf("attempt %d counted a transient error: %+v", i, d.State)
		}
		st, at = d.State, d.Until.Add(time.Second)
	}
	if st.Attempts != 8 {
		t.Errorf("attempts %d", st.Attempts)
	}
}

func TestBackoffDoublesAcrossFailuresAndGivesUpAtTheLimit(t *testing.T) {
	st, at := helmrel.State{}, t0
	var waits []time.Duration
	var gave []bool
	for i := 1; i <= 7; i++ {
		d := helmrel.Step(st, failing("d1", int32(i), hookErr), policy, at, 0)
		waits = append(waits, d.Until.Sub(at))
		gave = append(gave, d.GaveUp)
		st, at = d.State, d.Until
	}
	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 30 * time.Minute, 30 * time.Minute, 30 * time.Minute}
	for i := range want {
		if waits[i] != want[i] {
			t.Errorf("failure %d waits %s, want %s", i+1, waits[i], want[i])
		}
	}
	// GaveUp is reported once, by the failure that reached the limit.
	for i, g := range gave {
		if g != (i == 4) {
			t.Errorf("failure %d: GaveUp=%v", i+1, g)
		}
	}
	if !st.GaveUp || st.Counted != 7 {
		t.Errorf("final state %+v", st)
	}
}

func TestSameFailureObservedTwiceCountsOnce(t *testing.T) {
	first := helmrel.Step(helmrel.State{}, failing("d1", 1, hookErr), policy, t0, 0)

	// The pause is not in the cache yet: the same failure is seen again.
	again := helmrel.Step(first.State, failing("d1", 1, hookErr), policy, t0.Add(5*time.Second), 0)

	if again.State.Attempts != 1 || again.State.Counted != 1 {
		t.Errorf("recounted: %+v", again.State)
	}
	if again.Action != helmrel.Pause || !again.Until.Equal(first.Until) {
		t.Errorf("must keep the pause in place until the same time: %v %v", again.Action, again.Until)
	}
}

func TestPausedByUsWaitsThenUnpauses(t *testing.T) {
	first := helmrel.Step(helmrel.State{}, failing("d1", 1, hookErr), policy, t0, 0)
	paused := failing("d1", 1, hookErr)
	paused.Paused, paused.PausedByUs, paused.BackoffUntil = true, true, first.Until

	waiting := helmrel.Step(first.State, paused, policy, t0.Add(30*time.Second), 0)
	due := helmrel.Step(first.State, paused, policy, t0.Add(time.Minute), 0)

	if waiting.Action != helmrel.None || !waiting.State.NextRetry.Equal(first.Until) {
		t.Errorf("before the time: %v %+v", waiting.Action, waiting.State)
	}
	if due.Action != helmrel.Unpause {
		t.Errorf("at the time: %v", due.Action)
	}
	if due.State.Attempts != 1 {
		t.Errorf("unpausing must keep the attempts: %+v", due.State)
	}
}

func TestRetryThatFailsAgainCountsOnFailedIncreasing(t *testing.T) {
	first := helmrel.Step(helmrel.State{}, failing("d1", 1, hookErr), policy, t0, 0)
	// Unpaused at the time; the provider has not retried yet: same failed count, same error.
	waiting := helmrel.Step(first.State, failing("d1", 1, hookErr), policy, t0.Add(61*time.Second), 0)
	// The retry failed: failed is 2.
	failedAgain := helmrel.Step(first.State, failing("d1", 2, hookErr), policy, t0.Add(75*time.Second), 0)

	if waiting.Action != helmrel.None || waiting.State.Attempts != 1 {
		t.Errorf("a retry that has not run yet is not a failure: %v %+v", waiting.Action, waiting.State)
	}
	if failedAgain.Action != helmrel.Pause || failedAgain.State.Attempts != 2 || failedAgain.Until.Sub(t0.Add(75*time.Second)) != 2*time.Minute {
		t.Errorf("second failure: %v %+v until %v", failedAgain.Action, failedAgain.State, failedAgain.Until)
	}
}

func TestRetryThatNeverReportsIsCountedAfterTheWaitTimeout(t *testing.T) {
	first := helmrel.Step(helmrel.State{}, failing("d1", 1, hookErr), policy, t0, 0)
	late := first.State.NextRetry.Add(10*time.Minute + 2*time.Minute + time.Second)

	d := helmrel.Step(first.State, failing("d1", 1, hookErr), policy, late, 0)

	if d.Action != helmrel.Pause || d.State.Attempts != 2 {
		t.Errorf("an identical failure after the timeout is a new attempt: %v %+v", d.Action, d.State)
	}
}

func TestNewDigestResetsEverythingAndUnpauses(t *testing.T) {
	st := helmrel.State{Digest: "old", Attempts: 4, Counted: 4, Failures: 4, NextRetry: t0.Add(time.Hour), Class: helmrel.ClassRelease, LastError: hookErr}
	o := failing("new", 4, hookErr)
	o.Paused, o.PausedByUs, o.BackoffUntil = true, true, t0.Add(time.Hour)

	d := helmrel.Step(st, o, policy, t0, 0)

	if d.Action != helmrel.Unpause {
		t.Errorf("a new spec starts again at once: %v", d.Action)
	}
	if d.State.Attempts != 0 || d.State.Counted != 0 || d.State.GaveUp || !d.State.NextRetry.IsZero() || d.State.Digest != "new" {
		t.Errorf("state %+v", d.State)
	}
}

func TestNewDigestEndsAGiveUp(t *testing.T) {
	st := helmrel.State{Digest: "old", Attempts: 9, Counted: 5, GaveUp: true, NextRetry: t0.Add(time.Hour)}

	d := helmrel.Step(st, helmrel.Observation{Digest: "new", Timeout: 10 * time.Minute}, policy, t0, 0)

	if d.State.GaveUp || d.State.Counted != 0 {
		t.Errorf("state %+v", d.State)
	}
}

func TestReadyResets(t *testing.T) {
	st := helmrel.State{Digest: "d1", Attempts: 3, Counted: 3, Failures: 3, NextRetry: t0, LastError: hookErr, Class: helmrel.ClassRelease}

	d := helmrel.Step(st, helmrel.Observation{Digest: "d1", Ready: true}, policy, t0.Add(time.Hour), 0)

	if d.Action != helmrel.None || d.State.Attempts != 0 || d.State.Counted != 0 || d.State.LastError != "" ||
		d.State.Class != helmrel.ClassNone || !d.State.NextRetry.IsZero() || d.State.Digest != "d1" {
		t.Errorf("%v %+v", d.Action, d.State)
	}
}

func TestAnOperatorsPauseIsNeverTouched(t *testing.T) {
	o := failing("d1", 3, hookErr)
	o.Paused = true // crossplane.io/paused without our marker: an operator's or a freeze's

	d := helmrel.Step(helmrel.State{Digest: "d1"}, o, policy, t0, 0)

	if d.Action != helmrel.None || d.State.Attempts != 0 || d.State.Counted != 0 {
		t.Errorf("%v %+v", d.Action, d.State)
	}
}

func TestNoErrorMeansNothingToDo(t *testing.T) {
	d := helmrel.Step(helmrel.State{Digest: "d1"}, helmrel.Observation{Digest: "d1", HelmState: "pending-install"}, policy, t0, 0)

	if d.Action != helmrel.None || d.State.Attempts != 0 {
		t.Errorf("%v %+v", d.Action, d.State)
	}
}

func TestBackoffMaxBelowAMinuteIsRespected(t *testing.T) {
	small := helmrel.Policy{BackoffMax: 20 * time.Second, MaxFailures: 5}

	d := helmrel.Step(helmrel.State{}, failing("d1", 1, hookErr), small, t0, 0)

	if d.Until.Sub(t0) != 20*time.Second {
		t.Errorf("%s", d.Until.Sub(t0))
	}
}

func TestZeroPolicyUsesDefaults(t *testing.T) {
	d := helmrel.Step(helmrel.State{Attempts: 40, Counted: 3}, failing("d1", 41, hookErr), helmrel.Policy{}, t0, 0)

	if d.Until.Sub(t0) != helmrel.DefaultBackoffMax {
		t.Errorf("%s", d.Until.Sub(t0))
	}
	if helmrel.DefaultMaxFailures != 5 || helmrel.DefaultBackoffMax != 30*time.Minute {
		t.Error("defaults of the design")
	}
}
