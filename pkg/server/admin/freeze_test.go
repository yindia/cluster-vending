package admin

import (
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect/v2"

	"github.com/yindia/cluster-vending/pkg/auth"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/rollout"
)

// Freeze holds every cluster (one platform hold), shows in ListPauses as
// "the platform", lets single clusters through, and Unfreeze lifts it and
// forgets them.
func TestFreezeAndUnfreeze(t *testing.T) {
	f := newFleet(t)
	ctx := auth.WithIdentity(t.Context(), &auth.Identity{Username: "alice"})

	res, err := f.s.Freeze(ctx, &cloudv1.FreezeRequest{Reason: "provider upgrade"})

	if err != nil || res.GetClusters() != 4 {
		t.Fatalf("Freeze: %v, %v; want 4 clusters", res, err)
	}
	holds := f.holds(t)
	if len(holds) != 1 || holds[0].Name != rollout.FreezeName || !holds[0].Spec.Platform || holds[0].Spec.By != "alice" ||
		holds[0].Spec.Reason != "provider upgrade" {
		t.Fatalf("holds %+v", holds)
	}
	pauses, err := f.s.ListPauses(ctx, &cloudv1.ListPausesRequest{})
	if err != nil || len(pauses.GetPauses()) != 1 || pauses.GetPauses()[0].GetTarget() != "the platform" ||
		pauses.GetPauses()[0].GetClusters() != 4 {
		t.Fatalf("ListPauses %v, %v", pauses, err)
	}

	un, err := f.s.Unfreeze(ctx, &cloudv1.UnfreezeRequest{ClusterId: acmeDev})
	if err != nil || !strings.Contains(un.GetNote(), acmeDev) {
		t.Fatalf("Unfreeze one: %v, %v", un, err)
	}
	const network = "0190a000-0000-7000-8000-0000000000c0"
	if un, err := f.s.Unfreeze(ctx, &cloudv1.UnfreezeRequest{NetworkId: network}); err != nil || !strings.Contains(un.GetNote(), "network "+network) {
		t.Fatalf("Unfreeze a network: %v, %v", un, err)
	}
	if _, err := f.s.Freeze(ctx, &cloudv1.FreezeRequest{Reason: "still deploying"}); err != nil {
		t.Fatal(err)
	}
	holds = f.holds(t)
	if !slices.Equal(holds[0].Spec.Except, []string{acmeDev, network}) || holds[0].Spec.Reason != "still deploying" {
		t.Errorf("freezing again keeps who is let through and changes why: %+v", holds[0].Spec)
	}

	if _, err := f.s.Unfreeze(ctx, &cloudv1.UnfreezeRequest{}); err != nil {
		t.Fatal(err)
	}
	if holds := f.holds(t); len(holds) != 0 {
		t.Errorf("after Unfreeze: holds %+v", holds)
	}
	_, err = f.s.Unfreeze(ctx, &cloudv1.UnfreezeRequest{ClusterId: acmeDev})
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("letting a cluster through an unfrozen platform: %v, want FAILED_PRECONDITION", err)
	}
}
