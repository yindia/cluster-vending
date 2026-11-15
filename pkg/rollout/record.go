package rollout

import (
	"fmt"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/upgrade"
)

// A cluster's status.rollout is the record the controller keeps of its
// place in its channel's rollout: Pending or Blocked (with why) before it
// starts, then InProgress, Succeeded, Failed or TimedOut for the upgrade
// the rollout started. The planned results are a report only; the
// others decide the cluster's state (fromRecord), so a started upgrade
// counts against maxInProgress before the cluster reports it, a failure
// counts against maxFailed until the cluster moves on, and a success
// gives the soak its exact start.

// fromRecord is the cluster's row as its rollout record says: false when
// the record decides nothing (none, another channel's, a planned one, or
// about a minor the cluster has passed).
func fromRecord(c *cloudv1alpha1.Cluster, channel string, r Row, cmp int) (Row, bool) {
	rec := c.Status.Rollout
	if rec == nil || !strings.EqualFold(rec.Channel, channel) {
		return r, false
	}
	switch rec.Result {
	case cloudv1alpha1.ClusterRolloutInProgress:
		r.State, r.Target, r.Since = upgrade.InProgress, rec.ToVersion, rec.StartTime.Time
		return r, true
	case cloudv1alpha1.ClusterRolloutFailed, cloudv1alpha1.ClusterRolloutTimedOut:
		if behind, ok := CompareMinors(r.Current, rec.ToVersion); ok && behind < 0 {
			r.State, r.Target, r.Since = upgrade.Failed, rec.ToVersion, finishOf(rec)
			return r, true
		}
	case cloudv1alpha1.ClusterRolloutSucceeded:
		same, ok := CompareMinors(r.Current, rec.ToVersion)
		if cmp == 0 && ok && same == 0 && !upgrading(c) && rec.FinishTime != nil {
			r.State, r.Since = upgrade.Succeeded, rec.FinishTime.Time
			return r, true
		}
	default: // Pending, Blocked: a report of the last plan
	}
	return r, false
}

func finishOf(rec *cloudv1alpha1.ClusterRolloutRecord) time.Time {
	if rec.FinishTime == nil {
		return time.Time{}
	}
	return rec.FinishTime.Time
}

// upgrading: the cluster reports a Kubernetes upgrade in progress.
func upgrading(c *cloudv1alpha1.Cluster) bool {
	return c.Status.Upgrade != nil && c.Status.Upgrade.Phase != ""
}

// Finish is the record a cluster's rollout record moves to at now:
//
//   - an upgrade in progress: Succeeded once the cluster runs the target
//     and reports no upgrade (every step of it done), Failed when the
//     cluster failed while upgrading, TimedOut when it made no progress
//     for timeout: its current step started longer ago than that (or,
//     never seen upgrading, the hop did);
//   - a record the rollout never started (Pending, Blocked): Succeeded,
//     "reached by an update", once the cluster runs its target with no
//     upgrade in progress, however it got there.
//
// nil when the record stays as it is.
func Finish(c *cloudv1alpha1.Cluster, now time.Time, timeout time.Duration) *cloudv1alpha1.ClusterRolloutRecord {
	rec := c.Status.Rollout
	if rec == nil {
		return nil
	}
	out := rec.DeepCopy()
	finished := metav1.NewTime(now)
	out.FinishTime = &finished
	reached, comparable := CompareMinors(c.Status.CurrentKubernetesVersion, rec.ToVersion)
	done := comparable && reached >= 0 && !upgrading(c)
	switch rec.Result {
	case cloudv1alpha1.ClusterRolloutPending, cloudv1alpha1.ClusterRolloutBlocked:
		if !done {
			return nil
		}
		out.Result = cloudv1alpha1.ClusterRolloutSucceeded
		out.Message = fmt.Sprintf("%s reached by an update, not started by the rollout", c.Status.CurrentKubernetesVersion)
		return out
	case cloudv1alpha1.ClusterRolloutInProgress:
	default:
		return nil
	}
	switch since := stepStart(c, rec); {
	case done:
		out.Result, out.Message = cloudv1alpha1.ClusterRolloutSucceeded, ""
	case upgrading(c) && c.Status.Phase == cloudv1alpha1.ClusterPhaseFailed:
		out.Result, out.Message = cloudv1alpha1.ClusterRolloutFailed, orDefault(c.Status.Message, "the upgrade failed")
	case now.Sub(since) > timeout:
		out.Result = cloudv1alpha1.ClusterRolloutTimedOut
		out.Message = fmt.Sprintf("no progress for %s", timeout)
		if u := c.Status.Upgrade; u != nil && u.Phase != "" {
			out.Message += fmt.Sprintf(" at step %s", u.Phase)
		}
		if c.Status.Message != "" {
			out.Message += "; last: " + c.Status.Message
		}
	default:
		return nil
	}
	return out
}

// stepStart is when an upgrade in progress last made progress: when the
// cluster's current step started, or, while the cluster reports no
// upgrade yet, when the rollout started the hop.
func stepStart(c *cloudv1alpha1.Cluster, rec *cloudv1alpha1.ClusterRolloutRecord) time.Time {
	if u := c.Status.Upgrade; u != nil && u.PhaseStartTime != nil {
		return u.PhaseStartTime.Time
	}
	return rec.StartTime.Time
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// Planned is the record of a cluster the rollout has not started: Pending
// with what it waits for, or Blocked with what keeps it back. nil when
// there is nothing to write: the cluster is in progress, done or failed
// (what happened stays), or its record says this already (writes are
// transition-only; a record that keeps its result and target keeps when
// it entered that state).
func Planned(r Row, c *cloudv1alpha1.Cluster, res Result, now time.Time) *cloudv1alpha1.ClusterRolloutRecord {
	next := &cloudv1alpha1.ClusterRolloutRecord{Channel: res.Channel, FromVersion: r.Current, ToVersion: r.Target,
		StartTime: metav1.NewTime(now), Tier: Count32(int(r.Tier))}
	switch r.State {
	case upgrade.Pending:
		next.Result, next.Message = cloudv1alpha1.ClusterRolloutPending, pendingMessage(r, res)
	case upgrade.Blocked:
		next.Result = cloudv1alpha1.ClusterRolloutBlocked
		if r.Gate != nil {
			next.Message = r.Gate.Message
		}
	default:
		return nil
	}
	old := c.Status.Rollout
	if old == nil || old.Result != next.Result || old.ToVersion != next.ToVersion {
		return next
	}
	if old.FromVersion == next.FromVersion && old.Message == next.Message && old.Tier == next.Tier && old.Channel == next.Channel {
		return nil
	}
	next.StartTime = old.StartTime
	return next
}

// pendingMessage is what a pending cluster waits for.
func pendingMessage(r Row, res Result) string {
	rolling := Tier(res.Tier)
	switch {
	case slices.Contains(res.Start, r.Key()) && res.Rollout.ReportOnly:
		return "automatic upgrades are off (would start)"
	case slices.Contains(res.Start, r.Key()):
		return "next to start"
	case res.Rollout.Halted:
		return "waiting: " + res.waiting
	case r.Tier < rolling:
		return fmt.Sprintf("waiting: tier %d first", rolling)
	case res.waiting != "" && len(res.Start) == 0:
		return "waiting: " + res.waiting
	default:
		return fmt.Sprintf("waiting its turn in tier %d", r.Tier)
	}
}

// notDoneWhileBelow keeps the rollout from being done while a member is
// still below the target: one kept back by its own check, or one that
// failed. The tier rolled out is the first tier (2, 1, 0) that has one, and the
// rollout says why it is not done.
func notDoneWhileBelow(d upgrade.Decision, members []upgrade.Member, blocked []cloudv1alpha1.BlockedCluster) upgrade.Decision {
	if !d.Done {
		return d
	}
	reasons := map[string]string{}
	for _, b := range blocked {
		reasons[b.Cluster] = b.Message
	}
	var nBlocked, nFailed int
	first, rolling, found := "", upgrade.Tier0, false
	for _, tier := range []upgrade.Tier{upgrade.Tier2, upgrade.Tier1, upgrade.Tier0} {
		for _, m := range members {
			if m.Tier != tier || (m.State != upgrade.Blocked && m.State != upgrade.Failed) {
				continue
			}
			if !found {
				rolling, found = tier, true
			}
			if m.State == upgrade.Failed {
				nFailed++
				continue
			}
			nBlocked++
			if first == "" {
				first = reasons[m.ID]
			}
		}
	}
	if !found {
		return d
	}
	d.Done, d.Tier = false, rolling
	if nBlocked > 0 {
		d.Waiting = fmt.Sprintf("%s blocked: %s", clusterCount(nBlocked), first)
	} else {
		d.Waiting = clusterCount(nFailed) + " failed"
	}
	return d
}
