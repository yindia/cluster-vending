package rollout

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/convert"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/maintenance"
	"github.com/yindia/cluster-vending/pkg/reconcile"
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
	out := cloudv1alpha1.ChannelReleaseMoves{Release: in.CodeEpoch}
	discovering := false
	members := make([]upgrade.Member, 0, len(in.Networks)+len(in.Clusters))
	parents := map[string]*cloudv1alpha1.Network{}
	add := func(member upgrade.Member, gate *upgrade.Gate) {
		members = append(members, member)
		if gate != nil && len(out.Blocked) < cloudv1alpha1.MaxRolloutClusters {
			out.Blocked = append(out.Blocked, cloudv1alpha1.BlockedCluster{Cluster: member.ID, Gate: string(gate.Code), Message: gate.Message})
		}
	}
	for i := range in.Networks {
		network := &in.Networks[i]
		parents[network.Namespace+"/"+network.Name] = network
		clusters := in.NetworkClusters
		if clusters == nil {
			clusters = in.Clusters
		}
		if member, gate, ok := m.networkCodeMember(network, clusters); ok {
			add(member, gate)
			children := networkChildren(network, clusters)
			frozen := FrozenNetwork(network.Labels[cloudstore.LabelID], strings.TrimPrefix(network.Namespace, "tenant-"), children, in.Holds, in.Now) != nil
			if in.CodeEpoch != "" && member.State != upgrade.Blocked && !frozen && network.GetAnnotations()["crossplane.io/paused"] != "true" && !strings.HasPrefix(NetworkCodeTarget(network), in.CodeEpoch+"/") {
				discovering = true
			}
		}
	}
	for i := range in.Clusters {
		cluster := &in.Clusters[i]
		member, gate, ok := m.codeMember(cluster)
		if !ok {
			continue
		}
		// A dependent cluster must not consume a batch slot before its network
		// can start. The network remains a real pending/in-progress/failed member.
		if member.State == upgrade.Pending {
			parent := parents[cluster.Namespace+"/"+cluster.Spec.Network.NetworkRef.Name]
			if parent != nil && networkCodeOutstanding(parent) {
				member.State = upgrade.Blocked
				gate = &upgrade.Gate{Code: "NETWORK_CODE_PENDING", Blocking: true, Message: "waiting for parent network " + parent.Name + " code change first"}
			}
		}
		add(member, gate)
	}
	out = planChangeMembers(out, ch, policy, in.Now, members)
	if discovering {
		out.Admitted, out.Done = nil, false
		out.Waiting = "waiting for every participating network to observe this controller's render before code admission"
	}
	return out
}

// The API's typed CodeChange projection omits target; the persisted contract
// retains it. Missing/malformed observations never satisfy an epoch barrier.
func NetworkCodeTarget(network *cloudv1alpha1.Network) string {
	if network.Status.ContractStatus == nil {
		return ""
	}
	var status struct {
		CodeChange struct {
			Target string `json:"target"`
		} `json:"codeChange"`
	}
	if json.Unmarshal(network.Status.ContractStatus.Raw, &status) != nil {
		return ""
	}
	return status.CodeChange.Target
}

// NetworkCodeKey separates network members from namespace/name cluster keys.
// Binding admission to the pending render target rejects plans from older code.
func NetworkCodeKey(network *cloudv1alpha1.Network) string {
	key := "network:" + network.Namespace + "/" + network.Name
	if target := NetworkCodeTarget(network); target != "" {
		key += "@" + target
	}
	return key
}

func networkCodeOutstanding(network *cloudv1alpha1.Network) bool {
	change := network.Status.CodeChange
	return change != nil && change.Phase != "" && (change.Phase != convert.CodeChangeDone || !networkCodeReady(network))
}

func networkCodeReady(network *cloudv1alpha1.Network) bool {
	if network.Status.Phase != cloudv1alpha1.NetworkPhaseReady || meta.IsStatusConditionFalse(network.Status.Conditions, "Ready") || network.Status.ObservedGeneration < network.Generation {
		return false
	}
	if ready := meta.FindStatusCondition(network.Status.Conditions, "Ready"); ready != nil && ready.ObservedGeneration > 0 && ready.ObservedGeneration < network.Generation {
		return false
	}
	if network.Status.ContractStatus != nil {
		status := &cloudv1.NetworkStatus{}
		if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(network.Status.ContractStatus.Raw, status); err != nil {
			return false
		}
		for _, resource := range status.ManagedResources {
			// No observedGeneration: the provider publishes none (provider-aws
			// v2.9.0). The network's reconciler proved the observation came
			// after its apply before it marked the change Done, so READY counts.
			stale := resource.ObservedGeneration > 0 && resource.Generation > resource.ObservedGeneration
			if resource.Kind != "Usage" && stale || resource.State != cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY || resource.Paused {
				return false
			}
		}
	}
	return true
}

func (in memberInputs) networkCodeMember(network *cloudv1alpha1.Network, clusters []cloudv1alpha1.Cluster) (upgrade.Member, *upgrade.Gate, bool) {
	m := upgrade.Member{ID: NetworkCodeKey(network), Tier: Tier(network.Spec.Tier)}
	if !network.DeletionTimestamp.IsZero() || network.Status.Phase == cloudv1alpha1.NetworkPhaseImported || network.Status.Phase == cloudv1alpha1.NetworkPhaseProvisioning || (network.Status.Phase == "" && network.Status.CodeChange == nil) {
		return m, nil, false
	}
	children := networkChildren(network, clusters)
	if network.GetAnnotations()["crossplane.io/paused"] == "true" && (network.Status.CodeChange == nil || network.Status.CodeChange.Phase != convert.CodeChangeApplying) {
		m.State = upgrade.Blocked
		return m, &upgrade.Gate{Code: upgrade.GatePaused, Blocking: true, Message: "the network is paused"}, true
	}
	if (network.Status.Phase == cloudv1alpha1.NetworkPhaseFailed || meta.IsStatusConditionFalse(network.Status.Conditions, "Synced")) && (network.Status.CodeChange == nil || network.Status.CodeChange.Phase == convert.CodeChangeDone) {
		m.State = upgrade.Blocked
		return m, &upgrade.Gate{Code: upgrade.GateNotReady, Blocking: true, Message: "the network is not provisionable"}, true
	}
	if hold := FrozenNetwork(network.Labels[cloudstore.LabelID], strings.TrimPrefix(network.Namespace, "tenant-"), children, in.holds, in.now); hold != nil && (network.Status.CodeChange == nil || network.Status.CodeChange.Phase != convert.CodeChangeApplying) {
		m.State = upgrade.Blocked
		return m, &upgrade.Gate{Code: upgrade.GateFrozen, Blocking: true, Message: FreezeMessage(hold)}, true
	}
	change := network.Status.CodeChange
	if change == nil || change.Phase == "" {
		m.State = upgrade.Succeeded
		return m, nil, true
	}
	switch change.Phase {
	case convert.CodeChangeHalted:
		m.State = upgrade.Failed
	case convert.CodeChangeApplying:
		m.State = upgrade.InProgress
		if network.Status.Phase == cloudv1alpha1.NetworkPhaseFailed {
			m.State = upgrade.Failed
		}
	case convert.CodeChangeDone:
		m.State = upgrade.InProgress // desired objects applied; wait for cloud convergence
		if network.Status.Phase == cloudv1alpha1.NetworkPhaseFailed {
			m.State = upgrade.Failed
		} else if networkCodeReady(network) {
			m.State = upgrade.Succeeded
		}
	default:
		if hold := FrozenNetwork(network.Labels[cloudstore.LabelID], strings.TrimPrefix(network.Namespace, "tenant-"), children, in.holds, in.now); hold != nil {
			m.State = upgrade.Blocked
			return m, &upgrade.Gate{Code: upgrade.GateFrozen, Blocking: true, Message: FreezeMessage(hold)}, true
		}
		if network.GetAnnotations()["crossplane.io/paused"] == "true" {
			m.State = upgrade.Blocked
			return m, &upgrade.Gate{Code: upgrade.GatePaused, Blocking: true, Message: "the network is paused"}, true
		}
		m.State = upgrade.Pending
	}
	if change.FinishTime != nil {
		m.FinishedAt = change.FinishTime.Time
	}
	if ready := meta.FindStatusCondition(network.Status.Conditions, "Ready"); ready != nil && ready.Status == metav1.ConditionTrue && ready.LastTransitionTime.After(m.FinishedAt) {
		m.FinishedAt = ready.LastTransitionTime.Time
	}
	return m, nil, true
}

func networkChildren(network *cloudv1alpha1.Network, clusters []cloudv1alpha1.Cluster) []cloudv1alpha1.Cluster {
	return slices.DeleteFunc(slices.Clone(clusters), func(cluster cloudv1alpha1.Cluster) bool {
		return cluster.Namespace != network.Namespace || cluster.Spec.Network.NetworkRef.Name != network.Name
	})
}

// codeMember is the cluster's place in the channel's code rollout; false
// when it is not part of it (another channel, being deleted).
func (in memberInputs) codeMember(c *cloudv1alpha1.Cluster) (m upgrade.Member, gate *upgrade.Gate, ok bool) {
	if !strings.EqualFold(string(c.Spec.ReleaseChannel), in.channel) || !c.DeletionTimestamp.IsZero() {
		return m, nil, false
	}
	m = upgrade.Member{ID: c.Namespace + "/" + c.Name, Tier: Tier(c.Status.Tier)}
	change := clusterChange(c)
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
	// the platform's), the deprecation scan (it keeps the minor) and the
	// health of the components: the change may be what repairs a failed
	// one (a render that kept its pods off every node), and holding it
	// for that component would keep the cluster broken for good.
	f := in.facts(c, "")
	f.Mode, f.DeprecationsScanned, f.DeprecationsTarget, f.RemovedInUse = upgrade.Automatic, true, "", 0
	f.PlatformReady, f.TenantNotReady = true, false
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
	if cc := clusterChange(c); cc != nil && cc.FinishTime != nil {
		return cc.FinishTime.Time
	}
	return FinishedAt(c)
}

// changeRank orders the phases of a cluster's code and catalog changes:
// the one further from done stands for the cluster.
var changeRank = map[string]int{convert.CodeChangeApplying: 4, convert.CodeChangeHalted: 3, convert.CodeChangePending: 2, convert.CodeChangeDone: 1}

// clusterChange is the cluster's code change and catalog change (PROP-01
// 4.7) as one member of the code rollout: applying while either applies,
// failed while either halted, pending while either waits, done once both
// are, finished when the later of the two finished. nil when neither was
// recorded.
func clusterChange(c *cloudv1alpha1.Cluster) *cloudv1alpha1.CodeChange {
	code, cat := c.Status.CodeChange, c.Status.CatalogChange
	if cat == nil || cat.Phase == "" {
		return code
	}
	catalog := &cloudv1alpha1.CodeChange{Phase: cat.Phase, Summary: cat.Summary, Message: cat.Message, StartTime: cat.StartTime, FinishTime: cat.FinishTime}
	if code == nil || code.Phase == "" {
		return catalog
	}
	switch rc, rk := changeRank[code.Phase], changeRank[cat.Phase]; {
	case rk > rc:
		return catalog
	case rk < rc:
		return code
	}
	out := code.DeepCopy()
	if catalog.FinishTime != nil && (out.FinishTime == nil || catalog.FinishTime.After(out.FinishTime.Time)) {
		out.FinishTime = catalog.FinishTime
	}
	return out
}

// NetworkCodeAdmitted reports whether a network's code change may be
// applied now, and why not: no freeze holds it (FrozenNetwork), and the
// code rollout of every channel of its clusters has reached the network's
// own tier, so it goes before its clusters of that tier.
// channels are the release channels by name.
func NetworkCodeAdmitted(network *cloudv1alpha1.Network, clusters []cloudv1alpha1.Cluster, channels map[string]*cloudv1alpha1.ReleaseChannel,
	holds []cloudv1alpha1.ReleaseHold, now time.Time) (bool, string) {
	if h := FrozenNetwork(network.Labels[cloudstore.LabelID], strings.TrimPrefix(network.Namespace, "tenant-"), clusters, holds, now); h != nil {
		return false, FreezeMessage(h)
	}
	seen := map[string]bool{}
	for i := range clusters {
		name := strings.ToLower(string(clusters[i].Spec.ReleaseChannel))
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		if why := networkAdmission(name, channels[name], network); why != "" {
			return false, why
		}
	}

	// Without channel-bound children, use every configured platform code rollout.
	if len(seen) == 0 {
		if len(channels) == 0 {
			return false, "waiting for a platform code rollout to be planned"
		}
		names := make([]string, 0, len(channels))
		for name := range channels {
			names = append(names, name)
		}
		slices.Sort(names)
		for _, name := range names {
			if why := networkAdmission(name, channels[name], network); why != "" {
				return false, why
			}
		}
	}
	return true, ""
}

// channelReached says why the channel's code rollout has not reached
// tier yet; empty when it has (or is done).
func networkAdmission(name string, ch *cloudv1alpha1.ReleaseChannel, network *cloudv1alpha1.Network) string {
	if ch == nil {
		return fmt.Sprintf("release channel %s does not exist", name)
	}
	if ch.Spec.Paused {
		return "release channel " + name + " is paused"
	}
	tier := Tier(network.Spec.Tier)
	cc := ch.Status.CodeChanges
	if cc != nil && cc.Release != "" && !strings.HasPrefix(NetworkCodeTarget(network), cc.Release+"/") {
		return "waiting for this network to observe channel " + name + "'s current controller render"
	}
	switch {
	case cc == nil:
		return fmt.Sprintf("waiting for channel %s's code rollout to be planned", name)
	case cc.Halted:
		return fmt.Sprintf("channel %s's code rollout is halted: %s", name, cc.Waiting)
	case cc.Done || cc.Tier == "" || Tier(cc.Tier) != tier:
		return fmt.Sprintf("waiting for channel %s's code rollout to reach tier %d (the network goes before its clusters of that tier)",
			name, tier)
	}
	if slices.Contains(cc.Admitted, NetworkCodeKey(network)) {
		return ""
	}
	return "waiting for channel " + name + " to admit this network's current render target"
}

// FrozenNetwork is the freeze that holds a network; nil when none does.
// A scoped cluster exception never resumes the network. Cluster release
// locks can also hold a shared network code change.
func FrozenNetwork(id, tenant string, clusters []cloudv1alpha1.Cluster, holds []cloudv1alpha1.ReleaseHold, now time.Time) *cloudv1alpha1.ReleaseHold {
	for i := range holds {
		h := &holds[i]
		if !Active(*h, now) || slices.Contains(h.Spec.Except, id) {
			continue
		}
		switch {
		case h.Spec.Platform:
			if frozen := reconcile.FreezeForTarget(h, now, reconcile.FreezeTarget{TenantID: tenant, NetworkID: id}); frozen.Holds() {
				current := h.DeepCopy()
				current.Spec.Reason = frozen.Reason
				current.Spec.Until = nil
				if frozen.Until != nil {
					until := metav1.NewTime(*frozen.Until)
					current.Spec.Until = &until
				}
				if current.Annotations == nil {
					current.Annotations = map[string]string{}
				}
				current.Annotations["cloud.cluster-vending.io/effective-freeze-scope"] = frozen.Scope
				return current
			}
		case h.Spec.ClusterID != "":
			if slices.ContainsFunc(clusters, func(c cloudv1alpha1.Cluster) bool { return Matches(h.Spec, &c) }) {
				return h
			}
		}
	}
	return nil
}
