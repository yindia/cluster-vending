package controller

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	"google.golang.org/protobuf/proto"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/crossplane"
	"github.com/yindia/cluster-vending/pkg/crossplane/managed"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/naas"
	platform "github.com/yindia/cluster-vending/pkg/reconcile"
	reconcilecluster "github.com/yindia/cluster-vending/pkg/reconcile/cluster"
)

type subTenantSafetyEvents struct{ reasons []string }

func (e *subTenantSafetyEvents) Event(_ context.Context, _ platform.Object, _, reason, _ string) {
	e.reasons = append(e.reasons, reason)
}

func TestSubTenantSafetyBlocksNativeClusterDeletionAndNaaSDisable(t *testing.T) {
	for _, deleting := range []bool{true, false} {
		w := newWorld(t)
		parent := handMadeCluster(t, "safety-parent", "net")
		parent.UID = types.UID("safety-parent-uid")
		parent.Labels = map[string]string{cloudstore.LabelID: "parent-id", cloudstore.LabelTenant: tenant}
		parent.Finalizers = []string{cloudv1alpha1.ClusterFinalizer}
		parent.Generation, parent.Status.ObservedGeneration = 4, 2
		parent.Spec.NaaS = ptr.To(deleting)
		w.must(t, w.k.Create(t.Context(), parent))
		if deleting {
			w.must(t, w.k.Delete(t.Context(), parent))
		}
		// The ordinary client/cache contains neither child. Direct List must
		// discover both an unlabeled reference and a terminating UID reference.
		fresh := fake.NewClientBuilder().WithScheme(w.k.Scheme()).Build()
		child := &cloudv1alpha1.SubTenant{ObjectMeta: metav1.ObjectMeta{Name: "unlabeled", Namespace: namespace}, Spec: cloudv1alpha1.SubTenantSpec{ClusterRef: cloudv1alpha1.SubTenantClusterReference{ID: "parent-id"}}}
		w.must(t, fresh.Create(t.Context(), child))
		terminating := &cloudv1alpha1.SubTenant{ObjectMeta: metav1.ObjectMeta{Name: "terminating", Namespace: namespace, Finalizers: []string{"test/cleanup"}}, Spec: cloudv1alpha1.SubTenantSpec{ClusterRef: cloudv1alpha1.SubTenantClusterReference{UID: string(parent.UID)}}}
		w.must(t, fresh.Create(t.Context(), terminating))
		w.must(t, fresh.Delete(t.Context(), terminating))
		lists := 0
		w.deps.Direct = interceptor.NewClient(w.k.(client.WithWatch), interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*cloudv1alpha1.SubTenantList); ok {
				lists++
				return fresh.List(ctx, list, opts...)
			}
			return c.List(ctx, list, opts...)
		}})
		events := &subTenantSafetyEvents{}
		w.deps.Events = events
		r := NewClusterReconciler(w.deps)
		for range 2 {
			result, err := r.Reconcile(t.Context(), request(parent.Name))
			if err != nil || result.RequeueAfter != failedDelay {
				t.Fatalf("deleting=%t: result=%v err=%v", deleting, result, err)
			}
		}
		got := w.cluster(t, parent.Name)
		condition := meta.FindStatusCondition(got.Status.Conditions, naas.ConditionParentSafety)
		if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != naas.ReasonDependents || !strings.Contains(condition.Message, "2 SubTenants") {
			t.Fatalf("missing dependency condition: %+v", condition)
		}
		if got.Status.ObservedGeneration != 2 || got.Generation != 4 || !slices.Contains(got.Finalizers, cloudv1alpha1.ClusterFinalizer) {
			t.Fatal("blocked parent advanced observation or lost finalizer")
		}
		if ptr.Deref(got.Spec.NaaS, false) != deleting {
			t.Fatal("guard silently restored the desired NaaS value")
		}
		if lists != 2 || len(events.reasons) != 1 {
			t.Fatalf("fresh lists=%d events=%v, want 2 lists and one transition event", lists, events.reasons)
		}
		w.must(t, fresh.Delete(t.Context(), child))
		w.must(t, fresh.Get(t.Context(), client.ObjectKeyFromObject(terminating), terminating))
		terminating.Finalizers = nil
		w.must(t, fresh.Update(t.Context(), terminating))
		// No watch event is needed: the periodic retry rechecks the direct
		// reader and releases the safety hold after cleanup.
		if _, stop, err := r.guardSubTenantSafety(t.Context(), got, nil); err != nil || stop {
			t.Fatalf("cleanup did not unblock parent: stop=%t err=%v", stop, err)
		}
		if condition := meta.FindStatusCondition(w.cluster(t, parent.Name).Status.Conditions, naas.ConditionParentSafety); condition != nil {
			t.Fatal("stale safety condition remains after cleanup")
		}
	}
}

func TestSubTenantSafetyNativeReadFailureFailsClosedAndEnablingRemainsAllowed(t *testing.T) {
	w := newWorld(t)
	parent := handMadeCluster(t, "safety-read", "net")
	parent.Spec.NaaS = ptr.To(false)
	w.must(t, w.k.Create(t.Context(), parent))
	readErr := errors.New("management API list failed")
	lists := 0
	w.deps.Direct = interceptor.NewClient(w.k.(client.WithWatch), interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		if _, ok := list.(*cloudv1alpha1.SubTenantList); ok {
			lists++
			return readErr
		}
		return c.List(ctx, list, opts...)
	}})
	r := NewClusterReconciler(w.deps)
	if result, err := r.Reconcile(t.Context(), request(parent.Name)); !errors.Is(err, readErr) || result.RequeueAfter != failedDelay {
		t.Fatalf("read failure not retried/propagated: %v %v", result, err)
	}
	got := w.cluster(t, parent.Name)
	if condition := meta.FindStatusCondition(got.Status.Conditions, naas.ConditionParentSafety); condition == nil || condition.Reason != naas.ReasonReadFailed {
		t.Fatal("read error has no explicit condition")
	}
	got.Spec.NaaS = ptr.To(true)
	w.must(t, w.k.Update(t.Context(), got))
	if _, stop, err := r.guardSubTenantSafety(t.Context(), got, nil); err != nil || stop {
		t.Fatalf("enabling NaaS was blocked: %t %v", stop, err)
	}
	if lists != 1 {
		t.Fatal("enabling unnecessarily read SubTenant dependencies")
	}
}

func TestSubTenantSafetyFrozenNativeDisableRemainsPendingUntilCleanup(t *testing.T) {
	w := newWorld(t)
	w.readyNetwork(t)
	parent := handMadeCluster(t, "frozen-parent", "net")
	parent.Generation = 1 // Kubernetes assigns this; the fake does not.
	parent.Spec.NaaS = ptr.To(true)
	w.must(t, w.k.Create(t.Context(), parent))
	r := NewClusterReconciler(w.deps)
	for range 2 {
		_, err := r.Reconcile(t.Context(), request(parent.Name))
		w.must(t, err)
	}
	parent = w.cluster(t, parent.Name)
	id := parent.Labels[cloudstore.LabelID]
	providers := &awseks.ClusterList{}
	w.must(t, w.k.List(t.Context(), providers, client.MatchingLabels{crossplane.LabelCluster: id}))
	if len(providers.Items) != 1 {
		t.Fatal("missing provider fixture")
	}
	providerKey := client.ObjectKeyFromObject(&providers.Items[0])
	hold := &cloudv1alpha1.ReleaseHold{ObjectMeta: metav1.ObjectMeta{Name: platform.FreezeName}, Spec: cloudv1alpha1.ReleaseHoldSpec{Platform: true, By: "operator", Reason: "maintenance"}}
	w.must(t, w.k.Create(t.Context(), hold))
	_, err := r.Reconcile(t.Context(), request(parent.Name))
	w.must(t, err)
	parent = w.cluster(t, parent.Name)
	observed := parent.Status.ObservedGeneration
	baseline := &cloudv1.ClusterStatus{}
	w.must(t, reconcilecluster.Lenient.Unmarshal(parent.Status.ContractStatus.Raw, baseline))
	baseline.PlatformComponents = []*cloudv1.PlatformComponent{{Name: "capsule", Version: "0.15.2", Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_READY}}
	baseline.KubeconfigSecret = "keep-existing-kubeconfig"
	baselineRaw, err := reconcilecluster.Encode(baseline)
	w.must(t, err)
	parent.Status.ContractStatus.Raw = baselineRaw
	w.must(t, w.k.Status().Update(t.Context(), parent))
	before, err := w.store.Clusters().Get(t.Context(), tenant, id)
	w.must(t, err)
	assertContract := func(frozen bool) {
		t.Helper()
		row, err := w.store.Clusters().Get(t.Context(), tenant, id)
		w.must(t, err)
		st := &cloudv1.ClusterStatus{}
		w.must(t, reconcilecluster.Lenient.Unmarshal(row.Status, st))
		safety, paused := false, false
		for _, condition := range st.Conditions {
			if condition.Type == naas.ConditionParentSafety {
				safety = condition.Reason == naas.ReasonDependents && condition.Status == cloudv1.ConditionStatus_CONDITION_STATUS_FALSE && strings.Contains(condition.Message, "child")
			}
			if condition.Type == platform.ConditionFrozen {
				paused = condition.Status == cloudv1.ConditionStatus_CONDITION_STATUS_TRUE
			}
		}
		if !safety || paused != frozen || (st.Freeze != nil) != frozen || !strings.Contains(st.Message, "blocked") {
			t.Fatalf("canonical API status missing safety/freeze information: %v", st)
		}
		// The guard owns only these fields; every other field, including
		// observed_generation and platform/managed inventory, must survive.
		normalize := func(status *cloudv1.ClusterStatus) {
			status.Message, status.Freeze = "", nil
			status.Conditions = slices.DeleteFunc(status.Conditions, func(c *cloudv1.Condition) bool {
				return c.Type == naas.ConditionParentSafety || c.Type == platform.ConditionFrozen
			})
		}
		expected := proto.Clone(baseline).(*cloudv1.ClusterStatus)
		normalize(expected)
		normalize(st)
		if !proto.Equal(expected, st) {
			t.Fatalf("safety changed unrelated canonical status: got %v, want %v", st, expected)
		}
	}
	child := &cloudv1alpha1.SubTenant{ObjectMeta: metav1.ObjectMeta{Name: "child", Namespace: namespace}, Spec: cloudv1alpha1.SubTenantSpec{ClusterRef: cloudv1alpha1.SubTenantClusterReference{ID: id}}}
	w.must(t, w.k.Create(t.Context(), child))
	parent.Spec.NaaS = ptr.To(false)
	parent.Generation++
	w.must(t, w.k.Update(t.Context(), parent))
	wantPaused := true
	checks := 0
	direct := interceptor.NewClient(w.k.(client.WithWatch), interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		if _, ok := list.(*cloudv1alpha1.SubTenantList); ok {
			provider := &awseks.Cluster{}
			w.must(t, c.Get(ctx, providerKey, provider))
			if managed.IsPaused(provider) != wantPaused {
				t.Fatal("safety read ran before SyncFreeze synchronized provider pause")
			}
			checks++
		}
		return c.List(ctx, list, opts...)
	}})
	w.deps.Direct, w.deps.KubeReader = direct, direct
	r = NewClusterReconciler(w.deps)
	for range 2 {
		result, err := r.Reconcile(t.Context(), request(parent.Name))
		w.must(t, err)
		if result.RequeueAfter != failedDelay {
			t.Fatalf("blocked freeze does not retry: %+v", result)
		}
	}
	parent = w.cluster(t, parent.Name)
	if parent.Status.ObservedGeneration != observed || parent.Status.ObservedGeneration >= parent.Generation || !meta.IsStatusConditionFalse(parent.Status.Conditions, naas.ConditionParentSafety) || !meta.IsStatusConditionTrue(parent.Status.Conditions, platform.ConditionFrozen) || !strings.Contains(parent.Status.Message, "frozen") {
		t.Fatalf("frozen native edit acknowledged or missing safety/pause status: %+v", parent.Status)
	}
	assertContract(true)
	w.must(t, w.k.Delete(t.Context(), hold))
	wantPaused = false
	_, err = r.Reconcile(t.Context(), request(parent.Name))
	w.must(t, err)
	parent = w.cluster(t, parent.Name)
	after, err := w.store.Clusters().Get(t.Context(), tenant, id)
	w.must(t, err)
	if parent.Status.ObservedGeneration != observed || !meta.IsStatusConditionFalse(parent.Status.Conditions, naas.ConditionParentSafety) || meta.IsStatusConditionTrue(parent.Status.Conditions, platform.ConditionFrozen) || after.Generation != before.Generation || after.AppliedGeneration != before.AppliedGeneration {
		t.Fatal("unfreeze bypassed safety or acknowledged the pending native edit")
	}
	assertContract(false)
	w.must(t, w.k.Delete(t.Context(), child))
	_, err = r.Reconcile(t.Context(), request(parent.Name))
	w.must(t, err)
	parent = w.cluster(t, parent.Name)
	after, err = w.store.Clusters().Get(t.Context(), tenant, id)
	w.must(t, err)
	spec, err := reconcilecluster.SpecOf(after)
	w.must(t, err)
	if checks < 4 || spec.GetNaas() || after.Generation <= before.Generation || meta.FindStatusCondition(parent.Status.Conditions, naas.ConditionParentSafety) != nil {
		t.Fatalf("cleanup did not resume native NaaS disable: checks=%d naas=%t generation=%d before=%d safety=%+v", checks, spec.GetNaas(), after.Generation, before.Generation, meta.FindStatusCondition(parent.Status.Conditions, naas.ConditionParentSafety))
	}
	finalStatus := &cloudv1.ClusterStatus{}
	w.must(t, reconcilecluster.Lenient.Unmarshal(after.Status, finalStatus))
	for _, condition := range finalStatus.Conditions {
		if condition.Type == naas.ConditionParentSafety {
			t.Fatal("API status retained safety hold after cleanup")
		}
	}
}
