package cluster

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect/v2"
	"github.com/google/uuid"

	corecluster "github.com/yindia/cluster-vending/pkg/cluster"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	reconcilecluster "github.com/yindia/cluster-vending/pkg/reconcile/cluster"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

// ImportCluster stores the import of an existing EKS cluster: the
// request, and a cluster that is IMPORTED with its import DISCOVERING. It
// does not look at the cloud. The reconciler looks the cluster up once,
// read-only, stores the spec derived from it, the plan (status.import:
// PLANNED, or BLOCKED and then the cluster is FAILED and nothing is
// imported) and the objects planned for it, and creates those (a managed
// resource for each of the cluster's resources that only observes). A
// cluster that does not exist or cannot be read is the cluster's status
// (FAILED, with the reason), not the answer of this call. Nothing changes
// in the cloud.
func (s *ClusterService) ImportCluster(ctx context.Context, req *cloudv1.ImportClusterRequest) (_ *cloudv1.ImportClusterResponse, err error) {
	defer func() { err = rpcError(err) }()
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	if req.GetAws() == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, "only aws clusters can be imported")
	}
	if rid := req.GetRequestId(); rid != "" {
		if row, err := s.deps.Store.Clusters().GetByCreateRequest(ctx, req.GetTenantId(), rid); err == nil {
			c, err := stored(row)
			return &cloudv1.ImportClusterResponse{Cluster: c}, err
		} else if !errors.Is(err, repository.ErrNotFound) {
			return nil, service.ToConnect(err)
		}
	}
	network, _, err := s.ImportNetwork(ctx, req.GetTenantId(), req.GetNetworkId())
	if err != nil {
		return nil, err
	}
	if err := s.NameTaken(ctx, req.GetTenantId(), req.GetName()); err != nil {
		return nil, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error())
	}
	row, err := reconcilecluster.NewImportRow(req, network, id.String())
	if err != nil {
		return nil, err
	}
	if err := s.deps.Store.Clusters().Create(ctx, row); err != nil {
		return nil, createError(err, row)
	}
	s.logger().InfoContext(ctx, "cluster import stored", "tenant_id", row.TenantID, "cluster_id", row.ID, "name", row.Name,
		"network_id", network.ID)
	c, err := stored(row)
	return &cloudv1.ImportClusterResponse{Cluster: c}, err
}

// TakeOverCluster stores an IMPORTED cluster as taken over. The reconciler
// then makes the platform its owner: the objects carried over switch to
// full management (without Delete where marked); the rest is rendered from
// the spec like any cluster's, which drops their Observe-only policy, adds
// the platform's tags and creates what the plan lists as WILL_CREATE. The
// network must be owned by then.
func (s *ClusterService) TakeOverCluster(ctx context.Context, req *cloudv1.TakeOverClusterRequest) (_ *cloudv1.TakeOverClusterResponse, err error) {
	defer func() { err = rpcError(err) }()
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	row, err := s.get(ctx, req.GetTenantId(), req.GetClusterId())
	if err != nil {
		return nil, err
	}
	imp, err := takeOverAllowed(row, req)
	if err != nil {
		return nil, err
	}
	// The plan that was reviewed is the one applied: a spec changed on the
	// object behind the API's back, which no pass has put back yet, is
	// dropped here.
	if _, err := reconcilecluster.RestoreDiscovered(row); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error())
	}
	spec, err := reconcilecluster.SpecOf(row)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error())
	}
	// Refuses a network that is still imported read-only.
	if _, err := s.Target(ctx, row.TenantID, spec); err != nil {
		return nil, err
	}
	// The row only: once anything is owned the cluster is no longer an
	// imported, read-only one (deleting that leaves the cloud alone), so
	// the reconciler owns the carried-over objects and renders the rest
	// once the row says so.
	imp.Phase = cloudv1.ImportPhase_IMPORT_PHASE_CONVERGING
	if row.Status, err = reconcilecluster.Encode(&cloudv1.ClusterStatus{Import: imp}); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error())
	}
	if req.GetSkipPlatformComponents() {
		// The cluster runs its own: the platform's are never installed.
		if row.Adoption, err = reconcilecluster.WithoutPlatformComponents(row.Adoption); err != nil {
			return nil, connect.NewError(connect.CodeInternal, err.Error())
		}
	}
	row.State = reconcilecluster.StateUpdating
	row.Generation++
	// The versions are the ones the cluster runs: nothing to look up.
	row.ResolvedGeneration = row.Generation
	if err := s.deps.Store.Clusters().Update(ctx, row); err != nil {
		return nil, service.ToConnect(err)
	}
	s.logger().InfoContext(ctx, "cluster takeover stored", "tenant_id", row.TenantID, "cluster_id", row.ID)
	c, err := stored(row)
	return &cloudv1.TakeOverClusterResponse{Cluster: c}, err
}

// takeOverAllowed checks state, etag, phase and the acknowledgement of
// NOT_IMPORTED resources, and returns the stored plan.
func takeOverAllowed(row *model.Cluster, req *cloudv1.TakeOverClusterRequest) (*cloudv1.ImportStatus, error) {
	if row.State != reconcilecluster.StateImported {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Sprintf("cluster %s is not IMPORTED (only imported clusters are taken over)", row.ID))
	}
	if etag := req.GetEtag(); etag != "" && etag != row.ETag {
		return nil, connect.NewError(connect.CodeAborted, "etag mismatch: the cluster changed; get it again and retry")
	}
	if row.AppliedGeneration < row.Generation {
		// Its Observe-only objects are not there yet: there is nothing to own.
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Sprintf("cluster %s is still being imported; retry once its resources are observed (cluster status)", row.ID))
	}
	imp := reconcilecluster.StoredImport(row)
	if imp.GetPhase() != cloudv1.ImportPhase_IMPORT_PHASE_PLANNED {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Sprintf("import phase is %s, want PLANNED", imp.GetPhase()))
	}
	left := corecluster.NotImported(imp.GetResources())
	if len(left) > 0 && !req.GetAcknowledgeNotImported() {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Sprintf(
			"%d resources are NOT_IMPORTED (first: %s %s: %s); give them another owner, then retry with acknowledge_not_imported",
			len(left), left[0].GetKind(), left[0].GetExternalName(), left[0].GetDetail()))
	}
	return imp, nil
}
