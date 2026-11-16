package rollout

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	"github.com/yindia/cluster-vending/pkg/upgrade"
)

var now = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC) // a Saturday

func cluster(name string, tier cloudv1alpha1.ClusterTier, version string) cloudv1alpha1.Cluster {
	scanned := metav1.NewTime(now.Add(-time.Hour))
	ready := metav1.NewTime(now.Add(-48 * time.Hour))
	return cloudv1alpha1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "tenant-acme",
			Labels: map[string]string{cloudstore.LabelID: "id-" + name, cloudstore.LabelTenant: "acme"}},
		Spec: cloudv1alpha1.ClusterSpec{ReleaseChannel: cloudv1alpha1.ReleaseChannelStable,
			UpgradeMode:  cloudv1alpha1.UpgradeModeAutomatic,
			ControlPlane: &cloudv1alpha1.ControlPlaneSpec{KubernetesVersion: version}},
		Status: cloudv1alpha1.ClusterStatus{Tier: tier, Phase: cloudv1alpha1.ClusterPhaseRunning, CurrentKubernetesVersion: version,
			Conditions: []metav1.Condition{
				{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Ready", LastTransitionTime: ready},
				{Type: cloudv1alpha1.ConditionPlatformComponentsReady, Status: metav1.ConditionTrue, Reason: "Ready", LastTransitionTime: ready},
			},
			Deprecations: &cloudv1alpha1.ClusterDeprecations{TargetVersion: "1.36", ScannedAt: &scanned}},
	}
}

func input(clusters ...cloudv1alpha1.Cluster) Input {
	rel := &cloudv1alpha1.Release{ObjectMeta: metav1.ObjectMeta{Name: "shipped-00000001"},
		Spec: cloudv1alpha1.ReleaseSpec{Content: cloudv1alpha1.ReleaseContent{Kubernetes: []cloudv1alpha1.KubernetesRelease{
			{Minor: "1.38", Channel: "alpha"}, {Minor: "1.37", Channel: "beta"}, {Minor: "1.36", Channel: "stable"}, {Minor: "1.35"},
		}}}}
	ch := &cloudv1alpha1.ReleaseChannel{ObjectMeta: metav1.ObjectMeta{Name: "stable"},
		Spec: cloudv1alpha1.ReleaseChannelSpec{Promotion: cloudv1alpha1.PromotionAutomatic,
			Policy: cloudv1alpha1.RolloutPolicy{MaxInProgress: 2, MaxFailed: 1, Soak: metav1.Duration{Duration: time.Hour}}},
		Status: cloudv1alpha1.ReleaseChannelStatus{Release: rel.Name, KubernetesMinor: "1.36"}}
	return Input{Channel: ch, Release: rel, Clusters: clusters, Now: now}
}

func rowOf(t *testing.T, res Result, name string) Row {
	t.Helper()
	for _, r := range res.Rows {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no row for %s in %+v", name, res.Rows)
	return Row{}
}

func TestComputeRowsSayWhereEachClusterIs(t *testing.T) {
	upgrading := cluster("moving", cloudv1alpha1.ClusterTier1, "1.35")
	start := metav1.NewTime(now.Add(-12 * time.Minute))
	upgrading.Status.Upgrade = &cloudv1alpha1.ClusterUpgrade{FromVersion: "1.35", ToVersion: "1.36", Phase: "ControlPlane", StartTime: &start}
	manual := cluster("manual", cloudv1alpha1.ClusterTier0, "1.35")
	manual.Spec.UpgradeMode = cloudv1alpha1.UpgradeModeManual
	ahead := cluster("ahead", cloudv1alpha1.ClusterTier2, "1.37")
	other := cluster("beta-one", cloudv1alpha1.ClusterTier2, "1.35")
	other.Spec.ReleaseChannel = cloudv1alpha1.ReleaseChannelBeta
	in := input(cluster("done", cloudv1alpha1.ClusterTier2, "1.36"), cluster("next", cloudv1alpha1.ClusterTier2, "1.35"),
		upgrading, manual, ahead, other)

	res := Compute(in)

	if len(res.Rows) != 4 {
		t.Fatalf("rows = %d, want 4 (ahead and the beta cluster are not members): %+v", len(res.Rows), res.Rows)
	}
	wantOrder := []string{"done", "next", "moving", "manual"} // tier 2 first, then by name
	for i, name := range wantOrder {
		if res.Rows[i].Name != name {
			t.Errorf("row %d = %s, want %s", i, res.Rows[i].Name, name)
		}
	}
	done := rowOf(t, res, "done")
	if done.State != upgrade.Succeeded || done.Target != "1.36" || !done.Since.Equal(now.Add(-48*time.Hour)) {
		t.Errorf("done = %+v", done)
	}
	next := rowOf(t, res, "next")
	if next.State != upgrade.Pending || next.Current != "1.35" || next.Target != "1.36" || next.Gate != nil {
		t.Errorf("next = %+v", next)
	}
	if next.ID != "id-next" || next.Tenant != "acme" || next.Key() != "tenant-acme/next" {
		t.Errorf("next identity = %+v", next)
	}
	moving := rowOf(t, res, "moving")
	if moving.State != upgrade.InProgress || moving.Target != "1.36" || !moving.Since.Equal(start.Time) {
		t.Errorf("moving = %+v", moving)
	}
	m := rowOf(t, res, "manual")
	if m.State != upgrade.Blocked || m.Gate == nil || m.Gate.Code != upgrade.GateManual {
		t.Errorf("manual = %+v", m)
	}
	if len(res.Rollout.WouldStart) != 1 || res.Rollout.WouldStart[0] != "tenant-acme/next" {
		t.Errorf("wouldStart = %v", res.Rollout.WouldStart)
	}
	if !res.Rollout.ReportOnly || !strings.Contains(res.Rollout.Waiting, "automatic upgrades are off") {
		t.Errorf("rollout = %+v", res.Rollout)
	}
}

func TestComputeTwoMinorsBehindHopsOne(t *testing.T) {
	old := cluster("old", cloudv1alpha1.ClusterTier2, "1.34")
	old.Status.Deprecations.TargetVersion = "1.35"
	gone := cluster("gone", cloudv1alpha1.ClusterTier2, "1.33")

	res := Compute(input(old, gone))

	if r := rowOf(t, res, "old"); r.State != upgrade.Pending || r.Target != "1.35" {
		t.Errorf("1.34 on a 1.36 channel moves to 1.35 first: %+v", r)
	}
	if r := rowOf(t, res, "gone"); r.State != upgrade.Blocked || r.Gate == nil || r.Gate.Code != GateNoHop {
		t.Errorf("1.33 when 1.34 is not offered: %+v", r)
	}
}

func TestComputeWithoutAReleaseSaysWhy(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*Input)
		want   string
	}{
		"no release yet": {func(in *Input) { in.Channel.Status.Release, in.Release = "", nil }, "offers no release yet"},
		"release gone":   {func(in *Input) { in.Release = nil }, "release shipped-00000001 is gone"},
		"no minor on the channel": {func(in *Input) { in.Channel.Name = "edge" },
			"puts no Kubernetes minor on channel edge"},
		"bad policy": {func(in *Input) { in.Channel.Spec.Policy.Soak = metav1.Duration{Duration: -time.Hour} }, "is negative"},
	} {
		t.Run(name, func(t *testing.T) {
			in := input(cluster("a", cloudv1alpha1.ClusterTier2, "1.35"))
			tc.mutate(&in)
			res := Compute(in)
			if !strings.Contains(res.Rollout.Waiting, tc.want) || len(res.Rows) != 0 {
				t.Errorf("waiting = %q, rows = %d; want %q and none", res.Rollout.Waiting, len(res.Rows), tc.want)
			}
		})
	}
}

// A freeze matches the cluster it names, or every cluster of its tenant,
// or of its channel (of one tier, with a tier), while its until has not
// passed, unless it lets the cluster through. A matched cluster is
// blocked with the freeze's reason, in words that say it is frozen.
func TestHoldsMatchClusters(t *testing.T) {
	past, future := metav1.NewTime(now.Add(-time.Minute)), metav1.NewTime(now.Add(time.Hour))
	c := cluster("prod", cloudv1alpha1.ClusterTier0, "1.35")
	tests := []struct {
		name string
		spec cloudv1alpha1.ReleaseHoldSpec
		want bool
	}{
		{"its ID", cloudv1alpha1.ReleaseHoldSpec{ClusterID: "id-prod"}, true},
		{"another ID", cloudv1alpha1.ReleaseHoldSpec{ClusterID: "id-dev"}, false},
		{"its ID, until later", cloudv1alpha1.ReleaseHoldSpec{ClusterID: "id-prod", Until: &future}, true},
		{"its ID, until passed", cloudv1alpha1.ReleaseHoldSpec{ClusterID: "id-prod", Until: &past}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hold := tt.spec
			hold.Reason = "quarter-end freeze"
			in := input(c)
			in.Holds = []cloudv1alpha1.ReleaseHold{{Spec: hold}}

			got := HoldOf(in.Holds, &c, now) != nil
			row := rowOf(t, Compute(in), "prod")

			if got != tt.want {
				t.Fatalf("HoldOf = %v, want %v", got, tt.want)
			}
			frozen := row.Gate != nil && row.Gate.Code == upgrade.GateFrozen
			if frozen != tt.want {
				t.Fatalf("row %+v: frozen %v, want %v", row, frozen, tt.want)
			}
			if frozen && (!strings.Contains(row.Gate.Message, "is frozen: quarter-end freeze") || strings.Contains(row.Gate.Message, "hold")) {
				t.Errorf("message %q", row.Gate.Message)
			}
		})
	}
}
