package controller

import (
	"context"
	"fmt"
	"log/slog"
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
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	platform "github.com/yindia/cluster-vending/pkg/reconcile"
	"github.com/yindia/cluster-vending/pkg/rollout"
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
// ReleaseChannel (the tier it is at, the per-tier progress, the clusters that
// start now and those kept back with why) and on each member cluster
// (status.rollout). With automatic upgrades on it also starts the plan's
// hops (rollout_apply.go); off, it never changes a cluster's version.
type RolloutReconciler struct {
	c   client.Client
	now func() time.Time
	// automatic is the controller's automatic-upgrades setting; timeout
	// its upgrade-timeout (0: the default).
	automatic bool
	timeout   time.Duration
	events    platform.Events
	// store writes clusters: their version (etag-guarded) and their
	// rollout record. Nil: nothing is written to clusters.
	store repository.Store
	log   *slog.Logger
}

// NewRolloutReconciler returns the rollout controller on d's manager
// client (SetupWithManager sets it).
func NewRolloutReconciler(d Deps) *RolloutReconciler {
	return &RolloutReconciler{automatic: d.AutomaticUpgrades, timeout: d.UpgradeTimeout, events: d.Events,
		store: d.Store, log: d.Logger}
}

func (r *RolloutReconciler) logger() *slog.Logger {
	if r.log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return r.log
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
		rollout.SpecVersion(c), c.Annotations[cloudstore.AnnotationPaused], c.DeletionTimestamp,
		c.Status.Phase, c.Status.Conditions, c.Status.CurrentKubernetesVersion, c.Status.Upgrade,
		c.Status.Deprecations, c.Status.PlatformComponents, recordResult(c)}
}

// recordResult is the result of the cluster's rollout record: a retried
// upgrade (failed to in progress again) is planned at once. The rollout's
// own writes of it are transition-only, so this wakes it once each.
func recordResult(c *cloudv1alpha1.Cluster) cloudv1alpha1.ClusterRolloutResult {
	if c.Status.Rollout == nil {
		return ""
	}
	return c.Status.Rollout.Result
}

// Reconcile plans the channel's rollout now and writes it to its status.
func (r *RolloutReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ch := &cloudv1alpha1.ReleaseChannel{}
	if err := r.c.Get(ctx, req.NamespacedName, ch); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	now := r.clock()
	plan, in, err := r.plan(ctx, ch, now)
	if err != nil {
		return ctrl.Result{}, err
	}
	status := *ch.Status.DeepCopy()
	status.RolloutTier, status.Tiers, status.Rollout = plan.Tier, plan.Tiers, plan.Rollout
	meta.SetStatusCondition(&status.Conditions, plan.condition(ch.Generation))
	if !equality.Semantic.DeepEqual(status, ch.Status) {
		was := ch.Status
		ch.Status = status
		if err := r.c.Status().Update(ctx, ch); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		r.report(ctx, ch.Name, was, status)
	}
	started := r.startHops(ctx, plan.Result, now)
	r.recordPlans(ctx, plan.Result, in, started, now)
	return ctrl.Result{RequeueAfter: plan.requeue(now)}, nil
}

// rolloutPlan is one pass's result (rollout.Compute).
type rolloutPlan struct{ rollout.Result }

func (p rolloutPlan) condition(generation int64) metav1.Condition {
	c := metav1.Condition{Type: ConditionRolloutPlanned, Status: metav1.ConditionTrue, Reason: "Planned",
		Message: "the rollout is planned", ObservedGeneration: generation}
	if p.Problem != "" {
		c.Status, c.Reason, c.Message = metav1.ConditionFalse, "InvalidPolicy", p.Problem
	}
	return c
}

func (p rolloutPlan) requeue(now time.Time) time.Duration {
	if p.Wake.IsZero() || !p.Wake.After(now) {
		return rolloutRecheck
	}
	return min(p.Wake.Sub(now)+time.Second, rolloutRecheck)
}

// plan reads the channel's release, clusters and holds, ends the
// upgrades it started that finished, and plans the rollout at now
// (rollout.Compute).
func (r *RolloutReconciler) plan(ctx context.Context, ch *cloudv1alpha1.ReleaseChannel, now time.Time) (rolloutPlan, rollout.Input, error) {
	in, err := rollout.Read(ctx, r.c, ch)
	if err != nil {
		return rolloutPlan{}, in, err
	}
	in.Now, in.Automatic = now, r.automatic
	r.finishUpgrades(ctx, ch.Name, &in, now)
	return rolloutPlan{rollout.Compute(in)}, in, nil
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
	case is.RolloutTier != "" && is.RolloutTier != was.RolloutTier:
		r.events.Event(ctx, obj, "Normal", platform.ReasonRolloutTier,
			fmt.Sprintf("rolling out Kubernetes %s to tier %s", is.Rollout.TargetMinor, strings.TrimPrefix(string(is.RolloutTier), "Tier")))
	}
}

func (r *RolloutReconciler) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}
