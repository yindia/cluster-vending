package cluster

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	awsiam "github.com/upbound/provider-aws/v2/apis/namespaced/iam/v1beta1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// countingWorld is a world that counts every write the reconciler sends
// to the management cluster.
func countingWorld(t *testing.T) (world, *atomic.Int64) {
	t.Helper()
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	writes := &atomic.Int64{}
	apply := dryRunApply(scheme)
	funcs := interceptor.Funcs{
		Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
			writes.Add(1)
			return apply(ctx, c, obj, opts...)
		},
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			writes.Add(1)
			return c.Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			writes.Add(1)
			return c.Update(ctx, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			writes.Add(1)
			return c.Patch(ctx, obj, patch, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			writes.Add(1)
			return c.Delete(ctx, obj, opts...)
		},
	}
	return newWorld(t, funcs, nil), writes
}

func (w world) pause(t *testing.T, p *model.Freeze) {
	t.Helper()
	hold := &cloudv1alpha1.ReleaseHold{ObjectMeta: metav1.ObjectMeta{Name: reconcile.FreezeName}}
	if p == nil {
		if err := client.IgnoreNotFound(w.kube.Delete(t.Context(), hold)); err != nil {
			t.Fatal(err)
		}
		return
	}
	hold.CreationTimestamp = metav1.NewTime(p.Since)
	hold.Spec = cloudv1alpha1.ReleaseHoldSpec{Platform: true, By: p.By, Reason: p.Reason}
	if p.Until != nil {
		at := metav1.NewTime(*p.Until)
		hold.Spec.Until = &at
	}
	if err := w.kube.Create(t.Context(), hold); err != nil {
		t.Fatal(err)
	}
	if _, err := reconcile.SetProvidersPaused(t.Context(), w.svc.deps, reconcile.PauseScope{TenantID: worldTenant, ClusterID: w.row.ID}, true); err != nil {
		t.Fatal(err)
	}
}

var alice = &model.Freeze{By: "alice", Since: time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC), Reason: "provider upgrade"}

// A paused cluster is only observed: the pass writes nothing to the
// management cluster, the generation stays unapplied, and the status says
// who paused it. After the resume the generation is applied, once.
func TestPausedClusterIsObservedNotApplied(t *testing.T) {
	w, writes := countingWorld(t)
	ctx := t.Context()
	w.pause(t, alice)
	writes.Store(0)

	for range 2 {
		if err := w.svc.Reconcile(ctx, worldTenant, w.row.ID); err != nil {
			t.Fatal(err)
		}
	}

	if n := writes.Load(); n != 0 {
		t.Fatalf("%d writes to the management cluster while paused", n)
	}
	row := w.cluster(t)
	if row.AppliedGeneration != 1 {
		t.Fatalf("applied generation %d while paused, want 1", row.AppliedGeneration)
	}
	st := &cloudv1.ClusterStatus{}
	if err := Lenient.Unmarshal(row.Status, st); err != nil {
		t.Fatal(err)
	}
	if st.GetFreeze().GetBy() != "alice" || st.GetFreeze().GetReason() != "provider upgrade" ||
		!st.GetFreeze().GetSince().AsTime().Equal(alice.Since) {
		t.Errorf("status.pause = %v", st.GetFreeze())
	}
	if !strings.Contains(st.GetMessage(), "frozen by alice since 2026-11-14T09:00:00Z") {
		t.Errorf("message %q does not say who paused", st.GetMessage())
	}
	if !hasCondition(st, reconcile.ConditionFrozen) {
		t.Errorf("no Paused condition in %v", st.GetConditions())
	}

	// Resume: the generation that waited is applied by the next pass.
	w.pause(t, nil)
	if err := w.svc.Reconcile(ctx, worldTenant, w.row.ID); err != nil {
		t.Fatal(err)
	}
	row = w.cluster(t)
	if row.AppliedGeneration != row.Generation {
		t.Fatalf("applied generation %d of %d after the resume", row.AppliedGeneration, row.Generation)
	}
	applied := writes.Load()
	if applied == 0 {
		t.Fatal("nothing was written after the resume")
	}
	st = &cloudv1.ClusterStatus{}
	if err := Lenient.Unmarshal(row.Status, st); err != nil {
		t.Fatal(err)
	}
	if st.GetFreeze() != nil || hasCondition(st, reconcile.ConditionFrozen) {
		t.Errorf("status still says paused: %v, %v", st.GetFreeze(), st.GetConditions())
	}
}

func TestScopedFreezePreventsManagedResourceCreation(t *testing.T) {
	for _, scope := range []string{"cluster", "network", "tenant"} {
		t.Run(scope, func(t *testing.T) {
			w, writes := countingWorld(t)
			rule := cloudv1alpha1.FreezeRule{Frozen: true, Reason: "maintenance"}
			switch scope {
			case "cluster":
				rule.ClusterID = w.row.ID
			case "network":
				rule.NetworkID = w.row.NetworkID
			case "tenant":
				rule.TenantID = worldTenant
			}
			hold := &cloudv1alpha1.ReleaseHold{ObjectMeta: metav1.ObjectMeta{Name: reconcile.FreezeName},
				Spec: cloudv1alpha1.ReleaseHoldSpec{Platform: true, Scoped: true, Rules: []cloudv1alpha1.FreezeRule{rule}}}
			if err := w.kube.Create(t.Context(), hold); err != nil {
				t.Fatal(err)
			}
			// Pausing existing resources is an expected metadata write.
			if _, err := w.svc.deps.SyncFreeze(t.Context(), reconcile.PauseScope{TenantID: worldTenant, NetworkID: w.row.NetworkID, ClusterID: w.row.ID}); err != nil {
				t.Fatal(err)
			}
			writes.Store(0)
			for range 2 {
				if err := w.svc.Reconcile(t.Context(), worldTenant, w.row.ID); err != nil {
					t.Fatal(err)
				}
			}
			st := &cloudv1.ClusterStatus{}
			if err := Lenient.Unmarshal(w.cluster(t).Status, st); err != nil {
				t.Fatal(err)
			}
			if writes.Load() != 0 || st.GetFreeze() == nil || st.GetFreeze().GetLetThrough() {
				t.Fatalf("frozen cluster: %d managed resource writes, freeze=%+v", writes.Load(), st.GetFreeze())
			}
			if err := w.kube.Delete(t.Context(), hold); err != nil {
				t.Fatal(err)
			}
			writes.Store(0)
			if err := w.svc.Reconcile(t.Context(), worldTenant, w.row.ID); err != nil {
				t.Fatal(err)
			}
			if writes.Load() == 0 {
				t.Fatal("unfreezing did not resume managed resource creation")
			}
		})
	}
}

func hasCondition(st *cloudv1.ClusterStatus, conditionType string) bool {
	for _, c := range st.GetConditions() {
		if c.GetType() == conditionType {
			return true
		}
	}
	return false
}

// A spec change made while the cluster is paused is stored, held, and
// applied after the resume: the newest generation, not the one that was
// current when the pause began.
func TestSpecChangeWhilePausedIsAppliedAfterResume(t *testing.T) {
	w, writes := countingWorld(t)
	ctx := t.Context()
	w.pause(t, alice)
	writes.Store(0)
	row := w.cluster(t)
	row.Generation++ // what UpdateCluster does
	if err := w.store.Clusters().Update(ctx, row); err != nil {
		t.Fatal(err)
	}
	if err := w.svc.Reconcile(ctx, worldTenant, w.row.ID); err != nil {
		t.Fatal(err)
	}
	if got := w.cluster(t); got.AppliedGeneration != 1 || got.Generation != 3 || writes.Load() != 0 {
		t.Fatalf("while paused: applied %d of %d, %d writes", got.AppliedGeneration, got.Generation, writes.Load())
	}
	if got, err := w.svc.deps.Freeze(ctx, w.row.ID); err != nil || !got.Holds() {
		t.Fatal("the update removed the pause")
	}

	w.pause(t, nil)
	if err := w.svc.Reconcile(ctx, worldTenant, w.row.ID); err != nil {
		t.Fatal(err)
	}

	if got := w.cluster(t); got.AppliedGeneration != 3 {
		t.Fatalf("applied generation %d after the resume, want 3", got.AppliedGeneration)
	}
}

// A delete asked for while the cluster is paused is held: nothing is
// deleted until the resume, and then the deletion runs to its end.
func TestDeleteWhilePausedIsHeld(t *testing.T) {
	w, writes := countingWorld(t)
	ctx := t.Context()
	if err := w.svc.Reconcile(ctx, worldTenant, w.row.ID); err != nil { // applies everything
		t.Fatal(err)
	}
	roles := &awsiam.RoleList{}
	if err := w.kube.List(ctx, roles); err != nil || len(roles.Items) == 0 {
		t.Fatalf("%d roles before the delete (%v)", len(roles.Items), err)
	}
	before := len(roles.Items)
	w.pause(t, alice)
	writes.Store(0)
	row := w.cluster(t)
	row.State = StateDeleting // what DeleteCluster does
	if err := w.store.Clusters().Update(ctx, row); err != nil {
		t.Fatal(err)
	}
	writes.Store(0)

	for range 3 {
		if err := w.svc.Reconcile(ctx, worldTenant, w.row.ID); err != nil {
			t.Fatal(err)
		}
	}

	if err := w.kube.List(ctx, roles); err != nil || len(roles.Items) != before || writes.Load() != 0 {
		t.Fatalf("while paused: %d of %d roles left, %d writes (%v)", len(roles.Items), before, writes.Load(), err)
	}
	if got := w.cluster(t); got.State != StateDeleting {
		t.Fatalf("state %s while paused, want the delete kept", got.State)
	}

	w.pause(t, nil)
	for range 6 {
		if err := w.svc.Reconcile(ctx, worldTenant, w.row.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.kube.List(ctx, roles); err != nil || len(roles.Items) != 0 {
		t.Fatalf("%d roles left after the resume (%v)", len(roles.Items), err)
	}
}

// The status of a paused row says so whatever its resources say: also
// with none (a cluster created while its tenant is paused), where it stays
// PROVISIONING with the reason.
func TestStatusOfAPausedRow(t *testing.T) {
	ready := &cloudv1.ManagedResource{Kind: "Cluster", Name: "c", State: cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY,
		Synced: cloudv1.ConditionStatus_CONDITION_STATUS_TRUE, Generation: 1, ObservedGeneration: 1}
	tests := []struct {
		name      string
		row       model.Cluster
		views     []*cloudv1.ManagedResource
		want      cloudv1.ClusterState
		wantInMsg []string
	}{
		{"created while its tenant is paused", model.Cluster{Resource: model.Resource{State: StateProvisioning, Generation: 1, Frozen: alice}},
			nil, cloudv1.ClusterState_CLUSTER_STATE_PROVISIONING,
			[]string{"frozen by alice since 2026-11-14T09:00:00Z (provider upgrade)", "nothing is created, changed or deleted"}},
		{"running and paused", model.Cluster{Resource: model.Resource{State: StateRunning, Generation: 1, AppliedGeneration: 1,
			ObservedGeneration: 1, Frozen: &model.Freeze{By: "anonymous", Since: alice.Since}}},
			[]*cloudv1.ManagedResource{ready}, cloudv1.ClusterState_CLUSTER_STATE_RUNNING, []string{"frozen by anonymous since 2026-11-14T09:00:00Z"}},
		{"deleting and paused", model.Cluster{Resource: model.Resource{State: StateDeleting, Generation: 1, AppliedGeneration: 1, Frozen: alice}},
			[]*cloudv1.ManagedResource{ready}, cloudv1.ClusterState_CLUSTER_STATE_DELETING, []string{"deleting", "frozen by alice"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := status(&tt.row, tt.views, false)

			if st.GetState() != tt.want || st.GetFreeze().GetBy() != tt.row.Frozen.By || !hasCondition(st, reconcile.ConditionFrozen) {
				t.Fatalf("state %s, pause %v, conditions %v", st.GetState(), st.GetFreeze(), st.GetConditions())
			}
			for _, want := range tt.wantInMsg {
				if !strings.Contains(st.GetMessage(), want) {
					t.Errorf("message %q does not say %q", st.GetMessage(), want)
				}
			}
		})
	}
}

// A pause and a resume of the cluster each emit one event, with who paused.
func TestPauseAndResumeOfAClusterAreEvents(t *testing.T) {
	events := &capturedEvents{}
	w := newWorld(t, interceptor.Funcs{}, nil, func(d *reconcile.Deps) { d.Events = events })
	ctx := t.Context()
	if err := w.svc.Reconcile(ctx, worldTenant, w.row.ID); err != nil {
		t.Fatal(err)
	}
	w.pause(t, alice)
	events.reasons, events.messages = nil, nil

	for range 2 {
		if err := w.svc.Reconcile(ctx, worldTenant, w.row.ID); err != nil {
			t.Fatal(err)
		}
	}
	w.pause(t, nil)
	if err := w.svc.Reconcile(ctx, worldTenant, w.row.ID); err != nil {
		t.Fatal(err)
	}

	if got := events.count(reconcile.ReasonPlatformFrozen); got != 1 || !events.said(reconcile.ReasonPlatformFrozen, "frozen by alice") {
		t.Errorf("Paused events %d (%v %q), want one naming alice", got, events.reasons, events.messages)
	}
	if got := events.count(reconcile.ReasonPlatformUnfrozen); got != 1 {
		t.Errorf("Resumed events %d (%v), want one", got, events.reasons)
	}
}

type capturedEvents struct {
	reasons  []string
	messages []string
}

func (c *capturedEvents) Event(_ context.Context, _ reconcile.Object, _, reason, message string) {
	c.reasons = append(c.reasons, reason)
	c.messages = append(c.messages, message)
}

func (c *capturedEvents) count(reason string) int {
	n := 0
	for _, r := range c.reasons {
		if r == reason {
			n++
		}
	}
	return n
}

func (c *capturedEvents) said(reason, text string) bool {
	for i, r := range c.reasons {
		if r == reason && strings.Contains(c.messages[i], text) {
			return true
		}
	}
	return false
}
