package cluster

import (
	"context"
	"errors"

	"github.com/yindia/cluster-vending/pkg/database/repository"
)

// Reconcile moves a cluster on without a reader: it creates the add-ons
// once EKS is ACTIVE, retires replaced node groups, and deletes the next
// tier of a deleting cluster (removing its row when everything is gone).
// The controller calls it on every change of the cluster's resources. It
// reports whether the cluster is still changing and should be looked at
// again. A cluster that does not exist is done.
func (s *ClusterService) Reconcile(ctx context.Context, tenantID, id string) (bool, error) {
	row, err := s.deps.Store.Clusters().Get(ctx, tenantID, id)
	if errors.Is(err, repository.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	s.progress(ctx, row)
	if _, err := s.view(ctx, row); err != nil { // also retires replaced node groups
		return true, err
	}
	gone, err := s.finalize(ctx, row)
	if err != nil || gone {
		return !gone, err
	}
	return row.State == stateProvisioning || row.State == stateUpdating || row.State == stateDeleting, nil
}
