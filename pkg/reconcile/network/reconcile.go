// Package network moves networks towards their stored spec: it applies a
// network's managed resources (in order, through the sequence engine),
// takes over and re-tags imported ones, deletes them tier by tier,
// observes them for the network's status, and records what it saw,
// including what went wrong. The background loop calls Reconcile; the API
// handlers (pkg/server/network) only store what was asked for, and use
// this package to check a request and to read. It knows no RPC library: a
// failure a caller can act on is a fault (pkg/fault).
package network

import (
	"context"
	"errors"
	"fmt"

	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// Reconciler moves networks on.
type Reconciler struct{ deps reconcile.Deps }

// New returns the Reconciler.
func New(d reconcile.Deps) *Reconciler { return &Reconciler{deps: d} }

// PendingStates are the stored states of networks the background loop
// looks at every pass.
// Imported networks are only observed: the loop records their status.
func PendingStates() []string {
	return []string{StateProvisioning, StateUpdating, StateDeleting, StateImported}
}

// Reconcile moves one network on; the background loop calls it, so no read
// has to, and no API call does: the handlers only store what was asked
// for. It moves a deletion on (the next tier, then the row), applies
// the network again while its generation or renderer version is behind
// (after a change, and for the NAT gateways held back until the internet
// gateway is attached: crossplaneaws.NetworkSet), and records the network's status (what
// GetNetwork returns) and READY once it is (settle). Imported networks
// get their Observe-only objects once and are observed from then on. What
// fails is recorded as the network's status (ConditionReconciled) and
// tried again on the next pass. Every step is idempotent. A network that
// does not exist is done.
func (s *Reconciler) Reconcile(ctx context.Context, tenantID, id string) error {
	row, err := s.deps.Store.Networks().Get(ctx, tenantID, id)
	if errors.Is(err, repository.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if row.State == StateDeleting {
		failed := s.Finalize(ctx, row)
		// What GetNetwork returns while it goes: how far the deletion is.
		current, err := s.deps.Store.Networks().Get(ctx, tenantID, id)
		if errors.Is(err, repository.ErrNotFound) {
			return failed
		}
		if err != nil {
			return errors.Join(failed, err)
		}
		n, err := s.view(ctx, current, FailureCondition(failed))
		if err != nil {
			return errors.Join(failed, err)
		}
		return errors.Join(failed, s.remember(ctx, current, n.GetStatus()))
	}
	// What went wrong is the network's status (settle) and the error of
	// the pass: the loop logs it and comes back.
	failed := s.moveOn(ctx, row)
	return errors.Join(failed, s.settle(ctx, row, failed))
}

// moveOn applies a network again while its generation or renderer version
// is behind.
func (s *Reconciler) moveOn(ctx context.Context, row *model.Network) error {
	if row.AppliedGeneration >= row.Generation && !s.deps.RenderBehind(row.RenderVersion, row.TenantID, row.ID) {
		return nil
	}
	// Imported (also after takeover): its objects are rendered from the
	// discovered VPC, not from the spec. Rendering the spec would create a
	// second set of resources.
	if row.State == StateImported || row.State == StateBlocked {
		if row.State == StateImported && row.AppliedGeneration < row.Generation {
			// The import was stored; its Observe-only objects are created here.
			if err := s.applyImported(ctx, row); err != nil {
				return fmt.Errorf("apply: %w", err)
			}
		}
		return s.MarkApplied(ctx, row)
	}
	if StoredImport(row) != nil {
		// Taken over: its objects are owned (the rest of a takeover that
		// stopped half-way), and what a change can touch is its labels, as
		// tags on the objects it has (also the rest of an update that
		// failed half-way).
		if err := s.OwnAll(ctx, row); err != nil {
			return err
		}
		return s.Retag(ctx, row)
	}
	spec, err := SpecOf(row)
	if err != nil {
		return err
	}
	if err := s.Reapply(ctx, row, spec); err != nil {
		return fmt.Errorf("apply: %w", err)
	}
	s.logger().InfoContext(ctx, "network applied again", "network_id", row.ID, "generation", row.Generation,
		"render_version", row.RenderVersion)
	return nil
}
