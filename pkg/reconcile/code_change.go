package reconcile

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/yindia/cluster-vending/pkg/crossplane/sequence"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// CodeRecoveries is how many failed applies of a code change are tried
// again; the next failure halts it.
const CodeRecoveries = 3

// CodeEpoch identifies this controller session. Networks must report the render
// they observed in this session before any new network code rollout can start.
// A restart deliberately requires observation again; this is not an admission
// carried over from the previous binary/process.
var codeEpoch = uuid.NewString()

func CodeEpoch() string { return codeEpoch }

// CodeFacts are what deciding a resource's code change reads: what a new
// controller build renders for it against what was applied.
type CodeFacts struct {
	// Epoch optionally binds a code target to the controller that observed it.
	Epoch string
	// WaitForConvergence keeps a network's apply in flight until current
	// provider observations confirm its newly rendered objects.
	WaitForConvergence bool
	// Applied are the fingerprints recorded when it was last applied (nil:
	// by a build that did not record them); Now those of this build.
	Applied, Now map[string]string
	// Behind: its renderer version is below the target (Deps.RenderBehind).
	Behind bool
	// Prev is the code change its status recorded.
	Prev *cloudv1.CodeChange
	// Frozen: a freeze holds it; FrozenWhy says which and why.
	Frozen    bool
	FrozenWhy string
	// Admitted: its rollout lets it apply now; NotAdmitted says why not.
	Admitted    bool
	NotAdmitted string
	// Retry: a person asked to try again (cluster retry).
	Retry bool
	At    time.Time
}

// CodeDecision is what a pass does about a code change.
type CodeDecision struct {
	// Status is what the resource's status says; nil: nothing.
	Status *cloudv1.CodeChange
	// Adopt: record Now as applied without applying (the first build that
	// fingerprints a resource takes what it renders as what was applied).
	Adopt bool
	// Apply: apply the resource now; Started: its turn came this pass.
	Apply, Started bool
}

// Pending reports whether the render differs from what was applied.
func (f CodeFacts) Pending() bool {
	return f.Behind || f.WaitForConvergence || (f.Applied != nil && sequence.Diff(f.Applied, f.Now).Total() > 0)
}

func (f CodeFacts) Target() string {
	target := sequence.Digest(f.Now)
	if f.Epoch != "" {
		return f.Epoch + "/" + target
	}
	return target
}

// summary says what changes, for people.
func (f CodeFacts) summary() (string, int32) {
	if f.Applied == nil {
		return "the platform's renderer changed", 0
	}
	c := sequence.Diff(f.Applied, f.Now)
	if c.Total() == 0 {
		return "the platform's renderer changed", 0
	}
	return c.String(), int32(min(c.Total(), math.MaxInt32)) //nolint:gosec // capped above
}

// DecideCode decides a pass's code change: none pending keeps what the
// status said; a halted one waits for a retry or a newer build; one being applied goes on;
// a frozen or not yet admitted one waits (Pending, with why); an admitted
// one starts (Applying).
func DecideCode(f CodeFacts) CodeDecision {
	if f.Applied == nil && !f.Behind {
		return CodeDecision{Status: f.observedEpoch(), Adopt: true}
	}
	if !f.Pending() {
		if f.Epoch != "" {
			return CodeDecision{Status: f.observedEpoch()}
		}
		// Applied meanwhile (by an update's apply, say): done.
		if p := f.Prev.GetPhase(); p == cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_PENDING || p == cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_APPLYING {
			st := proto.CloneOf(f.Prev)
			st.Phase, st.FinishTime, st.Message, st.Failures = cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_DONE, timestamppb.New(f.At), "", 0
			return CodeDecision{Status: st}
		}
		return CodeDecision{Status: f.Prev}
	}
	summary, objects := f.summary()
	target := f.Target()
	st := &cloudv1.CodeChange{Phase: cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_PENDING, Summary: summary, Objects: objects, Target: target}
	prev := f.Prev.GetPhase()
	switch {
	case prev == cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_HALTED && !f.Retry && f.Prev.GetTarget() == target:
		st = proto.CloneOf(f.Prev)
		st.Summary, st.Objects = summary, objects
		return CodeDecision{Status: st}
	case prev == cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_APPLYING:
		st = proto.CloneOf(f.Prev)
		st.Summary, st.Objects, st.Target = summary, objects, target
		return CodeDecision{Status: st, Apply: true}
	case f.Frozen:
		st.Message = f.FrozenWhy
		return CodeDecision{Status: st}
	case !f.Admitted:
		st.Message = f.NotAdmitted
		return CodeDecision{Status: st}
	}
	st.Phase, st.StartTime = cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_APPLYING, timestamppb.New(f.At)
	return CodeDecision{Status: st, Apply: true, Started: true}
}

// Observation of unchanged code is not a new apply or soak. Preserve the last
// real finish time and emit no Started/Finished transition for a process restart.
func (f CodeFacts) observedEpoch() *cloudv1.CodeChange {
	if f.Epoch == "" {
		return f.Prev
	}
	st := proto.CloneOf(f.Prev)
	if st == nil {
		st = &cloudv1.CodeChange{}
	}
	st.Phase, st.Target = cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_DONE, f.Target()
	return st
}

// CodeApplied is the code change once its apply ended: done when what is
// applied now is what the build renders (complete), else still applying;
// a failed apply is tried again CodeRecoveries times, then it halts. The
// note to report is set for a change that finished or halted.
func CodeApplied(st *cloudv1.CodeChange, complete bool, failure error, at time.Time) (*cloudv1.CodeChange, *Note) {
	out := proto.CloneOf(st)
	switch {
	case failure != nil:
		out.Failures++
		if out.GetFailures() > CodeRecoveries {
			out.Phase = cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_HALTED
			out.Message = fmt.Sprintf("applying it failed %d times: %v", out.GetFailures(), failure)
			n := Warning(ReasonCodeChangeHalted, "the code change ("+out.GetSummary()+") halted: "+out.GetMessage())
			return out, &n
		}
		out.Message = fmt.Sprintf("applying it failed (%d of %d recoveries): %v; tried again", out.GetFailures(), CodeRecoveries, failure)
		return out, nil
	case complete:
		out.Phase, out.FinishTime, out.Message, out.Failures = cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_DONE, timestamppb.New(at), "", 0
		n := Normal(ReasonCodeChangeFinished, "the code change ("+out.GetSummary()+") is applied")
		return out, &n
	}
	out.Message = "applying; waiting for the objects it holds back"
	return out, nil
}

// CodeStarted is the note of a code change whose turn came.
func CodeStarted(st *cloudv1.CodeChange) Note {
	return Normal(ReasonCodeChangeStarted, "applying the code change: "+st.GetSummary())
}

// codeKey holds the code change one pass decided, for the status the pass
// records.
type codeKey struct{}

type codeHolder struct{ c *cloudv1.CodeChange }

// WithCodeHolder returns a context a pass's code change is left in.
func WithCodeHolder(ctx context.Context) context.Context {
	return context.WithValue(ctx, codeKey{}, &codeHolder{})
}

// SetCode leaves the pass's code change in ctx (WithCodeHolder).
func SetCode(ctx context.Context, c *cloudv1.CodeChange) {
	if h, ok := ctx.Value(codeKey{}).(*codeHolder); ok {
		h.c = c
	}
}

// CodeOf is the code change this pass decided, else the stored one.
func CodeOf(ctx context.Context, stored *cloudv1.CodeChange) *cloudv1.CodeChange {
	if h, ok := ctx.Value(codeKey{}).(*codeHolder); ok && h.c != nil {
		return h.c
	}
	return stored
}
