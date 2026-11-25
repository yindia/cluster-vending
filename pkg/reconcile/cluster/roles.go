package cluster

import (
	"context"

	"github.com/yindia/cluster-vending/pkg/component"
	"github.com/yindia/cluster-vending/pkg/database/model"
)

// nodeProvisioner is the name of the cluster's NodeProvisioner: the
// catalog's definition of component.RoleNodeProvisioner for the cluster's
// cloud. Its Ready phase releases the node groups, and its controller
// policy is the one Karpenter's node classes need. When the catalog cannot
// be read it is the shipped definition's name (component.RoleName).
func (s *Reconciler) nodeProvisioner(ctx context.Context, row *model.Cluster) string {
	if s.deps.Platform == nil {
		return component.RoleName(nil, component.RoleNodeProvisioner, string(row.Provider))
	}
	defs, err := s.deps.Platform.Definitions(ctx)
	if err != nil {
		s.logger().WarnContext(ctx, "the component catalog cannot be read; the node provisioner is taken to be the shipped one", "cluster_id", row.ID, "error", err)
		return component.RoleName(nil, component.RoleNodeProvisioner, string(row.Provider))
	}
	return component.RoleNameOf(defs, component.RoleNodeProvisioner, string(row.Provider))
}
