package network

import (
	"context"
	"errors"
	"fmt"

	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
)

// PendingStates are the stored states of networks the background loop
// looks at every pass.
// Imported networks are only observed: the loop records their status.
func PendingStates() []string {
	return []string{stateProvisioning, stateUpdating, stateDeleting, stateImported}
}

// Reconcile moves one network on; the background loop calls it, so no read
// has to. It moves a deletion on (the next tier, then the row), applies
// the network again while its generation or renderer version is behind
// (after a change, and for the NAT gateways held back until the internet
// gateway is attached: holdNAT), and records the network's status (what
// GetNetwork returns) and READY once it is (settle). Imported networks
// are observed only: nothing is applied for them. Every step is
// idempotent. A network that does not exist is done.
func (s *NetworkService) Reconcile(ctx context.Context, tenantID, id string) error {
	row, err := s.deps.Store.Networks().Get(ctx, tenantID, id)
	if errors.Is(err, repository.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if row.State == stateDeleting {
		if err := s.finalize(ctx, row); err != nil {
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
		n, err := s.view(ctx, current)
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
func (s *NetworkService) moveOn(ctx context.Context, row *model.Network) error {
	if row.AppliedGeneration >= row.Generation && !s.deps.RenderBehind(row.RenderVersion, row.TenantID, row.ID) {
		return nil
	}
	// Imported (also after takeover): its objects were rendered from the
	// discovered VPC, not from the spec. Rendering the spec would create a
	// second set of resources.
	if row.State == stateImported || row.State == stateBlocked {
		return s.markApplied(ctx, row)
	}
	if storedImport(row) != nil {
		// Taken over: what a change can touch is its labels, as tags on the
		// objects it has (also the rest of an update that failed half-way).
		return s.retag(ctx, row)
	}
	spec, err := specOf(row)
	if err != nil {
		return err
	}
	if err := s.reapply(ctx, row, spec); err != nil {
		return fmt.Errorf("apply: %w", err)
	}
	s.logger().InfoContext(ctx, "network applied again", "network_id", row.ID, "generation", row.Generation,
		"render_version", row.RenderVersion)
	return nil
}
