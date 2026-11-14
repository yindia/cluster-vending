package cluster

import (
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	reconcilecluster "github.com/yindia/cluster-vending/pkg/reconcile/cluster"
)

// rolloutOf is the cluster's rollout record, as the controller last wrote
// it (Clusters.SetRollout); nil when there is none or it cannot be read.
func rolloutOf(row *model.Cluster) *cloudv1.ClusterRollout {
	if len(row.Rollout) == 0 {
		return nil
	}
	r := &cloudv1.ClusterRollout{}
	if err := reconcilecluster.Lenient.Unmarshal(row.Rollout, r); err != nil {
		return nil
	}
	return r
}
