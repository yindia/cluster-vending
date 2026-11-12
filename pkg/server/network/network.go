// Package network implements the cloud.v1 NetworkService: the RPC
// handlers. They validate a request and store what was asked for (the row
// is the source of truth); they change nothing in the management cluster.
// pkg/reconcile/network, called by the controller, builds the network
// from Crossplane managed resources in the tenant namespace, and reports
// what goes wrong doing so as the network's status.
package network

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"connectrpc.com/connect/v2"

	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1/cloudv1connect"
	"github.com/yindia/cluster-vending/pkg/reconcile"

	netreconcile "github.com/yindia/cluster-vending/pkg/reconcile/network"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

// List paging.
const (
	defaultPageSize = 50
	maxPageSize     = 200
)

// NetworkService implements cloud.v1.NetworkService.
type NetworkService struct{ deps reconcile.Deps }

var _ cloudv1connect.NetworkServiceHandler = (*NetworkService)(nil)

// NewNetworkService returns the NetworkService.
func NewNetworkService(d reconcile.Deps) *NetworkService { return &NetworkService{deps: d} }

// rec is what moves networks towards their spec (pkg/reconcile/network).
// The handlers use it to check a request and to read; the controller to
// apply.
func (s *NetworkService) rec() *netreconcile.Reconciler { return netreconcile.New(s.deps) }

// Reconcile moves one network on (netreconcile.Reconciler.Reconcile); the
// controller calls it, and tests.
func (s *NetworkService) Reconcile(ctx context.Context, tenantID, id string) error {
	return s.rec().Reconcile(ctx, tenantID, id)
}

func (s *NetworkService) logger() *slog.Logger {
	if s.deps.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return s.deps.Logger
}

// CreateNetwork stores the network with its resolved layout (defaults
// filled in), PROVISIONING. Its managed resources are created by the
// reconciler; a failure there is the network's status. A retry with the
// same request_id returns the network created by the first call.
func (s *NetworkService) CreateNetwork(ctx context.Context, req *cloudv1.CreateNetworkRequest) (*cloudv1.CreateNetworkResponse, error) {
	if err := service.Validate(req); err != nil {
		return nil, service.ToConnect(err)
	}
	if id := req.GetRequestId(); id != "" {
		row, err := s.deps.Store.Networks().GetByCreateRequest(ctx, req.GetTenantId(), id)
		if err == nil {
			n, err := s.stored(ctx, row)
			return &cloudv1.CreateNetworkResponse{Network: n}, service.ToConnect(err)
		}
		if !errors.Is(err, repository.ErrNotFound) {
			return nil, service.ToConnect(err)
		}
	}
	row, err := s.rec().NewRow(ctx, req)
	if err != nil {
		return nil, service.ToConnect(err)
	}
	if err := s.deps.Store.Networks().Create(ctx, row); err != nil {
		if errors.Is(err, repository.ErrAlreadyExists) {
			return nil, connect.NewError(connect.CodeAlreadyExists, fmt.Sprintf(
				"a network named %q already exists in tenant %s: network names are unique per tenant (or the request_id was used before)",
				row.Name, row.TenantID))
		}
		return nil, service.ToConnect(err)
	}
	s.logger().InfoContext(ctx, "network stored", "tenant_id", row.TenantID, "network_id", row.ID,
		"name", row.Name, "bootstrap_id", row.CloudAccountID, "region", row.Region)
	n, err := s.stored(ctx, row)
	return &cloudv1.CreateNetworkResponse{Network: n}, service.ToConnect(err)
}

// bootstrapped refuses an account that cannot hold managed resources yet
// (netreconcile.Bootstrapped), as FAILED_PRECONDITION.
func bootstrapped(a *model.CloudAccount) error {
	return service.ToConnect(netreconcile.Bootstrapped(a))
}

// GetNetwork returns one network from the database: its spec and the
// status last recorded for it (stored). It does not read the management
// cluster; GetNetworkStatus does.
func (s *NetworkService) GetNetwork(ctx context.Context, req *cloudv1.GetNetworkRequest) (*cloudv1.GetNetworkResponse, error) {
	if err := service.Validate(req); err != nil {
		return nil, service.ToConnect(err)
	}
	row, err := s.get(ctx, req.GetTenantId(), req.GetNetworkId())
	if err != nil {
		return nil, service.ToConnect(err)
	}
	n, err := s.stored(ctx, row)
	return &cloudv1.GetNetworkResponse{Network: n}, service.ToConnect(err)
}

// ListNetworks returns a page of the tenant's networks, ordered by id,
// from the database like GetNetwork.
func (s *NetworkService) ListNetworks(ctx context.Context, req *cloudv1.ListNetworksRequest) (*cloudv1.ListNetworksResponse, error) {
	if err := service.Validate(req); err != nil {
		return nil, service.ToConnect(err)
	}
	size := int(req.GetPageSize())
	if size <= 0 {
		size = defaultPageSize
	}
	size = min(size, maxPageSize)
	rows, err := s.deps.Store.Networks().List(ctx, req.GetTenantId(), repository.Page{After: req.GetPageToken(), Limit: size + 1})
	if err != nil {
		return nil, service.ToConnect(err)
	}
	res := &cloudv1.ListNetworksResponse{}
	if len(rows) > size {
		rows = rows[:size]
		res.NextPageToken = rows[size-1].ID
	}
	for i := range rows {
		n, err := s.stored(ctx, &rows[i])
		if err != nil {
			return nil, service.ToConnect(err)
		}
		res.Networks = append(res.Networks, n)
	}
	return res, nil
}

func (s *NetworkService) get(ctx context.Context, tenantID, networkID string) (*model.Network, error) {
	row, err := s.deps.Store.Networks().Get(ctx, tenantID, networkID)
	if errors.Is(err, repository.ErrNotFound) {
		return nil, notFound(tenantID, networkID)
	}
	if err != nil {
		return nil, service.ToConnect(err)
	}
	return row, nil
}

func notFound(tenantID, networkID string) error {
	return connect.NewError(connect.CodeNotFound, fmt.Sprintf("tenant %s has no network %s", tenantID, networkID))
}
