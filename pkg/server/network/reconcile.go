package network

import (
	"context"
	"errors"
	"fmt"

	"github.com/yindia/cluster-vending/pkg/database/repository"
)

// PendingStates are the stored states of networks the background loop
// looks at every pass.
func PendingStates() []string { return []string{stateDeleting} }

// Reconcile moves one network on; the background loop calls it, so no read
// has to. It moves a deletion on (the next tier, then the row), and
// applies the network again while its generation or renderer version is
// behind. Imported networks are observed only: nothing is applied for
// them. Every step is idempotent. A network that does not exist is done.
func (s *NetworkService) Reconcile(ctx context.Context, tenantID, id string) error {
	row, err := s.deps.Store.Networks().Get(ctx, tenantID, id)
	if errors.Is(err, repository.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if row.State == stateDeleting {
		return s.finalize(ctx, row)
	}
	if row.AppliedGeneration >= row.Generation && !s.deps.RenderBehind(row.RenderVersion, row.TenantID, row.ID) {
		return nil
	}
	if row.State == stateImported || row.State == stateBlocked {
		return s.markApplied(ctx, row) // observe-only: rendered at import, nothing to roll out
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
