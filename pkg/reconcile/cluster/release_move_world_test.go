package cluster

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// movePlatform is platformWorld's catalog with several releases, stable
// offering one of them, and a rollout that admits or not.
type movePlatform struct {
	*fakePlatform
	releases map[string]*cloudv1alpha1.Release
	admitted bool
}

func (m *movePlatform) Release(_ context.Context, name string) (*cloudv1alpha1.Release, error) {
	return m.releases[name], nil
}

func (m *movePlatform) MoveAdmitted(context.Context, string, string, string, string) (bool, string, error) {
	if m.admitted {
		return true, "", nil
	}
	return false, "waiting for tier 2 to finish", nil
}

func moveRelease(name, base, mesh string) *cloudv1alpha1.Release {
	return &cloudv1alpha1.Release{ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: cloudv1alpha1.ReleaseSpec{Content: cloudv1alpha1.ReleaseContent{Kubernetes: []cloudv1alpha1.KubernetesRelease{
			{Minor: "1.37", Components: []cloudv1alpha1.ComponentPin{{Name: "base", Version: base}, {Name: "mesh", Version: mesh}}}}}}}
}

// moveWorld is platformWorld with component upgrades on, an automatic
// cluster, and its components installed and deployed from 2026-11-a.
func moveWorld(t *testing.T) (world, *movePlatform, *model.Cluster) {
	t.Helper()
	mp := &movePlatform{admitted: true, releases: map[string]*cloudv1alpha1.Release{"2026-11-a": moveRelease("2026-11-a", "1.2.0", "2.0.0")}}
	w, src := platformWorld(t, func(d *reconcile.Deps) { d.ComponentUpgrades = true })
	mp.fakePlatform = src
	w.svc.deps.Platform = mp
	w.helmConfig(t)
	row := w.cluster(t)
	spec, err := SpecOf(row)
	if err != nil {
		t.Fatal(err)
	}
	spec.UpgradeMode = cloudv1.UpgradeMode_UPGRADE_MODE_AUTOMATIC
	if row.Spec, err = Encode(spec); err != nil {
		t.Fatal(err)
	}
	row = w.pass(t, row)
	w.deployed(t, "base")
	row = w.pass(t, row)
	w.deployed(t, "mesh")
	row = w.pass(t, row)
	if got := phases(storedPlatform(row)); got != "base=READY,mesh=READY" {
		t.Fatalf("installed from A: %s", got)
	}
	return w, mp, row
}

// pass installs the platform components of row and records what the pass
// found as row's status, as a reconcile does.
func (w world) pass(t *testing.T, row *model.Cluster) *model.Cluster {
	t.Helper()
	p, err := w.svc.installPlatform(t.Context(), row)
	if err != nil {
		t.Fatal(err)
	}
	st := &cloudv1.ClusterStatus{}
	withPlatform(st, p)
	if row.Status, err = Encode(st); err != nil {
		t.Fatal(err)
	}
	return row
}

func (w world) chartVersion(t *testing.T, name string) string {
	t.Helper()
	u := w.release(t, name)
	if u == nil {
		return ""
	}
	v, _, _ := unstructured.NestedString(u.Object, "spec", "forProvider", "chart", "version")
	return v
}

// A release that changes both components moves them one at a time, in
// install order: mesh (which requires base) takes its new version only
// once base runs its own and is Ready. The release recorded is the new
// one once both are.
func TestReleaseMoveOneComponentAtATime(t *testing.T) {
	w, mp, row := moveWorld(t)
	mp.releases["2026-12-a"] = moveRelease("2026-12-a", "1.3.0", "2.1.0")
	mp.offered = "2026-12-a"

	row = w.pass(t, row)

	move := storedPlatform(row).move
	if move.GetPhase() != moveComponents || move.GetComponent() != "base" || move.GetStep() != 1 || move.GetSteps() != 2 {
		t.Fatalf("move = %v", move)
	}
	if b, m := w.chartVersion(t, "base"), w.chartVersion(t, "mesh"); b != "1.3.0" || m != "2.0.0" {
		t.Fatalf("base %s, mesh %s: want base moved first, mesh kept", b, m)
	}
	if storedPlatform(row).release != "2026-11-a" {
		t.Errorf("release recorded = %s during the move, want the current one", storedPlatform(row).release)
	}

	w.deployed(t, "base")
	row = w.pass(t, row) // base settles at 1.3.0
	row = w.pass(t, row) // mesh starts

	if move := storedPlatform(row).move; move.GetComponent() != "mesh" || w.chartVersion(t, "mesh") != "2.1.0" {
		t.Fatalf("after base: move %v, mesh %s", move, w.chartVersion(t, "mesh"))
	}

	w.deployed(t, "mesh")
	row = w.pass(t, row) // mesh settles
	row = w.pass(t, row) // done

	if p := storedPlatform(row); p.release != "2026-12-a" || p.move.GetPhase() != moveIdle || p.move.GetCurrent() != "2026-12-a" {
		t.Fatalf("done: release %s, move %v", p.release, p.move)
	}
}

// With component upgrades off (the default) the cluster keeps the release
// it was installed from, as before.
func TestReleaseMoveOff(t *testing.T) {
	w, mp, row := moveWorld(t)
	w.svc.deps.ComponentUpgrades = false
	mp.releases["2026-12-a"] = moveRelease("2026-12-a", "1.3.0", "2.1.0")
	mp.offered = "2026-12-a"

	row = w.pass(t, row)

	if p := storedPlatform(row); p.release != "2026-11-a" || p.move != nil || w.chartVersion(t, "base") != "1.2.0" {
		t.Fatalf("off: release %s, move %v, base %s", p.release, p.move, w.chartVersion(t, "base"))
	}
}

// A cluster the channel's rollout does not admit yet waits, and changes
// nothing.
func TestReleaseMoveWaitsForTheRollout(t *testing.T) {
	w, mp, row := moveWorld(t)
	mp.admitted = false
	mp.releases["2026-12-a"] = moveRelease("2026-12-a", "1.3.0", "2.1.0")
	mp.offered = "2026-12-a"

	row = w.pass(t, row)

	move := storedPlatform(row).move
	if move.GetPhase() != movePending || move.GetMessage() != "waiting for tier 2 to finish" || w.chartVersion(t, "base") != "1.2.0" {
		t.Fatalf("not admitted: move %v, base %s", move, w.chartVersion(t, "base"))
	}
}

// A component only in the new release is installed at its turn; one only
// in the old one is kept until the move is done.
func TestReleaseMoveAddsAComponent(t *testing.T) {
	w, mp, row := moveWorld(t)
	extra := platformDef("extra", "mesh")
	mp.defs = append(mp.defs, extra)
	b := moveRelease("2026-12-a", "1.2.0", "2.0.0")
	b.Spec.Content.Kubernetes[0].Components = append(b.Spec.Content.Kubernetes[0].Components, cloudv1alpha1.ComponentPin{Name: "extra", Version: "0.1.0"})
	mp.releases["2026-12-a"] = b
	mp.offered = "2026-12-a"

	row = w.pass(t, row)

	if move := storedPlatform(row).move; move.GetComponent() != "extra" || move.GetSteps() != 1 || w.chartVersion(t, "extra") != "0.1.0" {
		t.Fatalf("move %v, extra %s", move, w.chartVersion(t, "extra"))
	}
}
