package controller

import (
	"strconv"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/storetest"
	reconcilecluster "github.com/yindia/cluster-vending/pkg/reconcile/cluster"
)

func (w world) adoptedCondition(t *testing.T, name string) (*cloudv1alpha1.Cluster, *metav1.Condition) {
	t.Helper()
	obj := w.cluster(t, name)
	return obj, meta.FindStatusCondition(obj.Status.Conditions, ConditionAdopted)
}

func TestKubectlClusterOnAnUnverifiedAccountIsFlaggedNotAdopted(t *testing.T) {
	w := newWorld(t)
	w.readyNetwork(t)
	storetest.SetAccess(t, w.k, tenant, w.account.ID, &model.AccountAccess{})
	w.must(t, w.k.Create(t.Context(), handMadeCluster(t, "prod", "net")))
	res, err := NewClusterReconciler(w.deps).Reconcile(t.Context(), request("prod"))
	w.must(t, err)
	if res.RequeueAfter != failedDelay {
		t.Errorf("result %+v", res)
	}
	got, cond := w.adoptedCondition(t, "prod")
	if cond == nil || cond.Status != metav1.ConditionFalse || got.Labels[cloudstore.LabelID] != "" {
		t.Fatalf("condition %+v, labels %v", cond, got.Labels)
	}
	if want := "has not been verified yet"; !contains(cond.Message, want) {
		t.Errorf("message %q lacks %q", cond.Message, want)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func (w world) row(t *testing.T, id string) *model.Cluster {
	t.Helper()
	row, err := w.store.Clusters().Get(t.Context(), tenant, id)
	w.must(t, err)
	return row
}

// An earlier build set EditRefused on an edit outside the node group
// allowlist; tenants define no node groups now, so the next pass removes it.
func TestAnEarlierEditRefusalIsCleared(t *testing.T) {
	w := newWorld(t)
	r, _ := w.settledCluster(t)
	obj := w.cluster(t, "prod")
	meta.SetStatusCondition(&obj.Status.Conditions, metav1.Condition{Type: ConditionEditRefused, Status: metav1.ConditionTrue,
		Reason: reasonInvalid, Message: `node group "gpu": instance family "p5" is not allowed`})
	w.must(t, w.k.Status().Update(t.Context(), obj))
	_, err := r.Reconcile(t.Context(), request("prod"))
	w.must(t, err)
	if cond := meta.FindStatusCondition(w.cluster(t, "prod").Status.Conditions, ConditionEditRefused); cond != nil {
		t.Fatalf("the old refusal is still there: %+v", cond)
	}
}

// An API update writes the object (generation ahead) and, a moment later,
// the status. The controller waking in between must not take the update
// for a kubectl edit.
func TestAPIUpdateIsNotBumpedAgainByTheControllerBeforeItsStatusWrite(t *testing.T) {
	w := newWorld(t)
	r, id := w.settledCluster(t)
	row := w.row(t, id)
	row.State = reconcilecluster.StateRunning // a release-only update leaves RUNNING
	row.Generation++                          // as UpdateCluster does
	w.must(t, w.store.Clusters().Update(t.Context(), row))
	// What the API server did with that update: counted the generation (the
	// fake one does not); the store recorded it in the same update.
	obj := w.cluster(t, "prod")
	obj.Generation = 2
	obj.Annotations[cloudstore.AnnotationRowGeneration] = strconv.Itoa(2)
	w.must(t, w.k.Update(t.Context(), obj))
	obj = w.cluster(t, "prod")
	obj.Status.ObservedGeneration = 1 // the status update has not happened yet
	w.must(t, w.k.Status().Update(t.Context(), obj))
	after := w.row(t, id)

	_, err := r.Reconcile(t.Context(), request("prod"))
	w.must(t, err)
	got := w.row(t, id)
	if got.Generation != after.Generation {
		t.Fatalf("bumped again: generation %d, want %d", got.Generation, after.Generation)
	}
	if got.ETag != after.ETag {
		t.Errorf("the API caller's etag rotated: %s, was %s", got.ETag, after.ETag)
	}
	if got.State != after.State || got.State == reconcilecluster.StateUpdating {
		t.Errorf("state %s (was %s): a release-only update was forced to a full re-apply", got.State, after.State)
	}
	if o := w.cluster(t, "prod"); edited(o.Generation, o.Status.ObservedGeneration) {
		t.Errorf("still counts as an edit: generation %d, observed %d", o.Generation, o.Status.ObservedGeneration)
	}
}

func TestKubectlEditAfterAnAPIUpdateIsStillBumped(t *testing.T) {
	w := newWorld(t)
	r, id := w.settledCluster(t)
	row := w.row(t, id)
	row.Generation++
	w.must(t, w.store.Clusters().Update(t.Context(), row))
	obj := w.cluster(t, "prod")
	obj.Generation = 2
	obj.Annotations[cloudstore.AnnotationRowGeneration] = "2"
	w.must(t, w.k.Update(t.Context(), obj))
	obj = w.cluster(t, "prod")
	obj.Status.ObservedGeneration = 2
	w.must(t, w.k.Status().Update(t.Context(), obj))
	after := w.row(t, id)
	// kubectl edit: generation 3, no record of the store for it.
	w.edit(t, 3, editCIDRs)
	_, err := r.Reconcile(t.Context(), request("prod"))
	w.must(t, err)
	if got := w.row(t, id); got.Generation != after.Generation+1 || publicCIDR(t, got) != editedCIDR {
		t.Fatalf("generation %d (was %d), public CIDR %q: the edit was not bumped", got.Generation, after.Generation, publicCIDR(t, got))
	}
}
