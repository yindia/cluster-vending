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

func record(result cloudv1alpha1.ClusterRolloutResult, from, to string, started time.Time) *cloudv1alpha1.ClusterRolloutRecord {
	return &cloudv1alpha1.ClusterRolloutRecord{Channel: "stable", FromVersion: from, ToVersion: to,
		StartTime: metav1.NewTime(started), Result: result, Tier: 2}
}

// The record the rollout wrote decides a cluster's state: started is in
// progress before the cluster reports an upgrade (so maxInProgress
// holds), a failure stays a failure (so maxFailed holds), and a success
// gives the soak its exact start.
func TestComputeReadsTheRolloutRecord(t *testing.T) {
	started := cluster("started", cloudv1alpha1.ClusterTier2, "1.35")
	started.Status.Rollout = record(cloudv1alpha1.ClusterRolloutInProgress, "1.35", "1.36", now.Add(-5*time.Minute))
	failed := cluster("failed", cloudv1alpha1.ClusterTier2, "1.35")
	failed.Status.Rollout = record(cloudv1alpha1.ClusterRolloutFailed, "1.35", "1.36", now.Add(-time.Hour))
	finished := now.Add(-20 * time.Minute)
	done := cluster("done", cloudv1alpha1.ClusterTier2, "1.36")
	done.Status.Rollout = record(cloudv1alpha1.ClusterRolloutSucceeded, "1.35", "1.36", now.Add(-time.Hour))
	done.Status.Rollout.FinishTime = &metav1.Time{Time: finished}
	blocked := cluster("planned", cloudv1alpha1.ClusterTier2, "1.35")
	blocked.Status.Rollout = record(cloudv1alpha1.ClusterRolloutBlocked, "1.35", "1.36", now.Add(-time.Hour))

	res := Compute(input(started, failed, done, blocked))

	if r := rowOf(t, res, "started"); r.State != upgrade.InProgress || r.Target != "1.36" || !r.Since.Equal(now.Add(-5*time.Minute)) {
		t.Errorf("started = %+v", r)
	}
	if r := rowOf(t, res, "failed"); r.State != upgrade.Failed {
		t.Errorf("failed = %+v", r)
	}
	if r := rowOf(t, res, "done"); r.State != upgrade.Succeeded || !r.Since.Equal(finished) {
		t.Errorf("done = %+v, want succeeded at the record's finish %v", r, finished)
	}
	if r := rowOf(t, res, "planned"); r.State != upgrade.Pending {
		t.Errorf("a planned record is only a report: %+v", r)
	}
}

// An upgrade in progress ends by its own facts: the cluster runs the
// target (succeeded), the upgrade failed, or it took too long.
func TestFinish(t *testing.T) {
	const timeout = 90 * time.Minute
	tests := []struct {
		name   string
		mutate func(*cloudv1alpha1.Cluster)
		want   cloudv1alpha1.ClusterRolloutResult
	}{
		{"still upgrading", func(c *cloudv1alpha1.Cluster) {
			c.Status.Upgrade = &cloudv1alpha1.ClusterUpgrade{Phase: "ControlPlane", ToVersion: "1.36"}
		}, ""},
		{"runs the target", func(c *cloudv1alpha1.Cluster) { c.Status.CurrentKubernetesVersion = "1.36" }, cloudv1alpha1.ClusterRolloutSucceeded},
		{"target but nodes still upgrading", func(c *cloudv1alpha1.Cluster) {
			c.Status.CurrentKubernetesVersion = "1.36"
			c.Status.Upgrade = &cloudv1alpha1.ClusterUpgrade{Phase: "NodesAndAddons", ToVersion: "1.36"}
		}, ""},
		{"control plane on the target, a node group the provider still sees behind", func(c *cloudv1alpha1.Cluster) {
			c.Status.CurrentKubernetesVersion = "1.36"
			c.Status.Upgrade = &cloudv1alpha1.ClusterUpgrade{Phase: "NodesAndAddons", ToVersion: "1.36",
				NodeGroups: []cloudv1alpha1.ClusterUpgradeNodeGroup{{Name: "default", FromVersion: "1.35", ToVersion: "1.36", State: "Updating"}}}
		}, ""},
		{"failed", func(c *cloudv1alpha1.Cluster) {
			c.Status.Phase, c.Status.Message = cloudv1alpha1.ClusterPhaseFailed, "UpdateClusterVersion: InvalidRequest"
			c.Status.Upgrade = &cloudv1alpha1.ClusterUpgrade{Phase: "ControlPlane", ToVersion: "1.36"}
		}, cloudv1alpha1.ClusterRolloutFailed},
		{"no progress in one step for too long", func(c *cloudv1alpha1.Cluster) {
			c.Status.Rollout.StartTime = metav1.NewTime(now.Add(-3 * timeout))
			stalled := metav1.NewTime(now.Add(-timeout - time.Minute))
			c.Status.Upgrade = &cloudv1alpha1.ClusterUpgrade{Phase: "Addons", ToVersion: "1.36", PhaseStartTime: &stalled}
		}, cloudv1alpha1.ClusterRolloutTimedOut},
		{"a long upgrade that keeps moving is not timed out", func(c *cloudv1alpha1.Cluster) {
			c.Status.Rollout.StartTime = metav1.NewTime(now.Add(-3 * timeout))
			moving := metav1.NewTime(now.Add(-time.Minute))
			c.Status.Upgrade = &cloudv1alpha1.ClusterUpgrade{Phase: "PlatformComponents", ToVersion: "1.36", PhaseStartTime: &moving}
		}, ""},
		{"the add-ons halted after their recoveries", func(c *cloudv1alpha1.Cluster) {
			moving := metav1.NewTime(now.Add(-time.Minute))
			c.Status.Upgrade = &cloudv1alpha1.ClusterUpgrade{Phase: "Addons", ToVersion: "1.36", PhaseStartTime: &moving,
				Recoveries: 3, Halted: true, Message: "halted: the add-ons coredns did not settle after 3 recoveries: UPDATE_FAILED"}
		}, cloudv1alpha1.ClusterRolloutFailed},
		{"never seen upgrading for too long", func(c *cloudv1alpha1.Cluster) {
			c.Status.Rollout.StartTime = metav1.NewTime(now.Add(-timeout - time.Minute))
		}, cloudv1alpha1.ClusterRolloutTimedOut},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := cluster("c", cloudv1alpha1.ClusterTier2, "1.35")
			c.Status.Rollout = record(cloudv1alpha1.ClusterRolloutInProgress, "1.35", "1.36", now.Add(-10*time.Minute))
			tt.mutate(&c)

			got := Finish(&c, now, timeout)

			switch {
			case tt.want == "" && got != nil:
				t.Fatalf("finished as %+v, want still in progress", got)
			case tt.want == "":
			case got == nil || got.Result != tt.want || got.FinishTime == nil:
				t.Fatalf("finish = %+v, want %s with a finish time", got, tt.want)
			case tt.want != cloudv1alpha1.ClusterRolloutSucceeded && got.Message == "":
				t.Errorf("a %s record says why: %+v", tt.want, got)
			}
		})
	}
	idle := cluster("idle", cloudv1alpha1.ClusterTier2, "1.35")
	if Finish(&idle, now, timeout) != nil {
		t.Error("a cluster with no upgrade in progress has nothing to finish")
	}
}

// A record the rollout never started (Pending, Blocked) is closed once the
// cluster runs its target with no upgrade in progress, however the cluster
// got there (an update a person made, a controller that did not record the
// start): it says so, and is Succeeded with when it was seen.
func TestFinishClosesARecordTheClusterReachedOtherwise(t *testing.T) {
	const timeout = 90 * time.Minute
	for _, result := range []cloudv1alpha1.ClusterRolloutResult{cloudv1alpha1.ClusterRolloutPending, cloudv1alpha1.ClusterRolloutBlocked} {
		t.Run(string(result), func(t *testing.T) {
			c := cluster("c", cloudv1alpha1.ClusterTier2, "1.36")
			c.Status.Rollout = record(result, "1.35", "1.36", now.Add(-time.Hour))

			got := Finish(&c, now, timeout)

			if got == nil || got.Result != cloudv1alpha1.ClusterRolloutSucceeded || got.FinishTime == nil ||
				!strings.Contains(got.Message, "reached by an update") {
				t.Fatalf("Finish = %+v, want Succeeded, finished now, saying it was reached by an update", got)
			}
		})
	}

	behind := cluster("behind", cloudv1alpha1.ClusterTier2, "1.35")
	behind.Status.Rollout = record(cloudv1alpha1.ClusterRolloutPending, "1.35", "1.36", now.Add(-time.Hour))
	if got := Finish(&behind, now, timeout); got != nil {
		t.Errorf("a pending record of a cluster still behind was closed: %+v", got)
	}
	upgradingNow := cluster("upgrading", cloudv1alpha1.ClusterTier2, "1.36")
	upgradingNow.Status.Rollout = record(cloudv1alpha1.ClusterRolloutPending, "1.35", "1.36", now.Add(-time.Hour))
	upgradingNow.Status.Upgrade = &cloudv1alpha1.ClusterUpgrade{Phase: "NodeGroups", ToVersion: "1.36"}
	if got := Finish(&upgradingNow, now, timeout); got != nil {
		t.Errorf("a pending record was closed while the cluster upgrades: %+v", got)
	}
}

// Before it starts, the record says where the cluster is in the rollout,
// in the words cluster get shows.
func TestPlanned(t *testing.T) {
	next := cluster("a-next", cloudv1alpha1.ClusterTier2, "1.35")
	waits := cluster("b-waits", cloudv1alpha1.ClusterTier2, "1.35")
	window := cluster("c-window", cloudv1alpha1.ClusterTier2, "1.35")
	window.Spec.MaintenanceWindow = &cloudv1alpha1.MaintenanceWindow{Days: []cloudv1alpha1.Weekday{cloudv1alpha1.Weekday("Monday")},
		Start: "02:00", Duration: "4h"}
	manual := cluster("d-manual", cloudv1alpha1.ClusterTier2, "1.35")
	manual.Spec.UpgradeMode = cloudv1alpha1.UpgradeModeManual
	in := input(next, waits, window, manual)
	in.Channel.Spec.Policy.MaxInProgress = 1

	res := Compute(in)
	planned := func(name string) *cloudv1alpha1.ClusterRolloutRecord {
		t.Helper()
		for i := range in.Clusters {
			if in.Clusters[i].Name == name {
				return Planned(rowOf(t, res, name), &in.Clusters[i], res, now)
			}
		}
		t.Fatalf("no cluster %s", name)
		return nil
	}

	tests := []struct {
		name   string
		result cloudv1alpha1.ClusterRolloutResult
		says   string
	}{
		{"a-next", cloudv1alpha1.ClusterRolloutPending, "automatic upgrades are off (would start)"},
		{"b-waits", cloudv1alpha1.ClusterRolloutPending, "waiting its turn in tier 2"},
		{"c-window", cloudv1alpha1.ClusterRolloutBlocked, "maintenance window"},
		{"d-manual", cloudv1alpha1.ClusterRolloutBlocked, "manual upgrades: 1.36 is available, not started"},
	}
	for _, tt := range tests {
		got := planned(tt.name)
		if got == nil || got.Result != tt.result || !strings.Contains(got.Message, tt.says) ||
			got.FromVersion != "1.35" || got.ToVersion != "1.36" || got.Channel != "stable" || got.Tier != 2 {
			t.Errorf("%s: %+v, want %s saying %q", tt.name, got, tt.result, tt.says)
		}
	}
}

// A planned record is written only when it changes, and never over an
// upgrade the rollout started or one that failed.
func TestPlannedKeepsWhatHappened(t *testing.T) {
	c := cluster("c", cloudv1alpha1.ClusterTier2, "1.35")
	c.Status.Rollout = record(cloudv1alpha1.ClusterRolloutFailed, "1.35", "1.36", now.Add(-time.Hour))
	res := Compute(input(c))

	if got := Planned(rowOf(t, res, "c"), &c, res, now); got != nil {
		t.Errorf("a failed upgrade was replaced by %+v", got)
	}

	c.Status.Rollout = nil
	res = Compute(input(c))
	first := Planned(rowOf(t, res, "c"), &c, res, now)
	c.Status.Rollout = first
	again := Planned(rowOf(t, res, "c"), &c, res, now.Add(time.Minute))

	if first == nil || again != nil {
		t.Errorf("first = %+v, again = %+v; want one write, then none", first, again)
	}
}

// A cluster kept back by its own check does not end the rollout: the
// channel is not done while it is below the target.
func TestComputeIsNotDoneWhileAClusterIsBlocked(t *testing.T) {
	blocked := cluster("blocked", cloudv1alpha1.ClusterTier2, "1.35")
	blocked.Spec.UpgradeMode = cloudv1alpha1.UpgradeModeManual

	res := Compute(input(blocked, cluster("done", cloudv1alpha1.ClusterTier1, "1.36")))

	if res.Rollout.Done || res.Tier != cloudv1alpha1.ClusterTier2 {
		t.Errorf("done = %v, tier = %s; want not done, at Tier2", res.Rollout.Done, res.Tier)
	}
	if !strings.Contains(res.Rollout.Waiting, "1 cluster blocked: manual upgrades") {
		t.Errorf("waiting = %q", res.Rollout.Waiting)
	}
}

// A platform component being removed (its definition was switched off)
// does not keep a cluster from upgrading; one that failed does.
func TestPlatformReadyIgnoresComponentsBeingRemoved(t *testing.T) {
	tests := []struct {
		phase   string
		blocked bool
	}{
		{"Deleting", false}, {"Failed", true}, {"Installing", true}, {"Pending", true},
	}
	for _, tt := range tests {
		t.Run(tt.phase, func(t *testing.T) {
			c := cluster("c", cloudv1alpha1.ClusterTier2, "1.35")
			c.Status.Conditions[1].Status = metav1.ConditionFalse
			c.Status.PlatformComponents = []cloudv1alpha1.PlatformComponentStatus{{Name: "karpenter", Phase: "Ready"},
				{Name: "kyverno", Phase: tt.phase}}

			r := rowOf(t, Compute(input(c)), "c")

			if blocked := r.Gate != nil && r.Gate.Code == upgrade.GatePlatform; blocked != tt.blocked {
				t.Errorf("blocked by the platform = %v, want %v (%+v)", blocked, tt.blocked, r)
			}
		})
	}
}

// Tenant components come after the platform's: an automatic Kubernetes
// upgrade waits until every enabled one of the cluster is Ready. One
// being removed does not hold it back, nor does another cluster's.
func TestTenantComponentsNotReadyHoldTheUpgrade(t *testing.T) {
	tests := []struct {
		phase, cluster string
		blocked        bool
	}{
		{"Ready", "id-c", false}, {"Deleting", "id-c", false}, {"Installing", "id-c", true},
		{"Pending", "id-c", true}, {"Failed", "id-c", true}, {"", "id-c", true}, {"Failed", "id-other", false},
	}
	for _, tt := range tests {
		t.Run(tt.phase+"/"+tt.cluster, func(t *testing.T) {
			in := input(cluster("c", cloudv1alpha1.ClusterTier2, "1.35"))
			in.Components = []cloudv1alpha1.Component{{
				ObjectMeta: metav1.ObjectMeta{Name: "grafana", Namespace: "tenant-acme",
					Labels: map[string]string{cloudstore.LabelClusterID: tt.cluster}},
				Status: cloudv1alpha1.ComponentStatus{Phase: tt.phase}}}

			r := rowOf(t, Compute(in), "c")

			if blocked := r.Gate != nil && r.Gate.Code == upgrade.GateTenant; blocked != tt.blocked {
				t.Errorf("blocked by a tenant component = %v, want %v (%+v)", blocked, tt.blocked, r)
			}
		})
	}
}

// A cluster held on purpose (a freeze, UpgradePaused) does not advance
// toward TimedOut however long the hold lasts, and gets the time it had
// left once it is released.
func TestFinishHeldClusterDoesNotTimeOut(t *testing.T) {
	const timeout = 90 * time.Minute
	stalled := func() cloudv1alpha1.Cluster {
		c := cluster("c", cloudv1alpha1.ClusterTier2, "1.35")
		c.Status.Rollout = record(cloudv1alpha1.ClusterRolloutInProgress, "1.35", "1.36", now.Add(-3*timeout))
		return c
	}
	paused := stalled()
	paused.Status.Conditions = []metav1.Condition{{Type: "UpgradePaused", Status: metav1.ConditionTrue}}
	for name, c := range map[string]struct {
		c      cloudv1alpha1.Cluster
		frozen bool
	}{"freeze": {stalled(), true}, "UpgradePaused": {paused, false}} {
		t.Run(name, func(t *testing.T) {
			got := FinishHeld(&c.c, now, timeout, c.frozen)
			if got != nil && (got.Result != cloudv1alpha1.ClusterRolloutInProgress || got.FinishTime != nil) {
				t.Fatalf("held cluster finished as %+v, want InProgress", got)
			}
			if got == nil || !got.StartTime.Time.Equal(now) {
				t.Fatalf("the step clock is not moved up to now: %+v", got)
			}
			// Released a little later: the refreshed clock gives it the time left.
			c.c.Status.Rollout = got
			c.c.Status.Conditions = nil
			if r := FinishHeld(&c.c, now.Add(timeout-time.Minute), timeout, false); r != nil {
				t.Errorf("after the hold it timed out early: %+v", r)
			}
			r := FinishHeld(&c.c, now.Add(timeout+time.Minute), timeout, false)
			if r == nil || r.Result != cloudv1alpha1.ClusterRolloutTimedOut {
				t.Errorf("after the remaining time it should time out, got %+v", r)
			}
		})
	}
	// A hold alone does not hide a success.
	done := stalled()
	done.Status.CurrentKubernetesVersion = "1.36"
	if got := FinishHeld(&done, now, timeout, true); got == nil || got.Result != cloudv1alpha1.ClusterRolloutSucceeded {
		t.Errorf("held but done = %+v, want Succeeded", got)
	}
}

// A cluster being deleted keeps no slot in the rollout and its upgrade
// record neither times out nor counts as failed.
func TestDeletingClusterLeavesTheRollout(t *testing.T) {
	const timeout = 90 * time.Minute
	del := cluster("gone", cloudv1alpha1.ClusterTier2, "1.35")
	del.Status.Phase = cloudv1alpha1.ClusterPhaseDeleting
	del.Status.Rollout = record(cloudv1alpha1.ClusterRolloutInProgress, "1.35", "1.36", now.Add(-3*timeout))
	if got := Finish(&del, now, timeout); got != nil {
		t.Errorf("a deleting cluster's record was finished: %+v", got)
	}
	res := Compute(input(del))
	for _, r := range res.Rows {
		if r.Name == "gone" {
			t.Errorf("a deleting cluster is a member: %+v", r)
		}
	}
}
