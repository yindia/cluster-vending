package cli

import (
	"fmt"
	"io"
	"strings"
	"unicode"

	"google.golang.org/protobuf/types/known/timestamppb"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// The marks of a cluster in a plan's batch.
var planMarks = map[cloudv1.RolloutState]string{
	cloudv1.RolloutState_ROLLOUT_STATE_SUCCEEDED:   "✓",
	cloudv1.RolloutState_ROLLOUT_STATE_FAILED:      "✗",
	cloudv1.RolloutState_ROLLOUT_STATE_IN_PROGRESS: "▶",
	cloudv1.RolloutState_ROLLOUT_STATE_PENDING:     "·",
	cloudv1.RolloutState_ROLLOUT_STATE_BLOCKED:     "⏸",
}

// rollingTierWord is the tier a rollout is at; a rollout at none is done
// or has nothing to upgrade.
func rollingTierWord(t cloudv1.ClusterTier, s *cloudv1.RolloutSummary, plan *cloudv1.RolloutPlan) string {
	if t != cloudv1.ClusterTier_CLUSTER_TIER_UNSPECIFIED {
		return tierWord(t)
	}
	empty, done := len(s.GetTiers()) == 0, s.GetDone()
	if plan != nil { // the plan, when the server sends one, says it all
		empty, done = len(plan.GetTiers()) == 0, plan.GetDone()
	}
	switch {
	case done && empty:
		return "none (nothing to upgrade)"
	case done:
		return "none (done)"
	}
	return none
}

// planState is a rollout in a few words, from its plan: the tier and
// batch rolling, the soak waited for, done or halted.
func planState(r *cloudv1.Rollout) string {
	plan := r.GetPlan()
	if plan == nil {
		return rolloutState(r.GetSummary(), r.GetPaused())
	}
	var state string
	switch {
	case plan.GetHalted():
		return "Halted"
	case plan.GetDone():
		return "Done"
	case !plan.GetRolling():
		state = rolloutState(r.GetSummary(), false)
	default:
		state = rollingState(plan)
	}
	if r.GetPaused() {
		return "Paused; " + state
	}
	return state
}

func rollingState(plan *cloudv1.RolloutPlan) string {
	tier := tierWord(plan.GetRollingTier())
	for i, t := range plan.GetTiers() {
		if t.GetTier() != plan.GetRollingTier() {
			continue
		}
		if t.GetState() == cloudv1.RolloutTierState_ROLLOUT_TIER_STATE_SOAKING && i > 0 {
			return fmt.Sprintf("Soaking: tier %s starts %s", tier, stampText(plan.GetTiers()[i-1].GetSoakUntil()))
		}
	}
	if plan.GetBatch() == 0 {
		return fmt.Sprintf("Rolling tier %s: waiting for clusters blocked by their own checks", tier)
	}
	return fmt.Sprintf("Rolling tier %s, batch %d of %d", tier, plan.GetBatch(), plan.GetBatches())
}

// printRolloutPlan is a rollout's plan: each tier's progress, then its batches
// and the clusters waiting on their own checks.
func printRolloutPlan(w io.Writer, plan *cloudv1.RolloutPlan, policy *cloudv1.RolloutPolicy) error {
	if len(plan.GetTiers()) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString("\nPlan:\n")
	tiers := plan.GetTiers()
	for i, t := range tiers {
		var prev *cloudv1.RolloutTierPlan
		if i > 0 {
			prev = tiers[i-1]
		}
		fmt.Fprintf(&b, "  TIER %s  %s\n", tierWord(t.GetTier()), tierHeading(t, prev, policy, plan.GetHalted()))
		for n, batch := range t.GetBatches() {
			marks := make([]string, 0, len(batch.GetClusters()))
			for _, c := range batch.GetClusters() {
				marks = append(marks, planMark(c))
			}
			fmt.Fprintf(&b, "    batch %d  %s\n", n+1, strings.Join(marks, "  "))
		}
		for _, c := range t.GetWaits() {
			fmt.Fprintf(&b, "    waits    %s: %s\n", planMark(c), orNone(oneLine(c.GetReason())))
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// tierHeading is a tier's line: its state and progress.
func tierHeading(t, prev *cloudv1.RolloutTierPlan, policy *cloudv1.RolloutPolicy, halted bool) string {
	progress := fmt.Sprintf("done %d/%d, failed %d/%d", t.GetDone(), t.GetTotal(), t.GetFailed(), t.GetMaxFailed())
	pace := atATime(t)
	switch t.GetState() {
	case cloudv1.RolloutTierState_ROLLOUT_TIER_STATE_DONE:
		if t.GetFinishedTime() == nil {
			return progress
		}
		return fmt.Sprintf("%s, finished %s, %s", progress, stampText(t.GetFinishedTime()), soakText(t.GetSoakUntil()))
	case cloudv1.RolloutTierState_ROLLOUT_TIER_STATE_ROLLING:
		return fmt.Sprintf("rolling: %s, %s", progress, pace)
	case cloudv1.RolloutTierState_ROLLOUT_TIER_STATE_SOAKING:
		return fmt.Sprintf("soaking: starts %s, %s, %s", stampText(prev.GetSoakUntil()), progress, pace)
	case cloudv1.RolloutTierState_ROLLOUT_TIER_STATE_HALTED:
		return fmt.Sprintf("halted: %d failed, maxFailed %d", t.GetFailed(), t.GetMaxFailed())
	case cloudv1.RolloutTierState_ROLLOUT_TIER_STATE_NOT_STARTED, cloudv1.RolloutTierState_ROLLOUT_TIER_STATE_UNSPECIFIED:
	}
	switch {
	case halted:
		return "not started: the channel halted"
	case prev != nil:
		return fmt.Sprintf("not started: after tier %s and its %s soak, %s", tierWord(prev.GetTier()), tierSoak(policy, prev.GetTier()), pace)
	}
	return "not started, " + pace
}

func atATime(t *cloudv1.RolloutTierPlan) string {
	if t.GetBatchSize() <= 1 {
		return "one at a time"
	}
	return fmt.Sprintf("%d at a time", t.GetBatchSize())
}

// tierSoak is the soak after tier, as the policy says it.
func tierSoak(p *cloudv1.RolloutPolicy, tier cloudv1.ClusterTier) string {
	if d, ok := p.GetSoakPerTier()["Tier"+tierWord(tier)]; ok {
		return shortDuration(d)
	}
	return shortDuration(p.GetSoak())
}

func soakText(until *timestamppb.Timestamp) string {
	if until == nil {
		return "no soak"
	}
	if until.AsTime().After(clock()) {
		return "soak ends " + stampText(until)
	}
	return "soak ended " + stampText(until)
}

func stampText(t *timestamppb.Timestamp) string {
	if t == nil || !t.IsValid() {
		return none
	}
	return t.AsTime().UTC().Format("2006-01-02 15:04") + " UTC"
}

// planMark is a cluster of a batch: its mark, tenant/name, and for one
// upgrading the step it is at and for how long.
func planMark(c *cloudv1.RolloutPlanCluster) string {
	mark := planMarks[c.GetState()]
	if mark == "" {
		mark = "?"
	}
	s := fmt.Sprintf("%s %s/%s", mark, c.GetTenantId(), c.GetClusterName())
	if c.GetState() == cloudv1.RolloutState_ROLLOUT_STATE_IN_PROGRESS {
		var detail []string
		if p := c.GetPhase(); p != "" {
			detail = append(detail, spaced(p))
		}
		if c.GetSince() != nil {
			detail = append(detail, age(c.GetSince()))
		}
		if len(detail) > 0 {
			s += " (" + strings.Join(detail, ", ") + ")"
		}
	}
	return s
}

// spaced is a CamelCase word as lower-case words: ControlPlane is
// "control plane".
func spaced(s string) string {
	var b strings.Builder
	for i, r := range s {
		if unicode.IsUpper(r) && i > 0 {
			b.WriteByte(' ')
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}
