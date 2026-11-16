package reconcile

import (
	"context"

	"github.com/yindia/cluster-vending/pkg/database/model"
)

func (d Deps) SyncFreeze(ctx context.Context, scope PauseScope) (*model.Freeze, error) {
	frozen, err := d.FreezeFor(ctx, FreezeTarget{TenantID: scope.TenantID, ClusterID: scope.ClusterID, NetworkID: scope.NetworkID})
	if err != nil {
		return nil, err
	}
	if scope.ClusterID == "" && scope.NetworkID == "" {
		return frozen, nil
	}
	if _, err := SetProvidersPaused(ctx, d, scope, frozen.Holds()); err != nil {
		return nil, err
	}
	return frozen, nil
}
