package rollout

import (
	"fmt"
	"strings"
	"time"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/convert"
	"github.com/yindia/cluster-vending/pkg/maintenance"
	"github.com/yindia/cluster-vending/pkg/upgrade"
)

// GateKubernetes keeps a cluster from moving its components while a
// Kubernetes upgrade of it is in progress.
const GateKubernetes upgrade.GateCode = "KUBERNETES_UPGRADING"

// ComputeMoves plans the channel's rollout of its release to its
// automatic clusters' components, the way Compute plans its Kubernetes
// rollout: tier 2 first, at most maxInProgress moving at once per tier
// (tier 0: one), a soak between tiers, halted past maxFailed. A member is
// an automatic cluster of the channel with components installed: one on
// the release is done, one moving (status.releaseMove Components) in
// progress, a halted one failed; one kept back by its own checks (frozen,
// paused, not Ready, components not Ready, outside its
// maintenance window, a Kubernetes upgrade running) is blocked and does
// not hold its tier back.
func ComputeMoves(in Input) cloudv1alpha1.ChannelReleaseMoves {
	ch := in.Channel
	out := cloudv1alpha1.ChannelReleaseMoves{Release: ch.Status.Release}
	if out.Release == "" {
		out.Waiting = "the channel offers no release yet"
		return out
	}
	policy, problem := Policy(ch.Spec.Policy)
	if problem != "" {
		out.Waiting = problem
		return out
	}
	m := memberInputs{channel: ch.Name, holds: in.Holds, now: in.Now}
	var members []upgrade.Member
	for i := range in.Clusters {
		c := &in.Clusters[i]
		member, gate, ok := m.moveMember(c, out.Release)
		if !ok {
			continue
		}
		members = append(members, member)
		if gate != nil && len(out.Blocked) < cloudv1alpha1.MaxRolloutClusters {
			out.Blocked = append(out.Blocked, cloudv1alpha1.BlockedCluster{Cluster: member.ID, Gate: string(gate.Code), Message: gate.Message})
		}
	}
	if len(members) == 0 {
		out.Done = true
		return out
	}
	d := upgrade.Plan(policy, in.Now, members)
	if ch.Spec.Paused && !d.Halted && len(d.Start) > 0 {
		d.Start, d.Waiting = nil, "the channel is paused (spec.paused): no cluster starts"
	}
	out.Admitted, out.Waiting, out.Halted, out.Done = capped(d.Start), d.Waiting, d.Halted, d.Done
	for _, tier := range []upgrade.Tier{upgrade.Tier2, upgrade.Tier1, upgrade.Tier0} {
		c := d.Tiers[tier]
		if c.Pending+c.InProgress+c.Succeeded+c.Failed+c.Blocked == 0 {
			continue
		}
		out.Members = append(out.Members, cloudv1alpha1.RolloutTierCount{Tier: ClusterTier(tier),
			Pending: Count32(c.Pending), InProgress: Count32(c.InProgress), Succeeded: Count32(c.Succeeded),
			Failed: Count32(c.Failed), Blocked: Count32(c.Blocked)})
	}
	return out
}

// moveMember is the cluster's place in the channel's release rollout;
// false when it is not part of it (another channel, being deleted,
// manual, nothing installed yet). gate is why it is blocked.
func (in memberInputs) moveMember(c *cloudv1alpha1.Cluster, release string) (m upgrade.Member, gate *upgrade.Gate, ok bool) {
	if !strings.EqualFold(string(c.Spec.ReleaseChannel), in.channel) || !c.DeletionTimestamp.IsZero() ||
		c.Spec.UpgradeMode != cloudv1alpha1.UpgradeModeAutomatic || c.Status.PlatformRelease == "" {
		return m, nil, false
	}
	m = upgrade.Member{ID: c.Namespace + "/" + c.Name, Tier: Tier(c.Spec.Tier)}
	move := c.Status.ReleaseMove
	switch {
	case move != nil && move.Phase == convert.ReleaseMoveHalted:
		m.State, m.FinishedAt = upgrade.Failed, moveStart(move)
		return m, nil, true
	case move != nil && move.Phase == convert.ReleaseMoveComponents:
		m.State = upgrade.InProgress
		return m, nil, true
	case c.Status.PlatformRelease == release:
		m.State, m.FinishedAt = upgrade.Succeeded, FinishedAt(c)
		return m, nil, true
	}
	if c.Status.Upgrade != nil {
		m.State, gate = upgrade.Blocked, &upgrade.Gate{Code: GateKubernetes, Blocking: true,
			Message: fmt.Sprintf("a Kubernetes upgrade to %s is in progress", c.Status.Upgrade.ToVersion)}
		return m, gate, true
	}
	// The gates of a Kubernetes hop, without the deprecation scan (a
	// release move keeps the cluster's minor).
	f := in.facts(c, "")
	f.DeprecationsScanned, f.DeprecationsTarget, f.RemovedInUse = true, "", 0
	window := convert.ClusterSpecFromAPI(c.Spec, "", nil).GetMaintenanceWindow()
	f.WindowOpen, f.NextOpen = maintenance.Open(window, in.now), maintenance.Next(window, in.now)
	for _, g := range upgrade.Check(f) {
		if g.Blocking {
			m.State = upgrade.Blocked
			return m, &g, true
		}
	}
	m.State = upgrade.Pending
	return m, nil, true
}

// moveStart is when a move started; zero when unknown.
func moveStart(m *cloudv1alpha1.ClusterReleaseMove) time.Time {
	if m.StartTime == nil {
		return time.Time{}
	}
	return m.StartTime.Time
}
