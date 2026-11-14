package controller

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/convert"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/maintenance"
	platform "github.com/yindia/cluster-vending/pkg/reconcile"
	"github.com/yindia/cluster-vending/pkg/release"
	"github.com/yindia/cluster-vending/pkg/upgrade"
)

// rolloutRecheck is the longest a channel's rollout goes without being
// planned again: clusters' maintenance windows and soaks are also
// requeued for exactly, this catches what no event announces.
const rolloutRecheck = 10 * time.Minute

// ConditionRolloutPlanned is a ReleaseChannel's condition that its rollout
// could be planned (False: the policy cannot be read).
const ConditionRolloutPlanned = "RolloutPlanned"

// RolloutReconciler plans the rollout of each channel's Kubernetes minor
// to its automatic clusters (pkg/upgrade) and reports it on the
// ReleaseChannel: the stage, the per-tier progress, the clusters that
// start now and those kept back with why. It never changes a cluster.
type RolloutReconciler struct {
	c   client.Client
	now func() time.Time
	// automatic is the controller's automatic-upgrades setting.
	automatic bool
	events    platform.Events
}

// NewRolloutReconciler returns the rollout controller on d's manager
// client (SetupWithManager sets it).
func NewRolloutReconciler(d Deps) *RolloutReconciler {
	return &RolloutReconciler{automatic: d.AutomaticUpgrades, events: d.Events}
}

// SetupWithManager registers the controller: one worker, since a pass
// lists every cluster of the channel.
func (r *RolloutReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.c == nil {
		r.c = mgr.GetClient()
	}
	toChannel := handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []reconcile.Request {
		cl, ok := obj.(*cloudv1alpha1.Cluster)
		if !ok || cl.Spec.ReleaseChannel == "" {
			return nil
		}
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: strings.ToLower(string(cl.Spec.ReleaseChannel))}}}
	})
	allChannels := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, _ client.Object) []reconcile.Request {
		return enqueueAll(ctx, r.c, &cloudv1alpha1.ReleaseChannelList{})
	})
	return ctrl.NewControllerManagedBy(mgr).Named("rollout").
		For(&cloudv1alpha1.ReleaseChannel{}, builder.WithPredicates(predicate.Or[client.Object](predicate.GenerationChangedPredicate{}, channelReleaseChanged()))).
		Watches(&cloudv1alpha1.Cluster{}, toChannel, builder.WithPredicates(rolloutInputChanged())).
		Watches(&cloudv1alpha1.Release{}, allChannels, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&cloudv1alpha1.ReleaseHold{}, allChannels).
		Complete(r)
}

// channelReleaseChanged also wakes the rollout when the channel starts
// offering another release (a status write, which GenerationChanged
// misses); its own status writes do not.
func channelReleaseChanged() predicate.Predicate {
	return predicate.Funcs{CreateFunc: func(event.CreateEvent) bool { return false },
		DeleteFunc: func(event.DeleteEvent) bool { return false }, GenericFunc: func(event.GenericEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			was, okWas := e.ObjectOld.(*cloudv1alpha1.ReleaseChannel)
			is, okIs := e.ObjectNew.(*cloudv1alpha1.ReleaseChannel)
			return !okWas || !okIs || was.Status.Release != is.Status.Release || was.Status.KubernetesMinor != is.Status.KubernetesMinor
		}}
}

// rolloutInputChanged passes a cluster's update only when something the
// rollout reads of it changed.
func rolloutInputChanged() predicate.Predicate {
	return predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
		was, okWas := e.ObjectOld.(*cloudv1alpha1.Cluster)
		is, okIs := e.ObjectNew.(*cloudv1alpha1.Cluster)
		if !okWas || !okIs {
			return true
		}
		return !equality.Semantic.DeepEqual(rolloutInput(was), rolloutInput(is))
	}}
}

// rolloutInput is what the rollout reads of a cluster.
func rolloutInput(c *cloudv1alpha1.Cluster) []any {
	return []any{c.Spec.ReleaseChannel, c.Spec.Tier, c.Spec.UpgradeMode, c.Spec.MaintenanceWindow,
		specVersion(c), c.Annotations[cloudstore.AnnotationPaused], c.DeletionTimestamp,
		c.Status.Phase, c.Status.Conditions, c.Status.CurrentKubernetesVersion, c.Status.Upgrade,
		c.Status.Deprecations, c.Status.PlatformComponents}
}

// Reconcile plans the channel's rollout now and writes it to its status.
func (r *RolloutReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ch := &cloudv1alpha1.ReleaseChannel{}
	if err := r.c.Get(ctx, req.NamespacedName, ch); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	now := r.clock()
	plan, err := r.plan(ctx, ch, now)
	if err != nil {
		return ctrl.Result{}, err
	}
	status := *ch.Status.DeepCopy()
	status.Stage, status.Tiers, status.Rollout = plan.stage, plan.tiers, plan.rollout
	meta.SetStatusCondition(&status.Conditions, plan.condition(ch.Generation))
	if !equality.Semantic.DeepEqual(status, ch.Status) {
		was := ch.Status
		ch.Status = status
		if err := r.c.Status().Update(ctx, ch); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		r.report(ctx, ch.Name, was, status)
	}
	// increment 6: for each cluster in plan.rollout.WouldStart, when
	// r.automatic, set spec.controlPlane.kubernetesVersion to its hop
	// through the store (an event on the cluster saying "automatic
	// upgrade <from> to <to>, channel <name>"). Irreversible: it stays
	// behind the automatic-upgrades setting and is not built until it was
	// run on a real EKS cluster. Nothing here writes a cluster.
	return ctrl.Result{RequeueAfter: plan.requeue(now)}, nil
}

// rolloutPlan is one pass's result.
type rolloutPlan struct {
	stage   cloudv1alpha1.ClusterTier
	tiers   []cloudv1alpha1.TierStatus
	rollout *cloudv1alpha1.ChannelRollout
	// problem is why the rollout cannot be planned; "" when it can.
	problem string
	// wake is when the plan may change by time alone (a soak ends, a
	// window opens); zero for none.
	wake time.Time
}

func (p rolloutPlan) condition(generation int64) metav1.Condition {
	c := metav1.Condition{Type: ConditionRolloutPlanned, Status: metav1.ConditionTrue, Reason: "Planned",
		Message: "the rollout is planned", ObservedGeneration: generation}
	if p.problem != "" {
		c.Status, c.Reason, c.Message = metav1.ConditionFalse, "InvalidPolicy", p.problem
	}
	return c
}

func (p rolloutPlan) requeue(now time.Time) time.Duration {
	if p.wake.IsZero() || !p.wake.After(now) {
		return rolloutRecheck
	}
	return min(p.wake.Sub(now)+time.Second, rolloutRecheck)
}

// plan reads the channel's release, clusters and holds and plans the
// rollout at now.
func (r *RolloutReconciler) plan(ctx context.Context, ch *cloudv1alpha1.ReleaseChannel, now time.Time) (rolloutPlan, error) {
	out := rolloutPlan{rollout: &cloudv1alpha1.ChannelRollout{ReportOnly: !r.automatic}}
	if ch.Status.Release == "" {
		out.rollout.Waiting = "the channel offers no release yet"
		return out, nil
	}
	rel := &cloudv1alpha1.Release{}
	if err := r.c.Get(ctx, client.ObjectKey{Name: ch.Status.Release}, rel); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return out, fmt.Errorf("read release %s: %w", ch.Status.Release, err)
		}
		out.rollout.Waiting = fmt.Sprintf("release %s is gone", ch.Status.Release)
		return out, nil
	}
	content := releasePins(rel.Name, rel.Spec.Content)
	target, ok := release.ChannelMinor(content, ch.Name)
	if !ok {
		out.rollout.Waiting = fmt.Sprintf("release %s puts no Kubernetes minor on channel %s", rel.Name, ch.Name)
		return out, nil
	}
	out.rollout.TargetMinor = target
	policy, problem := rolloutPolicy(ch.Spec.Policy)
	if problem != "" {
		out.problem, out.rollout.Waiting = problem, problem
		return out, nil
	}
	clusters := &cloudv1alpha1.ClusterList{}
	if err := r.c.List(ctx, clusters); err != nil {
		return out, fmt.Errorf("list clusters: %w", err)
	}
	held, err := r.held(ctx, now)
	if err != nil {
		return out, err
	}
	in := rolloutInputs{channel: ch.Name, target: target, offered: offeredMinors(content), held: held, now: now}
	var members []upgrade.Member
	var blocked []cloudv1alpha1.BlockedCluster
	for i := range clusters.Items {
		m, gate, wake, ok := in.member(&clusters.Items[i])
		if !ok {
			continue
		}
		members = append(members, m)
		if gate != nil {
			blocked = append(blocked, cloudv1alpha1.BlockedCluster{Cluster: m.ID, Gate: string(gate.Code), Message: gate.Message})
		}
		out.wake = earliest(out.wake, wake)
	}
	d := upgrade.Plan(policy, now, members)
	out.wake = earliest(out.wake, d.SoakUntil)
	if ch.Spec.Paused && !d.Halted && len(d.Start) > 0 {
		d.Start, d.Waiting = nil, "the channel is paused (spec.paused): no cluster starts"
	}
	out.fill(d, members, blocked)
	return out, nil
}

// fill writes the decision into the plan's status fields.
func (p *rolloutPlan) fill(d upgrade.Decision, members []upgrade.Member, blocked []cloudv1alpha1.BlockedCluster) {
	if len(members) == 0 {
		p.rollout.Done = true
		return
	}
	p.stage = clusterTier(d.Stage)
	p.rollout.Waiting, p.rollout.Halted, p.rollout.Done = d.Waiting, d.Halted, d.Done
	p.rollout.WouldStart = capped(d.Start)
	if len(d.Start) > 0 && p.rollout.ReportOnly {
		p.rollout.Waiting = fmt.Sprintf("would start %s (automatic upgrades are off)", clusterCount(len(d.Start)))
	}
	p.rollout.Blocked = blocked[:min(len(blocked), cloudv1alpha1.MaxRolloutClusters)]
	heldPerTier := [3]int32{}
	for _, b := range blocked {
		if b.Gate == string(upgrade.GateHeld) {
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
		name := clusterTier(tier)
		p.rollout.Members = append(p.rollout.Members, cloudv1alpha1.RolloutTierCount{Tier: name,
			Pending: count32(c.Pending), InProgress: count32(c.InProgress), Succeeded: count32(c.Succeeded),
			Failed: count32(c.Failed), Blocked: count32(c.Blocked)})
		p.tiers = append(p.tiers, cloudv1alpha1.TierStatus{Tier: name, Total: count32(total), Done: count32(c.Succeeded),
			InProgress: count32(c.InProgress), Failed: count32(c.Failed), Held: heldPerTier[tier]})
	}
}

// rolloutInputs are what deciding one cluster's place in the rollout
// needs.
type rolloutInputs struct {
	channel, target string
	offered         []string
	// held are the IDs of the clusters a ReleaseHold holds now.
	held map[string]bool
	now  time.Time
}

// member is the cluster's place in the channel's rollout: false when it
// is not part of it (another channel, being deleted, no version yet, or
// ahead of the channel). gate is the first reason it is blocked; wake when
// its window opens, when that is what blocks it.
func (in rolloutInputs) member(c *cloudv1alpha1.Cluster) (m upgrade.Member, gate *upgrade.Gate, wake time.Time, ok bool) {
	if !strings.EqualFold(string(c.Spec.ReleaseChannel), in.channel) || !c.DeletionTimestamp.IsZero() {
		return m, nil, wake, false
	}
	current := c.Status.CurrentKubernetesVersion
	if current == "" {
		current = specVersion(c)
	}
	cmp, comparable := compareMinors(current, in.target)
	if !comparable || cmp > 0 {
		return m, nil, wake, false
	}
	m = upgrade.Member{ID: c.Namespace + "/" + c.Name, Tier: rolloutTier(c.Spec.Tier)}
	upgrading := c.Status.Upgrade != nil && c.Status.Upgrade.Phase != ""
	switch {
	case cmp == 0 && !upgrading:
		m.State, m.FinishedAt = upgrade.Succeeded, finishedAt(c)
		return m, nil, wake, true
	// No upgrade reports a failure of its own (status.upgrade has no
	// failed step): a cluster whose phase is Failed while an upgrade is
	// in progress counts as a failed upgrade.
	case upgrading && c.Status.Phase == cloudv1alpha1.ClusterPhaseFailed:
		m.State, m.FinishedAt = upgrade.Failed, finishedAt(c)
		return m, nil, wake, true
	case upgrading:
		m.State = upgrade.InProgress
		return m, nil, wake, true
	}
	next, hop, why := upgrade.NextHop(current, in.target, in.offered)
	if !hop {
		m.State = upgrade.Blocked
		return m, &upgrade.Gate{Code: "NO_HOP", Blocking: true, Message: why}, wake, true
	}
	window := convert.ClusterSpecFromAPI(c.Spec, "", nil).GetMaintenanceWindow()
	facts := in.facts(c, next)
	facts.WindowOpen, facts.NextOpen = maintenance.Open(window, in.now), maintenance.Next(window, in.now)
	for _, g := range upgrade.Check(facts) {
		if !g.Blocking {
			continue
		}
		m.State = upgrade.Blocked
		if g.Code == upgrade.GateWindow {
			wake = facts.NextOpen
		}
		return m, &g, wake, true
	}
	m.State = upgrade.Pending
	return m, nil, wake, true
}

// specVersion is the Kubernetes version the cluster's spec asks for.
func specVersion(c *cloudv1alpha1.Cluster) string {
	if c.Spec.ControlPlane == nil {
		return ""
	}
	return c.Spec.ControlPlane.KubernetesVersion
}

// facts are what upgrade.Check reads of a cluster moving to target.
func (in rolloutInputs) facts(c *cloudv1alpha1.Cluster, target string) upgrade.Facts {
	f := upgrade.Facts{Mode: upgrade.Mode(c.Spec.UpgradeMode), Target: target,
		Ready:         meta.IsStatusConditionTrue(c.Status.Conditions, "Ready") || c.Status.Phase == cloudv1alpha1.ClusterPhaseRunning,
		PlatformReady: platformReady(c),
		Paused:        model.ParsePause(c.Annotations[cloudstore.AnnotationPaused]) != nil,
		Held:          in.held[c.Labels[cloudstore.LabelID]],
	}
	if dep := c.Status.Deprecations; dep != nil {
		f.DeprecationsScanned = dep.ScannedAt != nil
		f.DeprecationsTarget, f.RemovedInUse = dep.TargetVersion, int(dep.Removed)
	}
	return f
}

// platformReady: the PlatformComponentsReady condition is True, or,
// without it, every platform component is Ready.
func platformReady(c *cloudv1alpha1.Cluster) bool {
	if cond := meta.FindStatusCondition(c.Status.Conditions, cloudv1alpha1.ConditionPlatformComponentsReady); cond != nil {
		return cond.Status == metav1.ConditionTrue
	}
	for _, pc := range c.Status.PlatformComponents {
		if pc.Phase != "Ready" {
			return false
		}
	}
	return true
}

// finishedAt approximates when the cluster's last upgrade ended: no field
// records it, so it is when its Ready condition last changed (an upgrade
// ends with the cluster Ready again), else when the cluster was created.
// Soaks are counted from it.
func finishedAt(c *cloudv1alpha1.Cluster) time.Time {
	if cond := meta.FindStatusCondition(c.Status.Conditions, "Ready"); cond != nil {
		return cond.LastTransitionTime.Time
	}
	return c.CreationTimestamp.Time
}

// held are the IDs of the clusters an unexpired ReleaseHold holds at now.
func (r *RolloutReconciler) held(ctx context.Context, now time.Time) (map[string]bool, error) {
	holds := &cloudv1alpha1.ReleaseHoldList{}
	if err := r.c.List(ctx, holds); err != nil {
		return nil, fmt.Errorf("list release holds: %w", err)
	}
	out := map[string]bool{}
	for _, h := range holds.Items {
		if h.Spec.Until == nil || h.Spec.Until.After(now) {
			out[h.Spec.ClusterID] = true
		}
	}
	return out, nil
}

// report records the rollout's transitions as events of the channel.
func (r *RolloutReconciler) report(ctx context.Context, channel string, was, is cloudv1alpha1.ReleaseChannelStatus) {
	if r.events == nil || is.Rollout == nil {
		return
	}
	obj := platform.Object{Kind: platform.KindReleaseChannel, Name: channel}
	halted := was.Rollout != nil && was.Rollout.Halted
	switch {
	case is.Rollout.Halted && !halted:
		r.events.Event(ctx, obj, "Warning", platform.ReasonRolloutHalted, is.Rollout.Waiting)
	case is.Stage != "" && is.Stage != was.Stage:
		r.events.Event(ctx, obj, "Normal", platform.ReasonRolloutStage,
			fmt.Sprintf("the rollout of Kubernetes %s is at %s", is.Rollout.TargetMinor, is.Stage))
	}
}

func (r *RolloutReconciler) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// rolloutPolicy is the channel's policy for upgrade.Plan; a problem when
// it cannot be read.
func rolloutPolicy(p cloudv1alpha1.RolloutPolicy) (upgrade.Policy, string) {
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

// rolloutTier is a cluster's tier in the rollout; unset is Tier0, the
// most careful.
func rolloutTier(t cloudv1alpha1.ClusterTier) upgrade.Tier {
	if tier, ok := tierNames[t]; ok {
		return tier
	}
	return upgrade.Tier0
}

func clusterTier(t upgrade.Tier) cloudv1alpha1.ClusterTier {
	return cloudv1alpha1.ClusterTier("Tier" + strconv.Itoa(int(t)))
}

// offeredMinors are the minors a release offers.
func offeredMinors(r release.Release) []string {
	out := make([]string, 0, len(r.Kubernetes))
	for _, k := range r.Kubernetes {
		out = append(out, k.Minor)
	}
	return out
}

// compareMinors compares two versions by minor (1.35, 1.35.2, v1.35):
// -1, 0 or 1; false when either is not a version.
func compareMinors(a, b string) (int, bool) {
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

// count32 is a count of clusters as a status field holds it.
func count32(n int) int32 {
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

func earliest(a, b time.Time) time.Time {
	switch {
	case a.IsZero():
		return b
	case b.IsZero() || a.Before(b):
		return a
	}
	return b
}
