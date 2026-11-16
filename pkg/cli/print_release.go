package cli

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// The tables of the admin release commands: releases, channels and
// rollouts (AdminService).

func channelWord(c cloudv1.ReleaseChannel) string {
	if c == cloudv1.ReleaseChannel_RELEASE_CHANNEL_UNSPECIFIED {
		return none
	}
	return strings.ToLower(strings.TrimPrefix(c.String(), "RELEASE_CHANNEL_"))
}

func tierWord(t cloudv1.ClusterTier) string {
	if t == cloudv1.ClusterTier_CLUSTER_TIER_UNSPECIFIED {
		return none
	}
	return strings.TrimPrefix(t.String(), "CLUSTER_TIER_")
}

// shortDuration drops a duration's trailing zero units: 24h0m0s is 24h.
func shortDuration(d string) string {
	for _, zero := range []string{"0s", "0m"} {
		if t := strings.TrimSuffix(d, zero); t != d && t != "" && !strings.HasSuffix(t, ".") {
			d = t
		}
	}
	return d
}

// policyLine is a rollout policy in one line.
func policyLine(p *cloudv1.RolloutPolicy) string {
	if p == nil {
		return ""
	}
	line := fmt.Sprintf("maxInProgress %d, maxFailed %d, soak %s", p.GetMaxInProgress(), p.GetMaxFailed(), shortDuration(p.GetSoak()))
	if per := p.GetSoakPerTier(); len(per) > 0 {
		var parts []string
		for _, tier := range slices.Sorted(maps.Keys(per)) {
			parts = append(parts, tier+": "+shortDuration(per[tier]))
		}
		line += " (" + strings.Join(parts, ", ") + ")"
	}
	return line
}

func date(t *timestamppb.Timestamp) string {
	if t == nil || !t.IsValid() {
		return none
	}
	return t.AsTime().UTC().Format("2006-01-02")
}

var releaseHeader = []string{"NAME", "SHIPPED", "VALID", "CHANNELS", "PINNED", "AGE"}

func printReleases(w io.Writer, releases []*cloudv1.Release, wide bool) error {
	if len(releases) == 0 {
		_, err := fmt.Fprintln(w, "No releases found.")
		return err
	}
	header := releaseHeader
	if wide {
		header = append(slices.Clone(header), "REASON")
	}
	rows := make([][]string, 0, len(releases))
	for _, r := range releases {
		rw := []string{r.GetName(), yesNo(r.GetShipped()), validWord(r), channelList(r.GetChannels()),
			strconv.Itoa(len(r.GetHolds())), age(r.GetCreateTime())}
		if wide {
			rw = append(rw, orNone(oneLine(r.GetInvalidReason())))
		}
		rows = append(rows, rw)
	}
	return table(w, header, rows)
}

func validWord(r *cloudv1.Release) string {
	switch {
	case r.GetWithdrawn():
		return "Withdrawn"
	case r.GetValid():
		return "True"
	}
	return "False"
}

// channelList names channels, comma-separated; <none> for none.
func channelList(chs []cloudv1.ReleaseChannel) string {
	names := make([]string, 0, len(chs))
	for _, c := range chs {
		names = append(names, channelWord(c))
	}
	return orNone(strings.Join(names, ","))
}

// describeRelease is one release: its fields, its minors, and the
// components on each minor (wide adds their locked values).
func describeRelease(w io.Writer, r *cloudv1.Release, wide bool) error {
	kv := [][2]string{{"Name", r.GetName()}, {"Shipped", yesNo(r.GetShipped())}, {"Valid", validWord(r)},
		{"Reason", r.GetInvalidReason()}, {"Channels", channelList(r.GetChannels())},
		{"Pinned", strings.Join(r.GetHolds(), ", ")}, {"Age", age(r.GetCreateTime())}, {"Notes", oneLine(r.GetNotes())}}
	if err := fields(w, kv); err != nil {
		return err
	}
	minors := make([][]string, 0, len(r.GetKubernetes()))
	for _, m := range r.GetKubernetes() {
		minors = append(minors, []string{m.GetMinor(), channelWord(m.GetChannel()), yesNo(m.GetDefault()),
			orNone(m.GetUpgradeTo()), date(m.GetEndOfSupport()), endingWord(m.GetEnding())})
	}
	if err := section(w, "Kubernetes", []string{"MINOR", "CHANNEL", "DEFAULT", "UPGRADE-TO", "END-OF-SUPPORT", "ENDING"}, minors); err != nil {
		return err
	}
	for _, m := range r.GetKubernetes() {
		header := []string{"COMPONENT", "VERSION"}
		if wide {
			header = append(header, "VALUES")
		}
		rows := make([][]string, 0, len(m.GetComponents()))
		for _, c := range m.GetComponents() {
			rw := []string{c.GetName(), c.GetVersion()}
			if wide {
				rw = append(rw, valuesLine(c))
			}
			rows = append(rows, rw)
		}
		if err := section(w, "Components on "+m.GetMinor(), header, rows); err != nil {
			return err
		}
	}
	return nil
}

func endingWord(ending bool) string {
	if ending {
		return "yes: on no channel"
	}
	return "no"
}

// valuesLine is a component's locked values as one line of JSON.
func valuesLine(c *cloudv1.ReleaseComponent) string {
	if c.GetValues() == nil || len(c.GetValues().GetFields()) == 0 {
		return none
	}
	js, err := protojson.Marshal(c.GetValues())
	if err != nil {
		return none
	}
	return string(js)
}

var channelHeader = []string{"NAME", "PROMOTION", "RELEASE", "KUBERNETES", "ROLLING TIER", "STATE", "WAITING", "PAUSED", "AGE"}

func printChannels(w io.Writer, channels []*cloudv1.ReleaseChannelInfo, wide bool) error {
	if len(channels) == 0 {
		_, err := fmt.Fprintln(w, "No release channels found.")
		return err
	}
	header := channelHeader
	if wide {
		header = append(slices.Clone(header), "TIER2", "TIER1", "TIER0", "POLICY")
	}
	rows := make([][]string, 0, len(channels))
	for _, c := range channels {
		r := c.GetRollout()
		rw := []string{channelWord(c.GetChannel()), orNone(c.GetPromotion()), orNone(c.GetRelease()), orNone(c.GetKubernetesMinor()),
			rollingTierWord(c.GetRolloutTier(), r, nil), rolloutState(r, c.GetPaused()), orNone(channelWaiting(c)), yesNo(c.GetPaused()),
			age(c.GetCreateTime())}
		if wide {
			rw = append(rw, tierCell(r, cloudv1.ClusterTier_CLUSTER_TIER_2), tierCell(r, cloudv1.ClusterTier_CLUSTER_TIER_1),
				tierCell(r, cloudv1.ClusterTier_CLUSTER_TIER_0), policyLine(c.GetPolicy()))
		}
		rows = append(rows, rw)
	}
	return table(w, header, rows)
}

// describeChannel is admin channel get: what the channel offers, its
// policy, where its rollout is (planned now when the server sent the
// plan) and its release moves; then the rollout plan per tier and batch
// (wide adds every cluster), the releases it offered and its conditions.
func describeChannel(w io.Writer, res *cloudv1.GetReleaseChannelResponse, wide bool) error {
	if frozen := res.GetFreeze(); frozen != nil {
		if err := fields(w, [][2]string{{"Frozen", freezeLine(frozen)}}); err != nil {
			return err
		}
	}
	c, r := res.GetChannel(), res.GetRollout()
	summary := c.GetRollout()
	tier, state := rollingTierWord(c.GetRolloutTier(), summary, nil), rolloutState(summary, c.GetPaused())
	if r != nil {
		summary = r.GetSummary()
		tier, state = rollingTierWord(r.GetRolloutTier(), summary, r.GetPlan()), planState(r)
	}
	kv := [][2]string{{"Name", channelWord(c.GetChannel())}, {"Promotion", c.GetPromotion()}, {"Pinned Release", c.GetPinnedRelease()},
		{"Release", c.GetRelease()}, {"Kubernetes", c.GetKubernetesMinor()}, {"Paused", yesNo(c.GetPaused())},
		{"Policy", policyLine(c.GetPolicy())}, {"Rolling Tier", tier},
		{"Rollout", state}, {"Waiting", oneLine(summary.GetWaiting())},
		{"Would Start", strings.Join(summary.GetWouldStart(), ", ")}, {"Age", age(c.GetCreateTime())}}
	if c.GetPromotion() == "Automatic" {
		soak := c.GetPolicy().GetReleaseSoak()
		if soak == "" {
			soak = "default (beta 7d, stable 14d)"
		}
		kv = append(kv, [2]string{"Release Soak", soak})
	}
	if p := c.GetPending(); p != nil {
		kv = append(kv, [2]string{"Next Release", fmt.Sprintf("%s %s (%s)", p.GetRelease(), untilText(p.GetUntil()), date(p.GetUntil()))})
	}
	if m := c.GetReleaseMoves(); m != nil {
		kv = append(kv, [2]string{"Release Moves", changeRolloutLine(m)})
	}
	if m := c.GetCodeChanges(); m != nil {
		kv = append(kv, [2]string{"Code Changes", changeRolloutLine(m)})
	}
	if err := fields(w, kv); err != nil {
		return err
	}
	if err := describePlan(w, r, wide); err != nil {
		return err
	}
	if err := keptBack(w, "Release Moves Kept Back", c.GetReleaseMoves()); err != nil {
		return err
	}
	if err := keptBack(w, "Code Changes Kept Back", c.GetCodeChanges()); err != nil {
		return err
	}
	history := make([][]string, 0, len(c.GetHistory()))
	for _, h := range c.GetHistory() {
		to := "now"
		if h.GetTo() != nil {
			to = date(h.GetTo())
		}
		history = append(history, []string{h.GetRelease(), date(h.GetFrom()), to, orNone(h.GetReason())})
	}
	if err := section(w, "History", []string{"RELEASE", "FROM", "TO", "REASON"}, history); err != nil {
		return err
	}
	return section(w, "Conditions", conditionHeader, conditionRows(c.GetConditions()))
}

// channelWaiting is why a channel waits: the release it takes once its
// release soak ends, else what its rollout waits for.
func channelWaiting(c *cloudv1.ReleaseChannelInfo) string {
	if p := c.GetPending(); p != nil {
		return fmt.Sprintf("takes %s %s", p.GetRelease(), untilText(p.GetUntil()))
	}
	return oneLine(c.GetRollout().GetWaiting())
}

// rolloutState is a rollout in one word.
func rolloutState(r *cloudv1.RolloutSummary, paused bool) string {
	switch {
	case r == nil:
		return none
	case r.GetHalted():
		return "Halted"
	case r.GetDone():
		return "Done"
	case paused:
		return "Paused"
	case len(r.GetTiers()) == 0:
		return "Idle"
	}
	return "Rolling"
}

// tierCell is one tier's progress in a channel list: done/total, with
// the clusters upgrading now; "-" for a tier without clusters.
func tierCell(r *cloudv1.RolloutSummary, tier cloudv1.ClusterTier) string {
	for _, t := range r.GetTiers() {
		if t.GetTier() != tier {
			continue
		}
		total := t.GetPending() + t.GetInProgress() + t.GetSucceeded() + t.GetFailed() + t.GetBlocked()
		cell := fmt.Sprintf("%d/%d done", t.GetSucceeded(), total)
		if t.GetInProgress() > 0 {
			cell += fmt.Sprintf(", %d running", t.GetInProgress())
		}
		if t.GetFailed() > 0 {
			cell += fmt.Sprintf(", %d failed", t.GetFailed())
		}
		return cell
	}
	return "-"
}

var memberHeader = []string{"TIER", "TENANT", "CLUSTER", "RUNS", "TARGET", "STATE", "SINCE", "REASON"}

// describePlan is a channel's rollout planned now: its plan per tier and
// batch; wide (or a server without plans) adds its clusters' table, tier
// 2 first, with their IDs and gates when wide. Nothing without a rollout.
func describePlan(w io.Writer, r *cloudv1.Rollout, wide bool) error {
	if r == nil {
		return nil
	}
	if r.GetPlan() != nil {
		if err := printRolloutPlan(w, r.GetPlan(), r.GetPolicy()); err != nil {
			return err
		}
		if !wide {
			return nil
		}
	}
	header := memberHeader
	if wide {
		header = append(slices.Clone(header), "CLUSTER-ID", "GATE")
	}
	rows := make([][]string, 0, len(r.GetMembers()))
	for _, m := range r.GetMembers() {
		rw := []string{tierWord(m.GetTier()), orNone(m.GetTenantId()), m.GetClusterName(), orNone(m.GetCurrentVersion()),
			orNone(m.GetTargetVersion()), word(m.GetState().String(), "ROLLOUT_STATE_"), age(m.GetSince()), orNone(oneLine(m.GetReason()))}
		if wide {
			rw = append(rw, orNone(m.GetClusterId()), orNone(m.GetGate()))
		}
		rows = append(rows, rw)
	}
	return section(w, "Clusters", header, rows)
}

// changeRolloutLine is a rollout of release moves (or of code changes,
// which have no target) in one line: "2026-11-a: Rolling, 3/7 done;
// waiting: tier 1 soaks; 1 kept back".
func changeRolloutLine(m *cloudv1.ChangeRollout) string {
	var done, total int32
	for _, t := range m.GetTiers() {
		done += t.GetSucceeded()
		total += t.GetPending() + t.GetInProgress() + t.GetSucceeded() + t.GetFailed() + t.GetBlocked()
	}
	state := "Rolling"
	switch {
	case m.GetHalted():
		state = "Halted"
	case m.GetDone():
		state = "Done"
	}
	line := fmt.Sprintf("%s, %d/%d done", state, done, total)
	if m.GetTarget() != "" {
		line = m.GetTarget() + ": " + line
	}
	if w := m.GetWaiting(); w != "" {
		line += "; waiting: " + oneLine(w)
	}
	if n := len(m.GetBlocked()); n > 0 {
		line += fmt.Sprintf("; %d kept back", n)
	}
	return line
}

// keptBack lists the clusters a rollout of changes keeps back, and why;
// nothing when none is.
func keptBack(w io.Writer, title string, m *cloudv1.ChangeRollout) error {
	if len(m.GetBlocked()) == 0 {
		return nil
	}
	rows := make([][]string, 0, len(m.GetBlocked()))
	for _, b := range m.GetBlocked() {
		rows = append(rows, []string{b.GetCluster(), orNone(b.GetGate()), orNone(oneLine(b.GetReason()))})
	}
	return section(w, title, []string{"CLUSTER", "GATE", "REASON"}, rows)
}
