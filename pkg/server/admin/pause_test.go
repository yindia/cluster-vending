package admin

import (
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/auth"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/database/storetest"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// fleet is two tenants' clusters on channels and tiers, in one management
// cluster the store and the server share.
type fleet struct {
	store repository.Store
	kube  client.Client
	s     *Service
}

const (
	acmeProd = "0190a000-0000-7000-8000-0000000000a0" // acme, beta, tier 0
	acmeDev  = "0190a000-0000-7000-8000-0000000000a2" // acme, beta, tier 2
	globexDB = "0190a000-0000-7000-8000-0000000000b0" // globex, beta, tier 0
	globexQA = "0190a000-0000-7000-8000-0000000000b1" // globex, stable, no tier
)

var pauseNow = time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)

func newFleet(t *testing.T) fleet {
	t.Helper()
	kube := storetest.NewClient(t)
	store := cloudstore.New(kube)
	add := func(tenant, name, id, channel, tier string) {
		t.Helper()
		network := storetest.MustNetwork(t, store, tenant, name+"-net")
		spec := `{"region":"us-east-1","releaseChannel":"` + channel + `"`
		if tier != "" {
			spec += `,"tier":"` + tier + `"`
		}
		c := &model.Cluster{TenantID: tenant, Name: name, NetworkID: network.ID, Provider: "aws", Region: "us-east-1",
			Resource: model.Resource{Base: model.Base{ID: id}, State: "CLUSTER_STATE_RUNNING", Spec: model.JSON(spec + "}")}}
		if err := store.Clusters().Create(t.Context(), c); err != nil {
			t.Fatal(err)
		}
	}
	storetest.MustTenant(t, store, "acme")
	storetest.MustTenant(t, store, "globex")
	add("acme", "prod", acmeProd, "RELEASE_CHANNEL_BETA", "CLUSTER_TIER_0")
	add("acme", "dev", acmeDev, "RELEASE_CHANNEL_BETA", "CLUSTER_TIER_2")
	add("globex", "db", globexDB, "RELEASE_CHANNEL_BETA", "CLUSTER_TIER_0")
	add("globex", "qa", globexQA, "RELEASE_CHANNEL_STABLE", "")
	s := NewService(reconcile.Deps{Kube: kube, Store: store})
	s.now = func() time.Time { return pauseNow }
	return fleet{store: store, kube: kube, s: s}
}

func (f fleet) paused(t *testing.T, tenant, id string) *model.Pause {
	t.Helper()
	c, err := f.store.Clusters().Get(t.Context(), tenant, id)
	if err != nil {
		t.Fatal(err)
	}
	return c.Paused
}

// A full pause of a channel's tier pauses each of its clusters now, of
// every tenant, at that channel's scope; its resume resumes them, except a
// cluster also paused on its own, which stays paused.
func TestPauseOfAChannelsTier(t *testing.T) {
	f := newFleet(t)
	ctx := auth.WithIdentity(t.Context(), &auth.Identity{Username: "alice"})
	if _, err := f.s.PauseReconcile(ctx, &cloudv1.PauseReconcileRequest{TenantId: "acme", ClusterId: acmeProd, Reason: "fixing by hand"}); err != nil {
		t.Fatal(err)
	}

	res, err := f.s.PauseReconcile(ctx, &cloudv1.PauseReconcileRequest{Channel: cloudv1.ReleaseChannel_RELEASE_CHANNEL_BETA,
		Tier: cloudv1.ClusterTier_CLUSTER_TIER_0, Reason: "provider upgrade"})

	if err != nil {
		t.Fatal(err)
	}
	if res.GetClusters() != 2 || !strings.Contains(res.GetNote(), "later") {
		t.Errorf("response %v", res)
	}
	for _, id := range []string{acmeProd, globexDB} {
		tenant := map[string]string{acmeProd: "acme", globexDB: "globex"}[id]
		p := f.paused(t, tenant, id)
		if _, ok := p.Entry(model.ChannelScope("beta", "0")); !ok {
			t.Errorf("%s: pause %+v has no entry of channel beta tier 0", id, p)
		}
	}
	if f.paused(t, "acme", acmeDev) != nil || f.paused(t, "globex", globexQA) != nil {
		t.Error("a cluster of another tier or channel is paused")
	}

	res2, err := f.s.ResumeReconcile(ctx, &cloudv1.ResumeReconcileRequest{Channel: cloudv1.ReleaseChannel_RELEASE_CHANNEL_BETA,
		Tier: cloudv1.ClusterTier_CLUSTER_TIER_0})

	if err != nil {
		t.Fatal(err)
	}
	if res2.GetClusters() != 2 {
		t.Errorf("resumed %d clusters, want 2", res2.GetClusters())
	}
	if p := f.paused(t, "globex", globexDB); p != nil {
		t.Errorf("globex db still paused: %+v", p)
	}
	if p := f.paused(t, "acme", acmeProd); p == nil || p.Reason != "fixing by hand" {
		t.Errorf("acme prod's own pause is gone: %+v", p)
	}
}

// A pause of upgrades only records a hold of exactly its target: a
// cluster, a tenant, a channel or a channel's tier. Nothing is paused
// otherwise. Pausing again changes why and until when; its resume removes
// that hold only.
func TestPauseOfUpgradesOnly(t *testing.T) {
	until := timestamppb.New(pauseNow.Add(24 * time.Hour))
	tests := []struct {
		name    string
		pause   *cloudv1.PauseReconcileRequest
		resume  *cloudv1.ResumeReconcileRequest
		want    cloudv1alpha1.ReleaseHoldSpec
		matched int32
	}{
		{"one cluster",
			&cloudv1.PauseReconcileRequest{TenantId: "acme", ClusterId: acmeProd, OnlyUpgrades: true, Reason: "testing 1.36"},
			&cloudv1.ResumeReconcileRequest{TenantId: "acme", ClusterId: acmeProd, OnlyUpgrades: true},
			cloudv1alpha1.ReleaseHoldSpec{ClusterID: acmeProd}, 1},
		{"a tenant",
			&cloudv1.PauseReconcileRequest{TenantId: "globex", OnlyUpgrades: true, Reason: "audit", Until: until},
			&cloudv1.ResumeReconcileRequest{TenantId: "globex", OnlyUpgrades: true},
			cloudv1alpha1.ReleaseHoldSpec{TenantID: "globex"}, 2},
		{"a channel's tier",
			&cloudv1.PauseReconcileRequest{Channel: cloudv1.ReleaseChannel_RELEASE_CHANNEL_BETA, Tier: cloudv1.ClusterTier_CLUSTER_TIER_0,
				OnlyUpgrades: true, Reason: "quarter-end freeze", Until: until},
			&cloudv1.ResumeReconcileRequest{Channel: cloudv1.ReleaseChannel_RELEASE_CHANNEL_BETA, Tier: cloudv1.ClusterTier_CLUSTER_TIER_0, OnlyUpgrades: true},
			cloudv1alpha1.ReleaseHoldSpec{Channel: "beta", Tier: cloudv1alpha1.ClusterTier0}, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFleet(t)
			ctx := auth.WithIdentity(t.Context(), &auth.Identity{Username: "alice"})
			// Another hold, of the whole channel, that the resume must leave.
			other := &cloudv1.PauseReconcileRequest{Channel: cloudv1.ReleaseChannel_RELEASE_CHANNEL_STABLE, OnlyUpgrades: true, Reason: "other"}
			if _, err := f.s.PauseReconcile(ctx, other); err != nil {
				t.Fatal(err)
			}

			res, err := f.s.PauseReconcile(ctx, tt.pause)

			if err != nil {
				t.Fatal(err)
			}
			if res.GetClusters() != tt.matched || res.GetResources() != 0 {
				t.Errorf("response %v, want %d clusters", res, tt.matched)
			}
			holds := f.holds(t)
			if len(holds) != 2 {
				t.Fatalf("holds %+v", holds)
			}
			h := holds[slices.IndexFunc(holds, func(h cloudv1alpha1.ReleaseHold) bool { return h.Spec.Reason != "other" })]
			if h.Spec.ClusterID != tt.want.ClusterID || h.Spec.TenantID != tt.want.TenantID || h.Spec.Channel != tt.want.Channel ||
				h.Spec.Tier != tt.want.Tier || h.Spec.By != "alice" || h.Spec.Reason != tt.pause.GetReason() ||
				(tt.pause.GetUntil() != nil) != (h.Spec.Until != nil) {
				t.Errorf("hold %+v", h.Spec)
			}
			for _, id := range []string{acmeProd, acmeDev} {
				if f.paused(t, "acme", id) != nil {
					t.Errorf("%s is paused whole by a pause of upgrades", id)
				}
			}

			again := proto.CloneOf(tt.pause)
			again.Reason = "changed"
			if _, err := f.s.PauseReconcile(ctx, again); err != nil {
				t.Fatal(err)
			}
			if got := f.holds(t); len(got) != 2 || !slices.ContainsFunc(got, func(h cloudv1alpha1.ReleaseHold) bool { return h.Spec.Reason == "changed" }) {
				t.Errorf("holds after pausing again %+v", got)
			}

			if _, err := f.s.ResumeReconcile(ctx, tt.resume); err != nil {
				t.Fatal(err)
			}
			if got := f.holds(t); len(got) != 1 || got[0].Spec.Reason != "other" {
				t.Errorf("holds after the resume %+v", got)
			}
		})
	}
}

func (f fleet) holds(t *testing.T) []cloudv1alpha1.ReleaseHold {
	t.Helper()
	list := &cloudv1alpha1.ReleaseHoldList{}
	if err := f.kube.List(t.Context(), list); err != nil {
		t.Fatal(err)
	}
	return list.Items
}

// What the API refuses: a network's upgrades (it has none), a tier
// without a channel, a channel with a tenant, an until that has passed.
func TestPauseRefusesWhatItCannotDo(t *testing.T) {
	f := newFleet(t)
	ctx := t.Context()
	tests := []struct {
		name string
		req  *cloudv1.PauseReconcileRequest
	}{
		{"a network's upgrades", &cloudv1.PauseReconcileRequest{TenantId: "acme", NetworkId: acmeDev, OnlyUpgrades: true}},
		{"a tier without a channel", &cloudv1.PauseReconcileRequest{TenantId: "acme", Tier: cloudv1.ClusterTier_CLUSTER_TIER_0}},
		{"a channel and a tenant", &cloudv1.PauseReconcileRequest{TenantId: "acme", Channel: cloudv1.ReleaseChannel_RELEASE_CHANNEL_BETA}},
		{"no target", &cloudv1.PauseReconcileRequest{}},
		{"an until that passed", &cloudv1.PauseReconcileRequest{TenantId: "acme", Until: timestamppb.New(pauseNow.Add(-time.Minute))}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := f.s.PauseReconcile(ctx, tt.req); connect.CodeOf(err) != connect.CodeInvalidArgument {
				t.Fatalf("err = %v, want INVALID_ARGUMENT", err)
			}
		})
	}
}

// The list shows every pause in effect once, by target: a tenant's, a
// cluster's own, a channel tier's (with how many clusters it paused), and
// the pauses of upgrades; one whose until passed is not listed.
func TestListPauses(t *testing.T) {
	f := newFleet(t)
	ctx := auth.WithIdentity(t.Context(), &auth.Identity{Username: "alice"})
	future := timestamppb.New(pauseNow.Add(time.Hour))
	for _, req := range []*cloudv1.PauseReconcileRequest{
		{TenantId: "acme", Reason: "provider upgrade"},
		{TenantId: "globex", ClusterId: globexQA, Reason: "by hand", Until: future},
		{Channel: cloudv1.ReleaseChannel_RELEASE_CHANNEL_BETA, Tier: cloudv1.ClusterTier_CLUSTER_TIER_0, Reason: "tier freeze"},
		{Channel: cloudv1.ReleaseChannel_RELEASE_CHANNEL_BETA, OnlyUpgrades: true, Reason: "quarter end", Until: future},
	} {
		if _, err := f.s.PauseReconcile(ctx, req); err != nil {
			t.Fatalf("%v: %v", req, err)
		}
	}

	res, err := f.s.ListPauses(ctx, &cloudv1.ListPausesRequest{})

	if err != nil {
		t.Fatal(err)
	}
	got := map[string]*cloudv1.PauseRecord{}
	for _, p := range res.GetPauses() {
		got[p.GetTarget()] = p
	}
	want := map[string]struct {
		kind     cloudv1.PauseKind
		reason   string
		clusters int32
	}{
		"tenant acme":             {cloudv1.PauseKind_PAUSE_KIND_EVERYTHING, "provider upgrade", 2},
		"cluster " + globexQA:     {cloudv1.PauseKind_PAUSE_KIND_EVERYTHING, "by hand", 1},
		"channel beta tier 0":     {cloudv1.PauseKind_PAUSE_KIND_EVERYTHING, "tier freeze", 2},
		"channel beta (upgrades)": {cloudv1.PauseKind_PAUSE_KIND_UPGRADES, "quarter end", 3},
	}
	if len(got) != len(want) {
		t.Fatalf("pauses %v", res.GetPauses())
	}
	for target, w := range want {
		key := strings.TrimSuffix(target, " (upgrades)")
		p := got[key]
		if p == nil || p.GetKind() != w.kind || p.GetReason() != w.reason || p.GetClusters() != w.clusters || p.GetBy() != "alice" {
			t.Errorf("%s: %v, want %+v", target, p, w)
		}
	}
	if got["cluster "+globexQA].GetUntil() == nil {
		t.Error("the cluster's until is not listed")
	}
}
