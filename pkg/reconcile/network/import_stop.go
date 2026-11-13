package network

import (
	"context"
	"errors"

	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/fault"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// stopImport ends an import whose lookup said it cannot succeed
// (reconcile.ImportCannotSucceed): the network is stored as a blocked
// import whose plan is the one line saying why, with nothing to apply.
// From then on it is not DISCOVERING, so the cloud is not asked again; it
// stays FAILED with the reason until it is deleted (it keeps the VPC as
// its external ID until then). A row a call changed meanwhile is left for
// the next pass.
func (s *Reconciler) stopImport(ctx context.Context, row *model.Network, cause error) error {
	vpc := ""
	if row.ExternalID != nil {
		vpc = *row.ExternalID
	}
	status, err := Encode(&cloudv1.NetworkStatus{Import: &cloudv1.ImportStatus{Phase: cloudv1.ImportPhase_IMPORT_PHASE_BLOCKED,
		Resources: []*cloudv1.ImportedResource{{Kind: "VPC", ExternalName: vpc,
			Disposition: cloudv1.Disposition_DISPOSITION_BLOCKING, Detail: reconcile.ImportStoppedDetail(cause)}}}})
	if err != nil {
		return fault.New(fault.Internal, err.Error())
	}
	before := *row
	row.State, row.Status = StateBlocked, status
	if err := s.deps.Store.Networks().Update(ctx, row); err != nil {
		*row = before
		if errors.Is(err, repository.ErrConflict) || errors.Is(err, repository.ErrNotFound) {
			return nil
		}
		return err
	}
	s.logger().WarnContext(ctx, "network import stopped: it cannot succeed", "tenant_id", row.TenantID, "network_id", row.ID,
		"vpc_id", vpc, "reason", cause.Error())
	return nil
}
