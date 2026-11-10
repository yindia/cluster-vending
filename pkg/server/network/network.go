// Package network implements the cloud.v1 NetworkService for networks the
// platform creates: the stored row is the source of truth, and Crossplane
// managed resources in the tenant namespace build it in the tenant's
// account. Status is read live from those resources.
package network

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	awscatalog "github.com/yindia/cluster-vending/pkg/constant/aws"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1/cloudv1connect"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

// List paging.
const (
	defaultPageSize = 50
	maxPageSize     = 200
)

// NetworkService implements cloud.v1.NetworkService.
type NetworkService struct{ deps service.Deps }

var _ cloudv1connect.NetworkServiceHandler = (*NetworkService)(nil)

// NewNetworkService returns the NetworkService.
func NewNetworkService(d service.Deps) *NetworkService { return &NetworkService{deps: d} }

// CreateNetwork stores the network with its resolved layout (defaults
// filled in) and applies its managed resources. A retry with the same
// request_id returns the network created by the first call.
func (s *NetworkService) CreateNetwork(ctx context.Context, req *cloudv1.CreateNetworkRequest) (*cloudv1.CreateNetworkResponse, error) {
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	if id := req.GetRequestId(); id != "" {
		row, err := s.deps.Store.Networks().GetByCreateRequest(ctx, req.GetTenantId(), id)
		if err == nil {
			n, err := s.view(ctx, row)
			return &cloudv1.CreateNetworkResponse{Network: n}, err
		}
		if !errors.Is(err, repository.ErrNotFound) {
			return nil, service.ToConnect(err)
		}
	}
	account, layout, err := s.plan(ctx, req.GetTenantId(), req.GetNetwork().GetSpec())
	if err != nil {
		return nil, err
	}
	row, err := s.insert(ctx, req, account, layout)
	if err != nil {
		return nil, err
	}
	plan := newResolved(row, req.GetNetwork().GetSpec(), account, layout)
	if err := s.apply(ctx, row, account, plan); err != nil {
		s.rollback(ctx, row)
		return nil, err
	}
	s.logger().InfoContext(ctx, "network created", "tenant_id", row.TenantID, "network_id", row.ID,
		"name", row.Name, "prefix", plan.Prefix, "bootstrap_id", account.ID, "region", plan.Region,
		"cidr", plan.CIDRBlock, "subnets", len(plan.Subnets))
	n, err := s.view(ctx, row)
	return &cloudv1.CreateNetworkResponse{Network: n}, err
}

// insert stores the network as sent (input) and resolved (spec with the
// layout's defaults filled in), state PROVISIONING. The row ID becomes
// Network.id.
func (s *NetworkService) insert(ctx context.Context, req *cloudv1.CreateNetworkRequest, a *model.CloudAccount, l awsLayout) (*model.Network, error) {
	in := req.GetNetwork()
	input, err := encode(in)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error())
	}
	spec := proto.CloneOf(in.GetSpec())
	spec.Layout = layoutProto(l)
	raw, err := encode(spec)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error())
	}
	row := &model.Network{
		Resource: model.Resource{
			DisplayName: in.GetDisplayName(), Labels: in.GetLabels(), State: stateProvisioning, Spec: raw,
		},
		TenantID: req.GetTenantId(), Name: in.GetName(), Input: input, CloudAccountID: a.ID,
		Provider: model.ProviderAWS, Region: spec.GetRegion(),
	}
	if id := req.GetRequestId(); id != "" {
		row.CreateRequestID = &id
	}
	if err := s.deps.Store.Networks().Create(ctx, row); err != nil {
		return nil, service.ToConnect(err)
	}
	return row, nil
}

// plan checks the account and region and resolves the layout.
func (s *NetworkService) plan(ctx context.Context, tenantID string, spec *cloudv1.NetworkSpec) (*model.CloudAccount, awsLayout, error) {
	if spec.GetAws() == nil {
		return nil, awsLayout{}, connect.NewError(connect.CodeUnimplemented, "only aws networks are supported")
	}
	if !awscatalog.IsRegion(spec.GetRegion()) {
		return nil, awsLayout{}, connect.NewError(connect.CodeInvalidArgument,
			fmt.Sprintf("spec.region %q is not a known AWS region", spec.GetRegion()))
	}
	account, err := s.account(ctx, tenantID, spec.GetBootstrapId())
	if err != nil {
		return nil, awsLayout{}, err
	}
	layout, err := planAWSLayout(spec.GetRegion(), spec.GetLayout())
	switch {
	case errors.Is(err, errUnsupported):
		return nil, awsLayout{}, connect.NewError(connect.CodeUnimplemented, "spec.layout: "+err.Error())
	case err != nil:
		return nil, awsLayout{}, connect.NewError(connect.CodeInvalidArgument, "spec.layout: "+err.Error())
	}
	return account, layout, nil
}

// account returns the bootstrapped AWS account the network lives in.
func (s *NetworkService) account(ctx context.Context, tenantID, cloudAccountID string) (*model.CloudAccount, error) {
	a, err := s.deps.Store.Tenants().GetCloudAccountByID(ctx, tenantID, cloudAccountID)
	if errors.Is(err, repository.ErrNotFound) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Sprintf(
			"spec.bootstrap_id %s is not a bootstrapped account of tenant %s; run BootstrapTenant first",
			cloudAccountID, tenantID))
	}
	if err != nil {
		return nil, service.ToConnect(err)
	}
	if a.Provider != model.ProviderAWS {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Sprintf("account %s is a %s account; this network is aws", cloudAccountID, a.Provider))
	}
	return a, nil
}

// GetNetwork returns one network with its observed status.
func (s *NetworkService) GetNetwork(ctx context.Context, req *cloudv1.GetNetworkRequest) (*cloudv1.GetNetworkResponse, error) {
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	row, err := s.get(ctx, req.GetTenantId(), req.GetNetworkId())
	if err != nil {
		return nil, err
	}
	gone, err := s.finalize(ctx, row)
	if err != nil {
		return nil, err
	}
	if gone {
		return nil, notFound(req.GetTenantId(), req.GetNetworkId())
	}
	n, err := s.view(ctx, row)
	return &cloudv1.GetNetworkResponse{Network: n}, err
}

// ListNetworks returns a page of the tenant's networks, ordered by id.
func (s *NetworkService) ListNetworks(ctx context.Context, req *cloudv1.ListNetworksRequest) (*cloudv1.ListNetworksResponse, error) {
	if err := service.Validate(req); err != nil {
		return nil, err
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
		if gone, err := s.finalize(ctx, &rows[i]); err != nil {
			return nil, err
		} else if gone {
			continue
		}
		n, err := s.view(ctx, &rows[i])
		if err != nil {
			return nil, err
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

// view is the API view of a stored network: spec, observed status, the
// clusters using it.
func (s *NetworkService) view(ctx context.Context, row *model.Network) (*cloudv1.Network, error) {
	spec, err := specOf(row)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error())
	}
	o, err := s.observe(ctx, row.TenantID, row.ID)
	if err != nil {
		s.logger().ErrorContext(ctx, "observe network", "network_id", row.ID, "error", err)
		return nil, connect.NewError(connect.CodeUnavailable, "cannot read the network's resources from the management cluster")
	}
	records, err := s.records(ctx, row, o.objects)
	if err != nil {
		return nil, err
	}
	views, err := s.deps.Observe(ctx, records)
	if err != nil {
		return nil, err
	}
	clusters, err := s.deps.Store.Clusters().ListByNetwork(ctx, row.ID)
	if err != nil {
		return nil, service.ToConnect(err)
	}
	ids := make([]string, 0, len(clusters))
	for _, c := range clusters {
		ids = append(ids, c.Name)
	}
	return toProto(row, spec, status(row, views, o.resources, ids)), nil
}

func (s *NetworkService) logger() *slog.Logger {
	if s.deps.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return s.deps.Logger
}

// records returns the network's recorded objects, first recording any live
// object of the network (by label) that has no record yet: networks created
// before recording existed, or a record write that failed. The view so
// always covers every resource Crossplane has for the network.
func (s *NetworkService) records(ctx context.Context, row *model.Network, live []client.Object) ([]model.ManagedResource, error) {
	records, err := s.deps.Store.ManagedResources().ListByOwner(ctx, model.OwnerNetwork, row.ID)
	if err != nil {
		return nil, service.ToConnect(err)
	}
	known := make(map[string]bool, len(records))
	for _, r := range records {
		known[r.Kind+"/"+r.Name] = true
	}
	var missing []client.Object
	for _, obj := range live {
		gvk, err := apiutil.GVKForObject(obj, s.deps.Kube.Scheme())
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err.Error())
		}
		if !known[gvk.Kind+"/"+obj.GetName()] {
			missing = append(missing, obj)
		}
	}
	if len(missing) == 0 {
		return records, nil
	}
	sortByCreation(s.deps.Kube.Scheme(), missing)
	owner := service.Owner{TenantID: row.TenantID, CloudAccountID: row.CloudAccountID, Kind: model.OwnerNetwork, ID: row.ID}
	if err := s.deps.Record(ctx, owner, missing); err != nil {
		return nil, service.ToConnect(err)
	}
	s.logger().InfoContext(ctx, "recorded network resources found in the cluster", "network_id", row.ID, "count", len(missing))
	records, err = s.deps.Store.ManagedResources().ListByOwner(ctx, model.OwnerNetwork, row.ID)
	if err != nil {
		return nil, service.ToConnect(err)
	}
	return records, nil
}

// sortByCreation orders objects as NetworkResources creates them: VPC,
// internet gateway, subnets, EIPs, NAT gateways, route tables, routes,
// associations; by name within a kind.
func sortByCreation(scheme *runtime.Scheme, objects []client.Object) {
	rank := map[string]int{}
	kinds := crossplaneaws.NetworkObjects() // deletion order: reversed creation order
	for i, o := range kinds {
		if gvk, err := apiutil.GVKForObject(o, scheme); err == nil {
			rank[gvk.Kind] = len(kinds) - i
		}
	}
	kindOf := func(o client.Object) string {
		gvk, _ := apiutil.GVKForObject(o, scheme)
		return gvk.Kind
	}
	sort.SliceStable(objects, func(i, j int) bool {
		ri, rj := rank[kindOf(objects[i])], rank[kindOf(objects[j])]
		if ri != rj {
			return ri < rj
		}
		return objects[i].GetName() < objects[j].GetName()
	})
}
