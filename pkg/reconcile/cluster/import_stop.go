package cluster

import (
	"context"
	"errors"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/fault"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// stopImport ends an import whose lookup said it cannot succeed
// (reconcile.ImportCannotSucceed): the cluster is stored as a blocked
// import whose plan is the one line saying why, with nothing to apply.
// From then on it is not DISCOVERING, so the cloud is not asked again; it
// stays FAILED with the reason until it is deleted. A row a call changed
// meanwhile is left for the next pass.
func (s *Reconciler) stopImport(ctx context.Context, row *model.Cluster, cause error) error {
	status, err := Encode(&cloudv1.ClusterStatus{Import: &cloudv1.ImportStatus{Phase: cloudv1.ImportPhase_IMPORT_PHASE_BLOCKED,
		Resources: []*cloudv1.ImportedResource{{Kind: "Cluster", ExternalName: row.Name,
			Disposition: cloudv1.Disposition_DISPOSITION_BLOCKING, Detail: reconcile.ImportStoppedDetail(cause)}}}})
	if err != nil {
		return fault.New(fault.Internal, err.Error())
	}
	before := *row
	row.State, row.Status = StateBlocked, status
	row.AppliedGeneration, row.RenderVersion = row.Generation, crossplaneaws.RenderVersion
	if err := s.deps.Store.Clusters().Record(ctx, row); err != nil {
		*row = before
		if errors.Is(err, repository.ErrConflict) || errors.Is(err, repository.ErrNotFound) {
			return nil
		}
		return storeError(err)
	}
	s.logger().WarnContext(ctx, "cluster import stopped: it cannot succeed", "tenant_id", row.TenantID, "cluster_id", row.ID,
		"name", row.Name, "reason", cause.Error())
	return nil
}
