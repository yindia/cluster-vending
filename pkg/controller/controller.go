// Package controller holds the controllers of the platform's kinds
// (cloud.cluster-vending.io: CloudAccount owns Network owns Cluster).
//
// A controller here has no reconcile logic of its own. The objects are
// the rows of the store on these kinds (pkg/database/cloudstore), and
// what moves a row towards its spec already exists (pkg/reconcile/account,
// network, cluster). A controller turns a watch event into a call of
// that reconciler:
//
//   - it maps the object to its tenant and row ID (the store's labels);
//   - an object without them was created without the API: it is adopted
//     (given the row the API's create would have stored) or, where that
//     is not possible, marked with a condition that says why;
//   - an edit of the spec bumps the row's own generation, so the
//     reconciler applies it;
//   - a deletion is refused while the object has dependents (a network of
//     an account, a cluster of a network: never a cascade), else the row
//     is marked DELETING and the reconciler deletes the cloud resources;
//     the object goes when the reconciler removes the row;
//   - while the row is not settled the object is reconciled again after a
//     delay, and a change of one of its provider objects wakes it at once.
package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/fault"
	platform "github.com/yindia/cluster-vending/pkg/reconcile"
)

// How long until an object is reconciled again when nothing wakes it.
var (
	// soon: right after an adoption, to apply what was just made a row.
	soon = time.Second
	// applyingDelay: the row's generation is not applied in full yet (it
	// is staged: something waits for another resource).
	applyingDelay = 5 * time.Second
	// pendingDelay: applied, the cloud is working on it.
	pendingDelay = 20 * time.Second
	// observingDelay: an import that is only observed until it is taken
	// over.
	observingDelay = time.Minute
	// failedDelay: the last pass failed, or the object waits for a person
	// (an adoption that was refused, a deletion blocked by dependents).
	failedDelay = 30 * time.Second
	// upgradeDelay is how soon a cluster with an upgrade in progress is
	// looked at again: each step is read as AWS and Helm report it.
	upgradeDelay = 30 * time.Second
	// resyncDelay: settled. The safety net under the watches.
	resyncDelay = 10 * time.Minute
)

// Condition types the controllers set themselves; the others (Ready,
// Synced, Reconciled) are the reconcilers', written through the store.
const (
	// ConditionAdopted is False on an object that was created without the
	// API and could not be made a row, with the reason.
	ConditionAdopted = "Adopted"
	// ConditionDeletionBlocked is True on an object whose deletion was
	// asked for while it still has dependents.
	ConditionDeletionBlocked = "DeletionBlocked"
)

// Reasons of those conditions.
const (
	reasonNotThroughAPI = "NotCreatedThroughAPI"
	reasonInvalid       = "Invalid"
	reasonWaiting       = "WaitingForOwner"
	reasonDependents    = "HasDependents"
)

// Deps are what the controllers work with.
type Deps struct {
	// Deps is what the reconcilers work with. Its Store is the store on
	// the platform's kinds (cloudstore).
	platform.Deps
	// Direct reads and writes the platform's kinds at the API server, not
	// through the informer cache: an object is read right after it was
	// written.
	Direct client.Client
	// Workers is how many accounts, networks and clusters of each kind are
	// reconciled at once (objectOptions); 0 is one.
	Workers int
	// AutomaticUpgrades: the rollout controller upgrades automatic
	// clusters; off, it plans and reports the rollout only.
	AutomaticUpgrades bool
	// UpgradeTimeout is how long an automatic upgrade may take before the
	// rollout counts it as failed; 0 is config.DefaultUpgradeTimeout.
	UpgradeTimeout time.Duration
}

func (d Deps) logger() *slog.Logger {
	if d.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return d.Logger
}

// SetupAll registers the controllers with mgr: the three of the rows
// (accounts, networks, clusters) and those of the release objects.
func SetupAll(mgr ctrl.Manager, d Deps) error {
	setups := []struct {
		name  string
		setup func(ctrl.Manager) error
	}{
		{"cloudaccount", NewCloudAccountReconciler(d).SetupWithManager},
		{"network", NewNetworkReconciler(d).SetupWithManager},
		{"cluster", NewClusterReconciler(d).SetupWithManager},
	}
	for _, s := range setups {
		if err := s.setup(mgr); err != nil {
			return fmt.Errorf("set up the %s controller: %w", s.name, err)
		}
	}
	if err := setupReleases(mgr, d); err != nil {
		return fmt.Errorf("set up the release controllers: %w", err)
	}
	if err := NewRolloutReconciler(d).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("set up the rollout controller: %w", err)
	}
	if err := setupComponentDefinitions(mgr); err != nil {
		return fmt.Errorf("set up the component definition controller: %w", err)
	}
	scans, err := NewDeprecationReconciler(d)
	if err != nil {
		return fmt.Errorf("set up the deprecation scans: %w", err)
	}
	if err := scans.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("set up the deprecation scans: %w", err)
	}
	if !d.NoRecommendations {
		if err := NewRecommendationReconciler(d).SetupWithManager(mgr); err != nil {
			return fmt.Errorf("set up the recommendations: %w", err)
		}
	}
	return nil
}

// rowChanged lets through the events a controller must act on: a change
// of the spec (metadata.generation), of the labels or annotations (the
// store keeps a row's own state there: a delete or an update through the
// API changes only those), and a deletion. A change of the status alone
// is the controller's own write, or the reconciler's: acting on it would
// reconcile for ever.
func rowChanged() predicate.Predicate {
	return predicate.Or(predicate.GenerationChangedPredicate{}, predicate.LabelChangedPredicate{},
		predicate.AnnotationChangedPredicate{}, predicate.Funcs{
			UpdateFunc: func(e event.UpdateEvent) bool {
				return e.ObjectOld.GetDeletionTimestamp().IsZero() != e.ObjectNew.GetDeletionTimestamp().IsZero()
			},
		})
}

// forObject is the For option of every controller.
func forObject() builder.ForOption { return builder.WithPredicates(rowChanged()) }

// owners remembers which object is the row with an ID, so an event of a
// provider object (labelled with the row ID) finds its owner without a
// read.
type owners struct{ byID sync.Map }

func (o *owners) remember(id string, obj client.Object) {
	if id != "" {
		o.byID.Store(id, client.ObjectKeyFromObject(obj))
	}
}

func (o *owners) forget(id string) { o.byID.Delete(id) }

// find is the object of the row id in namespace: remembered, or looked up
// in list (an object the controller has not reconciled yet).
func (o *owners) find(ctx context.Context, c client.Reader, list client.ObjectList, namespace, id string) (types.NamespacedName, bool) {
	if key, ok := o.byID.Load(id); ok {
		return key.(types.NamespacedName), true
	}
	if c == nil {
		return types.NamespacedName{}, false
	}
	err := c.List(ctx, list, client.InNamespace(namespace), client.MatchingLabels{cloudstore.LabelID: id})
	if err != nil || meta.LenList(list) == 0 {
		return types.NamespacedName{}, false
	}
	items, err := meta.ExtractList(list)
	if err != nil {
		return types.NamespacedName{}, false
	}
	obj, ok := items[0].(client.Object)
	if !ok {
		return types.NamespacedName{}, false
	}
	o.remember(id, obj)
	return client.ObjectKeyFromObject(obj), true
}

// providerWatches makes b also watch every kind the platform renders
// (crossplaneaws.ManagedObjects, as the manager's cache holds them:
// unstructured) and wake the object that owner maps a changed provider
// object to (watchOwned).
func providerWatches(b *builder.Builder, mgr ctrl.Manager, owner handler.MapFunc) (*builder.Builder, error) {
	for _, obj := range crossplaneaws.ManagedObjects() {
		gvk, err := apiutil.GVKForObject(obj, mgr.GetScheme())
		if err != nil {
			return nil, err
		}
		b = watchOwned(b, gvk, owner)
	}
	return b, nil
}

// watchOwned makes b watch the objects of gvk (unstructured) that an owner
// recorded: a change its pass can see (providerObjectChanged) wakes the
// owner after batchWindow, with the other changes of its objects in that
// window (batchedOwner).
func watchOwned(b *builder.Builder, gvk schema.GroupVersionKind, owner handler.MapFunc) *builder.Builder {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	return b.Watches(u, batchedOwner(owner, batchWindow), builder.WithPredicates(providerObjectChanged()))
}

// ownerRequests is the MapFunc of providerWatches: the request for the
// object whose row ID is the provider object's label.
func ownerRequests(o *owners, c client.Reader, label string, list func() client.ObjectList) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		id := obj.GetLabels()[label]
		if id == "" {
			return nil
		}
		key, ok := o.find(ctx, c, list(), obj.GetNamespace(), id)
		if !ok {
			return nil
		}
		return []reconcile.Request{{NamespacedName: key}}
	}
}

// setCondition writes one condition of obj's status (conds: its
// status.conditions), unless it says that already.
func setCondition(ctx context.Context, c client.Client, obj client.Object, conds *[]metav1.Condition, cond metav1.Condition) error {
	cond.ObservedGeneration = obj.GetGeneration()
	if !meta.SetStatusCondition(conds, cond) {
		return nil
	}
	return c.Status().Update(ctx, obj)
}

// flag marks obj as not adopted, with why, and asks for another look
// later: someone has to change the object, or what it waits for has to
// appear.
func flag(ctx context.Context, d Deps, obj client.Object, conds *[]metav1.Condition, reason, message string) (ctrl.Result, error) {
	err := setCondition(ctx, d.Direct, obj, conds, metav1.Condition{Type: ConditionAdopted, Status: metav1.ConditionFalse,
		Reason: reason, Message: message})
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	d.logger().InfoContext(ctx, "object not adopted", "kind", fmt.Sprintf("%T", obj), "namespace", obj.GetNamespace(),
		"name", obj.GetName(), "reason", reason, "message", message)
	return ctrl.Result{RequeueAfter: failedDelay}, nil
}

// blocked marks obj's deletion as blocked by its dependents.
func blocked(ctx context.Context, d Deps, obj client.Object, conds *[]metav1.Condition, message string) (ctrl.Result, error) {
	err := setCondition(ctx, d.Direct, obj, conds, metav1.Condition{Type: ConditionDeletionBlocked, Status: metav1.ConditionTrue,
		Reason: reasonDependents, Message: message})
	return ctrl.Result{RequeueAfter: failedDelay}, client.IgnoreNotFound(err)
}

// refusal is what a refused row construction says to the person who
// created the object: the message the API would have answered with.
func refusal(err error) (reason, message string) {
	reason = reasonInvalid
	if fault.Is(err, fault.Unavailable) || fault.Is(err, fault.Internal) {
		reason = reasonWaiting
	}
	return reason, describe(err)
}

// describe is what a condition says of err: a fault's message for the
// caller, a storage error as it is, and nothing of any other error (its
// text may hold details of the management cluster).
func describe(err error) string {
	if f, ok := fault.As(err); ok {
		return f.Message
	}
	for _, known := range []error{repository.ErrNotFound, repository.ErrAlreadyExists, repository.ErrConflict,
		repository.ErrInUse, repository.ErrInvalid} {
		if errors.Is(err, known) {
			return err.Error()
		}
	}
	return "internal error (see the controller's log)"
}

// ensureFinalizer adds the kind's finalizer to obj, so its deletion waits
// for the reconciler.
func ensureFinalizer(ctx context.Context, c client.Client, obj client.Object, finalizer string) error {
	if !obj.GetDeletionTimestamp().IsZero() || !controllerutil.AddFinalizer(obj, finalizer) {
		return nil
	}
	return c.Update(ctx, obj)
}

// edited reports whether obj's spec was changed since the store last
// wrote its status: the store sets status.observedGeneration on every
// status write, and writes the status with every row it writes.
func edited(generation, observed int64) bool { return observed != 0 && generation > observed }

// next decides when a row is looked at again, from what the pass did and
// the row as it is now (nil: gone).
func next(r *model.Resource, pending []string, imported string, err error) ctrl.Result {
	res := nextOf(r, pending, imported, err)
	if r == nil {
		return res
	}
	return untilPauseEnds(r.Paused, res, time.Now())
}

// untilPauseEnds looks at a paused row again when a part of its pause
// ends by itself (its until), if that is sooner than res.
func untilPauseEnds(p *model.Pause, res ctrl.Result, now time.Time) ctrl.Result {
	end, ok := p.NextEnd()
	if !ok {
		return res
	}
	wait := max(end.Sub(now), time.Second)
	if res.RequeueAfter == 0 || wait < res.RequeueAfter {
		res.RequeueAfter = wait
	}
	return res
}

func nextOf(r *model.Resource, pending []string, imported string, err error) ctrl.Result {
	switch {
	case err != nil:
		return ctrl.Result{RequeueAfter: failedDelay}
	case r == nil:
		return ctrl.Result{}
	case r.State == imported:
		return ctrl.Result{RequeueAfter: observingDelay}
	case r.AppliedGeneration < r.Generation:
		return ctrl.Result{RequeueAfter: applyingDelay}
	}
	for _, state := range pending {
		if r.State == state {
			return ctrl.Result{RequeueAfter: pendingDelay}
		}
	}
	return ctrl.Result{RequeueAfter: resyncDelay}
}
