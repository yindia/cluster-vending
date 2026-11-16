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
	"time"

	"connectrpc.com/connect/v2"

	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1/cloudv1connect"
	"github.com/yindia/cluster-vending/pkg/reconcile"
	accountreconcile "github.com/yindia/cluster-vending/pkg/reconcile/account"

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
		if err := cloudstore.ResumeNetworkCreate(ctx, s.deps.Store, req.GetTenantId(), id); err != nil {
			return nil, service.ToConnect(err)
		}
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
	if err := s.usableByID(ctx, row.TenantID, row.CloudAccountID, row.Region); err != nil {
		return nil, err
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

// usable refuses an account whose access the controller has not verified
// in region (accountreconcile.Usable), as FAILED_PRECONDITION. A verdict
// that is stale is logged and allowed.
func (s *NetworkService) usable(ctx context.Context, a *model.CloudAccount, region string) error {
	warning, err := accountreconcile.Usable(a, region)
	if err != nil {
		s.startRegionCheck(ctx, a, region)
		return service.ToConnect(err)
	}
	if warning != "" {
		s.logger().WarnContext(ctx, warning, "tenant_id", a.TenantID, "bootstrap_id", a.ID, "region", region)
	}
	return nil
}

// usableByID is usable for the account of a network row.
func (s *NetworkService) usableByID(ctx context.Context, tenantID, accountID, region string) error {
	a, err := s.rec().Account(ctx, tenantID, accountID)
	if err != nil {
		return service.ToConnect(err)
	}
	return s.usable(ctx, a, region)
}

// GetNetwork returns one network from the database: its spec and the
// status last recorded for it (stored); with live, the status read now
// from the management cluster. Like every read it changes nothing.
func (s *NetworkService) GetNetwork(ctx context.Context, req *cloudv1.GetNetworkRequest) (*cloudv1.GetNetworkResponse, error) {
	if err := service.Validate(req); err != nil {
		return nil, service.ToConnect(err)
	}
	row, err := s.get(ctx, req.GetTenantId(), req.GetNetworkId())
	if err != nil {
		return nil, service.ToConnect(err)
	}
	read := func() (*cloudv1.Network, error) { return s.stored(ctx, row) }
	if req.GetLive() {
		read = func() (*cloudv1.Network, error) { return s.rec().View(ctx, row) }
	}
	n, err := read()
	if err != nil {
		return nil, service.ToConnect(err)
	}
	return &cloudv1.GetNetworkResponse{Network: n, AsRequested: req.GetAsRequested() && asRequested(row, n)}, nil
}

// asRequested gives n the spec, labels and display name its client asked
// for: the stored input (as created, with the updates since), and reports
// whether there was one with a spec.
func asRequested(row *model.Network, n *cloudv1.Network) bool {
	if len(row.Input) == 0 {
		return false
	}
	in := &cloudv1.Network{}
	if err := netreconcile.Lenient.Unmarshal(row.Input, in); err != nil || in.GetSpec() == nil {
		return false
	}
	n.Spec, n.Labels, n.DisplayName = in.GetSpec(), in.GetLabels(), in.GetDisplayName()
	return true
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

// startRegionCheck asks the controller to check region of a verified
// account that was never checked there, so the refusal fixes itself
// (accountreconcile.RegionToCheck); a failure to ask is only logged.
func (s *NetworkService) startRegionCheck(ctx context.Context, a *model.CloudAccount, region string) {
	regions, ok := accountreconcile.RegionToCheck(a, region)
	if !ok {
		return
	}
	if err := s.deps.Store.Tenants().RequestAccessCheck(ctx, a.TenantID, a.ID, time.Now(), regions); err != nil {
		s.logger().WarnContext(ctx, "start the check of a region", "tenant_id", a.TenantID, "bootstrap_id", a.ID, "region", region, "error", err)
	}
}
