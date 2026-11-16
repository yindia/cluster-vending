package cluster

import (
	"context"
	"errors"
	"fmt"

	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// PendingStates are the stored states of clusters that are not settled:
// the controller looks at them again soon.
// Imported clusters are only observed: their status is recorded.
func PendingStates() []string {
	return []string{StateProvisioning, StateUpdating, StateDeleting, StateImported}
}

// Reconcile moves one cluster on; the controller calls it, so no read
// has to, and no API call does: the handlers only store what was asked
// for. It applies the cluster again while its generation or renderer
// version is behind (advance), retires node groups a replacement
// superseded, moves a deletion on and removes the row when everything is
// gone, stores a converged generation, and records the cluster's status
// (what GetCluster returns). What fails is recorded as the cluster's
// status (ConditionReconciled) and tried again on the next pass. Every
// step is idempotent. A cluster that
// does not exist is done.
//
// It is also where the cloud is looked at, never the call that stored the
// row: the EKS cluster of an import is discovered here (discoverImport),
// and a new cluster's name and a generation's versions are checked
// against EKS here (resolve), before anything is rendered.
//
// What changed in a pass is reported as events of the cluster's object
// (events.go), once: when its status is recorded.
func (s *Reconciler) Reconcile(ctx context.Context, tenantID, id string) error {
	row, err := s.deps.Store.Clusters().Get(ctx, tenantID, id)
	if errors.Is(err, repository.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	ctx = withPlatformDeletion(reconcile.WithCodeHolder(withTeardownHolder(reconcile.WithNotes(ctx))))
	row.Frozen, err = s.deps.SyncFreeze(ctx, reconcile.PauseScope{TenantID: tenantID, ClusterID: row.ID, NetworkID: row.NetworkID})
	if err != nil {
		return err
	}
	if row.Frozen.Holds() {
		return s.observePaused(ctx, row)
	}
	// A person asked to try again what failed (cluster retry): the failed
	// upgrade now, the components where this pass installs them.
	if row.Retry != nil {
		ctx = withRetry(ctx, row.Retry)
		if row.Retry.Upgrade {
			if err := s.retryUpgrade(ctx, row); err != nil {
				return err
			}
		}
	}
	// What went wrong is the cluster's status (below) and the error of the
	// pass: the loop logs it and comes back.
	failed := s.moveOn(ctx, row)
	// What GetCluster returns: the status as it is after this pass. A
	// cluster that is gone has none.
	current, err := s.deps.Store.Clusters().Get(ctx, tenantID, id)
	if errors.Is(err, repository.ErrNotFound) {
		if row.State == StateDeleting {
			notes := reconcile.Notes(ctx)
			final := &cloudv1.ClusterStatus{State: cloudv1.ClusterState_CLUSTER_STATE_DELETING}
			notes = append(notes, managedLifecycleEvents(storedStatus(row), final)...)
			notes = append(notes, platformEvents(storedStatus(row), final)...)
			s.emit(ctx, row, notes)
			s.deps.Emit(ctx, object(row), reconcile.EventNormal, reconcile.ReasonDeleted,
				"the cluster's resources are gone and the cluster is removed")
		}
		return failed
	}
	if err != nil {
		return errors.Join(failed, err)
	}
	current.Frozen = row.Frozen
	// The components the platform installs with the cluster: after the
	// cluster's own pass, and never in its way. What fails here is logged
	// and tried again; the recorded components stay as they were.
	installed, perr := s.installPlatform(ctx, current)
	if perr != nil {
		perr = fmt.Errorf("platform components: %w", perr)
	}
	// What changed about them is logged here, once: the status alone would
	// leave a failed component out of the controller's log.
	s.logPlatform(ctx, current, storedPlatform(current), installed)
	// The components the tenant enabled, after the platform's (they may
	// require one of them); each writes its own status.
	if terr := s.installTenant(ctx, current, installed); terr != nil {
		perr = errors.Join(perr, fmt.Errorf("tenant components: %w", terr))
	}
	// The retry was acted on once the components were installed in this
	// pass (or it named none); a pass that did not get to them keeps it.
	if r := row.Retry; r != nil && (installed != nil || len(r.Components) == 0) {
		if err := s.deps.Store.Clusters().ClearRetry(ctx, tenantID, id, r.At); err != nil {
			perr = errors.Join(perr, fmt.Errorf("clear the retry: %w", err))
		}
	}
	c, err := s.viewWith(ctx, current, FailureCondition(failed), installed)
	if err != nil {
		return errors.Join(failed, perr, fmt.Errorf("observe: %w", err))
	}
	if addonsRecovering(storedUpgrade(current), c.GetStatus().GetUpgrade()) {
		if rerr := s.recoverAddons(ctx, current, now()); rerr != nil {
			perr = errors.Join(perr, fmt.Errorf("recover the add-ons: %w", rerr))
		}
	}
	return errors.Join(failed, perr, s.remember(ctx, current, c.GetStatus()))
}

// observePaused records a paused cluster's status: what its resources
// say now, the platform components as they were last recorded, and the
// failure recorded before the pause.
func (s *Reconciler) observePaused(ctx context.Context, row *model.Cluster) error {
	c, err := s.viewWith(ctx, row, storedFailure(row), nil)
	if err != nil {
		return fmt.Errorf("observe: %w", err)
	}
	return s.remember(ctx, row, c.GetStatus())
}

// moveOn is Reconcile without recording the status.
func (s *Reconciler) moveOn(ctx context.Context, row *model.Cluster) error {
	if row.State == StateDeleting {
		_, err := s.driveDelete(ctx, row)
		return err
	}
	if discovering(row) {
		// The import was stored without looking at the cloud: the EKS
		// cluster is looked up here, once, and the plan stored with the row.
		stored, err := s.discoverImport(ctx, row)
		if reconcile.ImportCannotSucceed(err) {
			// No such cluster, or one EKS refuses: asking again changes
			// nothing. The import stops, with the reason as its plan.
			return s.stopImport(ctx, row, err)
		}
		if err != nil {
			return fmt.Errorf("discover: %w", err)
		}
		if !stored {
			return nil // a call changed the row: the next pass sees it
		}
	}
	if row.State == StateImported || row.State == StateBlocked {
		// Read-only until its takeover: what the import discovered is
		// remembered, and a spec edited on the object is put back.
		if err := s.keepDiscovered(ctx, row); err != nil {
			return fmt.Errorf("keep the discovered spec: %w", err)
		}
		if row.State == StateImported && row.AppliedGeneration < row.Generation {
			// The import was stored; its Observe-only objects are created here.
			if err := s.applyStoredImport(ctx, row); err != nil {
				return fmt.Errorf("apply: %w", err)
			}
		} else if err := s.repairImported(ctx, row); err != nil {
			return fmt.Errorf("repair: %w", err)
		}
		// Observed only, until TakeOverCluster. Counted as applied with this
		// renderer, or a renderer change would list it as pending forever.
		return s.markApplied(ctx, row, nil)
	}
	if unresolved(row) {
		// The call stored the generation without looking at the cloud: the
		// EKS name and the versions are checked here, before any render.
		stored, err := s.resolve(ctx, row)
		if err != nil {
			return fmt.Errorf("resolve: %w", err)
		}
		if !stored {
			return nil // a call changed the row: the next pass sees it
		}
	}
	// The add-on versions EKS recommends, checked daily (not during an
	// upgrade or a release move): changed ones are applied by this pass.
	s.recheckAddons(ctx, row)
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
	if err := s.deps.Store.Clusters().Record(ctx, row); err != nil && !errors.Is(err, repository.ErrConflict) {
		return err // a conflict: an API call changed the row; the next pass sees it
	}
	return nil
}
