package controller

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	platform "github.com/yindia/cluster-vending/pkg/reconcile"
)

// subTenantUnreachable is, by SubTenant UID, when each SubTenant being
// deleted, whose parent is being deleted, first could not use the parent's
// workload cluster in the current outage. A restart forgets it: the timeout
// then counts again.
var subTenantUnreachable sync.Map

// subTenantOutage is a subTenantUnreachable entry. The key lets one be
// forgotten once its SubTenant is gone and only its name is known.
type subTenantOutage struct {
	key   types.NamespacedName
	since time.Time
}

// forgetSubTenantOutages forgets the outages recorded for SubTenants named
// key other than keep (the one that exists now, if any): those are gone.
func forgetSubTenantOutages(key types.NamespacedName, keep types.UID) {
	subTenantUnreachable.Range(func(uid, value any) bool {
		if uid.(types.UID) != keep && value.(subTenantOutage).key == key {
			subTenantUnreachable.Delete(uid)
		}
		return true
	})
}

// parentRemoval says whether the parent cluster is being deleted, and with
// force (DeleteClusterRequest.force: the in-cluster cleanup is skipped).
func parentRemoval(parent *cloud.Cluster) (deleting, force bool) {
	var row struct {
		State       string `json:"state"`
		DeleteForce bool   `json:"deleteForce"`
	}
	if raw := parent.Annotations[cloudstore.AnnotationRow]; raw != "" {
		_ = json.Unmarshal([]byte(raw), &row) // subTenantParent refused one that does not parse
	}
	deleting = !parent.DeletionTimestamp.IsZero() || parent.Status.Phase == cloud.ClusterPhaseDeleting || row.State == "CLUSTER_STATE_DELETING"
	return deleting, deleting && row.DeleteForce
}

// workloadUnavailable reports a parent workload cluster that cannot be used.
// A SubTenant being deleted whose parent is being deleted is released once
// that lasted the teardown timeout (as the cluster's own deletion goes on
// without its API then): nothing else would let the cluster go.
func (r *SubTenantReconciler) workloadUnavailable(ctx context.Context, obj *cloud.SubTenant, st cloud.SubTenantStatus, parent *cloud.Cluster, reason, message string, warning bool) (ctrl.Result, error) {
	if deleting, _ := parentRemoval(parent); obj.DeletionTimestamp.IsZero() || !deleting {
		subTenantUnreachable.Delete(obj.UID)
		return r.subTenantReport(ctx, obj, st, cloud.SubTenantStatePending, reason, message, warning)
	}
	first, _ := subTenantUnreachable.LoadOrStore(obj.UID, subTenantOutage{key: client.ObjectKeyFromObject(obj), since: time.Now()})
	if time.Since(first.(subTenantOutage).since) >= r.teardownTimeout() {
		return r.releaseWithParent(ctx, obj, st, "ParentUnreachable",
			"The parent cluster is being deleted and its workload API could not be reached; releasing this controller's protection, the cluster's deletion removes its namespaces")
	}
	return r.subTenantReport(ctx, obj, st, cloud.SubTenantStatePending, reason, message, warning)
}

func (r *SubTenantReconciler) teardownTimeout() time.Duration {
	if r.deps.TeardownTimeout > 0 {
		return r.deps.TeardownTimeout
	}
	return platform.DefaultTeardownTimeout
}

// releaseWithParent removes this controller's finalizer without a workload
// call: the parent cluster's deletion takes the namespaces down with it.
func (r *SubTenantReconciler) releaseWithParent(ctx context.Context, obj *cloud.SubTenant, st cloud.SubTenantStatus, reason, message string) (ctrl.Result, error) {
	subTenantUnreachable.Delete(obj.UID)
	if !controllerutil.ContainsFinalizer(obj, cloud.SubTenantFinalizer) {
		return ctrl.Result{}, nil
	}
	result, err := r.subTenantReport(ctx, obj, st, cloud.SubTenantStateDeleting, reason, message, true)
	if err != nil {
		return result, err
	}
	before := obj.DeepCopy()
	controllerutil.RemoveFinalizer(obj, cloud.SubTenantFinalizer)
	return ctrl.Result{}, client.IgnoreNotFound(r.deps.Direct.Patch(ctx, obj, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})))
}
