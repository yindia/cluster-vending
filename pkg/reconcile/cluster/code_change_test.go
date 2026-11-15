package cluster

import (
	"context"
	"maps"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// codeGate is a platform whose channel's code rollout admits the cluster,
// or not, and whose freezes are holds.
type codeGate struct {
	fakePlatform
	admitted bool
	why      string
	holds    []cloudv1alpha1.ReleaseHold
}

func (g *codeGate) ClusterCodeAdmitted(context.Context, string, string, string) (bool, string, error) {
	return g.admitted, g.why, nil
}

func (g *codeGate) NetworkCodeAdmitted(context.Context, string, string) (bool, string, error) {
	return g.admitted, g.why, nil
}

func (g *codeGate) Holds(context.Context) ([]cloudv1alpha1.ReleaseHold, error) { return g.holds, nil }

// codeWorld is a cluster applied in full by this build, on the stable
// channel, whose recorded fingerprints then say an older build applied
// something else: a code change is pending.
func codeWorld(t *testing.T, gate *codeGate) (world, map[string]string) {
	t.Helper()
	w := newWorld(t, interceptor.Funcs{}, nil, func(d *reconcile.Deps) { d.Platform, d.Releases = gate, gate })
	ctx := t.Context()
	onStable := w.cluster(t)
	spec, err := SpecOf(onStable)
	if err != nil {
		t.Fatal(err)
	}
	spec.ReleaseChannel = cloudv1.ReleaseChannel_RELEASE_CHANNEL_STABLE
	if onStable.Spec, err = Encode(spec); err != nil {
		t.Fatal(err)
	}
	if err := w.store.Clusters().Record(ctx, onStable); err != nil {
		t.Fatal(err)
	}
	if err := w.svc.Reconcile(ctx, worldTenant, w.row.ID); err != nil {
		t.Fatal(err)
	}
	row := w.cluster(t)
	if row.AppliedGeneration != row.Generation || len(row.RenderHashes) == 0 {
		t.Fatalf("applied %d of %d, fingerprints %v", row.AppliedGeneration, row.Generation, row.RenderHashes)
	}
	rendered := maps.Clone(row.RenderHashes)
	older := maps.Clone(rendered)
	for k := range older {
		older[k] = "older-build"
		break
	}
	row.RenderHashes = older
	if err := w.store.Clusters().Record(ctx, row); err != nil {
		t.Fatal(err)
	}
	return w, rendered
}

func codeOf(t *testing.T, w world) *cloudv1.CodeChange {
	t.Helper()
	return storedStatus(w.cluster(t)).GetCodeChange()
}

// A code change waits for its turn in the channel's rollout; once
// admitted it is applied and done, with what changed.
func TestCodeChangeWaitsForItsTurn(t *testing.T) {
	gate := &codeGate{why: "waiting its turn in the channel's code rollout: tier 2 first"}
	w, rendered := codeWorld(t, gate)
	ctx := t.Context()

	if err := w.svc.Reconcile(ctx, worldTenant, w.row.ID); err != nil {
		t.Fatal(err)
	}
	cc := codeOf(t, w)
	if cc.GetPhase() != cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_PENDING || !strings.Contains(cc.GetMessage(), "tier 2 first") ||
		cc.GetSummary() != "1 object changes" || maps.Equal(w.cluster(t).RenderHashes, rendered) {
		t.Fatalf("not admitted: %v; it must wait, not apply", cc)
	}

	gate.admitted = true
	if err := w.svc.Reconcile(ctx, worldTenant, w.row.ID); err != nil {
		t.Fatal(err)
	}
	cc = codeOf(t, w)
	if cc.GetPhase() != cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_DONE || cc.GetFinishTime() == nil || !maps.Equal(w.cluster(t).RenderHashes, rendered) {
		t.Fatalf("admitted: %v, fingerprints %v", cc, w.cluster(t).RenderHashes)
	}
}

// A freeze holds a code change even when its rollout admits it; a cluster
// let through goes ahead.
func TestCodeChangeFrozen(t *testing.T) {
	freeze := cloudv1alpha1.ReleaseHold{Spec: cloudv1alpha1.ReleaseHoldSpec{Platform: true, Reason: "deploy"}}
	gate := &codeGate{admitted: true}
	w, rendered := codeWorld(t, gate)
	gate.holds = []cloudv1alpha1.ReleaseHold{freeze}
	ctx := t.Context()

	if err := w.svc.Reconcile(ctx, worldTenant, w.row.ID); err != nil {
		t.Fatal(err)
	}
	if cc := codeOf(t, w); cc.GetPhase() != cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_PENDING || cc.GetMessage() != "the platform is frozen: deploy" {
		t.Fatalf("frozen: %v", cc)
	}

	freeze.Spec.Except = []string{w.row.ID}
	gate.holds = []cloudv1alpha1.ReleaseHold{freeze}
	if err := w.svc.Reconcile(ctx, worldTenant, w.row.ID); err != nil {
		t.Fatal(err)
	}
	if cc := codeOf(t, w); cc.GetPhase() != cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_DONE || !maps.Equal(w.cluster(t).RenderHashes, rendered) {
		t.Fatalf("let through: %v", cc)
	}
}

// A cluster in its network's tier waits for the network's pending code
// change: the network goes first.
func TestCodeChangeWaitsForItsNetwork(t *testing.T) {
	gate := &codeGate{admitted: true}
	w, _ := codeWorld(t, gate)
	ctx := t.Context()
	n, err := w.store.Networks().Get(ctx, worldTenant, w.row.NetworkID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := protojson.Marshal(&cloudv1.NetworkStatus{CodeChange: &cloudv1.CodeChange{Phase: cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_PENDING}})
	if err != nil {
		t.Fatal(err)
	}
	n.Status = model.JSON(raw)
	if err := w.store.Networks().Record(ctx, n); err != nil {
		t.Fatal(err)
	}

	if err := w.svc.Reconcile(ctx, worldTenant, w.row.ID); err != nil {
		t.Fatal(err)
	}
	if cc := codeOf(t, w); cc.GetPhase() != cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_PENDING ||
		!strings.Contains(cc.GetMessage(), "network "+n.ID+"'s code change first") {
		t.Fatalf("its network first: %v", cc)
	}
}
