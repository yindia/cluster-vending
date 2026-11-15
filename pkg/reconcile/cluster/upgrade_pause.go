package cluster

import (
	"context"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
	"github.com/yindia/cluster-vending/pkg/rollout"
)

// withUpgradePause writes into st whether a freeze holds the cluster (of
// the platform, its tenant or its channel: rollout.HoldOf), and why. Without a source of holds, or when they cannot be
// read, nothing is said.
func (s *Reconciler) withUpgradePause(ctx context.Context, row *model.Cluster, spec *cloudv1.ClusterSpec, st *cloudv1.ClusterStatus) {
	src, ok := s.releases().(reconcile.HoldReader)
	if !ok || st == nil {
		return
	}
	holds, err := src.Holds(ctx)
	if err != nil {
		s.logger().WarnContext(ctx, "read the freezes", "cluster_id", row.ID, "error", err)
		return
	}
	h := rollout.HoldOf(holds, holdView(row, spec), time.Now())
	if h == nil {
		st.UpgradePause = nil
		return
	}
	p := &cloudv1.Pause{By: h.Spec.By, Reason: h.Spec.Reason, Scope: rollout.HoldTarget(h.Spec),
		Since: timestamppb.New(h.CreationTimestamp.Time)}
	if h.Spec.Until != nil {
		p.Until = timestamppb.New(h.Spec.Until.Time)
	}
	st.UpgradePause = p
}

// holdView is the cluster as a release hold matches it: its ID, tenant,
// channel and tier.
func holdView(row *model.Cluster, spec *cloudv1.ClusterSpec) *cloudv1alpha1.Cluster {
	c := &cloudv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{
		Labels: map[string]string{cloudstore.LabelID: row.ID, cloudstore.LabelTenant: row.TenantID}}}
	if name, ok := namedChannel(spec); ok {
		c.Spec.ReleaseChannel = cloudv1alpha1.ReleaseChannelName(strings.ToUpper(name[:1]) + name[1:])
	}
	if tier := spec.GetTier(); tier != cloudv1.ClusterTier_CLUSTER_TIER_UNSPECIFIED {
		c.Spec.Tier = cloudv1alpha1.ClusterTier("Tier" + strings.TrimPrefix(tier.String(), "CLUSTER_TIER_"))
	}
	return c
}

// frozen reports whether a freeze (of the platform, the cluster's tenant
// or its channel: rollout.HoldOf) holds the cluster: no automatic change
// of it may start (the daily add-on upgrade, a code change); why is what
// its status says. Freezes that cannot be read count as frozen, so
// nothing automatic starts blind; no source of freezes, not.
func (s *Reconciler) frozen(ctx context.Context, row *model.Cluster, spec *cloudv1.ClusterSpec) (bool, string) {
	src, ok := s.releases().(reconcile.HoldReader)
	if !ok {
		return false, ""
	}
	holds, err := src.Holds(ctx)
	if err != nil {
		s.logger().WarnContext(ctx, "read the freezes", "cluster_id", row.ID, "error", err)
		return true, "cannot read whether the cluster is frozen; waiting"
	}
	h := rollout.HoldOf(holds, holdView(row, spec), time.Now())
	if h == nil {
		return false, ""
	}
	return true, rollout.FreezeMessage(h)
}
