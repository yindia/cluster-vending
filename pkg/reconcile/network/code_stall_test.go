package network

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// stallVPC is a VPC the provider observed at its current generation, with
// the given Ready and Synced conditions.
func stallVPC(ready, synced, message string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{"apiVersion": "ec2.aws.m.upbound.io/v1beta1", "kind": "VPC",
		"metadata": map[string]any{"name": "vpc", "namespace": "tenant-acme", "generation": int64(2)},
		"spec":     map[string]any{"forProvider": map[string]any{}},
		"status": map[string]any{"conditions": []any{
			map[string]any{"type": "Ready", "status": ready, "reason": "Creating", "observedGeneration": int64(2)},
			map[string]any{"type": "Synced", "status": synced, "reason": "ReconcileError", "message": message, "observedGeneration": int64(2)},
		}}}}
}

func stallReconciler(t *testing.T, object *unstructured.Unstructured) (*Reconciler, map[string]string) {
	t.Helper()
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(object).Build()
	record := model.ManagedResource{APIVersion: object.GetAPIVersion(), Kind: object.GetKind(), Namespace: object.GetNamespace(), Name: object.GetName()}
	s := New(reconcile.Deps{Kube: c, KubeReader: c, Store: codeStore{inventory: codeInventory{records: []model.ManagedResource{record}}}})
	return s, map[string]string{record.APIVersion + "/VPC/tenant-acme/vpc": "hash"}
}

// A resource the provider reports FAILED after a code change's apply is a
// failed attempt: it counts toward CodeRecoveries and the change halts
// after them, so the rollout counts the network as failed and moves on.
func TestNetworkCodeChangeHaltsOnFailedConvergence(t *testing.T) {
	s, rendered := stallReconciler(t, stallVPC("False", "False", "InvalidParameterValue: boom"))
	row := &model.Network{ID: "net", Generation: 1, AppliedGeneration: 1, RenderHashes: rendered}
	ctx := reconcile.WithCodeHolder(t.Context())
	reconcile.SetCode(ctx, &cloudv1.CodeChange{Phase: cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_APPLYING, StartTime: timestamppb.Now()})

	for attempt := 1; attempt <= reconcile.CodeRecoveries; attempt++ {
		s.codeApplied(ctx, row, rendered, nil)
		got := reconcile.CodeOf(ctx, nil)
		if got.GetPhase() != cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_APPLYING || got.GetFailures() != int32(attempt) {
			t.Fatalf("attempt %d: %v; want applying with %d failures", attempt, got, attempt)
		}
	}
	s.codeApplied(ctx, row, rendered, nil)
	got := reconcile.CodeOf(ctx, nil)
	if got.GetPhase() != cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_HALTED || !strings.Contains(got.GetMessage(), "boom") {
		t.Fatalf("after %d failures: %v; want halted naming the failure", reconcile.CodeRecoveries+1, got)
	}
}

// A resource that is Synced but never becomes Ready keeps the change
// applying only until CodeConvergenceTimeout; then it halts, saying so.
func TestNetworkCodeChangeHaltsWhenItNeverConverges(t *testing.T) {
	s, rendered := stallReconciler(t, stallVPC("False", "True", ""))
	row := &model.Network{ID: "net", Generation: 1, AppliedGeneration: 1, RenderHashes: rendered}

	ctx := reconcile.WithCodeHolder(t.Context())
	reconcile.SetCode(ctx, &cloudv1.CodeChange{Phase: cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_APPLYING, StartTime: timestamppb.Now()})
	s.codeApplied(ctx, row, rendered, nil)
	if got := reconcile.CodeOf(ctx, nil); got.GetPhase() != cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_APPLYING {
		t.Fatalf("fresh change: %v; want still applying", got)
	}

	ctx = reconcile.WithCodeHolder(t.Context())
	started := time.Now().Add(-reconcile.CodeConvergenceTimeout - time.Minute)
	reconcile.SetCode(ctx, &cloudv1.CodeChange{Phase: cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_APPLYING, StartTime: timestamppb.New(started)})
	s.codeApplied(ctx, row, rendered, nil)
	got := reconcile.CodeOf(ctx, nil)
	if got.GetPhase() != cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_HALTED || !strings.Contains(got.GetMessage(), "did not converge") {
		t.Fatalf("stalled change: %v; want halted, saying it did not converge", got)
	}
}
