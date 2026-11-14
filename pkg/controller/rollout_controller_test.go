package controller

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	platform "github.com/yindia/cluster-vending/pkg/reconcile"
)

var rolloutNow = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC) // a Saturday

// rolloutCluster is an automatic, Ready cluster on stable at version, with
// a clean deprecation scan against 1.36.
func rolloutCluster(name string, tier cloudv1alpha1.ClusterTier, version string) *cloudv1alpha1.Cluster {
	scanned := metav1.NewTime(rolloutNow.Add(-time.Hour))
	return &cloudv1alpha1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "tenant-acme",
			Labels: map[string]string{cloudstore.LabelID: "id-" + name}},
		Spec: cloudv1alpha1.ClusterSpec{ReleaseChannel: cloudv1alpha1.ReleaseChannelStable, Tier: tier,
			UpgradeMode:  cloudv1alpha1.UpgradeModeAutomatic,
			ControlPlane: &cloudv1alpha1.ControlPlaneSpec{KubernetesVersion: version}},
		Status: cloudv1alpha1.ClusterStatus{Phase: cloudv1alpha1.ClusterPhaseRunning, CurrentKubernetesVersion: version,
			Conditions: []metav1.Condition{
				{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Ready", LastTransitionTime: metav1.NewTime(rolloutNow.Add(-48 * time.Hour))},
				{Type: cloudv1alpha1.ConditionPlatformComponentsReady, Status: metav1.ConditionTrue, Reason: "Ready",
					LastTransitionTime: metav1.NewTime(rolloutNow.Add(-48 * time.Hour))},
			},
			Deprecations: &cloudv1alpha1.ClusterDeprecations{TargetVersion: "1.36", ScannedAt: &scanned}},
	}
}

// rolloutWorld is a release that puts 1.36 on stable, the stable channel
// offering it, and objects.
func rolloutWorld(t *testing.T, policy cloudv1alpha1.RolloutPolicy, objects ...client.Object) client.Client {
	t.Helper()
	rel := &cloudv1alpha1.Release{ObjectMeta: metav1.ObjectMeta{Name: "shipped-00000001"},
		Spec: cloudv1alpha1.ReleaseSpec{Content: cloudv1alpha1.ReleaseContent{Kubernetes: []cloudv1alpha1.KubernetesRelease{
			{Minor: "1.38", Channel: "alpha"}, {Minor: "1.37", Channel: "beta"}, {Minor: "1.36", Channel: "stable"}, {Minor: "1.35"},
		}}}}
	stable := &cloudv1alpha1.ReleaseChannel{ObjectMeta: metav1.ObjectMeta{Name: "stable"},
		Spec:   cloudv1alpha1.ReleaseChannelSpec{Promotion: cloudv1alpha1.PromotionAutomatic, Policy: policy},
		Status: cloudv1alpha1.ReleaseChannelStatus{Release: rel.Name, KubernetesMinor: "1.36"}}
	return releaseClient(t, append([]client.Object{rel, stable}, objects...)...)
}

type channelEventsSeen struct{ reasons []string }

func (e *channelEventsSeen) Event(_ context.Context, _ platform.Object, _, reason, _ string) {
	e.reasons = append(e.reasons, reason)
}

func planRollout(t *testing.T, c client.Client, now time.Time) (cloudv1alpha1.ReleaseChannelStatus, ctrl.Result, *channelEventsSeen) {
	t.Helper()
	events := &channelEventsSeen{}
	r := &RolloutReconciler{c: c, now: func() time.Time { return now }, events: events}
	res, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKey{Name: "stable"}})
	if err != nil {
		t.Fatal(err)
	}
	ch := &cloudv1alpha1.ReleaseChannel{}
	if err := c.Get(t.Context(), client.ObjectKey{Name: "stable"}, ch); err != nil {
		t.Fatal(err)
	}
	return ch.Status, res, events
}

func blockedBy(s cloudv1alpha1.ReleaseChannelStatus, cluster string) *cloudv1alpha1.BlockedCluster {
	for i := range s.Rollout.Blocked {
		if s.Rollout.Blocked[i].Cluster == cluster {
			return &s.Rollout.Blocked[i]
		}
	}
	return nil
}

var defaultPolicy = cloudv1alpha1.RolloutPolicy{MaxInProgress: 2, MaxFailed: 1, Soak: metav1.Duration{Duration: 24 * time.Hour}}

// Six clusters, two per tier, all on 1.35: tier 2's two would start, the
// others wait for it; nothing is changed on any cluster.
func TestRolloutStartsWithTier2(t *testing.T) {
	var objects []client.Object
	for _, c := range []struct {
		name string
		tier cloudv1alpha1.ClusterTier
	}{{"a0", cloudv1alpha1.ClusterTier0}, {"b0", cloudv1alpha1.ClusterTier0}, {"a1", cloudv1alpha1.ClusterTier1},
		{"b1", cloudv1alpha1.ClusterTier1}, {"a2", cloudv1alpha1.ClusterTier2}, {"b2", cloudv1alpha1.ClusterTier2}} {
		objects = append(objects, rolloutCluster(c.name, c.tier, "1.35"))
	}
	c := rolloutWorld(t, defaultPolicy, objects...)
	s, res, events := planRollout(t, c, rolloutNow)
	if s.Stage != cloudv1alpha1.ClusterTier2 || s.Rollout == nil || s.Rollout.TargetMinor != "1.36" || !s.Rollout.ReportOnly {
		t.Fatalf("status = %+v / %+v, want stage Tier2, target 1.36, report only", s, s.Rollout)
	}
	if want := []string{"tenant-acme/a2", "tenant-acme/b2"}; !reflect.DeepEqual(s.Rollout.WouldStart, want) {
		t.Errorf("wouldStart = %v, want %v", s.Rollout.WouldStart, want)
	}
	if s.Rollout.Waiting != "would start 2 clusters (automatic upgrades are off)" {
		t.Errorf("waiting = %q", s.Rollout.Waiting)
	}
	if len(s.Tiers) != 3 || s.Tiers[0].Tier != cloudv1alpha1.ClusterTier2 || s.Tiers[0].Total != 2 {
		t.Errorf("tiers = %+v, want three, Tier2 first with 2 clusters", s.Tiers)
	}
	if res.RequeueAfter <= 0 || res.RequeueAfter > rolloutRecheck {
		t.Errorf("requeue after %v, want at most %v", res.RequeueAfter, rolloutRecheck)
	}
	if !reflect.DeepEqual(events.reasons, []string{platform.ReasonRolloutStage}) {
		t.Errorf("events = %v, want one RolloutStage", events.reasons)
	}
	for _, name := range []string{"a2", "b0"} {
		got := &cloudv1alpha1.Cluster{}
		if err := c.Get(t.Context(), client.ObjectKey{Namespace: "tenant-acme", Name: name}, got); err != nil ||
			got.Spec.ControlPlane.KubernetesVersion != "1.35" {
			t.Errorf("cluster %s: version %q (%v), want it untouched at 1.35", name, got.Spec.ControlPlane.KubernetesVersion, err)
		}
	}
	// A second pass changes nothing and reports nothing again.
	if _, _, again := planRollout(t, c, rolloutNow); len(again.reasons) != 0 {
		t.Errorf("second pass events = %v, want none", again.reasons)
	}
}

// A tier-2 cluster whose window is closed is passed over: once the other
// tier-2 cluster is done and the soak passed, tier 1 starts. A manual
// cluster, one without a scan for 1.36 and one ahead of the channel are
// kept out or blocked with why.
func TestRolloutPassesOverBlockedClusters(t *testing.T) {
	closed := rolloutCluster("window", cloudv1alpha1.ClusterTier2, "1.35")
	closed.Spec.MaintenanceWindow = &cloudv1alpha1.MaintenanceWindow{Days: []cloudv1alpha1.Weekday{"Monday"}, Start: "02:00", Duration: "4h"}
	done := rolloutCluster("done", cloudv1alpha1.ClusterTier2, "1.36")
	manual := rolloutCluster("manual", cloudv1alpha1.ClusterTier1, "1.35")
	manual.Spec.UpgradeMode = cloudv1alpha1.UpgradeModeManual
	unscanned := rolloutCluster("unscanned", cloudv1alpha1.ClusterTier1, "1.35")
	unscanned.Status.Deprecations = nil
	ahead := rolloutCluster("ahead", cloudv1alpha1.ClusterTier1, "1.37")
	next := rolloutCluster("next", cloudv1alpha1.ClusterTier1, "1.35")
	c := rolloutWorld(t, defaultPolicy, closed, done, manual, unscanned, ahead, next)
	s, res, _ := planRollout(t, c, rolloutNow)

	if want := []string{"tenant-acme/next"}; !reflect.DeepEqual(s.Rollout.WouldStart, want) || s.Stage != cloudv1alpha1.ClusterTier1 {
		t.Errorf("stage %s, wouldStart = %v, want Tier1 and %v", s.Stage, s.Rollout.WouldStart, want)
	}
	for cluster, gate := range map[string]string{"tenant-acme/window": "MAINTENANCE_WINDOW", "tenant-acme/manual": "MANUAL",
		"tenant-acme/unscanned": "DEPRECATIONS"} {
		b := blockedBy(s, cluster)
		if b == nil || b.Gate != gate || b.Message == "" {
			t.Errorf("%s blocked = %+v, want gate %s with a message", cluster, b, gate)
		}
	}
	if b := blockedBy(s, "tenant-acme/window"); b != nil && !strings.Contains(b.Message, "Mon 16 Nov 02:00") {
		t.Errorf("window message %q, want the next opening (Monday 02:00)", b.Message)
	}
	if b := blockedBy(s, "tenant-acme/ahead"); b != nil {
		t.Errorf("a cluster ahead of the channel is not part of the rollout: %+v", b)
	}
	// The window opens Monday 02:00, in 38h; the recheck comes first.
	if res.RequeueAfter != rolloutRecheck {
		t.Errorf("requeue after %v, want %v", res.RequeueAfter, rolloutRecheck)
	}
}

// Tier 2 finished an hour ago: tier 1 waits for the soak, and the channel
// is planned again when it ends (here in 5m, before the 10m recheck).
func TestRolloutWaitsForTheSoak(t *testing.T) {
	done := rolloutCluster("done", cloudv1alpha1.ClusterTier2, "1.36")
	done.Status.Conditions[0].LastTransitionTime = metav1.NewTime(rolloutNow.Add(-time.Hour))
	c := rolloutWorld(t, cloudv1alpha1.RolloutPolicy{MaxInProgress: 2, MaxFailed: 1, Soak: metav1.Duration{Duration: 65 * time.Minute}},
		done, rolloutCluster("t1", cloudv1alpha1.ClusterTier1, "1.35"))
	s, res, _ := planRollout(t, c, rolloutNow)
	if len(s.Rollout.WouldStart) != 0 || !strings.Contains(s.Rollout.Waiting, "after the soak") {
		t.Errorf("wouldStart %v, waiting %q, want the soak", s.Rollout.WouldStart, s.Rollout.Waiting)
	}
	if res.RequeueAfter > 6*time.Minute || res.RequeueAfter < 4*time.Minute {
		t.Errorf("requeue after %v, want about 5m (the soak's end)", res.RequeueAfter)
	}
	// After the soak, tier 1 would start.
	if s, _, _ := planRollout(t, c, rolloutNow.Add(6*time.Minute)); !reflect.DeepEqual(s.Rollout.WouldStart, []string{"tenant-acme/t1"}) {
		t.Errorf("after the soak wouldStart = %v, want t1", s.Rollout.WouldStart)
	}
}

// More failures in a tier than maxFailed halt the channel: nothing would
// start, and a RolloutHalted event says why.
func TestRolloutHaltsOnFailures(t *testing.T) {
	failed := func(name string) *cloudv1alpha1.Cluster {
		c := rolloutCluster(name, cloudv1alpha1.ClusterTier2, "1.35")
		c.Status.Phase = cloudv1alpha1.ClusterPhaseFailed
		c.Status.Upgrade = &cloudv1alpha1.ClusterUpgrade{FromVersion: "1.35", ToVersion: "1.36", Phase: cloudv1alpha1.ClusterUpgradeControlPlane}
		return c
	}
	c := rolloutWorld(t, defaultPolicy, failed("f1"), failed("f2"), rolloutCluster("p", cloudv1alpha1.ClusterTier2, "1.35"))
	s, _, events := planRollout(t, c, rolloutNow)
	if !s.Rollout.Halted || len(s.Rollout.WouldStart) != 0 || !strings.Contains(s.Rollout.Waiting, "halted") {
		t.Errorf("rollout = %+v, want halted with nothing to start", s.Rollout)
	}
	if !reflect.DeepEqual(events.reasons, []string{platform.ReasonRolloutHalted}) {
		t.Errorf("events = %v, want RolloutHalted", events.reasons)
	}
}

// A policy the rollout cannot read is reported, not planned.
func TestRolloutReportsAnUnreadablePolicy(t *testing.T) {
	c := rolloutWorld(t, cloudv1alpha1.RolloutPolicy{MaxInProgress: 1,
		SoakPerTier: map[cloudv1alpha1.ClusterTier]metav1.Duration{"Tier9": {Duration: time.Hour}}},
		rolloutCluster("a", cloudv1alpha1.ClusterTier2, "1.35"))
	s, _, _ := planRollout(t, c, rolloutNow)
	var planned *metav1.Condition
	for i := range s.Conditions {
		if s.Conditions[i].Type == ConditionRolloutPlanned {
			planned = &s.Conditions[i]
		}
	}
	if planned == nil || planned.Status != metav1.ConditionFalse || !strings.Contains(planned.Message, "Tier9") ||
		len(s.Rollout.WouldStart) != 0 {
		t.Errorf("condition %+v, rollout %+v, want RolloutPlanned False naming Tier9", planned, s.Rollout)
	}
}

// Only the fields the rollout reads wake it.
func TestRolloutInputChanged(t *testing.T) {
	was := rolloutCluster("a", cloudv1alpha1.ClusterTier2, "1.35")
	same := was.DeepCopy()
	same.ResourceVersion, same.Status.Endpoint = "2", &cloudv1alpha1.ClusterEndpoint{URL: "https://x"}
	moved := was.DeepCopy()
	moved.Status.CurrentKubernetesVersion = "1.36"
	if !reflect.DeepEqual(rolloutInput(was), rolloutInput(same)) {
		t.Error("a change the rollout does not read counts as an input change")
	}
	if reflect.DeepEqual(rolloutInput(was), rolloutInput(moved)) {
		t.Error("the running version changing is not an input change")
	}
}
