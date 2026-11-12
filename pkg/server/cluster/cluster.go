// Package cluster implements the cloud.v1 ClusterService for AWS (EKS): the
// stored row is the source of truth, and Crossplane managed resources in the
// tenant namespace build the cluster in its network's account. Status is
// read live from those resources.
package cluster

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"connectrpc.com/connect/v2"
	"github.com/google/uuid"
	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1/cloudv1connect"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

// List paging.
const (
	defaultPageSize = 50
	maxPageSize     = 200
)

// Lifecycle states stored on the cluster row; the others are observed.
const (
	stateProvisioning = "CLUSTER_STATE_PROVISIONING"
	stateRunning      = "CLUSTER_STATE_RUNNING"
	stateUpdating     = "CLUSTER_STATE_UPDATING"
	stateDeleting     = "CLUSTER_STATE_DELETING"
)

// ClusterService implements cloud.v1.ClusterService.
type ClusterService struct{ deps service.Deps }

var _ cloudv1connect.ClusterServiceHandler = (*ClusterService)(nil)

// NewClusterService returns the ClusterService.
func NewClusterService(d service.Deps) *ClusterService { return &ClusterService{deps: d} }

// target is everything a cluster is built from: its network, account and
// the network's private subnets in the cluster's zones.
type target struct {
	network   *model.Network
	account   *model.CloudAccount
	zones     []string
	subnetIDs []string
	// subnetObjects are the Subnet managed resources of subnetIDs.
	subnetObjects []string
}

// CreateCluster stores the cluster with its defaults filled in and applies
// its managed resources. A retry with the same request_id returns the
// cluster created by the first call.
func (s *ClusterService) CreateCluster(ctx context.Context, req *cloudv1.CreateClusterRequest) (*cloudv1.CreateClusterResponse, error) {
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	if rid := req.GetRequestId(); rid != "" {
		if row, err := s.deps.Store.Clusters().GetByCreateRequest(ctx, req.GetTenantId(), rid); err == nil {
			c, err := s.view(ctx, row)
			return &cloudv1.CreateClusterResponse{Cluster: c}, err
		} else if !errors.Is(err, repository.ErrNotFound) {
			return nil, service.ToConnect(err)
		}
	}
	spec := req.GetCluster().GetSpec()
	if err := supported(spec); err != nil {
		return nil, err
	}
	if err := service.CheckAWSLabels(req.GetCluster().GetLabels()); err != nil {
		return nil, err
	}
	t, err := s.target(ctx, req.GetTenantId(), spec)
	if err != nil {
		return nil, err
	}
	if err := s.checkName(ctx, req.GetTenantId(), req.GetCluster().GetName(), spec.GetRegion(), t); err != nil {
		return nil, err
	}
	row, plan, err := s.insert(ctx, req, t)
	if err != nil {
		return nil, err
	}
	if err := s.apply(ctx, row, t.account, plan); err != nil {
		s.rollback(ctx, row, t.account.Namespace)
		return nil, err
	}
	if err := s.tagSubnets(ctx, row, t); err != nil {
		s.rollback(ctx, row, t.account.Namespace)
		return nil, err
	}
	s.logger().InfoContext(ctx, "cluster created", "tenant_id", row.TenantID, "cluster_id", row.ID, "name", row.Name,
		"eks_name", plan.ClusterName, "network_id", t.network.ID, "zones", t.zones)
	c, err := s.view(ctx, row)
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

// target loads the cluster's network and account and picks its subnets.
func (s *ClusterService) target(ctx context.Context, tenantID string, spec *cloudv1.ClusterSpec) (target, error) {
	n, err := s.deps.Store.Networks().Get(ctx, tenantID, spec.GetNetwork().GetNetworkId())
	if errors.Is(err, repository.ErrNotFound) {
		return target{}, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Sprintf("tenant %s has no network %s", tenantID, spec.GetNetwork().GetNetworkId()))
	}
	if err != nil {
		return target{}, service.ToConnect(err)
	}
	switch {
	case n.State == "NETWORK_STATE_DELETING", n.State == "NETWORK_STATE_IMPORTED", n.State == "NETWORK_STATE_FAILED":
		return target{}, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Sprintf("network %s is %s; clusters need a network the platform owns", n.ID, n.State))
	case n.Provider != model.ProviderAWS || n.Region != spec.GetRegion():
		return target{}, connect.NewError(connect.CodeInvalidArgument,
			fmt.Sprintf("network %s is %s/%s; the cluster is aws/%s", n.ID, n.Provider, n.Region, spec.GetRegion()))
	}
	account, err := s.deps.Store.Tenants().GetCloudAccountByID(ctx, tenantID, n.CloudAccountID)
	if err != nil {
		return target{}, service.ToConnect(err)
	}
	records, err := s.deps.Store.ManagedResources().ListByOwner(ctx, model.OwnerNetwork, n.ID)
	if err != nil {
		return target{}, service.ToConnect(err)
	}
	views, err := s.deps.Observe(ctx, records)
	if err != nil {
		return target{}, err
	}
	subnets := privateSubnets(views)
	zones, ids, err := chooseSubnets(subnets, spec.GetNetwork().GetZoneIds())
	if err != nil {
		return target{}, connect.NewError(connect.CodeFailedPrecondition, fmt.Sprintf("network %s: %v", n.ID, err))
	}
	return target{network: n, account: account, zones: zones, subnetIDs: ids, subnetObjects: objectNames(subnets, ids)}, nil
}

// insert stores the cluster as sent (input) and resolved (spec); the ID is
// chosen first, it names the resources.
func (s *ClusterService) insert(ctx context.Context, req *cloudv1.CreateClusterRequest, t target) (*model.Cluster, crossplaneaws.ClusterPlan, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return nil, crossplaneaws.ClusterPlan{}, connect.NewError(connect.CodeInternal, err.Error())
	}
	in := req.GetCluster()
	spec, err := resolveSpec(in.GetSpec(), t.zones)
	if err != nil {
		return nil, crossplaneaws.ClusterPlan{}, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	if err := s.resolveVersions(ctx, t.account, spec, pinnedAddons(in.GetSpec())); err != nil {
		return nil, crossplaneaws.ClusterPlan{}, err
	}
	input, err1 := encode(in)
	raw, err2 := encode(spec)
	if err := errors.Join(err1, err2); err != nil {
		return nil, crossplaneaws.ClusterPlan{}, connect.NewError(connect.CodeInternal, err.Error())
	}
	row := &model.Cluster{
		Resource: model.Resource{Base: model.Base{ID: id.String()}, DisplayName: in.GetDisplayName(),
			Labels: in.GetLabels(), State: stateProvisioning, Spec: raw},
		TenantID: req.GetTenantId(), Name: in.GetName(), Input: input, NetworkID: t.network.ID,
		Provider: model.ProviderAWS, Region: spec.GetRegion(),
	}
	if rid := req.GetRequestId(); rid != "" {
		row.CreateRequestID = &rid
	}
	if err := s.deps.Store.Clusters().Create(ctx, row); err != nil {
		return nil, crossplaneaws.ClusterPlan{}, createError(err, row)
	}
	return row, s.renderPlan(spec, row, t), nil
}

// createError maps a failed insert: a duplicate is a second cluster of the
// name in the account and region (the name check passed, but another
// create won the race) or a reused request_id; a conflict is the network
// being deleted or changed at the same moment.
func createError(err error, row *model.Cluster) error {
	if errors.Is(err, repository.ErrConflict) {
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Sprintf(
			"network %s changed while the cluster was being stored (is it being deleted?); get the network and retry", row.NetworkID))
	}
	if errors.Is(err, repository.ErrAlreadyExists) {
		return connect.NewError(connect.CodeAlreadyExists, fmt.Sprintf(
			"a cluster named %q already exists in this account and %s (or the request_id was used before)", row.Name, row.Region))
	}
	return service.ToConnect(err)
}

// renderPlan is the plan of the cluster's resources: the resolved spec on
// the network's subnets.
func (s *ClusterService) renderPlan(spec *cloudv1.ClusterSpec, row *model.Cluster, t target) crossplaneaws.ClusterPlan {
	p := clusterPlan(spec, t.subnetIDs, s.base(row, t.account))
	p.SubnetObjects = t.subnetObjects
	return p
}

func (s *ClusterService) base(row *model.Cluster, a *model.CloudAccount) crossplaneaws.ClusterPlan {
	return crossplaneaws.ClusterPlan{
		Namespace: a.Namespace, ProviderConfigName: a.ProviderConfigName, TenantID: row.TenantID,
		ClusterID: row.ID, ClusterName: row.Name, Prefix: crossplaneaws.ClusterPrefix(row.TenantID, row.ID, row.Name),
		Tags: row.Labels, Adoption: adoption(row),
	}
}

// apply records the plan's objects as the cluster's, applies them, and
// prunes recorded objects the plan no longer has (e.g. a removed admin).
// Recording comes first so a failed apply can always be rolled back: an
// object is never in the management cluster without a record.
func (s *ClusterService) apply(ctx context.Context, row *model.Cluster, a *model.CloudAccount, plan crossplaneaws.ClusterPlan) error {
	name, retiring, err := s.nodeGroupName(ctx, row, plan)
	if err != nil {
		s.logger().ErrorContext(ctx, "choose node group", "cluster_id", row.ID, "error", err)
		return connect.NewError(connect.CodeUnavailable, "cannot read the cluster's node group")
	}
	plan.NodeGroupName = name
	upgrading, err := s.gateUpgrade(ctx, row, &plan)
	if err != nil {
		s.logger().ErrorContext(ctx, "read EKS cluster version", "cluster_id", row.ID, "error", err)
		return connect.NewError(connect.CodeUnavailable, "cannot read the cluster's EKS cluster")
	}
	if err := s.fillLive(ctx, row, &plan); err != nil {
		s.logger().ErrorContext(ctx, "read the cluster's resources", "cluster_id", row.ID, "error", err)
		return connect.NewError(connect.CodeUnavailable, "cannot read the cluster's resources")
	}
	// Objects that wait for another resource are not rendered yet; the
	// cluster is applied again (advance) until none is pending.
	pending := plan.Pending()
	objects := crossplaneaws.ClusterResources(plan)
	hold, err := s.holdBack(ctx, row)
	if err != nil {
		s.logger().ErrorContext(ctx, "read EKS cluster state", "cluster_id", row.ID, "error", err)
		return connect.NewError(connect.CodeUnavailable, "cannot read the cluster's EKS cluster")
	}
	if hold {
		// Created in sequence: add-ons and access entries follow once EKS
		// is ACTIVE (advance).
		if objects, err = s.withoutAfterActive(objects); err != nil {
			return connect.NewError(connect.CodeInternal, err.Error())
		}
	}
	owner := service.Owner{TenantID: row.TenantID, CloudAccountID: a.ID, Kind: model.OwnerCluster, ID: row.ID}
	if err := s.deps.Record(ctx, owner, objects); err != nil {
		return connect.NewError(connect.CodeInternal, "could not record the cluster's resources: "+err.Error())
	}
	for _, obj := range objects {
		if err := s.applyOne(ctx, obj); err != nil {
			s.logger().ErrorContext(ctx, "apply cluster resource", "cluster_id", row.ID, "object", obj.GetName(), "error", err)
			return applyError(err)
		}
	}
	if len(pending) > 0 {
		// Nothing is pruned from a partial render: an object left out
		// because it waits (the EKS cluster of a cluster that turns
		// encryption on, while its key is created) is not a dropped one.
		return nil
	}
	if err := s.deps.Prune(ctx, owner, append(slices.Clone(objects), retiring...)); err != nil {
		return connect.NewError(connect.CodeInternal, "could not remove the cluster's dropped resources: "+err.Error())
	}
	if hold || upgrading {
		return nil // partly rendered: applied again (advance) until complete
	}
	return s.markApplied(ctx, row)
}

// markApplied records that every object of the row's generation was
// applied without error, with this build's renderer.
func (s *ClusterService) markApplied(ctx context.Context, row *model.Cluster) error {
	if row.AppliedGeneration == row.Generation && row.RenderVersion == crossplaneaws.RenderVersion {
		return nil
	}
	row.AppliedGeneration, row.RenderVersion = row.Generation, crossplaneaws.RenderVersion
	if err := s.deps.Store.Clusters().Update(ctx, row); err != nil {
		return connect.NewError(connect.CodeInternal, "could not record the applied generation: "+err.Error())
	}
	return nil
}

func applyError(err error) error {
	if meta.IsNoMatchError(err) {
		return connect.NewError(connect.CodeFailedPrecondition,
			"a Crossplane AWS provider the cluster needs (eks, iam, ec2, kms, cloudwatchlogs) is not installed in the management cluster")
	}
	return connect.NewError(connect.CodeInternal, "could not create the cluster's resources")
}

// rollback undoes a create whose resources could not be applied.
func (s *ClusterService) rollback(ctx context.Context, row *model.Cluster, namespace string) {
	records, err := s.deps.Store.ManagedResources().ListByOwner(ctx, model.OwnerCluster, row.ID)
	if err == nil {
		err = s.deps.DeleteRecorded(ctx, records)
	}
	err = errors.Join(err, s.untagSubnets(ctx, row, namespace))
	err = errors.Join(err, s.deps.Store.ManagedResources().DeleteByOwner(ctx, model.OwnerCluster, row.ID),
		s.deps.Store.Clusters().Delete(ctx, row.TenantID, row.ID))
	if err != nil {
		s.logger().ErrorContext(ctx, "rollback cluster", "cluster_id", row.ID, "error", err)
	}
}

// GetCluster returns one cluster with its observed status. Reads change
// nothing: the background loop (Reconcile) moves clusters on.
func (s *ClusterService) GetCluster(ctx context.Context, req *cloudv1.GetClusterRequest) (*cloudv1.GetClusterResponse, error) {
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	row, err := s.get(ctx, req.GetTenantId(), req.GetClusterId())
	if err != nil {
		return nil, err
	}
	c, err := s.view(ctx, row)
	if err != nil {
		return nil, err
	}
	return &cloudv1.GetClusterResponse{Cluster: c}, nil
}

// ListClusters returns a page of the tenant's clusters, oldest first.
func (s *ClusterService) ListClusters(ctx context.Context, req *cloudv1.ListClustersRequest) (*cloudv1.ListClustersResponse, error) {
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
	ids := make([]string, 0, len(rows))
	for i := range rows {
		ids = append(ids, rows[i].ID)
	}
	records, err := s.deps.Store.ManagedResources().ListByOwners(ctx, model.OwnerCluster, ids) // one query per page
	if err != nil {
		return nil, service.ToConnect(err)
	}
	for i := range rows {
		c, err := s.viewOf(ctx, &rows[i], records[rows[i].ID])
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

// finalize moves a DELETING cluster's deletion on (driveDelete) and
// reports whether the cluster is gone.
func (s *ClusterService) finalize(ctx context.Context, row *model.Cluster) (bool, error) {
	if row.State != stateDeleting {
		return false, nil
	}
	gone, err := s.driveDelete(ctx, row)
	if err != nil {
		s.logger().ErrorContext(ctx, "delete cluster", "cluster_id", row.ID, "error", err)
		return false, connect.NewError(connect.CodeUnavailable, "could not continue deleting the cluster: "+err.Error())
	}
	return gone, nil
}

func (s *ClusterService) logger() *slog.Logger {
	if s.deps.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return s.deps.Logger
}

// applyOne server-side applies one object. A NodeGroup's desired size goes
// in initProvider only (crossplaneaws.DesiredSizeAtCreateOnly), on create
// and update alike.
func (s *ClusterService) applyOne(ctx context.Context, obj client.Object) error {
	if _, ok := obj.(*awseks.NodeGroup); !ok {
		return kube.Apply(ctx, s.deps.Kube, obj)
	}
	u, err := kube.ApplyObject(obj, s.deps.Kube.Scheme())
	if err != nil {
		return err
	}
	crossplaneaws.DesiredSizeAtCreateOnly(u)
	return kube.Apply(ctx, s.deps.Kube, u)
}
