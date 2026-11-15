package admin

import (
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
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

// A freeze of a tenant or of a channel's tier is its own hold, named by
// its target: it covers the clusters that match (and those that join
// later), lets one through like the platform's, leaves full pauses alone,
// and its unfreeze removes that freeze only.
func TestScopedFreeze(t *testing.T) {
	until := timestamppb.New(pauseNow.Add(24 * time.Hour))
	beta, tier0 := cloudv1.ReleaseChannel_RELEASE_CHANNEL_BETA, cloudv1.ClusterTier_CLUSTER_TIER_0
	tests := []struct {
		name     string
		freeze   *cloudv1.FreezeRequest
		unfreeze *cloudv1.UnfreezeRequest
		hold     string
		want     cloudv1alpha1.ReleaseHoldSpec
		matched  int32
	}{
		{"a tenant",
			&cloudv1.FreezeRequest{TenantId: "globex", Reason: "audit", Until: until},
			&cloudv1.UnfreezeRequest{TenantId: "globex"},
			"freeze-tenant-globex", cloudv1alpha1.ReleaseHoldSpec{TenantID: "globex"}, 2},
		{"a channel's tier",
			&cloudv1.FreezeRequest{Channel: beta, Tier: tier0, Reason: "quarter end"},
			&cloudv1.UnfreezeRequest{Channel: beta, Tier: tier0},
			"freeze-channel-beta-tier0", cloudv1alpha1.ReleaseHoldSpec{Channel: "beta", Tier: cloudv1alpha1.ClusterTier0}, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFleet(t)
			ctx := auth.WithIdentity(t.Context(), &auth.Identity{Username: "alice"})
			// Another freeze, of the platform, that the unfreeze must leave.
			if _, err := f.s.Freeze(ctx, &cloudv1.FreezeRequest{Reason: "other"}); err != nil {
				t.Fatal(err)
			}

			res, err := f.s.Freeze(ctx, tt.freeze)

			if err != nil || res.GetClusters() != tt.matched {
				t.Fatalf("Freeze: %v, %v; want %d clusters", res, err, tt.matched)
			}
			holds := f.holds(t)
			i := slices.IndexFunc(holds, func(h cloudv1alpha1.ReleaseHold) bool { return h.Name == tt.hold })
			if len(holds) != 2 || i < 0 {
				t.Fatalf("holds %+v", holds)
			}
			h := holds[i].Spec
			if h.TenantID != tt.want.TenantID || h.Channel != tt.want.Channel || h.Tier != tt.want.Tier || h.Platform ||
				h.By != "alice" || h.Reason != tt.freeze.GetReason() || (tt.freeze.GetUntil() != nil) != (h.Until != nil) {
				t.Errorf("hold %+v", h)
			}
			for _, id := range []string{acmeProd, acmeDev} {
				if f.paused(t, "acme", id) != nil {
					t.Errorf("%s is paused whole by a freeze", id)
				}
			}

			let := proto.CloneOf(tt.unfreeze)
			let.ClusterId = acmeDev
			if _, err := f.s.Unfreeze(ctx, let); err != nil {
				t.Fatal(err)
			}
			if got := f.holds(t)[i].Spec.Except; !slices.Equal(got, []string{acmeDev}) {
				t.Errorf("let through %v", got)
			}

			if _, err := f.s.Unfreeze(ctx, tt.unfreeze); err != nil {
				t.Fatal(err)
			}
			if got := f.holds(t); len(got) != 1 || !got[0].Spec.Platform {
				t.Errorf("holds after the unfreeze %+v", got)
			}
		})
	}
}

// What Freeze refuses: a tier without a channel, a channel and a tenant,
// an until that has passed, an unknown tenant.
func TestFreezeRefuses(t *testing.T) {
	f := newFleet(t)
	ctx := t.Context()
	tests := []struct {
		name string
		req  *cloudv1.FreezeRequest
		code connect.Code
	}{
		{"a tier without a channel", &cloudv1.FreezeRequest{Tier: cloudv1.ClusterTier_CLUSTER_TIER_0}, connect.CodeInvalidArgument},
		{"a channel and a tenant", &cloudv1.FreezeRequest{TenantId: "acme", Channel: cloudv1.ReleaseChannel_RELEASE_CHANNEL_BETA}, connect.CodeInvalidArgument},
		{"an until that passed", &cloudv1.FreezeRequest{Until: timestamppb.New(pauseNow.Add(-time.Minute))}, connect.CodeInvalidArgument},
		{"an unknown tenant", &cloudv1.FreezeRequest{TenantId: "nobody"}, connect.CodeNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := f.s.Freeze(ctx, tt.req); connect.CodeOf(err) != tt.code {
				t.Fatalf("err = %v, want %v", err, tt.code)
			}
		})
	}
}
