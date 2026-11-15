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

// DefaultComponentUpgradeTimeout is how long one component of a release
// move may take to settle when the setting names none.
const DefaultComponentUpgradeTimeout = 30 * time.Minute

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
	Retry   bool
	Now     time.Time
	Timeout time.Duration
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
}

// decideMove is one pass of moving a cluster's components from Current to
// Target, one component at a time (component.PlanMove): a move starts
// when the cluster is Automatic, Running, not upgrading Kubernetes and
// admitted by its channel's rollout; it halts when the component being
// moved fails or does not settle within Timeout, and stays halted until a
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
	}
	retry := fmt.Sprintf("; cluster retry --id %s resumes the move", f.ClusterID)
	c := f.Components[m.Component]
	switch {
	case halted:
		st.Phase, st.Message = moveHalted, f.Prev.GetMessage()
	case c.GetPhase() == cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED && c.GetVersion() == f.To[m.Component].Version:
		st.Phase = moveHalted
		st.Message = fmt.Sprintf("%s %s failed: %s%s", m.Component, c.GetVersion(), oneLine(c.GetMessage()), retry)
	case f.Timeout > 0 && f.Now.Sub(st.GetComponentStartTime().AsTime()) > f.Timeout:
		st.Phase = moveHalted
		st.Message = fmt.Sprintf("%s did not run %s and become Ready within %s%s", m.Component, f.To[m.Component].Version, f.Timeout, retry)
	default:
		st.Message = fmt.Sprintf("upgrading %s to %s", m.Component, f.To[m.Component].Version)
	}
	return d
}

// moveRelease is this pass's release move (decideMove) with what it reads
// gathered: it changes in's pins (and the release recorded) while the
// cluster moves; nil when component upgrades are off. problem, when set,
// says why nothing is installed this pass (a frozen move).
func (s *Reconciler) moveRelease(ctx context.Context, row *model.Cluster, spec *cloudv1.ClusterSpec, in *component.PlatformInput, release string) (move *cloudv1.ClusterReleaseMove, recorded, problem string, err error) {
	if !s.deps.ComponentUpgrades {
		return nil, release, "", nil
	}
	stored := storedStatus(row)
	prev := stored.GetReleaseMove()
	f := moveFacts{Current: release, Prev: prev, Now: now(), Timeout: s.componentTimeout(), ClusterID: row.ID,
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
			return nil, release, "", fmt.Errorf("read release channel %s: %w", channel, err)
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
		return d.Status, d.Release, "the release move is halted: " + d.Status.GetMessage(), nil
	}
	if d.Pins != nil {
		in.Pins = d.Pins
	}
	in.Release = d.Release
	return d.Status, d.Release, "", nil
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

func (s *Reconciler) componentTimeout() time.Duration {
	if s.deps.ComponentUpgradeTimeout > 0 {
		return s.deps.ComponentUpgradeTimeout
	}
	return DefaultComponentUpgradeTimeout
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
