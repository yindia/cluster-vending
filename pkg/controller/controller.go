// Package controller moves networks and clusters on as their Crossplane
// resources change, without anyone reading them: the next tier of a
// delete, the add-ons once EKS is ACTIVE, retiring a replaced node group.
// It watches every managed resource kind through the manager's informers
// and reconciles the network or cluster that owns the changed object
// (its labels name it). Reconciling is idempotent and also runs on API
// reads, so a missed event only delays progress until the next one or the
// requeue.
package controller

import (
	"context"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/yindia/cluster-vending/pkg/crossplane"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
)

// requeueAfter is how soon a network or cluster that is still changing is
// looked at again even without events (e.g. waiting for AWS).
const requeueAfter = 30 * time.Second

// Owner kinds in a request's name: "<kind>/<id>", namespace the tenant.
const (
	kindNetwork = "network"
	kindCluster = "cluster"
)

// Reconciler moves one network or cluster on; it reports whether it is
// still changing (see ClusterService.Reconcile, NetworkService.Reconcile).
type Reconciler interface {
	Reconcile(ctx context.Context, tenantID, id string) (bool, error)
}

// Setup registers the controller with mgr.
func Setup(mgr manager.Manager, networks, clusters Reconciler) error {
	b := builder.ControllerManagedBy(mgr).Named("cluster-vending")
	for _, obj := range crossplaneaws.ManagedObjects() {
		b = b.Watches(obj, handler.EnqueueRequestsFromMapFunc(ownerOf))
	}
	return b.Complete(&reconciler{networks: networks, clusters: clusters})
}

// ownerOf is the network or cluster an object belongs to, from its labels.
func ownerOf(_ context.Context, obj client.Object) []reconcile.Request {
	l := obj.GetLabels()
	tenant := l[crossplane.LabelTenantID]
	if tenant == "" {
		return nil
	}
	var out []reconcile.Request
	if id := l[crossplane.LabelCluster]; id != "" {
		out = append(out, request(tenant, kindCluster, id))
	}
	if id := l[crossplane.LabelNetwork]; id != "" {
		out = append(out, request(tenant, kindNetwork, id))
	}
	return out
}

func request(tenant, kind, id string) reconcile.Request {
	return reconcile.Request{NamespacedName: client.ObjectKey{Namespace: tenant, Name: kind + "/" + id}}
}

type reconciler struct{ networks, clusters Reconciler }

func (r *reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	kind, id, ok := strings.Cut(req.Name, "/")
	if !ok {
		return reconcile.Result{}, nil
	}
	target := r.networks
	if kind == kindCluster {
		target = r.clusters
	}
	changing, err := target.Reconcile(ctx, req.Namespace, id)
	if err != nil {
		return reconcile.Result{}, err // retried with backoff
	}
	if changing {
		return reconcile.Result{RequeueAfter: requeueAfter}, nil
	}
	return reconcile.Result{}, nil
}
