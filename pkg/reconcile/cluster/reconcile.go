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
// Imported clusters are only observed: the loop records their status.
func PendingStates() []string {
	return []string{StateProvisioning, StateUpdating, StateDeleting, StateImported}
}

// Reconcile moves one cluster on; the background loop calls it, so no read
// has to. It applies the cluster again while its generation or renderer
// version is behind (advance), retires node groups a replacement
// superseded, moves a deletion on and removes the row when everything is
// gone, stores a converged generation, and records the cluster's status
// (what GetCluster returns). Every step is idempotent. A cluster that
// does not exist is done.
func (s *Reconciler) Reconcile(ctx context.Context, tenantID, id string) error {
	row, err := s.deps.Store.Clusters().Get(ctx, tenantID, id)
	if errors.Is(err, repository.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := s.moveOn(ctx, row); err != nil {
		return err
	}
	// What GetCluster returns: the status as it is after this pass. A
	// cluster that is gone has none.
	current, err := s.deps.Store.Clusters().Get(ctx, tenantID, id)
	if errors.Is(err, repository.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	c, err := s.View(ctx, current)
	if err != nil {
		return fmt.Errorf("observe: %w", err)
	}
	return s.remember(ctx, current, c.GetStatus())
}

// moveOn is Reconcile without recording the status.
func (s *Reconciler) moveOn(ctx context.Context, row *model.Cluster) error {
	if row.State == StateDeleting {
		_, err := s.driveDelete(ctx, row)
		return err
	}
	if row.State == StateImported || row.State == StateBlocked {
		// Observed only, until TakeOverCluster. Counted as applied with this
		// renderer, or a renderer change would list it as pending forever.
		return s.markApplied(ctx, row)
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
	if len(nodeGroupRecords(records)) > 1 {
		current, err := s.currentNodeGroup(ctx, row)
		if err != nil {
			return fmt.Errorf("find the current node group: %w", err)
		}
		if err := s.retireNodeGroups(ctx, records, views, current); err != nil {
			return fmt.Errorf("retire node groups: %w", err)
		}
		// A replaced node group is still being retired: the cluster stays
		// pending until it is gone and forgotten, or its record would be
		// left behind (and read as a missing resource) once RUNNING.
		return nil
	}
	return s.recordConverged(ctx, row, status(row, views, !createdAfterActive(records)))
}

// recordConverged stores a converged generation: observed_generation, and
// the cluster as RUNNING (the loop then leaves it alone until it changes).
func (s *Reconciler) recordConverged(ctx context.Context, row *model.Cluster, st *cloudv1.ClusterStatus) error {
	if st.GetState() != cloudv1.ClusterState_CLUSTER_STATE_RUNNING ||
		(row.ObservedGeneration == row.Generation && row.State == StateRunning) {
		return nil
	}
	row.ObservedGeneration, row.State = row.Generation, StateRunning
	if err := s.deps.Store.Clusters().Update(ctx, row); err != nil && !errors.Is(err, repository.ErrConflict) {
		return err // a conflict: an API call changed the row; the next pass sees it
	}
	return nil
}
