package rollout

import (
	"fmt"
	"slices"
	"strings"
	"time"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/convert"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	"github.com/yindia/cluster-vending/pkg/maintenance"
	"github.com/yindia/cluster-vending/pkg/upgrade"
)

// ComputeCode plans the channel's rollout of code changes (a new
// controller build rendering a cluster's objects differently:
// status.codeChange) the way ComputeMoves plans its release: tier 2 first,
// at most maxInProgress at once per tier (tier 0: one), a soak between
// tiers, halted past maxFailed. Every cluster of the channel is a member,
// Manual ones too (a code change is the platform's, not a version): one
// with no change pending is done, one applying in progress, a halted one
// failed; one kept back by its own checks (frozen, paused, not Ready,
// components not Ready, outside its maintenance window, a Kubernetes
// upgrade running) is blocked and does not hold its tier back.
func ComputeCode(in Input) cloudv1alpha1.ChannelReleaseMoves {
	ch := in.Channel
	policy, problem := Policy(ch.Spec.Policy)
	if problem != "" {
		return cloudv1alpha1.ChannelReleaseMoves{Waiting: problem}
	}
	m := memberInputs{channel: ch.Name, holds: in.Holds, now: in.Now}
	return planChanges(cloudv1alpha1.ChannelReleaseMoves{}, ch, policy, in, m.codeMember)
}

// codeMember is the cluster's place in the channel's code rollout; false
// when it is not part of it (another channel, being deleted).
func (in memberInputs) codeMember(c *cloudv1alpha1.Cluster) (m upgrade.Member, gate *upgrade.Gate, ok bool) {
	if !strings.EqualFold(string(c.Spec.ReleaseChannel), in.channel) || !c.DeletionTimestamp.IsZero() {
		return m, nil, false
	}
	m = upgrade.Member{ID: c.Namespace + "/" + c.Name, Tier: Tier(c.Spec.Tier)}
	change := c.Status.CodeChange
	switch {
	case change == nil || change.Phase == convert.CodeChangeDone || change.Phase == "":
		m.State, m.FinishedAt = upgrade.Succeeded, codeFinished(c)
		return m, nil, true
	case change.Phase == convert.CodeChangeHalted:
		m.State, m.FinishedAt = upgrade.Failed, codeFinished(c)
		return m, nil, true
	case change.Phase == convert.CodeChangeApplying:
		m.State = upgrade.InProgress
		return m, nil, true
	}
	if c.Status.Upgrade != nil {
		m.State, gate = upgrade.Blocked, &upgrade.Gate{Code: GateKubernetes, Blocking: true,
			Message: fmt.Sprintf("a Kubernetes upgrade to %s is in progress", c.Status.Upgrade.ToVersion)}
		return m, gate, true
	}
	// The gates of a Kubernetes hop, without the mode (a code change is
	// the platform's) and the deprecation scan (it keeps the minor).
	f := in.facts(c, "")
	f.Mode, f.DeprecationsScanned, f.DeprecationsTarget, f.RemovedInUse = upgrade.Automatic, true, "", 0
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

// codeFinished is when the cluster's code change was applied in full;
// else when it became Ready.
func codeFinished(c *cloudv1alpha1.Cluster) time.Time {
	if cc := c.Status.CodeChange; cc != nil && cc.FinishTime != nil {
		return cc.FinishTime.Time
	}
	return FinishedAt(c)
}

// NetworkTier is the tier a network's code change rolls out in: the most
// careful of its clusters' (tier 0 before 1 before 2); Tier2 without
// clusters.
func NetworkTier(clusters []cloudv1alpha1.Cluster) upgrade.Tier {
	tier := upgrade.Tier2
	for i := range clusters {
		tier = min(tier, Tier(clusters[i].Spec.Tier))
	}
	return tier
}

// rolloutOrder is how far a tier is in a rollout: Tier2 first.
func rolloutOrder(t upgrade.Tier) int { return int(upgrade.Tier2 - t) }

// NetworkCodeAdmitted reports whether a network's code change may be
// applied now, and why not: no freeze holds it (FrozenNetwork), and the
// code rollout of every channel of its clusters has reached the network's
// tier (NetworkTier), so it goes before its clusters of that tier.
// channels are the release channels by name.
func NetworkCodeAdmitted(id, tenant string, clusters []cloudv1alpha1.Cluster, channels map[string]*cloudv1alpha1.ReleaseChannel,
	holds []cloudv1alpha1.ReleaseHold, now time.Time) (bool, string) {
	if h := FrozenNetwork(id, tenant, clusters, holds, now); h != nil {
		return false, FreezeMessage(h)
	}
	tier := NetworkTier(clusters)
	seen := map[string]bool{}
	for i := range clusters {
		name := strings.ToLower(string(clusters[i].Spec.ReleaseChannel))
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		if why := channelReached(name, channels[name], tier); why != "" {
			return false, why
		}
	}
	return true, ""
}

// channelReached says why the channel's code rollout has not reached
// tier yet; empty when it has (or is done).
func channelReached(name string, ch *cloudv1alpha1.ReleaseChannel, tier upgrade.Tier) string {
	if ch == nil {
		return fmt.Sprintf("release channel %s does not exist", name)
	}
	cc := ch.Status.CodeChanges
	switch {
	case cc == nil:
		return fmt.Sprintf("waiting for channel %s's code rollout to be planned", name)
	case cc.Done:
		return ""
	case cc.Halted:
		return fmt.Sprintf("channel %s's code rollout is halted: %s", name, cc.Waiting)
	case cc.Tier == "" || rolloutOrder(Tier(cc.Tier)) < rolloutOrder(tier):
		return fmt.Sprintf("waiting for channel %s's code rollout to reach tier %d (the network goes before its clusters of that tier)",
			name, tier)
	}
	return ""
}

// FrozenNetwork is the freeze that holds a network; nil when none does.
// The platform's and its tenant's freezes hold it unless they let it, or
// one of its clusters, through; a channel's freeze holds it when it holds
// one of its clusters and does not let the network through.
func FrozenNetwork(id, tenant string, clusters []cloudv1alpha1.Cluster, holds []cloudv1alpha1.ReleaseHold, now time.Time) *cloudv1alpha1.ReleaseHold {
	for i := range holds {
		h := &holds[i]
		if !Active(*h, now) || slices.Contains(h.Spec.Except, id) {
			continue
		}
		switch {
		case h.Spec.Platform || (h.Spec.TenantID != "" && h.Spec.TenantID == tenant):
			if !letsAClusterThrough(h.Spec, clusters) {
				return h
			}
		case h.Spec.Channel != "" || h.Spec.ClusterID != "":
			if slices.ContainsFunc(clusters, func(c cloudv1alpha1.Cluster) bool { return Matches(h.Spec, &c) }) {
				return h
			}
		}
	}
	return nil
}

// letsAClusterThrough reports whether the freeze lets one of the clusters
// through: their network goes through with it.
func letsAClusterThrough(h cloudv1alpha1.ReleaseHoldSpec, clusters []cloudv1alpha1.Cluster) bool {
	return slices.ContainsFunc(clusters, func(c cloudv1alpha1.Cluster) bool {
		return slices.Contains(h.Except, c.Labels[cloudstore.LabelID])
	})
}
