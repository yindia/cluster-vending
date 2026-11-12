package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"connectrpc.com/connect/v2"
	"github.com/google/uuid"

	awscloud "github.com/yindia/cluster-vending/pkg/cloud/aws"
	corecluster "github.com/yindia/cluster-vending/pkg/cluster"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	reconcilecluster "github.com/yindia/cluster-vending/pkg/reconcile/cluster"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

// ImportCluster looks an existing EKS cluster up once, read-only, and
// stores the import: the spec derived from the cluster, the plan
// (status.import) and the objects planned for it. The reconciler creates
// those (a managed resource for each of the cluster's resources that only
// observes), without looking the cloud up again. Nothing changes in the
// cloud. The cluster is IMPORTED, or FAILED when the plan is BLOCKED (then
// nothing is imported).
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
	network, account, subnets, err := s.importNetwork(ctx, req.GetTenantId(), req.GetNetworkId())
	if err != nil {
		return nil, err
	}
	if err := s.nameTaken(ctx, req.GetTenantId(), req.GetName(), network.Region, account); err != nil {
		return nil, err
	}
	found, err := s.discover(ctx, account, network.Region, req)
	if err != nil {
		return nil, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error())
	}
	row := &model.Cluster{Resource: model.Resource{Base: model.Base{ID: id.String()}, DisplayName: req.GetDisplayName()},
		TenantID: req.GetTenantId(), Name: req.GetName(), NetworkID: network.ID, Provider: model.ProviderAWS,
		Region: network.Region, Imported: true}
	plan := corecluster.PlanImport(corecluster.ImportInput{Found: found, Name: req.GetName(), NodeGroup: req.GetAws().GetDefaultNodeGroupName(),
		NetworkID: network.ID, Region: network.Region, Subnets: subnets, Base: s.Base(row, account), Scheme: s.deps.Kube.Scheme()})
	if err := s.insertImport(ctx, req, row, plan); err != nil {
		return nil, err
	}
	s.logger().InfoContext(ctx, "cluster import stored", "tenant_id", row.TenantID, "cluster_id", row.ID, "name", row.Name,
		"network_id", network.ID, "blocked", plan.Blocked(), "plan_lines", len(plan.Lines), "objects", len(plan.Objects))
	c, err := stored(row)
	return &cloudv1.ImportClusterResponse{Cluster: c}, err
}

// importNetwork loads the network an imported cluster runs in, its account
// and its private subnets. The network may be imported (Observe-only) or
// owned; the takeover needs it owned.
func (s *ClusterService) importNetwork(ctx context.Context, tenantID, networkID string) (*model.Network, *model.CloudAccount, []corecluster.PrivateSubnet, error) {
	n, err := s.deps.Store.Networks().Get(ctx, tenantID, networkID)
	if errors.Is(err, repository.ErrNotFound) {
		return nil, nil, nil, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Sprintf("tenant %s has no network %s; import the cluster's network first (ImportNetwork)", tenantID, networkID))
	}
	if err != nil {
		return nil, nil, nil, service.ToConnect(err)
	}
	if n.State == "NETWORK_STATE_DELETING" || n.State == "NETWORK_STATE_FAILED" || n.Provider != model.ProviderAWS {
		return nil, nil, nil, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Sprintf("network %s is %s (%s); a cluster is imported into an aws network that is imported or owned", n.ID, n.State, n.Provider))
	}
	account, err := s.deps.Store.Tenants().GetCloudAccountByID(ctx, tenantID, n.CloudAccountID)
	if err != nil {
		return nil, nil, nil, service.ToConnect(err)
	}
	records, err := s.deps.Store.ManagedResources().ListByOwner(ctx, model.OwnerNetwork, n.ID)
	if err != nil {
		return nil, nil, nil, service.ToConnect(err)
	}
	views, err := s.deps.Observe(ctx, records)
	if err != nil {
		return nil, nil, nil, err
	}
	return n, account, corecluster.PrivateSubnets(views), nil
}

// discover makes the read-only AWS lookup.
func (s *ClusterService) discover(ctx context.Context, a *model.CloudAccount, region string, req *cloudv1.ImportClusterRequest) (*awscloud.Cluster, error) {
	if s.deps.AWSClusterDiscovery == nil {
		return nil, connect.NewError(connect.CodeUnavailable, "AWS lookups are not configured on this server")
	}
	access, err := s.deps.AWSAccess(ctx, a)
	if err != nil {
		return nil, err
	}
	found, err := s.deps.AWSClusterDiscovery.DiscoverCluster(ctx, access, region, req.GetName(), req.GetAws().GetDefaultNodeGroupName())
	switch {
	case err == nil:
		return found, nil
	case errors.Is(err, awscloud.ErrNotFound):
		return nil, connect.NewError(connect.CodeNotFound, err.Error())
	case errors.Is(err, awscloud.ErrInvalid):
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
	case errors.Is(err, awscloud.ErrAccessDenied):
		return nil, connect.NewError(connect.CodeFailedPrecondition, "cannot read the cluster with the account's credentials: "+err.Error())
	default:
		s.logger().ErrorContext(ctx, "discover cluster", "name", req.GetName(), "error", err)
		return nil, connect.NewError(connect.CodeUnavailable, "the AWS lookup failed: "+err.Error())
	}
}

// insertImport stores the imported cluster: the derived spec, the request
// as sent (input), the plan (status), and what it keeps of the roles it
// adopts together with the objects planned for it (adoption: the
// reconciler applies them), IMPORTED or, when blocked, FAILED. Its
// generation is not applied yet; a blocked import has nothing to apply.
func (s *ClusterService) insertImport(ctx context.Context, req *cloudv1.ImportClusterRequest, row *model.Cluster, plan corecluster.ImportPlan) error {
	phase := cloudv1.ImportPhase_IMPORT_PHASE_PLANNED
	row.State = reconcilecluster.StateImported
	if plan.Blocked() {
		phase, row.State = cloudv1.ImportPhase_IMPORT_PHASE_BLOCKED, reconcilecluster.StateBlocked
	}
	spec := plan.Spec
	if spec == nil {
		spec = &cloudv1.ClusterSpec{Region: row.Region, Network: &cloudv1.ClusterNetwork{NetworkId: row.NetworkID},
			Provider: &cloudv1.ClusterSpec_Aws{Aws: &cloudv1.AwsClusterSpec{}}}
	}
	rawSpec, err1 := reconcilecluster.Encode(spec)
	input, err2 := reconcilecluster.Encode(req)
	status, err3 := reconcilecluster.Encode(&cloudv1.ClusterStatus{Import: &cloudv1.ImportStatus{Phase: phase, Resources: plan.Lines}})
	raw, err4 := json.Marshal(plan.Adoption)
	adoption := model.JSON(raw)
	row.Generation, row.AppliedGeneration, row.RenderVersion = 1, 1, crossplaneaws.RenderVersion
	if err4 == nil && !plan.Blocked() {
		adoption, err4 = reconcilecluster.WithImportObjects(adoption, plan, s.deps.Kube.Scheme())
		row.AppliedGeneration, row.RenderVersion = 0, 0 // the reconciler applies the objects
	}
	if err := errors.Join(err1, err2, err3, err4); err != nil {
		return connect.NewError(connect.CodeInternal, err.Error())
	}
	row.Spec, row.Input, row.Status, row.Adoption = rawSpec, input, status, adoption
	if rid := req.GetRequestId(); rid != "" {
		row.CreateRequestID = &rid
	}
	if err := s.deps.Store.Clusters().Create(ctx, row); err != nil {
		return createError(err, row)
	}
	return nil
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
	row.State = reconcilecluster.StateUpdating
	row.Generation++
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
