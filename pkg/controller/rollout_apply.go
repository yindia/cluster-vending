package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/config"
	"github.com/yindia/cluster-vending/pkg/convert"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	platform "github.com/yindia/cluster-vending/pkg/reconcile"
	reconcilecluster "github.com/yindia/cluster-vending/pkg/reconcile/cluster"
	"github.com/yindia/cluster-vending/pkg/rollout"
	"github.com/yindia/cluster-vending/pkg/upgrade"
)

// What a rollout pass does to the channel's clusters, besides reporting
// the plan on the channel: it ends the upgrades it started (their records
// in status.rollout), starts the plan's hops when automatic upgrades are
// on, and records where every other member is (Pending or Blocked, with
// why), so cluster get shows the rollout before an upgrade starts. Every
// cluster write goes through the store: the version etag-guarded, the
// record status-only (Clusters.SetRollout), and only when it changes.

// finishUpgrades moves the record of each upgrade the rollout started on
// the channel that ended (rollout.Finish) and puts the new record on in's
// copy, so the plan that follows counts it.
func (r *RolloutReconciler) finishUpgrades(ctx context.Context, channel string, in *rollout.Input, now time.Time) {
	for i := range in.Clusters {
		c := &in.Clusters[i]
		if c.Status.Rollout == nil || !strings.EqualFold(c.Status.Rollout.Channel, channel) {
			continue
		}
		started := c.Status.Rollout.Result == cloudv1alpha1.ClusterRolloutInProgress
		next := rollout.Finish(c, now, r.upgradeTimeout())
		if next == nil || !r.writeRecord(ctx, c, next) {
			continue
		}
		c.Status.Rollout = next
		if !started {
			continue // a record the rollout never started, closed: no upgrade of its to report
		}
		obj := platform.Object{Kind: platform.KindCluster, Namespace: c.Namespace, Name: c.Name}
		hop := fmt.Sprintf("automatic upgrade %s → %s (channel %s)", next.FromVersion, next.ToVersion, next.Channel)
		if next.Result == cloudv1alpha1.ClusterRolloutSucceeded {
			r.event(ctx, obj, "Normal", platform.ReasonAutomaticUpgradeSucceeded, hop+" succeeded")
		} else {
			r.event(ctx, obj, "Warning", platform.ReasonAutomaticUpgradeFailed, fmt.Sprintf("%s %s: %s", hop, strings.ToLower(string(next.Result)), next.Message))
		}
	}
}

// startHops starts the plan's clusters (automatic upgrades on only) and
// returns the namespace/name of each one started.
func (r *RolloutReconciler) startHops(ctx context.Context, plan rollout.Result, now time.Time) map[string]bool {
	started := map[string]bool{}
	if !r.automatic || r.store == nil {
		return started
	}
	rows := map[string]rollout.Row{}
	for _, row := range plan.Rows {
		rows[row.Key()] = row
	}
	for _, key := range plan.Start {
		row, ok := rows[key]
		if !ok || row.State != upgrade.Pending {
			continue
		}
		if r.startHop(ctx, plan.Channel, row, now) {
			started[key] = true
		}
	}
	return started
}

// startHop sets the cluster's next minor, read again right before: it is
// still automatic, on this channel, asks for the minor the plan saw, is
// not paused or being deleted, and the hop goes up. A write that loses to
// another change is skipped; the next pass plans again. Irreversible: EKS
// cannot downgrade.
func (r *RolloutReconciler) startHop(ctx context.Context, channel string, row rollout.Row, now time.Time) bool {
	log := r.logger().With("cluster", row.Key(), "from", row.Current, "to", row.Target)
	stored, err := r.store.Clusters().Get(ctx, row.Tenant, row.ID)
	if err != nil {
		log.WarnContext(ctx, "automatic upgrade: read the cluster", "error", err)
		return false
	}
	spec, err := reconcilecluster.SpecOf(stored)
	if err != nil {
		log.WarnContext(ctx, "automatic upgrade: read the cluster's spec", "error", err)
		return false
	}
	why := notStartable(stored.State, stored.Paused != nil, spec, channel, row)
	if why == "" && reconcilecluster.ReleaseMoving(stored) {
		// A release move and a Kubernetes upgrade never run at once.
		why = "its components are moving to a new release"
	}
	if why != "" {
		log.InfoContext(ctx, "automatic upgrade not started", "why", why)
		return false
	}
	if err := reconcilecluster.SetAutomaticVersion(stored, row.Target); err != nil {
		log.WarnContext(ctx, "automatic upgrade: set the version", "error", err)
		return false
	}
	if err := r.store.Clusters().Update(ctx, stored); err != nil {
		if errors.Is(err, repository.ErrConflict) {
			log.InfoContext(ctx, "automatic upgrade skipped: the cluster changed; planned again next pass")
		} else {
			log.WarnContext(ctx, "automatic upgrade: store the version", "error", err)
		}
		return false
	}
	rec := &cloudv1alpha1.ClusterRolloutRecord{Channel: channel, FromVersion: row.Current, ToVersion: row.Target,
		Result: cloudv1alpha1.ClusterRolloutInProgress, Tier: rollout.Count32(int(row.Tier))}
	rec.StartTime.Time = now
	r.writeRecordFor(ctx, row.Tenant, row.ID, row.Key(), rec)
	message := fmt.Sprintf("automatic upgrade %s → %s (channel %s, tier %d)", row.Current, row.Target, channel, row.Tier)
	r.event(ctx, platform.Object{Kind: platform.KindCluster, Namespace: row.Namespace, Name: row.Name},
		"Normal", platform.ReasonAutomaticUpgradeStarted, message)
	r.event(ctx, platform.Object{Kind: platform.KindReleaseChannel, Name: channel},
		"Normal", platform.ReasonAutomaticUpgradeStarted, row.Key()+": "+message)
	log.InfoContext(ctx, "automatic upgrade started")
	return true
}

// notStartable is why the cluster, as stored now, must not be upgraded
// to row's target; "" when it may.
func notStartable(state string, paused bool, spec *cloudv1.ClusterSpec, channel string, row rollout.Row) string {
	asked := spec.GetControlPlane().GetKubernetesVersion()
	same, comparable := rollout.CompareMinors(asked, row.Current)
	up, upComparable := rollout.CompareMinors(row.Target, row.Current)
	switch {
	case spec.GetUpgradeMode() != cloudv1.UpgradeMode_UPGRADE_MODE_AUTOMATIC:
		return "no longer automatic"
	case !strings.EqualFold(channelName(spec.GetReleaseChannel()), channel):
		return "no longer on channel " + channel
	case !comparable || same != 0:
		return fmt.Sprintf("asks for %s now, not %s", asked, row.Current)
	case !upComparable || up <= 0:
		return "not an upgrade" // never a downgrade
	case paused:
		return "paused"
	case state == reconcilecluster.StateDeleting || state == reconcilecluster.StateImported || state == reconcilecluster.StateBlocked:
		return "the cluster is " + state
	}
	return ""
}

func channelName(c cloudv1.ReleaseChannel) string {
	return strings.ToLower(strings.TrimPrefix(c.String(), "RELEASE_CHANNEL_"))
}

// recordPlans records where each member the pass did not start is
// (rollout.Planned: transition-only).
func (r *RolloutReconciler) recordPlans(ctx context.Context, plan rollout.Result, in rollout.Input, started map[string]bool, now time.Time) {
	clusters := map[string]*cloudv1alpha1.Cluster{}
	for i := range in.Clusters {
		c := &in.Clusters[i]
		clusters[c.Namespace+"/"+c.Name] = c
	}
	for _, row := range plan.Rows {
		c, ok := clusters[row.Key()]
		if !ok || started[row.Key()] {
			continue
		}
		if rec := rollout.Planned(row, c, plan, now); rec != nil {
			r.writeRecord(ctx, c, rec)
		}
	}
}

// writeRecord stores rec as the cluster's rollout record; false when it
// could not (logged).
func (r *RolloutReconciler) writeRecord(ctx context.Context, c *cloudv1alpha1.Cluster, rec *cloudv1alpha1.ClusterRolloutRecord) bool {
	tenant := c.Labels[cloudstore.LabelTenant]
	if tenant == "" {
		tenant = c.Namespace
	}
	return r.writeRecordFor(ctx, tenant, c.Labels[cloudstore.LabelID], c.Namespace+"/"+c.Name, rec)
}

func (r *RolloutReconciler) writeRecordFor(ctx context.Context, tenant, id, key string, rec *cloudv1alpha1.ClusterRolloutRecord) bool {
	if r.store == nil || id == "" {
		return false
	}
	raw, err := protojson.Marshal(convert.RolloutFromAPI(rec))
	if err == nil {
		err = r.store.Clusters().SetRollout(ctx, tenant, id, raw)
	}
	if err != nil {
		r.logger().WarnContext(ctx, "record the cluster's rollout", "cluster", key, "error", err)
		return false
	}
	return true
}

func (r *RolloutReconciler) event(ctx context.Context, obj platform.Object, eventType, reason, message string) {
	if r.events != nil {
		r.events.Event(ctx, obj, eventType, reason, message)
	}
}

func (r *RolloutReconciler) upgradeTimeout() time.Duration {
	if r.timeout > 0 {
		return r.timeout
	}
	return config.DefaultUpgradeTimeout
}
