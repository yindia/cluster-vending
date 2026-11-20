package controller

import (
	"context"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/convert"
	"github.com/yindia/cluster-vending/pkg/crossplane/managed"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/deprecation"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
	"github.com/yindia/cluster-vending/pkg/reconcile/deprecations"
	"github.com/yindia/cluster-vending/pkg/reconcile/resultsink"
	"github.com/yindia/cluster-vending/pkg/results"
)

// notScannableDelay is how long until a cluster that cannot be scanned
// yet (not running, being deleted) is looked at again.
const notScannableDelay = 5 * time.Minute

// defaultChannel is the channel of a cluster that names none.
const defaultChannel = "stable"

// DeprecationReconciler scans running clusters for objects written with
// deprecated or removed API versions and records what it finds in
// status.deprecations (pkg/reconcile/deprecations). It reads the workload
// cluster with the kubeconfig its ClusterAuth wrote and changes nothing
// there. It is apart from the cluster's own controller: a scan that
// fails never fails the cluster.
type DeprecationReconciler struct {
	deps    Deps
	scanner deprecations.Scanner
	// sink writes the full scan to the cluster's results bucket.
	sink *resultsink.Sink
}

// NewDeprecationReconciler returns the scanner of Clusters.
func NewDeprecationReconciler(d Deps) (*DeprecationReconciler, error) {
	rules, err := deprecation.Default()
	if err != nil {
		return nil, err
	}
	return &DeprecationReconciler{deps: d, scanner: deprecations.Scanner{Rules: rules}, sink: resultsink.New(d.Deps, d.logger())}, nil
}

// The kubeconfig of a workload cluster is a Secret its ClusterAuth writes
// in the tenant's namespace. The controller has no Secret rule of its own:
// each tenant namespace binds it to the get-only ClusterRole
// reconcile.KubeconfigReaderRole (the account reconcile applies that
// RoleBinding). To create a binding to a role whose permissions it does
// not hold, it needs the bind verb on exactly that role.
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=create;patch
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles,resourceNames=cluster-vending-kubeconfig-reader;cluster-vending-usage-history-writer,verbs=bind

// SetupWithManager registers the scanner: one cluster at a time, woken by
// a change of what it scans for (scanTriggered) and by its own schedule.
func (r *DeprecationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).Named("cluster-deprecations").
		For(&cloudv1alpha1.Cluster{}, builder.WithPredicates(predicate.Funcs{
			UpdateFunc: func(e event.UpdateEvent) bool {
				o, okOld := e.ObjectOld.(*cloudv1alpha1.Cluster)
				n, okNew := e.ObjectNew.(*cloudv1alpha1.Cluster)
				return !okOld || !okNew || scanTriggered(o, n)
			},
		})).
		Complete(r)
}

// scanTriggered: what the scanner scans for changed between old and nu:
// the spec, the version the cluster runs, or the minor it can move to
// next (status.kubernetes.available, which the rollout waits on a scan of).
func scanTriggered(old, nu *cloudv1alpha1.Cluster) bool {
	return old.Generation != nu.Generation ||
		old.Status.CurrentKubernetesVersion != nu.Status.CurrentKubernetesVersion ||
		nextMinorOf(old) != nextMinorOf(nu)
}

func nextMinorOf(c *cloudv1alpha1.Cluster) string {
	if c.Status.Kubernetes == nil {
		return ""
	}
	return c.Status.Kubernetes.Available
}

// Reconcile scans the cluster when a scan is due and comes back when the
// next one is.
func (r *DeprecationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	leave := r.deps.FreezeBarrier.Enter()
	defer leave()
	obj := &cloudv1alpha1.Cluster{}
	if err := r.deps.Direct.Get(ctx, req.NamespacedName, obj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	tenant, id := obj.Labels[cloudstore.LabelTenant], obj.Labels[cloudstore.LabelID]
	frozen, err := r.deps.Freeze(ctx, id)
	if err != nil {
		return ctrl.Result{}, err
	}
	if frozen.Holds() || managed.IsPaused(obj) || id == "" || !scannable(obj) {
		return ctrl.Result{RequeueAfter: notScannableDelay}, nil
	}
	current := obj.Status.CurrentKubernetesVersion
	if current == "" && obj.Spec.ControlPlane != nil {
		current = obj.Spec.ControlPlane.KubernetesVersion
	}
	scanner := r.scanner
	scanner.Recorder = r.deps.Store.Clusters()
	// What a scan's result means is an event of the cluster.
	scanner.Report = func(ctx context.Context, _ deprecations.Cluster, e deprecations.Event) {
		r.deps.Emit(ctx, reconcile.Object{Kind: reconcile.KindCluster, Namespace: obj.Namespace, Name: obj.Name},
			e.Type, e.Reason, e.Message)
	}
	// A scan that worked is also written whole to the cluster's results
	// bucket, and the Cluster object then keeps its counts only.
	scanner.Publish = func(ctx context.Context, _ deprecations.Cluster, _, full *cloudv1.ClusterDeprecations) {
		r.publish(ctx, tenant, id, full)
	}
	next, err := scanner.Run(ctx, deprecations.Cluster{
		TenantID: tenant, ID: id, Current: current, UpgradeTo: r.upgradeTo(ctx, obj, current),
		Last:       convert.DeprecationsFromAPI(obj.Status.Deprecations),
		Kubeconfig: func(ctx context.Context) ([]byte, error) { return r.kubeconfig(ctx, obj) },
	})
	if err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: next}, nil
}

// publish writes the scan (every finding) to the results bucket and records
// its summary. It never fails the scan: without a bucket the Cluster object
// keeps the findings it has (the sink logs why).
func (r *DeprecationReconciler) publish(ctx context.Context, tenant, id string, full *cloudv1.ClusterDeprecations) {
	content := proto.CloneOf(full)
	content.ScannedAt, content.AttemptedAt = nil, nil // times change on every scan, content does not
	at := full.GetScannedAt().AsTime()
	staged, _ := r.sink.Stage(ctx, tenant, id, results.Deprecations, full, at, model.ResultRef{
		Digest: resultsink.Digest(content), CollectedAt: at, AttemptedAt: full.GetAttemptedAt().AsTime(),
		Total: full.GetTotal(), Removed: full.GetRemoved()})
	if err := staged.Commit(ctx); err != nil {
		r.deps.logger().WarnContext(ctx, "deprecation scan summary not recorded", "tenant_id", tenant, "cluster_id", id, "error", err)
	}
}

// scannable: the cluster runs (created by the platform, or imported) and
// is not being deleted. An imported cluster that is only observed has no
// ClusterAuth, and so no kubeconfig Secret, until its takeover: there is
// nothing to scan it with, which is not a failed scan.
func scannable(obj *cloudv1alpha1.Cluster) bool {
	if !obj.DeletionTimestamp.IsZero() {
		return false
	}

	if obj.Status.Phase == cloudv1alpha1.ClusterPhaseImported {
		return obj.Status.KubeconfigSecret != ""
	}
	return obj.Status.Phase == cloudv1alpha1.ClusterPhaseRunning
}

// kubeconfig reads the cluster's kubeconfig Secret at the API server.
func (r *DeprecationReconciler) kubeconfig(ctx context.Context, obj *cloudv1alpha1.Cluster) ([]byte, error) {
	return reconcile.ReadKubeconfig(ctx, r.deps.Direct, obj.Namespace, obj.Status.KubeconfigSecret)
}

// upgradeTo is the minor the release of the cluster's channel moves the
// cluster's minor to; "" when the channel offers no release, or the
// release does not move that minor (the scan then judges against the next
// minor).
func (r *DeprecationReconciler) upgradeTo(ctx context.Context, obj *cloudv1alpha1.Cluster, current string) string {
	// The minor the cluster's rollout moves it to next: the scan the
	// rollout's deprecation check waits for.
	if next := nextMinorOf(obj); next != "" {
		return next
	}
	channel := strings.ToLower(string(obj.Spec.ReleaseChannel))
	if channel == "" {
		channel = defaultChannel
	}
	ch := &cloudv1alpha1.ReleaseChannel{}
	if err := r.deps.getCached(ctx, client.ObjectKey{Name: channel}, ch); err != nil || ch.Status.Release == "" {
		return ""
	}
	release := &cloudv1alpha1.Release{}
	if err := r.deps.getCached(ctx, client.ObjectKey{Name: ch.Status.Release}, release); err != nil {
		return ""
	}
	// Two versions are of one minor when the same minor follows them.
	mine, err := deprecation.NextMinor(current)
	if err != nil {
		return ""
	}
	for _, k := range release.Spec.Content.Kubernetes {
		if next, err := deprecation.NextMinor(k.Minor); err == nil && next == mine {
			return k.UpgradeTo
		}
	}
	return ""
}
