// Package rollout plans the rollout of a release channel's Kubernetes
// minor from the objects it reads: the ReleaseChannel, the release it
// offers, the clusters and the release holds. It reads nothing itself, so
// the controller (which writes the plan to the channel's status) and the
// API server (which shows it, cluster by cluster) plan the same way.
//
// The rules are pkg/upgrade's: one minor at a time toward the channel's,
// never down, tier 2 then 1 then 0, in batches, with gates per cluster.
package rollout

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/convert"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	"github.com/yindia/cluster-vending/pkg/maintenance"
	"github.com/yindia/cluster-vending/pkg/reconcile"
	"github.com/yindia/cluster-vending/pkg/release"
	"github.com/yindia/cluster-vending/pkg/upgrade"
)

// GateNoHop is the gate of a cluster whose next minor the release does
// not offer.
const GateNoHop upgrade.GateCode = "NO_HOP"

// Input is what one plan reads.
type Input struct {
	Channel *cloudv1alpha1.ReleaseChannel
	// Release is the release the channel offers (status.release); nil when
	// it offers none or the release is gone.
	Release  *cloudv1alpha1.Release
	Clusters []cloudv1alpha1.Cluster
	Holds    []cloudv1alpha1.ReleaseHold
	Now      time.Time
	// Automatic is the controller's automatic-upgrades setting; off, the
	// plan is reported only.
	Automatic bool
}

// Row is one cluster of the channel's rollout.
type Row struct {
	// ID is the cluster's API ID; Namespace/Name its object; Tenant its
	// tenant.
	ID, Tenant, Namespace, Name string
	Tier                        upgrade.Tier
	State                       upgrade.State
	// Current is the version it runs; Target the minor it moves to next
	// (its hop), or the one it runs once there.
	Current, Target string
	// Since is when it entered its state (an upgrade's start, when the
	// last one ended); zero when unknown.
	Since time.Time
	// Gate is why it is blocked; nil when it is not.
	Gate *upgrade.Gate
	// Phase is the step its Kubernetes upgrade is at (status.upgrade);
	// empty when none runs.
	Phase string
}

// Key is the row's namespace/name, as the channel's status names clusters.
func (r Row) Key() string { return r.Namespace + "/" + r.Name }

// Result is a plan: the status fields of the channel and the rows.
type Result struct {
	Tier    cloudv1alpha1.ClusterTier
	Tiers   []cloudv1alpha1.TierStatus
	Rollout *cloudv1alpha1.ChannelRollout
	// Problem is why the rollout cannot be planned (the policy cannot be
	// read); "" when it can.
	Problem string
	// Wake is when the plan may change by time alone (a soak ends, a
	// window opens); zero for none.
	Wake time.Time
	// Rows are the channel's clusters in the rollout, tier 2 first, then
	// by namespace/name.
	Rows []Row
	// Channel is the channel's name; Start the namespace/name of every
	// cluster to start now (WouldStart, uncapped).
	Channel string
	Start   []string
	// Plan lays the decision out per tier and batch (upgrade.Project).
	Plan upgrade.Projection
	// waiting is why the plan starts nothing (more), before a report-only
	// plan's wording replaced it.
	waiting string
}

// Compute plans the channel's rollout at in.Now.
func Compute(in Input) Result {
	ch := in.Channel
	out := Result{Rollout: &cloudv1alpha1.ChannelRollout{ReportOnly: !in.Automatic}, Channel: ch.Name}
	switch {
	case ch.Status.Release == "":
		out.Rollout.Waiting = "the channel offers no release yet"
		return out
	case in.Release == nil:
		out.Rollout.Waiting = fmt.Sprintf("release %s is gone", ch.Status.Release)
		return out
	}
	content := reconcile.ReleaseOf(in.Release.Name, in.Release.Spec.Content)
	target, ok := release.ChannelMinor(content, ch.Name)
	if !ok {
		out.Rollout.Waiting = fmt.Sprintf("release %s puts no Kubernetes minor on channel %s", in.Release.Name, ch.Name)
		return out
	}
	out.Rollout.TargetMinor = target
	policy, problem := Policy(ch.Spec.Policy)
	if problem != "" {
		out.Problem, out.Rollout.Waiting = problem, problem
		return out
	}
	m := memberInputs{channel: ch.Name, target: target, offered: OfferedMinors(content), holds: in.Holds, now: in.Now}
	var members []upgrade.Member
	var blocked []cloudv1alpha1.BlockedCluster
	for i := range in.Clusters {
		row, wake, ok := m.row(&in.Clusters[i])
		if !ok {
			continue
		}
		out.Rows = append(out.Rows, row)
		members = append(members, upgrade.Member{ID: row.Key(), Tier: row.Tier, State: row.State, FinishedAt: finishedOf(row)})
		if row.Gate != nil {
			blocked = append(blocked, cloudv1alpha1.BlockedCluster{Cluster: row.Key(), Gate: string(row.Gate.Code), Message: row.Gate.Message})
		}
		out.Wake = Earliest(out.Wake, wake)
	}
	sortRows(out.Rows)
	d := upgrade.Plan(policy, in.Now, members)
	out.Wake = Earliest(out.Wake, d.SoakUntil)
	if ch.Spec.Paused && !d.Halted && len(d.Start) > 0 {
		d.Start, d.Waiting = nil, "the channel is paused (spec.paused): no cluster starts"
	}
	d = notDoneWhileBelow(d, members, blocked)
	out.Plan = upgrade.Project(policy, d, members)
	out.fill(d, members, blocked)
	return out
}

// finishedOf is when a finished row's upgrade ended, for soaks.
func finishedOf(r Row) time.Time {
	if r.State == upgrade.Succeeded || r.State == upgrade.Failed {
		return r.Since
	}
	return time.Time{}
}

// sortRows orders rows tier 2 first, then by namespace/name.
func sortRows(rows []Row) {
	for i := 1; i < len(rows); i++ {
		for j := i; j > 0 && less(rows[j], rows[j-1]); j-- {
			rows[j], rows[j-1] = rows[j-1], rows[j]
		}
	}
}

func less(a, b Row) bool {
	if a.Tier != b.Tier {
		return a.Tier > b.Tier
	}
	return a.Key() < b.Key()
}

// fill writes the decision into the result's status fields.
func (p *Result) fill(d upgrade.Decision, members []upgrade.Member, blocked []cloudv1alpha1.BlockedCluster) {
	if len(members) == 0 {
		p.Rollout.Done = true
		return
	}
	if !d.Done {
		// A done rollout is at no tier (status.rolloutTier unset).
		p.Tier = ClusterTier(d.Tier)
	}
	p.Rollout.Waiting, p.Rollout.Halted, p.Rollout.Done = d.Waiting, d.Halted, d.Done
	p.Rollout.WouldStart = capped(d.Start)
	p.Start, p.waiting = d.Start, d.Waiting
	if len(d.Start) > 0 && p.Rollout.ReportOnly {
		p.Rollout.Waiting = fmt.Sprintf("would start %s (automatic upgrades are off)", clusterCount(len(d.Start)))
	}
	p.Rollout.Blocked = blocked[:min(len(blocked), cloudv1alpha1.MaxRolloutClusters)]
	heldPerTier := [3]int32{}
	for _, b := range blocked {
		if b.Gate == string(upgrade.GateFrozen) {
			for _, m := range members {
				if m.ID == b.Cluster {
					heldPerTier[m.Tier]++
				}
			}
		}
	}
	for _, tier := range []upgrade.Tier{upgrade.Tier2, upgrade.Tier1, upgrade.Tier0} {
		c := d.Tiers[tier]
		total := c.Pending + c.InProgress + c.Succeeded + c.Failed + c.Blocked
		if total == 0 {
			continue
		}
		name := ClusterTier(tier)
		p.Rollout.Members = append(p.Rollout.Members, cloudv1alpha1.RolloutTierCount{Tier: name,
			Pending: Count32(c.Pending), InProgress: Count32(c.InProgress), Succeeded: Count32(c.Succeeded),
			Failed: Count32(c.Failed), Blocked: Count32(c.Blocked)})
		p.Tiers = append(p.Tiers, cloudv1alpha1.TierStatus{Tier: name, Total: Count32(total), Done: Count32(c.Succeeded),
			InProgress: Count32(c.InProgress), Failed: Count32(c.Failed), Held: heldPerTier[tier]})
	}
}

// memberInputs are what deciding one cluster's place in the rollout
// needs.
type memberInputs struct {
	channel, target string
	offered         []string
	// holds are the release holds: the freezes (HoldOf).
	holds []cloudv1alpha1.ReleaseHold
	now   time.Time
}

// row is the cluster's place in the channel's rollout: false when it is
// not part of it (another channel, being deleted, no version yet, or
// ahead of the channel). wake is when its window opens, when that is what
// blocks it.
func (in memberInputs) row(c *cloudv1alpha1.Cluster) (r Row, wake time.Time, ok bool) {
	if !strings.EqualFold(string(c.Spec.ReleaseChannel), in.channel) || !c.DeletionTimestamp.IsZero() {
		return r, wake, false
	}
	current := c.Status.CurrentKubernetesVersion
	if current == "" {
		current = SpecVersion(c)
	}
	cmp, comparable := CompareMinors(current, in.target)
	if !comparable || cmp > 0 {
		return r, wake, false
	}
	r = Row{ID: c.Labels[cloudstore.LabelID], Tenant: tenantOf(c), Namespace: c.Namespace, Name: c.Name,
		Tier: Tier(c.Status.Tier), Current: current, Target: in.target}
	if u := c.Status.Upgrade; u != nil {
		r.Phase = string(u.Phase)
	}
	if recorded, ok := fromRecord(c, in.channel, r, cmp); ok {
		return recorded, wake, true
	}
	upgrading := upgrading(c)
	switch {
	case cmp == 0 && !upgrading:
		r.State, r.Since = upgrade.Succeeded, FinishedAt(c)
		return r, wake, true
	// No upgrade reports a failure of its own (status.upgrade has no
	// failed step): a cluster whose phase is Failed while an upgrade is
	// in progress counts as a failed upgrade.
	case upgrading && c.Status.Phase == cloudv1alpha1.ClusterPhaseFailed:
		r.State, r.Since, r.Target = upgrade.Failed, FinishedAt(c), upgradeTarget(c, in.target)
		return r, wake, true
	case upgrading:
		r.State, r.Target = upgrade.InProgress, upgradeTarget(c, in.target)
		if st := c.Status.Upgrade.StartTime; st != nil {
			r.Since = st.Time
		}
		return r, wake, true
	}
	next, hop, why := upgrade.NextHop(current, in.target, in.offered)
	if !hop {
		r.State, r.Gate = upgrade.Blocked, &upgrade.Gate{Code: GateNoHop, Blocking: true, Message: why}
		return r, wake, true
	}
	r.Target = next
	window := convert.ClusterSpecFromAPI(c.Spec, "", nil).GetMaintenanceWindow()
	facts := in.facts(c, next)
	facts.WindowOpen, facts.NextOpen = maintenance.Open(window, in.now), maintenance.Next(window, in.now)
	for _, g := range upgrade.Check(facts) {
		if !g.Blocking {
			continue
		}
		r.State, r.Gate = upgrade.Blocked, &g
		if g.Code == upgrade.GateWindow {
			wake = facts.NextOpen
		}
		return r, wake, true
	}
	r.State = upgrade.Pending
	return r, wake, true
}

// upgradeTarget is the minor an upgrade in progress moves to: what it
// reports, else the channel's.
func upgradeTarget(c *cloudv1alpha1.Cluster, target string) string {
	if c.Status.Upgrade != nil && c.Status.Upgrade.ToVersion != "" {
		return c.Status.Upgrade.ToVersion
	}
	return target
}

// tenantOf is the cluster's tenant: its label, else its namespace.
func tenantOf(c *cloudv1alpha1.Cluster) string {
	if t := c.Labels[cloudstore.LabelTenant]; t != "" {
		return t
	}
	return c.Namespace
}

// SpecVersion is the Kubernetes version the cluster's spec asks for.
func SpecVersion(c *cloudv1alpha1.Cluster) string {
	if c.Spec.ControlPlane == nil {
		return ""
	}
	return c.Spec.ControlPlane.KubernetesVersion
}

// facts are what upgrade.Check reads of a cluster moving to target.
func (in memberInputs) facts(c *cloudv1alpha1.Cluster, target string) upgrade.Facts {
	f := upgrade.Facts{Mode: upgrade.Mode(c.Spec.UpgradeMode), Target: target,
		Ready:         meta.IsStatusConditionTrue(c.Status.Conditions, "Ready") || c.Status.Phase == cloudv1alpha1.ClusterPhaseRunning,
		PlatformReady: platformReady(c),
	}
	if h := HoldOf(in.holds, c, in.now); h != nil {
		f.Frozen, f.FrozenMessage = true, FreezeMessage(h)
	}
	if dep := c.Status.Deprecations; dep != nil {
		f.DeprecationsScanned = dep.ScannedAt != nil
		f.DeprecationsTarget, f.RemovedInUse = dep.TargetVersion, int(dep.Removed)
	}
	return f
}

// platformReady: every platform component the cluster lists is Ready or
// being removed (Deleting: its definition was switched off, which must
// not hold an upgrade back); without a list, the PlatformComponentsReady
// condition is True (or missing).
func platformReady(c *cloudv1alpha1.Cluster) bool {
	if len(c.Status.PlatformComponents) > 0 {
		for _, pc := range c.Status.PlatformComponents {
			if pc.Phase != "Ready" && pc.Phase != "Deleting" {
				return false
			}
		}
		return true
	}
	if cond := meta.FindStatusCondition(c.Status.Conditions, cloudv1alpha1.ConditionPlatformComponentsReady); cond != nil {
		return cond.Status == metav1.ConditionTrue
	}
	return true
}

// FinishedAt approximates when the cluster's last upgrade ended: no field
// records it, so it is when its Ready condition last changed (an upgrade
// ends with the cluster Ready again), else when the cluster was created.
// Soaks are counted from it.
func FinishedAt(c *cloudv1alpha1.Cluster) time.Time {
	if cond := meta.FindStatusCondition(c.Status.Conditions, "Ready"); cond != nil {
		return cond.LastTransitionTime.Time
	}
	return c.CreationTimestamp.Time
}

// HoldOf is the first unexpired freeze that matches c at now (Matches):
// no automatic change of it starts. Nil when none does.
func HoldOf(holds []cloudv1alpha1.ReleaseHold, c *cloudv1alpha1.Cluster, now time.Time) *cloudv1alpha1.ReleaseHold {
	for i := range holds {
		if holds[i].Spec.Platform {
			target := reconcile.FreezeTarget{TenantID: c.Labels[cloudstore.LabelTenant], ClusterID: c.Labels[cloudstore.LabelID], NetworkID: c.Labels["cluster-vending.io/network"]}
			if frozen := reconcile.FreezeForTarget(&holds[i], now, target); frozen.Holds() {
				current := holds[i].DeepCopy()
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
			continue
		}
		if Active(holds[i], now) && Matches(holds[i].Spec, c) {
			return &holds[i]
		}
	}
	return nil
}

// Matches reports whether a hold covers c: the platform freeze, or c is
// the cluster it names. A cluster the freeze lets through (Except) is not
// covered.
func Matches(h cloudv1alpha1.ReleaseHoldSpec, c *cloudv1alpha1.Cluster) bool {
	if slices.Contains(h.Except, c.Labels[cloudstore.LabelID]) {
		return false
	}
	switch {
	case h.Platform:
		return reconcile.FreezeForTarget(&cloudv1alpha1.ReleaseHold{Spec: h}, time.Now(), reconcile.FreezeTarget{TenantID: c.Labels[cloudstore.LabelTenant], ClusterID: c.Labels[cloudstore.LabelID], NetworkID: c.Labels["cluster-vending.io/network"]}).Holds()
	case h.ClusterID != "":
		return c.Labels[cloudstore.LabelID] == h.ClusterID
	}
	return false
}

// HoldTarget names what a hold covers, for people: "the platform",
// "cluster <id>".
func HoldTarget(h cloudv1alpha1.ReleaseHoldSpec) string {
	if h.Platform || h.ClusterID == "" {
		return "the platform"
	}
	return "cluster " + h.ClusterID
}

// Active reports whether the hold holds its cluster at now.
func Active(h cloudv1alpha1.ReleaseHold, now time.Time) bool {
	if h.Spec.Platform {
		return reconcile.AnyFreezeActive(&h, now)
	}
	return h.Spec.Until == nil || h.Spec.Until.After(now)
}

// Policy is the channel's policy for upgrade.Plan; a problem when it
// cannot be read.
func Policy(p cloudv1alpha1.RolloutPolicy) (upgrade.Policy, string) {
	out := upgrade.Policy{MaxInProgress: int(p.MaxInProgress), MaxFailed: int(p.MaxFailed), Soak: p.Soak.Duration}
	if out.MaxInProgress == 0 {
		out.MaxInProgress = 5 // the CRD's default, for objects written without it
	}
	if p.Soak.Duration < 0 {
		return out, fmt.Sprintf("spec.policy.soak %s is negative", p.Soak.Duration)
	}
	for tier, d := range p.SoakPerTier {
		t, ok := tierNames[tier]
		if !ok {
			return out, fmt.Sprintf("spec.policy.soakPerTier names %q: the tiers are Tier0, Tier1 and Tier2", tier)
		}
		if d.Duration < 0 {
			return out, fmt.Sprintf("spec.policy.soakPerTier[%s] %s is negative", tier, d.Duration)
		}
		if out.SoakPerTier == nil {
			out.SoakPerTier = map[upgrade.Tier]time.Duration{}
		}
		out.SoakPerTier[t] = d.Duration
	}
	return out, ""
}

var tierNames = map[cloudv1alpha1.ClusterTier]upgrade.Tier{
	cloudv1alpha1.ClusterTier0: upgrade.Tier0, cloudv1alpha1.ClusterTier1: upgrade.Tier1, cloudv1alpha1.ClusterTier2: upgrade.Tier2,
}

// Tier is a cluster's tier in the rollout; unset is Tier0, the most
// careful.
func Tier(t cloudv1alpha1.ClusterTier) upgrade.Tier {
	if tier, ok := tierNames[t]; ok {
		return tier
	}
	return upgrade.Tier0
}

// ClusterTier is a rollout tier as the kinds name it.
func ClusterTier(t upgrade.Tier) cloudv1alpha1.ClusterTier {
	return cloudv1alpha1.ClusterTier("Tier" + strconv.Itoa(int(t)))
}

// OfferedMinors are the minors a release offers.
func OfferedMinors(r release.Release) []string {
	out := make([]string, 0, len(r.Kubernetes))
	for _, k := range r.Kubernetes {
		out = append(out, k.Minor)
	}
	return out
}

// CompareMinors compares two versions by minor (1.35, 1.35.2, v1.35):
// -1, 0 or 1; false when either is not a version.
func CompareMinors(a, b string) (int, bool) {
	ma, okA := minorOfVersion(a)
	mb, okB := minorOfVersion(b)
	if !okA || !okB {
		return 0, false
	}
	switch {
	case ma < mb:
		return -1, true
	case ma > mb:
		return 1, true
	}
	return 0, true
}

// minorOfVersion is 1.x's x.
func minorOfVersion(v string) (int, bool) {
	parts := strings.Split(strings.TrimPrefix(strings.TrimSpace(v), "v"), ".")
	if len(parts) < 2 || parts[0] != "1" {
		return 0, false
	}
	n, err := strconv.Atoi(parts[1])
	return n, err == nil
}

// Count32 is a count of clusters as a status field holds it.
func Count32(n int) int32 {
	if n > math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(n) //nolint:gosec // bounded just above
}

func clusterCount(n int) string {
	if n == 1 {
		return "1 cluster"
	}
	return strconv.Itoa(n) + " clusters"
}

func capped(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	return ids[:min(len(ids), cloudv1alpha1.MaxRolloutClusters)]
}

// Earliest is the earlier of two times; a zero time is none.
func Earliest(a, b time.Time) time.Time {
	switch {
	case a.IsZero():
		return b
	case b.IsZero() || a.Before(b):
		return a
	}
	return b
}

// FreezeName is the name of the release hold that is the platform freeze.
const FreezeName = reconcile.FreezeName

// FreezeMessage is what a cluster a freeze holds says: "the platform is
// frozen: provider upgrade (until 2026-12-31T00:00:00Z)".
func FreezeMessage(h *cloudv1alpha1.ReleaseHold) string {
	target := HoldTarget(h.Spec)
	if scope := h.Annotations["cloud.cluster-vending.io/effective-freeze-scope"]; scope != "" && scope != "platform" {
		target = scope
	}
	out := target + " is frozen"
	if h.Spec.Reason != "" {
		out += ": " + h.Spec.Reason
	}
	if h.Spec.Until != nil {
		out += " (until " + h.Spec.Until.UTC().Format(time.RFC3339) + ")"
	}
	return out
}

// Freeze is the active platform freeze among holds; nil when the
// platform is not frozen.
func Freeze(holds []cloudv1alpha1.ReleaseHold, now time.Time) *cloudv1alpha1.ReleaseHold {
	for i := range holds {
		if holds[i].Spec.Platform && Active(holds[i], now) {
			return &holds[i]
		}
	}
	return nil
}
