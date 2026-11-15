package admin

import (
	"maps"
	"slices"

	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/convert"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
	"github.com/yindia/cluster-vending/pkg/release"
	"github.com/yindia/cluster-vending/pkg/rollout"
	"github.com/yindia/cluster-vending/pkg/upgrade"
)

// channelEnums are the ReleaseChannel objects' names as the contract
// spells them.
var channelEnums = map[string]cloudv1.ReleaseChannel{
	cloudv1alpha1.ChannelAlpha:  cloudv1.ReleaseChannel_RELEASE_CHANNEL_ALPHA,
	cloudv1alpha1.ChannelBeta:   cloudv1.ReleaseChannel_RELEASE_CHANNEL_BETA,
	cloudv1alpha1.ChannelStable: cloudv1.ReleaseChannel_RELEASE_CHANNEL_STABLE,
}

// ChannelFromName is a channel object's name (alpha, beta, stable) as the
// contract's enum; UNSPECIFIED for any other.
func ChannelFromName(name string) cloudv1.ReleaseChannel { return channelEnums[name] }

// ChannelName is the channel object's name of the contract's channel; ""
// for UNSPECIFIED.
func ChannelName(c cloudv1.ReleaseChannel) string {
	for name, e := range channelEnums {
		if e == c {
			return name
		}
	}
	return ""
}

// ChannelNames are the channel objects' names, alpha first.
func ChannelNames() []string {
	return []string{cloudv1alpha1.ChannelAlpha, cloudv1alpha1.ChannelBeta, cloudv1alpha1.ChannelStable}
}

var tierEnums = map[cloudv1alpha1.ClusterTier]cloudv1.ClusterTier{
	cloudv1alpha1.ClusterTier0: cloudv1.ClusterTier_CLUSTER_TIER_0,
	cloudv1alpha1.ClusterTier1: cloudv1.ClusterTier_CLUSTER_TIER_1,
	cloudv1alpha1.ClusterTier2: cloudv1.ClusterTier_CLUSTER_TIER_2,
}

// TierFromAPI is a kind's tier as the contract spells it.
func TierFromAPI(t cloudv1alpha1.ClusterTier) cloudv1.ClusterTier { return tierEnums[t] }

// RolloutTierFromAPI is a rollout tier as the contract spells it.
func RolloutTierFromAPI(t upgrade.Tier) cloudv1.ClusterTier { return tierEnums[rollout.ClusterTier(t)] }

var rolloutStates = map[upgrade.State]cloudv1.RolloutState{
	upgrade.Pending:    cloudv1.RolloutState_ROLLOUT_STATE_PENDING,
	upgrade.InProgress: cloudv1.RolloutState_ROLLOUT_STATE_IN_PROGRESS,
	upgrade.Succeeded:  cloudv1.RolloutState_ROLLOUT_STATE_SUCCEEDED,
	upgrade.Failed:     cloudv1.RolloutState_ROLLOUT_STATE_FAILED,
	upgrade.Blocked:    cloudv1.RolloutState_ROLLOUT_STATE_BLOCKED,
}

// ConditionsFromAPI are a kind's conditions as the contract returns them.
func ConditionsFromAPI(in []metav1.Condition) []*cloudv1.Condition {
	out := make([]*cloudv1.Condition, 0, len(in))
	for _, c := range in {
		out = append(out, &cloudv1.Condition{Type: c.Type, Status: conditionStatus(c.Status),
			Reason: c.Reason, Message: c.Message, LastTransitionTime: timestamppb.New(c.LastTransitionTime.Time)})
	}
	return out
}

func timeOf(t *metav1.Time) *timestamppb.Timestamp {
	if t == nil || t.IsZero() {
		return nil
	}
	return timestamppb.New(t.Time)
}

// ReleaseFromAPI is a Release as the contract returns it; detail adds its
// minors and notes (GetRelease).
func ReleaseFromAPI(r *cloudv1alpha1.Release, detail bool) *cloudv1.Release {
	out := &cloudv1.Release{Name: r.Name, Shipped: r.Labels[cloudv1alpha1.LabelShipped] == "true",
		Valid: true, Withdrawn: r.Spec.Withdrawn, Holds: slices.Clone(r.Status.Holds),
		CreateTime: timeOf(&r.CreationTimestamp)}
	if c := meta.FindStatusCondition(r.Status.Conditions, cloudv1alpha1.ReleaseConditionValid); c != nil && c.Status != metav1.ConditionTrue {
		out.Valid, out.InvalidReason = false, c.Message
	}
	for _, ch := range r.Status.Channels {
		if e := ChannelFromName(ch); e != cloudv1.ReleaseChannel_RELEASE_CHANNEL_UNSPECIFIED {
			out.Channels = append(out.Channels, e)
		}
	}
	if !detail {
		return out
	}
	out.Notes = r.Spec.Notes
	pins := reconcile.ReleaseOf(r.Name, r.Spec.Content)
	for _, k := range r.Spec.Content.Kubernetes {
		m := &cloudv1.ReleaseMinor{Minor: k.Minor, Channel: ChannelFromName(k.Channel), Default: k.Default,
			UpgradeTo: k.UpgradeTo, EndOfSupport: timeOf(&k.EndOfSupport), Ending: release.Ending(pins, k.Minor)}
		for _, c := range k.Components {
			pc := &cloudv1.ReleaseComponent{Name: c.Name, Version: c.Version}
			if values, err := convert.RawToStruct(c.Values); err == nil {
				pc.Values = values
			}
			m.Components = append(m.Components, pc)
		}
		out.Kubernetes = append(out.Kubernetes, m)
	}
	return out
}

// RolloutPolicyFromAPI is a channel's rollout policy as the contract
// returns it.
func RolloutPolicyFromAPI(p cloudv1alpha1.RolloutPolicy) *cloudv1.RolloutPolicy {
	out := &cloudv1.RolloutPolicy{MaxInProgress: p.MaxInProgress, MaxFailed: p.MaxFailed, Soak: p.Soak.Duration.String()}
	if p.ReleaseSoak != nil {
		out.ReleaseSoak = p.ReleaseSoak.Duration.String()
	}
	if len(p.SoakPerTier) > 0 {
		out.SoakPerTier = map[string]string{}
		for _, tier := range slices.Sorted(maps.Keys(p.SoakPerTier)) {
			out.SoakPerTier[string(tier)] = p.SoakPerTier[tier].Duration.String()
		}
	}
	return out
}

// RolloutSummaryFromAPI is the rollout a channel's status reports.
func RolloutSummaryFromAPI(r *cloudv1alpha1.ChannelRollout) *cloudv1.RolloutSummary {
	if r == nil {
		return nil
	}
	return &cloudv1.RolloutSummary{TargetMinor: r.TargetMinor, ReportOnly: r.ReportOnly, Waiting: r.Waiting,
		Halted: r.Halted, Done: r.Done, WouldStart: slices.Clone(r.WouldStart), Tiers: rolloutTiersFromAPI(r.Members)}
}

// rolloutTiersFromAPI are a rollout's counts per tier.
func rolloutTiersFromAPI(members []cloudv1alpha1.RolloutTierCount) []*cloudv1.RolloutTier {
	out := make([]*cloudv1.RolloutTier, 0, len(members))
	for _, t := range members {
		out = append(out, &cloudv1.RolloutTier{Tier: TierFromAPI(t.Tier), Pending: t.Pending,
			InProgress: t.InProgress, Succeeded: t.Succeeded, Failed: t.Failed, Blocked: t.Blocked})
	}
	return out
}

// ChannelFromAPI is a ReleaseChannel as the contract returns it; detail
// adds its history (GetReleaseChannel).
func ChannelFromAPI(ch *cloudv1alpha1.ReleaseChannel, detail bool) *cloudv1.ReleaseChannelInfo {
	out := &cloudv1.ReleaseChannelInfo{Channel: ChannelFromName(ch.Name), Promotion: string(ch.Spec.Promotion),
		PinnedRelease: ch.Spec.Release, Release: ch.Status.Release, KubernetesMinor: ch.Status.KubernetesMinor,
		Paused: ch.Spec.Paused, Policy: RolloutPolicyFromAPI(ch.Spec.Policy), RolloutTier: TierFromAPI(ch.Status.RolloutTier),
		Rollout: RolloutSummaryFromAPI(ch.Status.Rollout), Conditions: ConditionsFromAPI(ch.Status.Conditions),
		CreateTime: timeOf(&ch.CreationTimestamp)}
	if p := ch.Status.Pending; p != nil {
		out.Pending = &cloudv1.PendingRelease{Release: p.Release, Since: timeOf(&p.Since), Until: timeOf(&p.Until)}
	}
	out.ReleaseMoves = ChangeRolloutFromAPI(ch.Status.ReleaseMoves)
	if detail {
		for _, h := range ch.Status.History {
			out.History = append(out.History, &cloudv1.ChannelHistoryEntry{Release: h.Release, From: timeOf(&h.From),
				To: timeOf(h.To), Reason: h.Reason})
		}
	}
	return out
}

// ChangeRolloutFromAPI is a channel's rollout of its release moves as its
// status reports it; nil when none is planned.
func ChangeRolloutFromAPI(m *cloudv1alpha1.ChannelReleaseMoves) *cloudv1.ChangeRollout {
	if m == nil {
		return nil
	}
	out := &cloudv1.ChangeRollout{Target: m.Release, Admitted: slices.Clone(m.Admitted), Waiting: m.Waiting, Halted: m.Halted,
		Done: m.Done, Tiers: rolloutTiersFromAPI(m.Members)}
	for _, b := range m.Blocked {
		out.Blocked = append(out.Blocked, &cloudv1.BlockedMember{Cluster: b.Cluster, Gate: b.Gate, Reason: b.Message})
	}
	return out
}

// RolloutFromAPI is a channel's rollout as its stored status reports it:
// no members.
func RolloutFromAPI(ch *cloudv1alpha1.ReleaseChannel) *cloudv1.Rollout {
	return &cloudv1.Rollout{Channel: ChannelFromName(ch.Name), Release: ch.Status.Release, RolloutTier: TierFromAPI(ch.Status.RolloutTier),
		Policy: RolloutPolicyFromAPI(ch.Spec.Policy), Paused: ch.Spec.Paused, Summary: RolloutSummaryFromAPI(ch.Status.Rollout)}
}

// PlannedRolloutFromAPI is a channel's rollout as planned now (GetReleaseChannel):
// the summary and rows of res; members are added by the caller, a page
// at a time (RolloutMemberFromRow).
func PlannedRolloutFromAPI(ch *cloudv1alpha1.ReleaseChannel, res rollout.Result) *cloudv1.Rollout {
	out := RolloutFromAPI(ch)
	out.RolloutTier, out.Summary = TierFromAPI(res.Tier), RolloutSummaryFromAPI(res.Rollout)
	out.Plan = RolloutPlanFromResult(res)
	return out
}

// RolloutMemberFromRow is one cluster of a planned rollout.
func RolloutMemberFromRow(r rollout.Row) *cloudv1.RolloutMember {
	out := &cloudv1.RolloutMember{Tier: RolloutTierFromAPI(r.Tier), TenantId: r.Tenant, ClusterId: r.ID, ClusterName: r.Name,
		CurrentVersion: r.Current, TargetVersion: r.Target, State: rolloutStates[r.State]}
	if !r.Since.IsZero() {
		out.Since = timestamppb.New(r.Since)
	}
	if r.Gate != nil {
		out.Gate, out.Reason = string(r.Gate.Code), r.Gate.Message
	}
	return out
}

func conditionStatus(s metav1.ConditionStatus) cloudv1.ConditionStatus {
	switch s {
	case metav1.ConditionTrue:
		return cloudv1.ConditionStatus_CONDITION_STATUS_TRUE
	case metav1.ConditionFalse:
		return cloudv1.ConditionStatus_CONDITION_STATUS_FALSE
	case metav1.ConditionUnknown:
		return cloudv1.ConditionStatus_CONDITION_STATUS_UNKNOWN
	default:
		return cloudv1.ConditionStatus_CONDITION_STATUS_UNSPECIFIED
	}
}
