package cluster

import (
	"context"

	"github.com/yindia/cluster-vending/pkg/database/model"
	accountreconcile "github.com/yindia/cluster-vending/pkg/reconcile/account"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

// usable refuses the account of the cluster's network when the controller
// has not verified its access in the network's region
// (accountreconcile.Usable), as FAILED_PRECONDITION. A verdict that is
// stale is logged and allowed.
func (s *ClusterService) usable(ctx context.Context, a *model.CloudAccount, region string) error {
	warning, err := accountreconcile.Usable(a, region)
	if err != nil {
		return err
	}
	if warning != "" {
		s.logger().WarnContext(ctx, warning, "tenant_id", a.TenantID, "bootstrap_id", a.ID, "region", region)
	}
	return nil
}

// usableNetwork is usable for the account of the network (by ID).
func (s *ClusterService) usableNetwork(ctx context.Context, tenantID, networkID string) error {
	n, err := s.deps.Store.Networks().Get(ctx, tenantID, networkID)
	if err != nil {
		return service.ToConnect(err)
	}
	a, err := s.deps.Store.Tenants().GetCloudAccountByID(ctx, tenantID, n.CloudAccountID)
	if err != nil {
		return service.ToConnect(err)
	}
	return s.usable(ctx, a, n.Region)
}
