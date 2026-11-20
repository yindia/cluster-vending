package controller

import (
	"strconv"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/storetest"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	reconcilecluster "github.com/yindia/cluster-vending/pkg/reconcile/cluster"
)

// allowlisted makes the controller enforce the platform admin's instance
// family allowlist, as the API server does.
func (w world) allowlisted(families ...string) *ClusterReconciler {
	d := w.deps
	d.NodeGroups.Karpenter.InstanceFamilies = families
	return NewClusterReconciler(d)
}

func group(name, family string) cloudv1alpha1.NodeGroupSpec {
	return cloudv1alpha1.NodeGroupSpec{Name: name, InstanceFamilies: []string{family}}
}

func (w world) adoptedCondition(t *testing.T, name string) (*cloudv1alpha1.Cluster, *metav1.Condition) {
	t.Helper()
	obj := w.cluster(t, name)
	return obj, meta.FindStatusCondition(obj.Status.Conditions, ConditionAdopted)
}

func TestKubectlClusterWithDisallowedFamilyIsFlaggedNotAdopted(t *testing.T) {
	w := newWorld(t)
	w.readyNetwork(t)
	obj := handMadeCluster(t, "prod", "net")
	obj.Spec.NodeGroups = []cloudv1alpha1.NodeGroupSpec{group("gpu", "p5")}
	w.must(t, w.k.Create(t.Context(), obj))
	res, err := w.allowlisted("m7i").Reconcile(t.Context(), request("prod"))
	w.must(t, err)
	if res.RequeueAfter != failedDelay {
		t.Errorf("result %+v", res)
	}
	got, cond := w.adoptedCondition(t, "prod")
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != reasonInvalid {
		t.Fatalf("condition %+v", cond)
	}
	want := `node group "gpu": instance family "p5" is not allowed; allowed families: m7i`
	if cond.Message != want {
		t.Errorf("message %q, want %q", cond.Message, want)
	}
	if got.Labels[cloudstore.LabelID] != "" {
		t.Errorf("adopted anyway: labels %v", got.Labels)
	}
}

func TestKubectlClusterWithAllowedFamilyIsAdopted(t *testing.T) {
	w := newWorld(t)
	w.readyNetwork(t)
	obj := handMadeCluster(t, "prod", "net")
	obj.Spec.NodeGroups = []cloudv1alpha1.NodeGroupSpec{group("web", "m7i")}
	w.must(t, w.k.Create(t.Context(), obj))
	_, err := w.allowlisted("m7i").Reconcile(t.Context(), request("prod"))
	w.must(t, err)
	if got, cond := w.adoptedCondition(t, "prod"); got.Labels[cloudstore.LabelID] == "" {
		t.Fatalf("not adopted: %+v", cond)
	}
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

func TestKubectlEditAddingADisallowedGroupIsRefusedAndNotApplied(t *testing.T) {
	w := newWorld(t)
	_, id := w.settledCluster(t)
	r := w.allowlisted("m7i")
	before := w.row(t, id)
	w.edit(t, 2, func(o *cloudv1alpha1.Cluster) { o.Spec.NodeGroups = []cloudv1alpha1.NodeGroupSpec{group("gpu", "p5")} })
	for range 2 {
		_, err := r.Reconcile(t.Context(), request("prod"))
		w.must(t, err)
	}
	row := w.row(t, id)
	if row.Generation != before.Generation || row.State != before.State || row.ETag != before.ETag {
		t.Fatalf("refused edit applied: generation %d (was %d), state %s (was %s)", row.Generation, before.Generation, row.State, before.State)
	}
	obj := w.cluster(t, "prod")
	cond := meta.FindStatusCondition(obj.Status.Conditions, ConditionEditRefused)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != reasonInvalid ||
		!contains(cond.Message, `instance family "p5" is not allowed`) {
		t.Fatalf("condition %+v", cond)
	}
	if obj.Status.ObservedGeneration != 1 {
		t.Errorf("a refused edit was observed: %d", obj.Status.ObservedGeneration)
	}

	// Fixed: the edit is taken up and the refusal goes.
	w.edit(t, 3, func(o *cloudv1alpha1.Cluster) { o.Spec.NodeGroups = []cloudv1alpha1.NodeGroupSpec{group("web", "m7i")} })
	_, err := r.Reconcile(t.Context(), request("prod"))
	w.must(t, err)
	if got := w.row(t, id); got.Generation != before.Generation+1 {
		t.Errorf("the corrected edit: generation %d, want %d", got.Generation, before.Generation+1)
	}
	if cond := meta.FindStatusCondition(w.cluster(t, "prod").Status.Conditions, ConditionEditRefused); cond != nil {
		t.Errorf("the refusal stays: %+v", cond)
	}
}

func TestKubectlEditAddingAnAllowedGroupIsApplied(t *testing.T) {
	w := newWorld(t)
	_, id := w.settledCluster(t)
	before := w.row(t, id)
	w.edit(t, 2, func(o *cloudv1alpha1.Cluster) { o.Spec.NodeGroups = []cloudv1alpha1.NodeGroupSpec{group("web", "m7i")} })
	_, err := w.allowlisted("m7i").Reconcile(t.Context(), request("prod"))
	w.must(t, err)
	if got := w.row(t, id); got.Generation != before.Generation+1 {
		t.Errorf("generation %d, want %d", got.Generation, before.Generation+1)
	}
	if cond := meta.FindStatusCondition(w.cluster(t, "prod").Status.Conditions, ConditionEditRefused); cond != nil {
		t.Errorf("refused: %+v", cond)
	}
}

func TestKubectlEditLeavesGroupsTheAllowlistNoLongerListsAlone(t *testing.T) {
	w := newWorld(t)
	_, id := w.settledCluster(t)
	// The stored input has a group outside the list (the list was narrowed
	// later): an unrelated edit is not refused for it.
	row := w.row(t, id)
	input := &cloudv1.Cluster{}
	w.must(t, reconcilecluster.Lenient.Unmarshal(row.Input, input))
	old := &cloudv1.NodeGroupSpec{Name: "old", InstanceFamilies: []string{"c5"}}
	input.Spec.NodeGroups = []*cloudv1.NodeGroupSpec{old}
	var err error
	row.Input, err = protojson.Marshal(input)
	w.must(t, err)
	w.must(t, w.store.Clusters().Record(t.Context(), row))
	w.edit(t, 2, func(o *cloudv1alpha1.Cluster) {
		o.Spec.NodeGroups = []cloudv1alpha1.NodeGroupSpec{group("old", "c5")}
		o.Spec.DefaultNodePool.Scaling.MaxSize = 9
	})
	_, err = w.allowlisted("m7i").Reconcile(t.Context(), request("prod"))
	w.must(t, err)
	if cond := meta.FindStatusCondition(w.cluster(t, "prod").Status.Conditions, ConditionEditRefused); cond != nil {
		t.Fatalf("an unchanged group was refused: %+v", cond)
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
	w.edit(t, 3, func(o *cloudv1alpha1.Cluster) { o.Spec.DefaultNodePool.Scaling.MaxSize = 9 })
	_, err := r.Reconcile(t.Context(), request("prod"))
	w.must(t, err)
	if got := w.row(t, id); got.Generation != after.Generation+1 || maxSize(t, got) != 9 {
		t.Fatalf("generation %d (was %d), max size %d: the edit was not bumped", got.Generation, after.Generation, maxSize(t, got))
	}
}
