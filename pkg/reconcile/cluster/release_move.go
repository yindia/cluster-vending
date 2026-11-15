package cluster

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/apimachinery/pkg/api/meta"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/component"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// How a release move watches the component it moves: it has
// componentWatch to run its new version and be Ready; one that fails or
// does not settle in that time is recovered (its chart tried again, the
// watch starting over) up to maxMoveRecoveries times, then the move halts.
// A recovered component's failure counts again only after recoveryGrace:
// the provider needs a moment to act on the new try.
const (
	componentWatch    = 30 * time.Minute
	maxMoveRecoveries = 3
	recoveryGrace     = 5 * time.Minute
)

// Release move phases.
const (
	moveIdle       = cloudv1.ClusterReleaseMovePhase_CLUSTER_RELEASE_MOVE_PHASE_IDLE
	movePending    = cloudv1.ClusterReleaseMovePhase_CLUSTER_RELEASE_MOVE_PHASE_PENDING
	moveComponents = cloudv1.ClusterReleaseMovePhase_CLUSTER_RELEASE_MOVE_PHASE_COMPONENTS
	moveHalted     = cloudv1.ClusterReleaseMovePhase_CLUSTER_RELEASE_MOVE_PHASE_HALTED
)

// moving reports whether a move is under way (moving components, or
// halted part way).
func moving(m *cloudv1.ClusterReleaseMove) bool {
	return m.GetPhase() == moveComponents || m.GetPhase() == moveHalted
}

// ReleaseMoving reports whether the status recorded with the row has a
// release move under way (moving components, or halted part way).
func ReleaseMoving(row *model.Cluster) bool { return moving(storedStatus(row).GetReleaseMove()) }

// moveFacts are what deciding one pass of a release move reads.
type moveFacts struct {
	// Current is the release the components run (the kept release);
	// Target the one to move to ("" for none).
	Current, Target string
	// Prev is the move as the last pass recorded it.
	Prev *cloudv1.ClusterReleaseMove
	// Automatic: the cluster's upgrade mode is Automatic.
	Automatic bool
	// Running: the cluster is RUNNING; KubernetesUpgrading: a Kubernetes
	// upgrade is in progress (status.upgrade).
	Running, KubernetesUpgrading bool
	// Admitted: the channel's rollout lets the cluster start now;
	// NotAdmitted says why not.
	Admitted    bool
	NotAdmitted string
	// Order are the components in install order; From and To the pins of
	// Current and Target on the cluster's minor (To nil: ToProblem says
	// why Target cannot be moved to).
	Order     []component.MoveGroup
	From, To  map[string]component.Pin
	ToProblem string
	// Components are the platform components as the last pass recorded
	// them, by name.
	Components map[string]*cloudv1.PlatformComponent
	// Retry: a person asked to try the cluster's failed work again.
	Retry bool
	Now   time.Time
	// ClusterID, for the retry hint.
	ClusterID string
}

// moveDecision is one pass of a release move: the move's status, the pins
// to install (nil: Current's) and the release to record (the target once
// the move is done).
type moveDecision struct {
	Status  *cloudv1.ClusterReleaseMove
	Pins    map[string]component.Pin
	Release string
	// Freeze: install nothing this pass. A move under way whose target
	// cannot be read has no pins for what it moved already; Current's would
	// take those components back.
	Freeze bool
	// Recover names the component this pass tries again (its chart
	// installed again now): it failed or did not settle in time.
	Recover string
}

// decideMove is one pass of moving a cluster's components from Current to
// Target, one component at a time (component.PlanMove): a move starts
// when the cluster is Automatic, Running, not upgrading Kubernetes and
// admitted by its channel's rollout. The component being moved is watched
// for componentWatch: one that fails or does not settle is recovered up to
// maxMoveRecoveries times, then the move halts, and stays halted until a
// retry. A started move goes on although the channel moved on: its target
// is kept until it is done.
func decideMove(f moveFacts) moveDecision {
	d := moveDecision{Release: f.Current}
	st := &cloudv1.ClusterReleaseMove{Current: f.Current, Target: f.Target, Phase: moveIdle}
	d.Status = st
	if f.Target == "" || f.Target == f.Current {
		st.Target = ""
		return d
	}
	if !moving(f.Prev) {
		st.Phase = movePending
		switch {
		case !f.Automatic:
			st.Message = fmt.Sprintf("release %s is available; the cluster is manual: set upgradeMode Automatic to move to it", f.Target)
			return d
		case f.KubernetesUpgrading:
			st.Message = "waiting for the Kubernetes upgrade in progress"
			return d
		case !f.Running:
			st.Message = "waiting for the cluster to be Running"
			return d
		case !f.Admitted:
			st.Message = orDefault(f.NotAdmitted, "waiting its turn in the channel's rollout")
			return d
		case f.To == nil:
			st.Message = f.ToProblem
			return d
		}
		st.StartTime = timestamppb.New(f.Now)
	} else {
		st.StartTime = f.Prev.GetStartTime()
	}
	if f.To == nil {
		st.Phase, st.Message, d.Freeze = moveHalted, f.ToProblem, true
		st.Component, st.Step, st.Steps = f.Prev.GetComponent(), f.Prev.GetStep(), f.Prev.GetSteps()
		return d
	}
	halted := f.Prev.GetPhase() == moveHalted && !f.Retry
	prevStep := int(f.Prev.GetStep())
	i := 0
	m := component.PlanMove(f.Order, f.From, f.To, func(name string, pin component.Pin) bool {
		i++
		switch {
		case i < prevStep:
			return true // moved in an earlier pass
		case i > prevStep || halted:
			return false
		}
		// Its state is read only from a pass after the one that applied its
		// new pin: that pass may still have seen the old version Ready.
		c := f.Components[name]
		return f.Prev.GetComponentApplied() && f.Prev.GetComponent() == name &&
			c.GetPhase() == cloudv1.ComponentPhase_COMPONENT_PHASE_READY && c.GetVersion() == pin.Version
	})
	st.Steps, st.Step, st.Component = count32(m.Steps), count32(m.Step), m.Component
	if m.Done {
		st.Current, st.Target, st.Phase, st.Component = f.Target, "", moveIdle, ""
		d.Pins, d.Release = m.Pins, f.Target
		return d
	}
	d.Pins, st.Phase = m.Pins, moveComponents
	st.ComponentStartTime = timestamppb.New(f.Now)
	if f.Prev.GetComponent() == m.Component && f.Prev.GetComponentStartTime() != nil && !f.Retry {
		st.ComponentStartTime, st.ComponentApplied = f.Prev.GetComponentStartTime(), true
		st.Recoveries = f.Prev.GetRecoveries()
	}
	watchComponent(f, m.Component, halted, st, &d)
	return d
}

// watchComponent is how the component being moved is doing: upgrading,
// recovered (d.Recover) when it failed or did not settle within
// componentWatch, or halted after the last recovery.
func watchComponent(f moveFacts, name string, halted bool, st *cloudv1.ClusterReleaseMove, d *moveDecision) {
	version := f.To[name].Version
	c := f.Components[name]
	since := f.Now.Sub(st.GetComponentStartTime().AsTime())
	failed := c.GetPhase() == cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED && c.GetVersion() == version &&
		(st.GetRecoveries() == 0 || since > recoveryGrace)
	stalled := st.GetComponentApplied() && since > componentWatch
	why := fmt.Sprintf("did not run %s and become Ready within %s", version, componentWatch)
	if failed {
		why = fmt.Sprintf("%s failed: %s", version, oneLine(c.GetMessage()))
	}
	switch {
	case halted:
		st.Phase, st.Message = moveHalted, f.Prev.GetMessage()
	case (failed || stalled) && st.GetRecoveries() < maxMoveRecoveries:
		st.Recoveries++
		st.ComponentStartTime = timestamppb.New(f.Now) // the watch starts over
		d.Recover = name
		st.Message = fmt.Sprintf("recovering %s (attempt %d of %d): it %s", name, st.GetRecoveries(), maxMoveRecoveries, why)
	case failed || stalled:
		st.Phase = moveHalted
		st.Message = fmt.Sprintf("%s %s after %d recoveries; cluster retry --id %s resumes the move", name, why, maxMoveRecoveries, f.ClusterID)
	default:
		st.Message = fmt.Sprintf("upgrading %s to %s", name, version)
	}
}

// moveRelease is this pass's release move (decideMove) with what it reads
// gathered: it changes in's pins (and the release recorded) while the
// cluster moves. problem, when set, says why nothing is installed this
// pass (a frozen move). The context returned is the pass's: with the
// component the move recovers marked to be tried again (withRetry).
func (s *Reconciler) moveRelease(ctx context.Context, row *model.Cluster, spec *cloudv1.ClusterSpec, in *component.PlatformInput, release string) (_ context.Context, move *cloudv1.ClusterReleaseMove, recorded, problem string, err error) {
	stored := storedStatus(row)
	prev := stored.GetReleaseMove()
	f := moveFacts{Current: release, Prev: prev, Now: now(), ClusterID: row.ID,
		Automatic:           spec.GetUpgradeMode() == cloudv1.UpgradeMode_UPGRADE_MODE_AUTOMATIC,
		Running:             row.State == StateRunning,
		KubernetesUpgrading: stored.GetUpgrade() != nil,
		Retry:               row.Retry != nil,
		Order:               component.MoveOrder(in.Entries), From: in.Pins,
		Components: map[string]*cloudv1.PlatformComponent{}}
	for _, c := range stored.GetPlatformComponents() {
		f.Components[c.GetName()] = c
	}
	channel := channelOf(spec)
	if moving(prev) && prev.GetTarget() != "" {
		f.Target = prev.GetTarget()
	} else {
		offered, err := s.deps.Platform.Offered(ctx, channel)
		if err != nil {
			return ctx, nil, release, "", fmt.Errorf("read release channel %s: %w", channel, err)
		}
		f.Target = offered
	}
	if f.Target != "" && f.Target != f.Current {
		f.To, f.ToProblem = s.movePins(ctx, f.Target, in.Cluster.KubernetesMinor)
		if !moving(prev) {
			f.Admitted, f.NotAdmitted = s.moveAdmitted(ctx, channel, f.Target, row)
		}
	}
	d := decideMove(f)
	if d.Freeze {
		return ctx, d.Status, d.Release, "the release move is halted: " + d.Status.GetMessage(), nil
	}
	if d.Pins != nil {
		in.Pins = d.Pins
	}
	in.Release = d.Release
	if d.Recover != "" {
		ctx = withRetry(ctx, retryWith(row.Retry, d.Recover, f.Now))
	}
	return ctx, d.Status, d.Release, "", nil
}

// retryWith is the pass's retry with component added: what a person's
// cluster retry of it would be.
func retryWith(r *model.Retry, component string, at time.Time) *model.Retry {
	out := &model.Retry{At: at, By: "release move recovery", Components: []string{component}}
	if r != nil {
		out.At, out.By, out.Upgrade = r.At, r.By, r.Upgrade
		out.Components = append(append([]string{}, r.Components...), component)
	}
	return out
}

// movePins are the target release's pins on minor; nil and why when it
// cannot be moved to (gone, not valid, no pins for the minor).
func (s *Reconciler) movePins(ctx context.Context, name, minor string) (map[string]component.Pin, string) {
	rel, err := s.deps.Platform.Release(ctx, name)
	switch {
	case err != nil:
		return nil, fmt.Sprintf("cannot read release %s: %v", name, err)
	case rel == nil:
		return nil, fmt.Sprintf("release %s is gone", name)
	case meta.IsStatusConditionFalse(rel.Status.Conditions, cloudv1alpha1.ReleaseConditionValid):
		return nil, fmt.Sprintf("release %s is not valid", name)
	}
	pins, problem := pinsOf(rel, minor)
	if problem != "" {
		return nil, problem
	}
	return pins, ""
}

// moveAdmitted asks the channel's rollout whether the cluster may start
// moving to release now; a source that cannot say admits it.
func (s *Reconciler) moveAdmitted(ctx context.Context, channel, release string, row *model.Cluster) (bool, string) {
	a, ok := s.deps.Platform.(reconcile.MoveAdmitter)
	if !ok {
		return true, ""
	}
	admitted, why, err := a.MoveAdmitted(ctx, channel, release, row.TenantID, row.Name)
	if err != nil {
		return false, fmt.Sprintf("cannot read channel %s's rollout: %v", channel, err)
	}
	return admitted, why
}

// oneLine is s on one line, for a move's message.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// count32 is a count of components as the status holds it.
func count32(n int) int32 {
	if n > math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(n) //nolint:gosec // bounded above
}
