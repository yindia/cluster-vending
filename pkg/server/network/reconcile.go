package network

import (
	"context"
	"errors"

	"github.com/yindia/cluster-vending/pkg/database/repository"
)

// Reconcile moves a deleting network on without a reader: it deletes the
// next tier and removes the network's row once everything is gone. The
// controller calls it on every change of the network's resources. It
// reports whether the network is still being deleted. A network that does
// not exist is done.
func (s *NetworkService) Reconcile(ctx context.Context, tenantID, id string) (bool, error) {
	row, err := s.deps.Store.Networks().Get(ctx, tenantID, id)
	if errors.Is(err, repository.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if row.State != stateDeleting {
		return false, nil
	}
	gone, err := s.finalize(ctx, row)
	return !gone, err
}
