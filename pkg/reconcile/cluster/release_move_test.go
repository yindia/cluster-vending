package cluster

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"

	"github.com/yindia/cluster-vending/pkg/component"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

var moveNow = time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

// A cluster on release A whose channel offers B: istio-base and karpenter
// change, external-secrets does not.
func moveBase() moveFacts {
	return moveFacts{Current: "rel-a", Target: "rel-b", Automatic: true, Running: true, Admitted: true,
		Order: []component.MoveGroup{{Name: "istio-base"}, {Name: "external-secrets"}, {Name: "karpenter"}},
		From:  map[string]component.Pin{"istio-base": {Version: "1.32.0"}, "external-secrets": {Version: "2.15.0"}, "karpenter": {Version: "1.15.1"}},
		To:    map[string]component.Pin{"istio-base": {Version: "1.33.0"}, "external-secrets": {Version: "2.15.0"}, "karpenter": {Version: "1.16.0"}},
		Components: map[string]*cloudv1.PlatformComponent{
			"istio-base": {Name: "istio-base", Version: "1.32.0", Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_READY},
			"karpenter":  {Name: "karpenter", Version: "1.15.1", Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_READY},
		},
		Now: moveNow, ClusterID: "c1"}
}

func moveAt(phase cloudv1.ClusterReleaseMovePhase, comp string, step int32, since time.Time) *cloudv1.ClusterReleaseMove {
	return &cloudv1.ClusterReleaseMove{Current: "rel-a", Target: "rel-b", Phase: phase, Component: comp, Step: step, Steps: 2,
		StartTime: timestamppb.New(moveNow.Add(-time.Hour)), ComponentStartTime: timestamppb.New(since), ComponentApplied: true}
}

func TestDecideMove(t *testing.T) {
	tests := []struct {
		name        string
		change      func(*moveFacts)
		wantPhase   cloudv1.ClusterReleaseMovePhase
		wantComp    string
		wantRelease string
		wantMessage string
		wantPins    map[string]string // nil: Current's pins (no change)
		// wantFreeze: nothing is installed this pass (a moved component is
		// never taken back to Current's pin).
		wantFreeze bool
		// wantRecover: the component this pass tries again.
		wantRecover string
	}{
		{name: "nothing to move", change: func(f *moveFacts) { f.Target = "rel-a" }, wantPhase: moveIdle, wantRelease: "rel-a"},
		{name: "the channel offers an older release: no move",
			change:    func(f *moveFacts) { f.OlderChannel = "stable" },
			wantPhase: movePending, wantRelease: "rel-a", wantMessage: "waiting: channel stable offers an older release than the cluster's"},
		{name: "manual: available, not started", change: func(f *moveFacts) { f.Automatic = false },
			wantPhase: movePending, wantRelease: "rel-a", wantMessage: "set upgradeMode Automatic"},
		{name: "a Kubernetes upgrade goes first", change: func(f *moveFacts) { f.KubernetesUpgrading = true },
			wantPhase: movePending, wantRelease: "rel-a", wantMessage: "Kubernetes upgrade"},
		{name: "not admitted by the channel's rollout",
			change:    func(f *moveFacts) { f.Admitted, f.NotAdmitted = false, "waiting for tier 2 to finish" },
			wantPhase: movePending, wantRelease: "rel-a", wantMessage: "tier 2"},
		{name: "starts: the first changed component, the others at A",
			wantPhase: moveComponents, wantComp: "istio-base", wantRelease: "rel-a",
			wantPins: map[string]string{"istio-base": "1.33.0", "karpenter": "1.15.1", "external-secrets": "2.15.0"}},
		{name: "waits until the component runs the new version and is Ready",
			change: func(f *moveFacts) {
				f.Prev = moveAt(moveComponents, "istio-base", 1, moveNow.Add(-5*time.Minute))
				f.Components["istio-base"] = &cloudv1.PlatformComponent{Version: "1.33.0", Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_INSTALLING}
			},
			wantPhase: moveComponents, wantComp: "istio-base", wantRelease: "rel-a",
			wantPins: map[string]string{"istio-base": "1.33.0", "karpenter": "1.15.1"}},
		{name: "settled: the next one starts",
			change: func(f *moveFacts) {
				f.Prev = moveAt(moveComponents, "istio-base", 1, moveNow.Add(-5*time.Minute))
				f.Components["istio-base"] = &cloudv1.PlatformComponent{Version: "1.33.0", Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_READY}
			},
			wantPhase: moveComponents, wantComp: "karpenter", wantRelease: "rel-a",
			wantPins: map[string]string{"istio-base": "1.33.0", "karpenter": "1.16.0"}},
		{name: "the component failed: recovered",
			change: func(f *moveFacts) {
				f.Prev = moveAt(moveComponents, "karpenter", 2, moveNow.Add(-5*time.Minute))
				f.Components["karpenter"] = &cloudv1.PlatformComponent{Version: "1.16.0", Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED, Message: "chart karpenter: boom"}
			},
			wantPhase: moveComponents, wantComp: "karpenter", wantRelease: "rel-a", wantMessage: "recovering karpenter (attempt 1 of 3): it 1.16.0 failed: chart karpenter: boom",
			wantPins: map[string]string{"karpenter": "1.16.0"}, wantRecover: "karpenter"},
		{name: "a recovered component's failure waits for the grace period",
			change: func(f *moveFacts) {
				f.Prev = moveAt(moveComponents, "karpenter", 2, moveNow.Add(-time.Minute))
				f.Prev.Recoveries = 1
				f.Components["karpenter"] = &cloudv1.PlatformComponent{Version: "1.16.0", Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED, Message: "boom"}
			},
			wantPhase: moveComponents, wantComp: "karpenter", wantRelease: "rel-a", wantMessage: "upgrading karpenter",
			wantPins: map[string]string{"karpenter": "1.16.0"}},
		{name: "still failed after the last recovery: halted, with the retry",
			change: func(f *moveFacts) {
				f.Prev = moveAt(moveComponents, "karpenter", 2, moveNow.Add(-10*time.Minute))
				f.Prev.Recoveries = maxMoveRecoveries
				f.Components["karpenter"] = &cloudv1.PlatformComponent{Version: "1.16.0", Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED, Message: "chart karpenter: boom"}
			},
			wantPhase: moveHalted, wantComp: "karpenter", wantRelease: "rel-a", wantMessage: "boom after 3 recoveries; cluster retry --id c1",
			wantPins: map[string]string{"karpenter": "1.16.0"}},
		{name: "not settled within the watch: recovered",
			change: func(f *moveFacts) {
				f.Prev = moveAt(moveComponents, "karpenter", 2, moveNow.Add(-31*time.Minute))
				f.Components["karpenter"] = &cloudv1.PlatformComponent{Version: "1.16.0", Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_INSTALLING}
			},
			wantPhase: moveComponents, wantComp: "karpenter", wantRelease: "rel-a", wantMessage: "within 30m",
			wantPins: map[string]string{"istio-base": "1.33.0", "karpenter": "1.16.0"}, wantRecover: "karpenter"},
		{name: "not settled after the last recovery: halted",
			change: func(f *moveFacts) {
				f.Prev = moveAt(moveComponents, "karpenter", 2, moveNow.Add(-31*time.Minute))
				f.Prev.Recoveries = maxMoveRecoveries
				f.Components["karpenter"] = &cloudv1.PlatformComponent{Version: "1.16.0", Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_INSTALLING}
			},
			wantPhase: moveHalted, wantComp: "karpenter", wantRelease: "rel-a", wantMessage: "within 30m0s after 3 recoveries",
			wantPins: map[string]string{"istio-base": "1.33.0", "karpenter": "1.16.0"}},
		{name: "halted stays halted, even once the component is Ready",
			change: func(f *moveFacts) {
				f.Prev = moveAt(moveHalted, "karpenter", 2, moveNow.Add(-40*time.Minute))
				f.Prev.Message = "karpenter 1.16.0 failed"
				f.Components["karpenter"] = &cloudv1.PlatformComponent{Version: "1.16.0", Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_READY}
			},
			wantPhase: moveHalted, wantComp: "karpenter", wantRelease: "rel-a", wantMessage: "failed",
			wantPins: map[string]string{"istio-base": "1.33.0", "karpenter": "1.16.0"}},
		{name: "a retry resumes a halted move, its clock restarted",
			change: func(f *moveFacts) {
				f.Prev, f.Retry = moveAt(moveHalted, "karpenter", 2, moveNow.Add(-40*time.Minute)), true
				f.Components["karpenter"] = &cloudv1.PlatformComponent{Version: "1.16.0", Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_INSTALLING}
			},
			wantPhase: moveComponents, wantComp: "karpenter", wantRelease: "rel-a",
			wantPins: map[string]string{"istio-base": "1.33.0", "karpenter": "1.16.0"}},
		{name: "done: B's pins and B recorded",
			change: func(f *moveFacts) {
				f.Prev = moveAt(moveComponents, "karpenter", 2, moveNow.Add(-5*time.Minute))
				f.Components["istio-base"] = &cloudv1.PlatformComponent{Version: "1.33.0", Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_READY}
				f.Components["karpenter"] = &cloudv1.PlatformComponent{Version: "1.16.0", Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_READY}
			},
			wantPhase: moveIdle, wantRelease: "rel-b",
			wantPins: map[string]string{"istio-base": "1.33.0", "karpenter": "1.16.0"}},
		{name: "the target cannot be used: halted part way",
			change: func(f *moveFacts) {
				f.Prev, f.To, f.ToProblem = moveAt(moveComponents, "karpenter", 2, moveNow), nil, "release rel-b is gone"
			},
			wantPhase: moveHalted, wantComp: "karpenter", wantRelease: "rel-a", wantMessage: "gone", wantFreeze: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := moveBase()
			if tt.change != nil {
				tt.change(&f)
			}

			d := decideMove(f)

			st := d.Status
			if st.GetPhase() != tt.wantPhase || st.GetComponent() != tt.wantComp || d.Release != tt.wantRelease {
				t.Fatalf("move = %s %q, release %s; want %s %q, release %s (%s)", st.GetPhase(), st.GetComponent(), d.Release,
					tt.wantPhase, tt.wantComp, tt.wantRelease, st.GetMessage())
			}
			if !strings.Contains(st.GetMessage(), tt.wantMessage) {
				t.Errorf("message = %q, want it to say %q", st.GetMessage(), tt.wantMessage)
			}
			if d.Freeze != tt.wantFreeze {
				t.Errorf("freeze = %v, want %v", d.Freeze, tt.wantFreeze)
			}
			if d.Recover != tt.wantRecover {
				t.Errorf("recover = %q, want %q", d.Recover, tt.wantRecover)
			}
			if tt.wantRecover != "" && !st.GetComponentStartTime().AsTime().Equal(moveNow) {
				t.Errorf("a recovery's watch starts at %v, want now", st.GetComponentStartTime().AsTime())
			}
			if tt.wantPins == nil && d.Pins != nil {
				t.Errorf("pins changed: %v", d.Pins)
			}
			for name, v := range tt.wantPins {
				if got := d.Pins[name].Version; got != v {
					t.Errorf("%s pinned at %q, want %s", name, got, v)
				}
			}
		})
	}
}

// A recovery is the pass's retry of the component, on top of what a person
// asked to retry.
func TestRetryWith(t *testing.T) {
	at := moveNow
	if r := retryWith(nil, "karpenter", at); !r.Has("karpenter") || r.Has("base") || !r.At.Equal(at) {
		t.Errorf("alone: %+v", r)
	}
	asked := &model.Retry{At: at.Add(-time.Minute), By: "alice", Components: []string{"base"}, Upgrade: true}
	r := retryWith(asked, "karpenter", at)
	if !r.Has("karpenter") || !r.Has("base") || !r.Upgrade || r.By != "alice" || len(asked.Components) != 1 {
		t.Errorf("with a person's retry: %+v (asked %+v)", r, asked)
	}
}

// The clock of the component being moved is kept across passes, and
// restarts for the next one.
func TestDecideMoveComponentClock(t *testing.T) {
	f := moveBase()
	f.Prev = moveAt(moveComponents, "istio-base", 1, moveNow.Add(-5*time.Minute))

	if got := decideMove(f).Status.GetComponentStartTime().AsTime(); !got.Equal(moveNow.Add(-5 * time.Minute)) {
		t.Errorf("same component: since %v, want it kept", got)
	}
	f.Components["istio-base"] = &cloudv1.PlatformComponent{Version: "1.33.0", Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_READY}
	if got := decideMove(f).Status.GetComponentStartTime().AsTime(); !got.Equal(moveNow) {
		t.Errorf("next component: since %v, want now", got)
	}
}

// The pass that applied a component's new pin may still have seen the old
// version Ready: that status does not settle it.
func TestDecideMoveIgnoresTheApplyingPass(t *testing.T) {
	f := moveBase()
	f.Prev = moveAt(moveComponents, "istio-base", 1, moveNow)
	f.Prev.ComponentApplied = false
	f.Components["istio-base"] = &cloudv1.PlatformComponent{Version: "1.33.0", Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_READY}

	d := decideMove(f)

	if d.Status.GetComponent() != "istio-base" || !d.Status.GetComponentApplied() {
		t.Fatalf("move = %v: want istio-base still moving, applied from now", d.Status)
	}
	f.Prev = d.Status
	if d := decideMove(f); d.Status.GetComponent() != "karpenter" {
		t.Fatalf("next pass: %v, want karpenter", d.Status)
	}
}

// Releases are ordered by when they were published: a move goes only to
// a newer one, and what cannot be ordered does not hold a move back.
func TestNotNewer(t *testing.T) {
	at := func(h int) *cloudv1alpha1.Release {
		return &cloudv1alpha1.Release{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(moveNow.Add(time.Duration(h) * time.Hour))}}
	}
	tests := []struct {
		name            string
		current, target *cloudv1alpha1.Release
		want            bool
	}{
		{"older target", at(5), at(1), true},
		{"same time", at(5), at(5), true},
		{"newer target", at(1), at(5), false},
		{"current without a time", &cloudv1alpha1.Release{}, at(1), false},
		{"target without a time", at(1), &cloudv1alpha1.Release{}, false},
	}
	for _, tt := range tests {
		if got := notNewer(tt.current, tt.target); got != tt.want {
			t.Errorf("%s: notNewer = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// releasesByName is a platform source that knows only releases.
type releasesByName struct {
	reconcile.PlatformSource
	byName map[string]*cloudv1alpha1.Release
}

func (r releasesByName) Release(_ context.Context, name string) (*cloudv1alpha1.Release, error) {
	return r.byName[name], nil
}

// A channel switch (or a repin) to an older release does not move the
// cluster's components back; to a newer one it does.
func TestReleaseNotNewerReadsTheReleases(t *testing.T) {
	at := func(h int) *cloudv1alpha1.Release {
		return &cloudv1alpha1.Release{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(moveNow.Add(time.Duration(h) * time.Hour))}}
	}
	s := &Reconciler{deps: reconcile.Deps{Platform: releasesByName{byName: map[string]*cloudv1alpha1.Release{"old": at(1), "new": at(5)}}}}
	ctx := context.Background()
	if !s.releaseNotNewer(ctx, "new", "old") {
		t.Error("a move from new to old must wait")
	}
	if s.releaseNotNewer(ctx, "old", "new") {
		t.Error("a move from old to new must go on")
	}
	if s.releaseNotNewer(ctx, "gone", "new") || s.releaseNotNewer(ctx, "old", "gone") || s.releaseNotNewer(ctx, "", "new") {
		t.Error("an unknown release does not hold a move back")
	}
}

// The order is the serial: a release re-created later (a newer creation
// time, the same serial) neither moves clusters back nor forward.
func TestNotNewerUsesTheSerial(t *testing.T) {
	rel := func(serial int64, h int) *cloudv1alpha1.Release {
		r := &cloudv1alpha1.Release{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(moveNow.Add(time.Duration(h) * time.Hour))}}
		if serial > 0 {
			r.Spec.Serial = &serial
		}
		return r
	}
	tests := []struct {
		name            string
		current, target *cloudv1alpha1.Release
		want            bool
	}{
		{"higher serial, older creation time (re-created)", rel(1, 9), rel(2, 1), false},
		{"lower serial, newer creation time (re-created)", rel(2, 1), rel(1, 9), true},
		{"same serial", rel(3, 1), rel(3, 9), true},
		{"serial beats a legacy release", rel(0, 9), rel(1, 1), false},
		{"legacy target of a serialed current", rel(1, 1), rel(0, 9), true},
	}
	for _, tt := range tests {
		if got := notNewer(tt.current, tt.target); got != tt.want {
			t.Errorf("%s: notNewer = %v, want %v", tt.name, got, tt.want)
		}
	}
}
