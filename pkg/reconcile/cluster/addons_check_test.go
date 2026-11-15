package cluster

import (
	"reflect"
	"testing"
	"time"

	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

func addonsSpec() *cloudv1.ClusterSpec {
	return &cloudv1.ClusterSpec{ControlPlane: &cloudv1.ControlPlaneSpec{KubernetesVersion: "1.37"},
		Provider: &cloudv1.ClusterSpec_Aws{Aws: &cloudv1.AwsClusterSpec{Addons: []*cloudv1.AwsAddon{
			{Name: "coredns", Version: "v1.12.0-eksbuild.1"},
			{Name: "vpc-cni", Version: "v1.20.0-eksbuild.1"},
			{Name: "kube-proxy", Version: "v1.37.0-eksbuild.1"},
		}}}}
}

// The add-ons move to what EKS recommends now, but a pinned add-on keeps
// the tenant's version, and one EKS says nothing of is left as it is.
func TestAddonChanges(t *testing.T) {
	recommended := map[string]string{"coredns": "v1.12.1-eksbuild.2", "vpc-cni": "v1.21.0-eksbuild.1", "kube-proxy": "v1.37.0-eksbuild.1"}

	got := addonChanges(addonsSpec(), map[string]string{"vpc-cni": "v1.20.0-eksbuild.1"}, recommended)

	want := []addonChange{{Name: "coredns", From: "v1.12.0-eksbuild.1", To: "v1.12.1-eksbuild.2"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("changes = %+v, want %+v", got, want)
	}
	if got := addonChanges(addonsSpec(), nil, map[string]string{}); len(got) != 0 {
		t.Errorf("no answer: %+v, want none", got)
	}
}

// The new versions are stored as a new, resolved generation the pass
// applies.
func TestWithAddonVersions(t *testing.T) {
	raw, err := Encode(addonsSpec())
	if err != nil {
		t.Fatal(err)
	}
	row := &model.Cluster{Resource: model.Resource{Generation: 4, State: StateRunning, Spec: raw}, ResolvedGeneration: 4}

	if err := withAddonVersions(row, []addonChange{{Name: "coredns", To: "v1.12.1-eksbuild.2"}}); err != nil {
		t.Fatal(err)
	}

	spec, err := SpecOf(row)
	if err != nil {
		t.Fatal(err)
	}
	if v := spec.GetAws().GetAddons()[0].GetVersion(); v != "v1.12.1-eksbuild.2" {
		t.Errorf("coredns at %s", v)
	}
	if v := spec.GetAws().GetAddons()[1].GetVersion(); v != "v1.20.0-eksbuild.1" {
		t.Errorf("vpc-cni changed to %s", v)
	}
	if row.Generation != 5 || row.ResolvedGeneration != 5 || row.State != StateUpdating {
		t.Errorf("row generation %d resolved %d state %s, want 5 5 %s", row.Generation, row.ResolvedGeneration, row.State, StateUpdating)
	}
}

// Only a Running, applied, unpaused cluster with no upgrade or release
// move under way has its add-ons checked, once a day.
func TestAddonsCheckable(t *testing.T) {
	running := func() *model.Cluster {
		return &model.Cluster{Resource: model.Resource{Generation: 2, AppliedGeneration: 2, State: StateRunning}}
	}
	upgrading := running()
	upgrading.Status, _ = Encode(&cloudv1.ClusterStatus{Upgrade: &cloudv1.ClusterUpgrade{ToVersion: "1.37"}})
	moving := running()
	moving.Status, _ = Encode(&cloudv1.ClusterStatus{ReleaseMove: &cloudv1.ClusterReleaseMove{Phase: moveComponents}})
	unapplied := running()
	unapplied.Generation = 3
	updating := running()
	updating.State = StateUpdating
	paused := running()
	paused.Paused = &model.Pause{}
	for name, tt := range map[string]struct {
		row  *model.Cluster
		want bool
	}{
		"running": {running(), true}, "upgrading": {upgrading, false}, "moving its release": {moving, false},
		"not applied": {unapplied, false}, "updating": {updating, false}, "paused": {paused, false},
	} {
		if got := addonsCheckable(tt.row); got != tt.want {
			t.Errorf("%s: %v, want %v", name, got, tt.want)
		}
	}

	at := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	c := &addonChecks{}
	if !c.due("c1", at) {
		t.Error("never checked: not due")
	}
	c.checkAgain("c1", at.Add(addonsCheckEvery))
	if c.due("c1", at.Add(23*time.Hour)) || !c.due("c1", at.Add(24*time.Hour)) {
		t.Error("checked: due again before a day, or not after it")
	}
	var none *addonChecks
	if none.due("c1", at) {
		t.Error("no checks kept: due")
	}
}
