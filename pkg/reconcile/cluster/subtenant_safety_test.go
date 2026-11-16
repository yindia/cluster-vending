package cluster

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/naas"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

func safetyParent(t *testing.T, w world) *cloudv1alpha1.Cluster {
	t.Helper()
	parent := &cloudv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: w.row.Name, Namespace: worldNS, UID: types.UID("parent-uid"), Generation: 5}}
	parent.Status.ObservedGeneration = 3
	if err := w.kube.Create(t.Context(), parent); err != nil {
		t.Fatal(err)
	}
	// The existing renderer fake has no native Cluster status subresource.
	// Preserve its renderer fixtures, and emulate that status write here.
	w.svc.deps.Kube = interceptor.NewClient(w.kube.(client.WithWatch), interceptor.Funcs{SubResourceUpdate: func(ctx context.Context, c client.Client, subresource string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
		if _, ok := obj.(*cloudv1alpha1.Cluster); ok && subresource == "status" {
			return c.Update(ctx, obj)
		}
		return c.SubResource(subresource).Update(ctx, obj, opts...)
	}})
	return parent
}

func safetyChild(t *testing.T, w world, uidOnly bool) *cloudv1alpha1.SubTenant {
	t.Helper()
	ref := cloudv1alpha1.SubTenantClusterReference{ID: w.row.ID}
	if uidOnly {
		ref = cloudv1alpha1.SubTenantClusterReference{UID: "parent-uid"}
	}
	child := &cloudv1alpha1.SubTenant{ObjectMeta: metav1.ObjectMeta{Name: "child", Namespace: worldNS}, Spec: cloudv1alpha1.SubTenantSpec{ClusterRef: ref}}
	if err := w.kube.Create(t.Context(), child); err != nil {
		t.Fatal(err)
	}
	return child
}

func TestSubTenantSafetyReconcilerBlocksCapsuleRemovalAndResumes(t *testing.T) {
	w, source := platformWorld(t)
	source.defs = []cloudv1alpha1.ComponentDefinition{platformDef("capsule")}
	source.release.Spec.Content.Kubernetes[0].Components = []cloudv1alpha1.ComponentPin{{Name: "capsule", Version: "0.15.2"}}
	w.helmConfig(t)
	w.setNaaS(t, true)
	w.install(t)
	w.deployed(t, "capsule")
	w.setNaaS(t, false)
	parent := safetyParent(t, w)
	child := safetyChild(t, w, true)
	parent.Spec.NaaS = ptr.To(true)
	if err := w.kube.Update(t.Context(), parent); err != nil {
		t.Fatal(err)
	}
	w.pause(t, alice)
	parent.Spec.NaaS = ptr.To(false)
	parent.Generation++
	if err := w.kube.Update(t.Context(), parent); err != nil {
		t.Fatal(err)
	}
	before := w.cluster(t)
	for range 2 {
		if err := w.svc.Reconcile(t.Context(), worldTenant, w.row.ID); err == nil || !strings.Contains(err.Error(), "1 SubTenants") {
			t.Fatalf("UID dependency did not hold reconcile: %v", err)
		}
	}
	if release := w.release(t, "capsule"); release == nil || release.GetDeletionTimestamp() != nil {
		t.Fatal("Capsule removed while UID-only child remained")
	}
	after := w.cluster(t)
	if after.AppliedGeneration != before.AppliedGeneration || after.Generation != before.Generation || after.ObservedGeneration != before.ObservedGeneration || string(after.Spec) != string(before.Spec) {
		t.Fatal("safety hold changed parent desired state or observed/applied generation")
	}
	if err := w.kube.Get(t.Context(), client.ObjectKeyFromObject(parent), parent); err != nil {
		t.Fatal(err)
	}
	condition := meta.FindStatusCondition(parent.Status.Conditions, naas.ConditionParentSafety)
	if parent.Status.ObservedGeneration != 3 || condition == nil || condition.Reason != naas.ReasonDependents {
		t.Fatalf("safety did not preserve native pending generation: %+v", parent.Status)
	}
	contract := &cloudv1.ClusterStatus{}
	if err := protojson.Unmarshal(parent.Status.ContractStatus.Raw, contract); err != nil {
		t.Fatal(err)
	}
	if contract.ObservedGeneration != storedStatus(before).ObservedGeneration {
		t.Fatal("blocked safety status advanced API contract observed_generation")
	}
	if !meta.IsStatusConditionTrue(parent.Status.Conditions, reconcile.ConditionFrozen) || contract.Freeze.GetBy() != alice.By || !hasCondition(contract, reconcile.ConditionFrozen) || !strings.Contains(parent.Status.Message, "frozen") {
		t.Fatalf("frozen guard lost paused status: %+v", parent.Status)
	}
	w.pause(t, nil)
	if err := w.svc.Reconcile(t.Context(), worldTenant, w.row.ID); err == nil {
		t.Fatal("unfreeze bypassed SubTenant safety")
	}
	if err := w.kube.Get(t.Context(), client.ObjectKeyFromObject(parent), parent); err != nil {
		t.Fatal(err)
	}
	if parent.Status.ObservedGeneration != 3 || meta.IsStatusConditionTrue(parent.Status.Conditions, reconcile.ConditionFrozen) || !meta.IsStatusConditionFalse(parent.Status.Conditions, naas.ConditionParentSafety) {
		t.Fatal("unfreeze acknowledged pending edit or lost safety condition")
	}
	if release := w.release(t, "capsule"); release == nil || release.GetDeletionTimestamp() != nil {
		t.Fatal("unfreeze removed Capsule before child cleanup")
	}
	// These existing fixtures deliberately separate the store's native CRs
	// from renderer objects. Transfer the recorded contract to the store so
	// recovery must remove a genuinely persisted safety condition.
	if err := w.store.Clusters().SetStatus(t.Context(), worldTenant, before.ID, before.ETag, parent.Status.ContractStatus.Raw); err != nil {
		t.Fatal(err)
	}
	if err := w.kube.Delete(t.Context(), child); err != nil {
		t.Fatal(err)
	}
	if err := w.svc.Reconcile(t.Context(), worldTenant, w.row.ID); err != nil {
		t.Fatal(err)
	}
	if release := w.release(t, "capsule"); release != nil && release.GetDeletionTimestamp() == nil {
		t.Fatal("Capsule removal did not resume after child cleanup")
	}
	for _, condition := range storedStatus(w.cluster(t)).Conditions {
		if condition.Type == naas.ConditionParentSafety {
			t.Fatal("normal contract status retained stale safety condition")
		}
	}
}

func TestSubTenantSafetyReconcilerBlocksCloudDeletion(t *testing.T) {
	deletes := 0
	w := newWorld(t, interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
		deletes++
		return c.Delete(ctx, obj, opts...)
	}}, nil)
	safetyParent(t, w)
	child := safetyChild(t, w, false)
	row := w.cluster(t)
	row.State, row.DeleteForce = StateDeleting, true
	if err := w.store.Clusters().Update(t.Context(), row); err != nil {
		t.Fatal(err)
	}
	if err := w.svc.Reconcile(t.Context(), worldTenant, row.ID); err == nil || deletes != 0 {
		t.Fatalf("forced cloud teardown escaped guard: deletes=%d err=%v", deletes, err)
	}
	if err := w.kube.Delete(t.Context(), child); err != nil {
		t.Fatal(err)
	}
	deletes = 0
	if err := w.svc.Reconcile(t.Context(), worldTenant, row.ID); err != nil || deletes == 0 {
		t.Fatalf("cloud teardown did not resume: deletes=%d err=%v", deletes, err)
	}
}

func TestSubTenantSafetyReconcilerFailsClosedOnReadAndAllowsEnabling(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{}, nil)
	parent := safetyParent(t, w)
	readErr := errors.New("SubTenant API unavailable")
	w.svc.deps.KubeReader = interceptor.NewClient(w.kube.(client.WithWatch), interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		if _, ok := list.(*cloudv1alpha1.SubTenantList); ok {
			return readErr
		}
		return c.List(ctx, list, opts...)
	}})
	if err := w.svc.Reconcile(t.Context(), worldTenant, w.row.ID); !errors.Is(err, readErr) {
		t.Fatalf("dependency read failure swallowed: %v", err)
	}
	if err := w.kube.Get(t.Context(), client.ObjectKeyFromObject(parent), parent); err != nil {
		t.Fatal(err)
	}
	if condition := meta.FindStatusCondition(parent.Status.Conditions, naas.ConditionParentSafety); condition == nil || condition.Reason != naas.ReasonReadFailed {
		t.Fatal("read failure missing explicit condition")
	}
	w.setNaaS(t, true)
	if err := w.svc.guardSubTenantSafety(t.Context(), w.cluster(t)); err != nil {
		t.Fatalf("enabling NaaS must remain allowed: %v", err)
	}
}
