package cluster

import (
	"context"
	"errors"
	"fmt"

	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// PendingStates are the stored states of clusters the background loop
// looks at every pass.
func PendingStates() []string { return []string{stateProvisioning, stateUpdating, stateDeleting} }

// Reconcile moves one cluster on; the background loop calls it, so no read
// has to. It applies the cluster again while its generation or renderer
// version is behind (advance), retires node groups a replacement
// superseded, moves a deletion on and removes the row when everything is
// gone, and stores a converged generation. Every step is idempotent. A
// cluster that does not exist is done.
func (s *ClusterService) Reconcile(ctx context.Context, tenantID, id string) error {
	row, err := s.deps.Store.Clusters().Get(ctx, tenantID, id)
	if errors.Is(err, repository.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if row.State == stateDeleting {
		_, err := s.driveDelete(ctx, row)
		return err
	}
	if err := s.advance(ctx, row); err != nil {
		return fmt.Errorf("apply: %w", err)
	}
	records, err := s.deps.Store.ManagedResources().ListByOwner(ctx, model.OwnerCluster, row.ID)
	if err != nil {
		return err
	}
	views, err := s.deps.Observe(ctx, records)
	if err != nil {
		return err
	}
	if err := s.retireNodeGroups(ctx, records, views); err != nil {
		return fmt.Errorf("retire node groups: %w", err)
	}
	if len(nodeGroupRecords(records)) > 1 {
		// A replaced node group is still being retired: the cluster stays
		// pending until it is gone and forgotten, or its record would be
		// left behind (and read as a missing resource) once RUNNING.
		return nil
	}
	return s.recordConverged(ctx, row, status(row, views, !createdAfterActive(records)))
}

// recordConverged stores a converged generation: observed_generation, and
// the cluster as RUNNING (the loop then leaves it alone until it changes).
func (s *ClusterService) recordConverged(ctx context.Context, row *model.Cluster, st *cloudv1.ClusterStatus) error {
	if st.GetState() != cloudv1.ClusterState_CLUSTER_STATE_RUNNING ||
		(row.ObservedGeneration == row.Generation && row.State == stateRunning) {
		return nil
	}
	row.ObservedGeneration, row.State = row.Generation, stateRunning
	if err := s.deps.Store.Clusters().Update(ctx, row); err != nil && !errors.Is(err, repository.ErrConflict) {
		return err // a conflict: an API call changed the row; the next pass sees it
	}
	return nil
}
