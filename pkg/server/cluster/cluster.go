// Package cluster implements the cloud.v1 ClusterService for AWS (EKS). It
// holds the request handling only: validation, preconditions, update
// masks, the stored row and the mapping of failures to Connect codes. The
// handlers store what was asked for (the row is the source of truth) and
// change nothing in the management cluster. What builds the cluster from
// the row (Crossplane managed resources in the tenant namespace) is
// pkg/reconcile/cluster, called by the background loop; what goes wrong
// there is the cluster's status. The handlers use it to check a request
// and to read.
package cluster

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"connectrpc.com/connect/v2"
	"github.com/google/uuid"

	corecluster "github.com/yindia/cluster-vending/pkg/cluster"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/fault"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1/cloudv1connect"
	reconcilecluster "github.com/yindia/cluster-vending/pkg/reconcile/cluster"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

// List paging.
const (
	defaultPageSize = 50
	maxPageSize     = 200
)

// ClusterService implements cloud.v1.ClusterService.
//
// It holds the request handling only. What moves a cluster towards its
// spec is the embedded Reconciler (pkg/reconcile/cluster): the background
// loop calls its Reconcile; the handlers only its checks and reads.
type ClusterService struct {
	*reconcilecluster.Reconciler
	deps service.Deps
}

var _ cloudv1connect.ClusterServiceHandler = (*ClusterService)(nil)

// NewClusterService returns the ClusterService.
func NewClusterService(d service.Deps) *ClusterService {
	return &ClusterService{Reconciler: reconcilecluster.New(d), deps: d}
}

func (s *ClusterService) logger() *slog.Logger {
	if s.deps.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return s.deps.Logger
}

// CreateCluster stores the cluster with its defaults filled in,
// PROVISIONING. Its managed resources are created by the reconciler; a
// failure there is the cluster's status. A retry with the same request_id
// returns the cluster created by the first call.
func (s *ClusterService) CreateCluster(ctx context.Context, req *cloudv1.CreateClusterRequest) (_ *cloudv1.CreateClusterResponse, err error) {
	defer func() { err = rpcError(err) }()
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	if rid := req.GetRequestId(); rid != "" {
		if row, err := s.deps.Store.Clusters().GetByCreateRequest(ctx, req.GetTenantId(), rid); err == nil {
			c, err := stored(row)
			return &cloudv1.CreateClusterResponse{Cluster: c}, err
		} else if !errors.Is(err, repository.ErrNotFound) {
			return nil, service.ToConnect(err)
		}
	}
	row, err := s.NewRow(ctx, req)
	if err != nil {
		return nil, err
	}
	if err := s.deps.Store.Clusters().Create(ctx, row); err != nil {
		return nil, createError(err, row)
	}
	s.logger().InfoContext(ctx, "cluster stored", "tenant_id", row.TenantID, "cluster_id", row.ID, "name", row.Name,
		"network_id", row.NetworkID)
	c, err := stored(row)
	return &cloudv1.CreateClusterResponse{Cluster: c}, err
}

// supported rejects what this version cannot build.
func supported(spec *cloudv1.ClusterSpec) error {
	switch {
	case spec.GetAws() == nil:
		return connect.NewError(connect.CodeUnimplemented, "only aws clusters are supported")
	case spec.GetExistingCluster() != nil:
		return connect.NewError(connect.CodeUnimplemented,
			"spec.existing_cluster is not implemented; import a cluster with ImportCluster and TakeOverCluster")
	}
	return nil
}

// NewRow checks a create request (the contract's rules, what this version
// can build, the labels, the network and its subnets, the name, the
// versions) and returns the row CreateCluster stores for it, not stored
// yet: the cluster as sent (input) and resolved (spec), PROVISIONING, its
// generation not applied yet (the reconciler applies it). The ID is
// chosen here, it names the resources. The controller builds the same row
// for a Cluster object that was created without the API.
func (s *ClusterService) NewRow(ctx context.Context, req *cloudv1.CreateClusterRequest) (*model.Cluster, error) {
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	if err := supported(req.GetCluster().GetSpec()); err != nil {
		return nil, err
	}
	if err := service.CheckAWSLabels(req.GetCluster().GetLabels()); err != nil {
		return nil, err
	}
	t, err := s.Target(ctx, req.GetTenantId(), req.GetCluster().GetSpec())
	if err != nil {
		return nil, err
	}
	if err := s.checkName(ctx, req.GetTenantId(), req.GetCluster().GetName(), req.GetCluster().GetSpec().GetRegion(), t); err != nil {
		return nil, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error())
	}
	in := req.GetCluster()
	spec, err := corecluster.ResolveSpec(in.GetSpec(), t.Zones)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	if err := s.resolveVersions(ctx, t.Account, spec, pinnedAddons(in.GetSpec())); err != nil {
		return nil, err
	}
	input, err1 := reconcilecluster.Encode(in)
	raw, err2 := reconcilecluster.Encode(spec)
	if err := errors.Join(err1, err2); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error())
	}
	row := &model.Cluster{
		Resource: model.Resource{Base: model.Base{ID: id.String()}, DisplayName: in.GetDisplayName(),
			Labels: in.GetLabels(), State: reconcilecluster.StateProvisioning, Spec: raw},
		TenantID: req.GetTenantId(), Name: in.GetName(), Input: input, NetworkID: t.Network.ID,
		Provider: model.ProviderAWS, Region: spec.GetRegion(),
	}
	if rid := req.GetRequestId(); rid != "" {
		row.CreateRequestID = &rid
	}
	return row, nil
}

// createError maps a failed insert: a duplicate is a second cluster of the
// name in the account and region (the name check passed, but another
// create won the race) or a reused request_id; a conflict is the network
// being deleted at the same moment.
func createError(err error, row *model.Cluster) error {
	if errors.Is(err, repository.ErrConflict) {
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Sprintf(
			"network %s is being deleted; a cluster cannot be created in it", row.NetworkID))
	}
	if errors.Is(err, repository.ErrAlreadyExists) {
		return connect.NewError(connect.CodeAlreadyExists, fmt.Sprintf(
			"a cluster named %q already exists in this account and %s (or the request_id was used before)", row.Name, row.Region))
	}
	return service.ToConnect(err)
}

// GetCluster returns one cluster from the database: its spec and the
// status last recorded for it (stored). It does not read the management
// cluster; GetClusterStatus does.
func (s *ClusterService) GetCluster(ctx context.Context, req *cloudv1.GetClusterRequest) (_ *cloudv1.GetClusterResponse, err error) {
	defer func() { err = rpcError(err) }()
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	row, err := s.get(ctx, req.GetTenantId(), req.GetClusterId())
	if err != nil {
		return nil, err
	}
	c, err := stored(row)
	if err != nil {
		return nil, err
	}
	return &cloudv1.GetClusterResponse{Cluster: c}, nil
}

// ListClusters returns a page of the tenant's clusters, oldest first, from
// the database like GetCluster.
func (s *ClusterService) ListClusters(ctx context.Context, req *cloudv1.ListClustersRequest) (_ *cloudv1.ListClustersResponse, err error) {
	defer func() { err = rpcError(err) }()
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	size := int(req.GetPageSize())
	if size <= 0 {
		size = defaultPageSize
	}
	size = min(size, maxPageSize)
	rows, err := s.deps.Store.Clusters().List(ctx, req.GetTenantId(), repository.Page{After: req.GetPageToken(), Limit: size + 1})
	if err != nil {
		return nil, service.ToConnect(err)
	}
	res := &cloudv1.ListClustersResponse{}
	if len(rows) > size {
		rows = rows[:size]
		res.NextPageToken = rows[size-1].ID
	}
	for i := range rows {
		c, err := stored(&rows[i])
		if err != nil {
			return nil, err
		}
		res.Clusters = append(res.Clusters, c)
	}
	return res, nil
}

func (s *ClusterService) get(ctx context.Context, tenantID, id string) (*model.Cluster, error) {
	row, err := s.deps.Store.Clusters().Get(ctx, tenantID, id)
	if errors.Is(err, repository.ErrNotFound) {
		return nil, notFound(tenantID, id)
	}
	if err != nil {
		return nil, service.ToConnect(err)
	}
	return row, nil
}

func notFound(tenantID, id string) error {
	return connect.NewError(connect.CodeNotFound, fmt.Sprintf("tenant %s has no cluster %s", tenantID, id))
}

// PendingStates are the stored states of clusters the background loop
// looks at every pass (reconcilecluster.PendingStates).
func PendingStates() []string { return reconcilecluster.PendingStates() }

// rpcError is the answer to an RPC that failed in code that knows no RPC
// library (the Reconciler): a fault becomes its Connect code and message.
// Every other error passes as it is.
func rpcError(err error) error {
	if _, ok := fault.As(err); ok {
		return service.ToConnect(err)
	}
	return err
}
