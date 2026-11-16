package cluster

import (
	"context"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
	"github.com/yindia/cluster-vending/pkg/rollout"
)

// holdView is the cluster as a release hold matches it: its ID, tenant,
// channel and tier.
func holdView(row *model.Cluster, spec *cloudv1.ClusterSpec) *cloudv1alpha1.Cluster {
	c := &cloudv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{
		Labels: map[string]string{cloudstore.LabelID: row.ID, cloudstore.LabelTenant: row.TenantID, "cluster-vending.io/network": row.NetworkID}}}
	if name, ok := namedChannel(spec); ok {
		c.Spec.ReleaseChannel = cloudv1alpha1.ReleaseChannelName(strings.ToUpper(name[:1]) + name[1:])
	}

	return c
}

// frozen reports whether the platform freeze or a release lock
// (rollout.HoldOf) holds the cluster: no automatic change
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
