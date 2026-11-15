package cli

import (
	"strings"
	"testing"
	"time"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

const (
	succeeded  = cloudv1.RolloutState_ROLLOUT_STATE_SUCCEEDED
	failed     = cloudv1.RolloutState_ROLLOUT_STATE_FAILED
	inProgress = cloudv1.RolloutState_ROLLOUT_STATE_IN_PROGRESS
	pending    = cloudv1.RolloutState_ROLLOUT_STATE_PENDING
	blocked    = cloudv1.RolloutState_ROLLOUT_STATE_BLOCKED
)

func planCluster(tenant, name string, s cloudv1.RolloutState) *cloudv1.RolloutPlanCluster {
	return &cloudv1.RolloutPlanCluster{TenantId: tenant, ClusterName: name, ClusterId: "id-" + name, State: s}
}

func batch(cs ...*cloudv1.RolloutPlanCluster) *cloudv1.RolloutBatch {
	return &cloudv1.RolloutBatch{Clusters: cs}
}

// channelGet is admin channel get's answer for the channel of r, with r
// as its rollout planned now.
func channelGet(r *cloudv1.Rollout) *cloudv1.GetReleaseChannelResponse {
	info := &cloudv1.ReleaseChannelInfo{Channel: r.GetChannel(), Release: r.GetRelease(),
		KubernetesMinor: r.GetSummary().GetTargetMinor(), Policy: r.GetPolicy()}
	return &cloudv1.GetReleaseChannelResponse{Channel: info, Rollout: r}
}

// The plan view: tier 2 done, tier 1 rolling batch 2 of 3 with one cluster
// waiting on its maintenance window, tier 0 not started.
func TestPrintRolloutPlanRolling(t *testing.T) {
	withClock(t)
	payments := planCluster("acme", "payments", inProgress)
	payments.Phase, payments.Since = "ControlPlane", ago(12*time.Minute)
	waiting := planCluster("acme", "batch", blocked)
	waiting.Reason = "the maintenance window opens Sat 02:00 UTC"
	r := &cloudv1.Rollout{Channel: stable, Release: "shipped-1aed928c", RolloutTier: tier1, Policy: policy(),
		Summary: &cloudv1.RolloutSummary{TargetMinor: "1.36", WouldStart: []string{"tenant-acme/search"}},
		Plan: &cloudv1.RolloutPlan{Rolling: true, RollingTier: tier1, Batch: 2, Batches: 3, Tiers: []*cloudv1.RolloutTierPlan{
			{Tier: tier2, State: cloudv1.RolloutTierState_ROLLOUT_TIER_STATE_DONE, Total: 2, Done: 2, MaxFailed: 1, BatchSize: 2,
				FinishedTime: ago(49 * time.Hour), SoakUntil: ago(25 * time.Hour), Current: -1,
				Batches: []*cloudv1.RolloutBatch{batch(planCluster("acme", "dev", succeeded), planCluster("acme", "qa", succeeded))}},
			{Tier: tier1, State: cloudv1.RolloutTierState_ROLLOUT_TIER_STATE_ROLLING, Total: 7, Done: 2, InProgress: 1, Blocked: 1,
				MaxFailed: 1, BatchSize: 2, Current: 1, Waits: []*cloudv1.RolloutPlanCluster{waiting},
				Batches: []*cloudv1.RolloutBatch{
					batch(planCluster("acme", "a", succeeded), planCluster("acme", "b", succeeded)),
					batch(payments, planCluster("acme", "search", pending)),
					batch(planCluster("globex", "api", pending), planCluster("globex", "core", pending))}},
			{Tier: tier0, State: cloudv1.RolloutTierState_ROLLOUT_TIER_STATE_NOT_STARTED, Total: 2, MaxFailed: 1, BatchSize: 1,
				Current: 0, Batches: []*cloudv1.RolloutBatch{batch(planCluster("acme", "prod", pending)),
					batch(planCluster("globex", "prod", pending))}},
		}}}

	got := printed(t, channelGet(r), OutputTable)

	for _, want := range []string{
		"Name:             stable\n",
		"Kubernetes:       1.36\n",
		"Release:          shipped-1aed928c\n",
		"Policy:           maxInProgress 2, maxFailed 1, soak 24h (Tier1: 48h)\n",
		"Rolling Tier:     1\n",
		"Rollout:          Rolling tier 1, batch 2 of 3\n",
		"Would Start:      tenant-acme/search\n",
		"\nPlan:\n" +
			"  TIER 2  done 2/2, failed 0/1, finished 2026-11-12 11:00 UTC, soak ended 2026-11-13 11:00 UTC\n" +
			"    batch 1  ✓ acme/dev  ✓ acme/qa\n" +
			"  TIER 1  rolling: done 2/7, failed 0/1, 2 at a time\n" +
			"    batch 1  ✓ acme/a  ✓ acme/b\n" +
			"    batch 2  ▶ acme/payments (control plane, 12m)  · acme/search\n" +
			"    batch 3  · globex/api  · globex/core\n" +
			"    waits    ⏸ acme/batch: the maintenance window opens Sat 02:00 UTC\n" +
			"  TIER 0  not started: after tier 1 and its 48h soak, one at a time\n" +
			"    batch 1  · acme/prod\n" +
			"    batch 2  · globex/prod\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "\nClusters:\n") {
		t.Errorf("the cluster table is for -o wide:\n%s", got)
	}
	if wide := printed(t, channelGet(r), OutputWide); !strings.Contains(wide, "\nPlan:\n") ||
		!strings.Contains(wide, "\nClusters:\n") {
		t.Errorf("wide lacks the plan or the cluster table:\n%s", wide)
	}
}

func TestPrintRolloutPlanHalted(t *testing.T) {
	withClock(t)
	r := &cloudv1.Rollout{Channel: stable, RolloutTier: tier2, Policy: policy(),
		Summary: &cloudv1.RolloutSummary{TargetMinor: "1.36", Halted: true, Waiting: "halted: 2 clusters failed in tier 2, at most 1 allowed"},
		Plan: &cloudv1.RolloutPlan{Halted: true, Tiers: []*cloudv1.RolloutTierPlan{
			{Tier: tier2, State: cloudv1.RolloutTierState_ROLLOUT_TIER_STATE_HALTED, Total: 2, Failed: 2, MaxFailed: 1, BatchSize: 2,
				Current: -1, FinishedTime: ago(time.Hour), SoakUntil: ago(-23 * time.Hour),
				Batches: []*cloudv1.RolloutBatch{batch(planCluster("acme", "a", failed), planCluster("acme", "b", failed))}},
			{Tier: tier1, State: cloudv1.RolloutTierState_ROLLOUT_TIER_STATE_NOT_STARTED, Total: 1, MaxFailed: 1, BatchSize: 2,
				Batches: []*cloudv1.RolloutBatch{batch(planCluster("acme", "c", pending))}},
		}}}

	got := printed(t, channelGet(r), OutputTable)

	for _, want := range []string{
		"Rollout:          Halted\n",
		"\nPlan:\n" +
			"  TIER 2  halted: 2 failed, maxFailed 1\n" +
			"    batch 1  ✗ acme/a  ✗ acme/b\n" +
			"  TIER 1  not started: the channel halted\n" +
			"    batch 1  · acme/c\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("lacks %q:\n%s", want, got)
		}
	}
}

func TestPrintRolloutPlanDone(t *testing.T) {
	withClock(t)
	r := &cloudv1.Rollout{Channel: alpha, Release: "shipped-1aed928c", Policy: policy(),
		Summary: &cloudv1.RolloutSummary{TargetMinor: "1.37", Done: true},
		Plan: &cloudv1.RolloutPlan{Done: true, Tiers: []*cloudv1.RolloutTierPlan{
			{Tier: tier2, State: cloudv1.RolloutTierState_ROLLOUT_TIER_STATE_DONE, Total: 1, Done: 1, MaxFailed: 1, BatchSize: 2,
				Current: -1, FinishedTime: ago(time.Hour), SoakUntil: ago(-23 * time.Hour),
				Batches: []*cloudv1.RolloutBatch{batch(planCluster("acme", "prod-eks", succeeded))}},
		}}}

	got := printed(t, channelGet(r), OutputTable)

	for _, want := range []string{
		"Rolling Tier:     none (done)\n", "Rollout:          Done\n",
		"\nPlan:\n" +
			"  TIER 2  done 1/1, failed 0/1, finished 2026-11-14 11:00 UTC, soak ends 2026-11-15 11:00 UTC\n" +
			"    batch 1  ✓ acme/prod-eks\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("lacks %q:\n%s", want, got)
		}
	}
	empty := &cloudv1.Rollout{Channel: beta, Policy: policy(), Summary: &cloudv1.RolloutSummary{TargetMinor: "1.36", Done: true},
		Plan: &cloudv1.RolloutPlan{Done: true}}
	if got := printed(t, channelGet(empty), OutputTable); !strings.Contains(got,
		"Rolling Tier:     none (nothing to upgrade)\n") || strings.Contains(got, "Plan:") {
		t.Errorf("nothing to upgrade:\n%s", got)
	}
}
