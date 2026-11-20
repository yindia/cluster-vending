package helmrel

import "time"

// Policy is what the controller is told: --helm-backoff-max and
// --helm-max-failures. Zero values mean the defaults.
type Policy struct {
	BackoffMax  time.Duration
	MaxFailures int
}

// Cap is the longest delay between attempts: BackoffMax, or the default.
func (p Policy) Cap() time.Duration {
	if p.BackoffMax <= 0 {
		return DefaultBackoffMax
	}
	return p.BackoffMax
}

// Limit is how many counted failures in a row make a component give up:
// MaxFailures, or the default.
func (p Policy) Limit() int32 {
	if p.MaxFailures <= 0 {
		return DefaultMaxFailures
	}
	return int32(min(p.MaxFailures, 1<<30))
}

// State is what is remembered of a Release between passes (on the
// component's status).
type State struct {
	// Digest of the Release spec the state is about; a different one starts
	// over.
	Digest string
	// Attempts are the failed attempts backed off from, of any class; it
	// sets the delay.
	Attempts int32
	// Counted are the failures in a row that count toward giving up
	// (ClassRelease only).
	Counted int32
	// Failures is the Release's status.failed as last seen: a higher one is a
	// new failed attempt.
	Failures int32
	// NextRetry is when the pause ends; zero when not backing off.
	NextRetry time.Time
	// Class and LastError are the last failure.
	Class     ErrorClass
	LastError string
	// GaveUp: Counted reached the limit; the backoff stays at its cap.
	GaveUp bool
}

// Observation is what a pass reads of a Release.
type Observation struct {
	// Digest is the Release's spec digest (its annotation).
	Digest string
	// Ready: provider-helm reports it Ready and Helm says deployed.
	Ready bool
	// HelmState is status.atProvider.state.
	HelmState string
	// Failures is status.failed; Error the message of the Synced condition
	// while it is False.
	Failures int32
	Error    string
	// Paused is crossplane.io/paused; PausedByUs says the pause is the
	// platform's backoff (its marker), BackoffUntil what the marker says.
	Paused, PausedByUs bool
	BackoffUntil       time.Time
	// Timeout is the Release's wait timeout: how long a retry may run
	// without reporting.
	Timeout time.Duration
}

// Action is what the pass does to the Release.
type Action int

// The actions.
const (
	// None leaves the Release as it is.
	None Action = iota
	// Pause sets the platform's pause on the Release until Decision.Until.
	Pause
	// Unpause removes the platform's pause (never anyone else's).
	Unpause
)

// Decision is Step's answer.
type Decision struct {
	State  State
	Action Action
	// Until is when a Pause ends.
	Until time.Time
	// GaveUp is true once, in the decision of the failure that reached the
	// limit.
	GaveUp bool
}

const defaultTimeout = 10 * time.Minute

// Step is the whole policy for one Release: from what was remembered
// (prev), what the Release shows now (o) and the time, what to remember and
// what to do. jitter in [0, 1] is the random share of the delay.
//
//   - Ready, or a new digest, starts over; a new digest also lifts our pause.
//   - Someone else's pause (an operator's, a freeze) is never touched or
//     counted: it wins.
//   - Our pause is lifted when its time has come.
//   - A failure is new when the Release's failed count rose, its error
//     changed, or a retry that was due did not report within its timeout;
//     the same failure seen again (the pause is not in the cache yet) is
//     not counted, the pause is kept in place.
//   - A new failure backs off 1m, 2m, 4m, ... (capped), is counted when its
//     class counts, and after MaxFailures counted ones in a row the
//     component gives up: the backoff stays at the cap.
func Step(prev State, o Observation, p Policy, now time.Time, jitter float64) Decision {
	s := prev
	if o.Ready {
		return Decision{State: State{Digest: o.Digest}, Action: ifPausedByUs(o, Unpause)}
	}
	if prev.Digest != "" && o.Digest != prev.Digest {
		return Decision{State: State{Digest: o.Digest}, Action: ifPausedByUs(o, Unpause)}
	}
	s.Digest = o.Digest // nothing was remembered yet: adopt it
	if o.Paused && !o.PausedByUs {
		return Decision{State: s}
	}
	if o.PausedByUs {
		if !o.BackoffUntil.IsZero() {
			s.NextRetry = o.BackoffUntil
		}
		if !now.Before(s.NextRetry) {
			return Decision{State: s, Action: Unpause}
		}
		return Decision{State: s}
	}
	if o.Error == "" {
		return Decision{State: s}
	}
	if !isNewFailure(prev, o, now) {
		if now.Before(prev.NextRetry) {
			return Decision{State: s, Action: Pause, Until: prev.NextRetry}
		}
		return Decision{State: s}
	}
	return newFailure(s, o, p, now, jitter)
}

func ifPausedByUs(o Observation, a Action) Action {
	if o.PausedByUs {
		return a
	}
	return None
}

func isNewFailure(prev State, o Observation, now time.Time) bool {
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return prev.Attempts == 0 || o.Failures > prev.Failures || o.Error != prev.LastError ||
		(!prev.NextRetry.IsZero() && now.After(prev.NextRetry.Add(timeout+2*time.Minute)))
}

func newFailure(s State, o Observation, p Policy, now time.Time, jitter float64) Decision {
	s.Attempts++
	s.Failures, s.LastError, s.Class = o.Failures, o.Error, Classify(o.Error)
	wasGaveUp := s.GaveUp
	if s.Class.Counts() {
		s.Counted++
	}
	delay := Delay(int(s.Attempts), p.Cap(), jitter)
	if s.Counted >= p.Limit() {
		s.GaveUp = true
		delay = p.Cap()
	}
	s.NextRetry = now.Add(delay)
	return Decision{State: s, Action: Pause, Until: s.NextRetry, GaveUp: s.GaveUp && !wasGaveUp}
}
