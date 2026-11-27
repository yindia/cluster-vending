package cluster

import (
	"testing"

	"github.com/yindia/cluster-vending/pkg/component"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// The platform pool's catalog fingerprint covers the tags its
// EC2NodeClass gives the nodes: Karpenter replaces the nodes when they
// change, so a change of them (a build that adds them, a label change) is
// a catalog change, admitted per channel and tier. A label the tags leave
// out (one Karpenter reserves) changes nothing.
func TestThePoolFingerprintCoversItsNodeTags(t *testing.T) {
	pool := &crossplaneaws.PlatformPoolInput{Families: []string{"m7g"}, MaxPods: 110}
	row := func(labels map[string]string) *model.Cluster {
		return &model.Cluster{Base: model.Base{ID: "c1"}, TenantID: "acme", Name: "prod", Labels: labels}
	}
	base := poolFingerprint(row(map[string]string{"team": "a"}), pool)

	if poolFingerprint(row(map[string]string{"team": "a"}), pool) != base {
		t.Fatal("the same pool and labels must fingerprint the same")
	}
	if poolFingerprint(row(map[string]string{"team": "b"}), pool) == base {
		t.Fatal("a label change changes the pool's node tags, so its fingerprint")
	}
	if poolFingerprint(row(map[string]string{"team": "a", "Name": "x"}), pool) != base {
		t.Fatal("a label the tags leave out must not change the fingerprint")
	}
	if poolFingerprint(row(map[string]string{"team": "a"}), pool) == poolFingerprintWithoutTags(pool) {
		t.Fatal("the fingerprint must differ from the pool's alone: a build that adds the tags is a catalog change")
	}
}

// poolFingerprintWithoutTags is the fingerprint builds before the node
// tags recorded for the pool.
func poolFingerprintWithoutTags(pool *crossplaneaws.PlatformPoolInput) string {
	return component.Fingerprint(pool)
}

// A pool whose node tags changed waits for the channel's admission (until
// then the pass does not write it, so it keeps its old tags); a new
// cluster, with nothing applied yet, takes them at once.
func TestAPoolTagChangeWaitsForItsTurn(t *testing.T) {
	pool := &crossplaneaws.PlatformPoolInput{Families: []string{"m7g"}}
	before := poolFingerprint(&model.Cluster{Base: model.Base{ID: "c1"}, TenantID: "acme", Name: "prod"}, pool)
	after := poolFingerprint(&model.Cluster{Base: model.Base{ID: "c1"}, TenantID: "acme", Name: "prod", Labels: map[string]string{"team": "a"}}, pool)

	f := with(catalogWorld(), func(f *catalogFacts) {
		f.Prev.Applied = map[string]string{"base": "b1", "mesh": "m1", "policy": "p1", poolEntry: before}
		f.Now[poolEntry] = catalogEntry{Hash: after, Sequence: poolSequence}
		f.Admitted = func() (bool, string) { return false, "waiting for tier 2" }
	})
	if d := decideCatalog(f); !d.poolHeld() || d.Status.GetPhase() != cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_PENDING {
		t.Fatalf("not admitted: held %v phase %v; want the pool held, pending", d.poolHeld(), d.Status.GetPhase())
	}

	f.Admitted = func() (bool, string) { return true, "" }
	if d := decideCatalog(f); d.Current != poolEntry {
		t.Fatalf("admitted: applying %q, want the pool", d.Current)
	}

	fresh := with(catalogWorld(), func(f *catalogFacts) {
		f.Prev = &cloudv1.CatalogChange{}
		f.Now[poolEntry] = catalogEntry{Hash: after, Sequence: poolSequence}
		f.Admitted = func() (bool, string) { return false, "" }
	})
	if d := decideCatalog(fresh); d.poolHeld() {
		t.Fatal("a new cluster's pool must get its tags at once")
	}
}
