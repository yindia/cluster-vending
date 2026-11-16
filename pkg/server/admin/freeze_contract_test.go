package admin

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/yindia/cluster-vending/pkg/reconcile"

	v1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

func TestTypedFreezeTargetAndSharedScopedStatus(t *testing.T) {
	s, _ := freezeService(t)
	if _, err := s.Freeze(t.Context(), &v1.FreezeRequest{ClusterId: freezeOtherCluster}); err != nil {
		t.Fatal(err)
	}
	target := &v1.FreezeTarget{Scope: &v1.FreezeTarget_ClusterId{ClusterId: freezeCluster}}
	result, err := s.Freeze(t.Context(), &v1.FreezeRequest{Target: target})
	if err != nil || result.GetClusters() != 1 || result.GetStatus().GetTarget().GetClusterId() != freezeCluster || result.GetStatus().GetPhase() != v1.FreezePhase_FREEZE_PHASE_FREEZING {
		t.Fatalf("scoped freeze contract: %v %v", result, err)
	}
	confirmFreeze(t, s)
	view, err := s.GetFreeze(t.Context(), &v1.GetFreezeRequest{Target: target})
	if err != nil || view.GetStatus().GetPhase() != v1.FreezePhase_FREEZE_PHASE_FROZEN || view.GetStatus().GetClusters() != 1 {
		t.Fatalf("scoped status: %v %v", view, err)
	}
	resumed, err := s.Unfreeze(t.Context(), &v1.UnfreezeRequest{Target: target})
	if err != nil || resumed.GetStatus().GetPhase() != v1.FreezePhase_FREEZE_PHASE_UNFREEZING || resumed.GetStatus().GetTarget().GetClusterId() != freezeCluster {
		t.Fatalf("scoped unfreeze: %v %v", resumed, err)
	}
	confirmFreeze(t, s)
	other, err := s.GetFreeze(t.Context(), &v1.GetFreezeRequest{ClusterId: freezeOtherCluster})
	if err != nil || other.GetStatus().GetPhase() != v1.FreezePhase_FREEZE_PHASE_FROZEN {
		t.Fatalf("another scope was resumed: %v %v", other, err)
	}
}

func TestScopedFreezeConfirmsDespiteUnrelatedPauseFailure(t *testing.T) {
	s, k := freezeService(t)
	broken := interceptor.NewClient(k.(client.WithWatch), interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
		if obj.GetName() == "two-helm" {
			return errors.New("unrelated pause denied")
		}
		return c.Patch(ctx, obj, patch, opts...)
	}})
	s.deps.Kube, s.deps.KubeReader = broken, broken
	if _, err := s.Freeze(t.Context(), &v1.FreezeRequest{Reason: "maintenance"}); err != nil {
		t.Fatal(err)
	}
	if _, err := reconcile.ConfirmFreeze(t.Context(), s.deps, s.now()); err == nil {
		t.Fatal("expected unrelated provider pause failure")
	}
	view, err := s.GetFreeze(t.Context(), &v1.GetFreezeRequest{ClusterId: freezeCluster})
	if err != nil || view.GetStatus().GetPhase() != v1.FreezePhase_FREEZE_PHASE_FROZEN {
		t.Fatalf("selected scope could not confirm independently: %v %v", view, err)
	}
	global, err := s.GetFreeze(t.Context(), &v1.GetFreezeRequest{})
	if err != nil || global.GetPhase() != "Freezing" {
		t.Fatalf("global partial freeze incorrectly confirmed: %v %v", global, err)
	}
}

func TestExplicitFreezeTargetsRejectAmbiguity(t *testing.T) {
	s, _ := freezeService(t)
	for _, req := range []*v1.FreezeRequest{
		{Target: &v1.FreezeTarget{}},
		{Target: &v1.FreezeTarget{Scope: &v1.FreezeTarget_Platform{Platform: false}}},
		{Target: &v1.FreezeTarget{Scope: &v1.FreezeTarget_ClusterId{ClusterId: freezeCluster}}, NetworkId: freezeNetwork},
		{Target: &v1.FreezeTarget{Scope: &v1.FreezeTarget_TenantId{TenantId: "Bad/Tenant"}}},
	} {
		if _, err := s.Freeze(t.Context(), req); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("invalid target accepted: %v %v", req, err)
		}
	}
	// Dual-writing the same scope supports clients talking to older servers.
	if _, err := s.Freeze(t.Context(), &v1.FreezeRequest{Target: &v1.FreezeTarget{Scope: &v1.FreezeTarget_ClusterId{ClusterId: freezeCluster}}, ClusterId: freezeCluster}); err != nil {
		t.Fatal(err)
	}
}
