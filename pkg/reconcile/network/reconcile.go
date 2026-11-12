// Package network moves networks towards their stored spec: it applies a
// network's managed resources (in order, through the sequence engine),
// takes over and re-tags imported ones, deletes them tier by tier,
// observes them for the network's status, and records what it saw. The
// API handlers (pkg/server/network) and the background loop both call it.
// It knows no RPC library: a failure a caller can act on is a fault
// (pkg/fault).
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
// has to. It moves a deletion on (the next tier, then the row), applies
// the network again while its generation or renderer version is behind
// (after a change, and for the NAT gateways held back until the internet
// gateway is attached: crossplaneaws.NetworkSet), and records the network's status (what
// GetNetwork returns) and READY once it is (settle). Imported networks
// are observed only: nothing is applied for them. Every step is
// idempotent. A network that does not exist is done.
func (s *Reconciler) Reconcile(ctx context.Context, tenantID, id string) error {
	row, err := s.deps.Store.Networks().Get(ctx, tenantID, id)
	if errors.Is(err, repository.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if row.State == StateDeleting {
		if err := s.Finalize(ctx, row); err != nil {
			return err
		}
		// What GetNetwork returns while it goes: how far the deletion is.
		current, err := s.deps.Store.Networks().Get(ctx, tenantID, id)
		if errors.Is(err, repository.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		n, err := s.View(ctx, current)
		if err != nil {
			return err
		}
		return s.remember(ctx, current, n.GetStatus())
	}
	if err := s.moveOn(ctx, row); err != nil {
		return err
	}
	return s.settle(ctx, row)
}

// moveOn applies a network again while its generation or renderer version
// is behind.
func (s *Reconciler) moveOn(ctx context.Context, row *model.Network) error {
	if row.AppliedGeneration >= row.Generation && !s.deps.RenderBehind(row.RenderVersion, row.TenantID, row.ID) {
		return nil
	}
	// Imported (also after takeover): its objects were rendered from the
	// discovered VPC, not from the spec. Rendering the spec would create a
	// second set of resources.
	if row.State == StateImported || row.State == StateBlocked {
		return s.MarkApplied(ctx, row)
	}
	defer s.Recheck(ctx, row)
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
