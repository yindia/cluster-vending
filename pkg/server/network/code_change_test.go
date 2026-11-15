package network

import (
	"context"
	"maps"
	"strings"
	"testing"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// networkGate is a platform whose rollout admits the network's code
// change, or not, and says why (a freeze, the tier of its clusters).
type networkGate struct {
	reconcile.PlatformSource
	admitted bool
	why      string
}

func (g *networkGate) ClusterCodeAdmitted(context.Context, string, string, string) (bool, string, error) {
	return true, "", nil
}

func (g *networkGate) NetworkCodeAdmitted(context.Context, string, string) (bool, string, error) {
	return g.admitted, g.why, nil
}

// A network's code change (a new build renders its objects differently)
// waits while its rollout does not admit it (frozen, or the code rollout
// of its clusters' channels not at its tier yet), and is applied once it
// does (let through, or its tier reached).
func TestNetworkCodeChange(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	n := f.allReadyNetwork(t)
	row, err := f.store.Networks().Get(ctx, tenant, n)
	if err != nil || len(row.RenderHashes) == 0 {
		t.Fatalf("applied network fingerprints %v (%v)", row.RenderHashes, err)
	}
	rendered := maps.Clone(row.RenderHashes)
	for k := range row.RenderHashes {
		row.RenderHashes[k] = "older-build"
		break
	}
	if err := f.store.Networks().Record(ctx, row); err != nil {
		t.Fatal(err)
	}
	gate := &networkGate{why: "the platform is frozen: deploy"}
	f.svc = NewNetworkService(reconcile.Deps{Store: f.store, Kube: f.kube, Platform: gate})

	for _, why := range []string{"the platform is frozen: deploy", "waiting for channel stable's code rollout to reach tier 0"} {
		gate.why = why
		f.reconcile(t, n)
		cc := f.get(t, n).GetStatus().GetCodeChange()
		if cc.GetPhase() != cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_PENDING || cc.GetMessage() != why || cc.GetSummary() != "1 object changes" {
			t.Fatalf("not admitted (%s): %v", why, cc)
		}
		if got, _ := f.store.Networks().Get(ctx, tenant, n); maps.Equal(got.RenderHashes, rendered) {
			t.Fatalf("not admitted (%s), yet applied", why)
		}
	}

	gate.admitted, gate.why = true, ""
	f.reconcile(t, n)
	cc := f.get(t, n).GetStatus().GetCodeChange()
	got, err := f.store.Networks().Get(ctx, tenant, n)
	if err != nil || cc.GetPhase() != cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_DONE || !maps.Equal(got.RenderHashes, rendered) {
		t.Fatalf("admitted: %v, fingerprints %v (%v)", cc, got.RenderHashes, err)
	}
	if !strings.Contains(cc.GetSummary(), "1 object") {
		t.Errorf("summary %q", cc.GetSummary())
	}
}

// allReadyNetwork creates a network and applies it in full.
func (f fixture) allReadyNetwork(t *testing.T) string {
	t.Helper()
	n := f.create(t, "prod", nil)
	f.allReady(t, n.GetId())
	f.reconcile(t, n.GetId())
	return n.GetId()
}
