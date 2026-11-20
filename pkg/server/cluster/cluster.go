// Package cluster implements the cloud.v1 ClusterService for AWS (EKS). It
// holds the request handling only: validation, preconditions, update
// masks, the stored row and the mapping of failures to Connect codes. The
// handlers store what was asked for (the row is the source of truth) and
// change nothing in the management cluster. What builds the cluster from
// the row (Crossplane managed resources in the tenant namespace) is
// pkg/reconcile/cluster, called by the controller; what goes wrong
// there is the cluster's status. The handlers use it to check a request
// and to read.
package cluster

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"connectrpc.com/connect/v2"

	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/fault"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1/cloudv1connect"
	recon "github.com/yindia/cluster-vending/pkg/reconcile"
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
	deps recon.Deps
	// results reads scan results from the clusters' results buckets
	// (WithResults); nil: none can be.
	results ResultStore
}

var _ cloudv1connect.ClusterServiceHandler = (*ClusterService)(nil)

// NewClusterService returns the ClusterService.
func NewClusterService(d recon.Deps, opts ...Option) *ClusterService {
	s := &ClusterService{Reconciler: reconcilecluster.New(d), deps: d}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func (s *ClusterService) logger() *slog.Logger {
	if s.deps.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return s.deps.Logger
}

// CreateCluster stores the cluster with its defaults filled in,
// PROVISIONING (reconcilecluster.Reconciler.NewRow). It does not look at
// the cloud: the reconciler checks the name against EKS and resolves the
// Kubernetes and add-on versions before it creates anything, then creates
// the managed resources; a failure there (a taken name, a version EKS
// does not offer) is the cluster's status. A retry with the same
// request_id returns the cluster created by the first call.
func (s *ClusterService) CreateCluster(ctx context.Context, req *cloudv1.CreateClusterRequest) (_ *cloudv1.CreateClusterResponse, err error) {
	defer func() { err = rpcError(err) }()
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	if rid := req.GetRequestId(); rid != "" {
		if row, err := s.deps.Store.Clusters().GetByCreateRequest(ctx, req.GetTenantId(), rid); err == nil {
			if !createMatches(row, req) {
				return nil, requestIDReused(rid)
			}
			c, err := s.stored(ctx, row)
			return &cloudv1.CreateClusterResponse{Cluster: c}, err
		} else if !errors.Is(err, repository.ErrNotFound) {
			return nil, service.ToConnect(err)
		}
	}
	if err := checkNodeGroupAllowlist(req.GetCluster().GetSpec().GetNodeGroups(), nil,
		s.deps.NodeGroupInstanceFamilies, s.deps.NodeGroupInstanceTypes); err != nil {
		return nil, err
	}
	row, err := s.NewRow(ctx, req)
	if err != nil {
		return nil, err
	}
	if err := s.usableNetwork(ctx, row.TenantID, row.NetworkID); err != nil {
		return nil, err
	}
	if err := s.deps.Store.Clusters().Create(ctx, row); err != nil {
		return nil, createError(err, row)
	}
	s.logger().InfoContext(ctx, "cluster stored", "tenant_id", row.TenantID, "cluster_id", row.ID, "name", row.Name,
		"network_id", row.NetworkID)
	c, err := s.stored(ctx, row)
	return &cloudv1.CreateClusterResponse{Cluster: c}, err
}

// createError maps a failed insert: a duplicate is a second cluster of the
// name in the tenant (the name check passed, but another create won the
// race) or a reused request_id; a conflict is the network being deleted at
// the same moment.
func createError(err error, row *model.Cluster) error {
	if errors.Is(err, cloudstore.ErrEKSNameReserved) {
		return connect.NewError(connect.CodeAlreadyExists, cloudstore.ErrEKSNameReserved.Error())
	}
	if errors.Is(err, repository.ErrConflict) {
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Sprintf(
			"network %s is being deleted; a cluster cannot be created in it", row.NetworkID))
	}
	if errors.Is(err, repository.ErrAlreadyExists) {
		return connect.NewError(connect.CodeAlreadyExists, fmt.Sprintf(
			"a cluster named %q already exists in tenant %s: cluster names are unique per tenant (or the request_id was used before)",
			row.Name, row.TenantID))
	}
	return service.ToConnect(err)
}

// GetCluster returns one cluster from its stored Kubernetes state: its spec and the
// status last recorded for it (stored); with live, the status read now
// from the management cluster. Like every read it changes nothing.
func (s *ClusterService) GetCluster(ctx context.Context, req *cloudv1.GetClusterRequest) (_ *cloudv1.GetClusterResponse, err error) {
	defer func() { err = rpcError(err) }()
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	row, err := s.get(ctx, req.GetTenantId(), req.GetClusterId())
	if err != nil {
		return nil, err
	}
	read := func(row *model.Cluster) (*cloudv1.Cluster, error) { return s.stored(ctx, row) }
	if req.GetLive() {
		read = func(row *model.Cluster) (*cloudv1.Cluster, error) { return s.View(ctx, row) }
	}
	c, err := read(row)
	if err != nil {
		return nil, err
	}
	return &cloudv1.GetClusterResponse{Cluster: c, AsRequested: req.GetAsRequested() && asRequested(row, c)}, nil
}

// asRequested gives c the spec, labels and display name its client asked
// for: the stored input (as created, with the updates since), and reports
// whether there was one. An import's input is its import request, which
// has no spec: c keeps its resolved spec.
func asRequested(row *model.Cluster, c *cloudv1.Cluster) bool {
	if len(row.Input) == 0 {
		return false
	}
	in := &cloudv1.Cluster{}
	if err := reconcilecluster.Lenient.Unmarshal(row.Input, in); err != nil || in.GetSpec() == nil {
		return false
	}
	c.Spec, c.Labels, c.DisplayName = in.GetSpec(), in.GetLabels(), in.GetDisplayName()
	return true
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
		c, err := s.stored(ctx, &rows[i])
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

// rpcError is the answer to an RPC that failed in code that knows no RPC
// library (the Reconciler): a fault becomes its Connect code and message,
// an ended context CANCELED or DEADLINE_EXCEEDED. Every other error passes
// as it is.
func rpcError(err error) error {
	if _, ok := fault.As(err); ok || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return service.ToConnect(err)
	}
	return err
}
