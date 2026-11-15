package cluster

import (
	"slices"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// lagging plays the cloud providers for a world: every recorded object is
// Ready and caught up, and reports the version (EKS cluster, node group)
// or add-on version it asked for one look before, as a provider that
// sees the cloud at its poll interval does.
type lagging struct {
	w world
	// asked are the versions each object asked for at the last look.
	asked map[string]string
}

func (l *lagging) look(t *testing.T) {
	t.Helper()
	ctx := t.Context()
	records, err := l.w.store.ManagedResources().ListByOwner(ctx, model.OwnerCluster, l.w.row.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range records {
		gv, err := schema.ParseGroupVersion(r.APIVersion)
		if err != nil {
			t.Fatal(err)
		}
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(gv.WithKind(r.Kind))
		if err := l.w.kube.Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: r.Name}, u); err != nil {
			continue // not applied (yet)
		}
		field := map[string][]string{"Cluster": {"version"}, KindNodeGroup: {"version"}, "Addon": {"addonVersion"}}[r.Kind]
		if field != nil {
			key := r.Kind + "/" + r.Name
			if before, ok := l.asked[key]; ok && before != "" {
				_ = unstructured.SetNestedField(u.Object, before, append([]string{"status", "atProvider"}, field...)...)
			}
			now, _, _ := unstructured.NestedString(u.Object, append([]string{"spec", "forProvider"}, field...)...)
			l.asked[key] = now
		}
		_ = unstructured.SetNestedSlice(u.Object, []any{
			map[string]any{"type": "Ready", "status": "True", "reason": "Available", "observedGeneration": u.GetGeneration(),
				"lastTransitionTime": time.Now().UTC().Format(time.RFC3339)},
			map[string]any{"type": "Synced", "status": "True", "reason": "ReconcileSuccess", "observedGeneration": u.GetGeneration(),
				"lastTransitionTime": time.Now().UTC().Format(time.RFC3339)},
		}, "status", "conditions")
		if err := l.w.kube.Update(ctx, u); err != nil {
			t.Fatalf("%s %s: %v", r.Kind, r.Name, err)
		}
	}
}

// A two-hop upgrade, 1.35 → 1.36 → 1.37, driven pass by pass with
// providers that report one look late, goes through every step in order,
// never applies a step's change before its turn (node groups wait for the
// control plane, add-ons for the node groups), and finishes each hop: the
// upgrade is cleared, so the next hop can start.
func TestATwoHopUpgradeRunsEveryStepInOrder(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{}, nil)
	ctx := t.Context()
	l := &lagging{w: w, asked: map[string]string{}}
	settle := func() {
		for range 4 {
			if err := w.svc.Reconcile(ctx, worldTenant, w.row.ID); err != nil {
				t.Fatal(err)
			}
			l.look(t)
		}
	}
	setVersion := func(version, addonVersion string) {
		row := w.cluster(t)
		spec, err := SpecOf(row)
		if err != nil {
			t.Fatal(err)
		}
		spec.ControlPlane.KubernetesVersion = version
		// What resolve stores for the new minor: the add-on versions EKS
		// recommends for it.
		for _, a := range spec.GetAws().GetAddons() {
			a.Version = addonVersion
		}
		if row.Spec, err = Encode(spec); err != nil {
			t.Fatal(err)
		}
		row.Generation++
		row.State = StateUpdating
		row.ResolvedGeneration = row.Generation
		if err := w.store.Clusters().Update(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	// The cluster runs 1.35 with its add-ons at their 1.35 versions.
	setVersion("1.35", "v1.35-eksbuild.1")
	settle()
	if u := storedUpgrade(w.cluster(t)); u != nil {
		t.Fatalf("an upgrade before any was asked for: %v", u)
	}

	for _, hop := range []struct{ to, addons string }{
		{"1.36", "v1.36-eksbuild.1"},
		{"1.37", "v1.37-eksbuild.1"},
	} {
		setVersion(hop.to, hop.addons)
		var steps []cloudv1.ClusterUpgradePhase
		for pass := 0; pass < 60; pass++ {
			if err := w.svc.Reconcile(ctx, worldTenant, w.row.ID); err != nil {
				t.Fatal(err)
			}
			u := storedUpgrade(w.cluster(t))
			if u == nil {
				break
			}
			if len(steps) == 0 || steps[len(steps)-1] != u.GetPhase() {
				steps = append(steps, u.GetPhase())
			}
			l.assertNothingAhead(t, u.GetPhase(), hop.to, hop.addons)
			l.look(t)
		}
		want := []cloudv1.ClusterUpgradePhase{controlPlane, stepNodes, stepAddons, stepPlatform, stepTenant}
		if !slices.Equal(steps, want) {
			t.Fatalf("hop to %s went through %v, want %v", hop.to, steps, want)
		}
		if u := storedUpgrade(w.cluster(t)); u != nil {
			t.Fatalf("hop to %s did not finish: %v", hop.to, u)
		}
		if got := storedStatus(w.cluster(t)).GetCurrentKubernetesVersion(); !sameMinor(got, hop.to) {
			t.Fatalf("after the hop to %s the cluster reports %q", hop.to, got)
		}
	}
}

// assertNothingAhead fails when a step's change is applied before its
// turn: node groups ahead of the control plane step, add-ons on the new
// minor's versions before the add-ons' step.
func (l *lagging) assertNothingAhead(t *testing.T, step cloudv1.ClusterUpgradePhase, to, addons string) {
	t.Helper()
	for key, asked := range l.asked {
		kind, _, _ := cutKind(key)
		switch {
		case kind == KindNodeGroup && step == controlPlane && sameMinor(asked, to):
			t.Errorf("at %s the node group already asked for %s", step, asked)
		case kind == "Addon" && asked == addons && !reached(step, stepAddonsPhase):
			t.Errorf("at %s an add-on already asked for %s", step, asked)
		}
	}
}

func cutKind(key string) (kind, name string, ok bool) {
	for i := range key {
		if key[i] == '/' {
			return key[:i], key[i+1:], true
		}
	}
	return key, "", false
}
