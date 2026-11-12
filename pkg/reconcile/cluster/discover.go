package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	awscloud "github.com/yindia/cluster-vending/pkg/cloud/aws"
	corecluster "github.com/yindia/cluster-vending/pkg/cluster"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/fault"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// discoveringMessage is the status message of an import whose EKS cluster
// was not looked up yet.
const discoveringMessage = "looking the EKS cluster up; the import plan (status.import) follows"

// ImportNetwork loads the network a cluster is imported into and its
// account, from the store. The network may be imported (Observe-only) or
// owned; the takeover needs it owned.
func (s *Reconciler) ImportNetwork(ctx context.Context, tenantID, networkID string) (*model.Network, *model.CloudAccount, error) {
	n, err := s.deps.Store.Networks().Get(ctx, tenantID, networkID)
	if errors.Is(err, repository.ErrNotFound) {
		return nil, nil, fault.New(fault.FailedPrecondition,
			fmt.Sprintf("tenant %s has no network %s; import the cluster's network first (ImportNetwork)", tenantID, networkID))
	}
	if err != nil {
		return nil, nil, storeError(err)
	}
	if n.State == "NETWORK_STATE_DELETING" || n.State == "NETWORK_STATE_FAILED" || n.Provider != model.ProviderAWS {
		return nil, nil, fault.New(fault.FailedPrecondition,
			fmt.Sprintf("network %s is %s (%s); a cluster is imported into an aws network that is imported or owned", n.ID, n.State, n.Provider))
	}
	account, err := s.deps.Store.Tenants().GetCloudAccountByID(ctx, tenantID, n.CloudAccountID)
	if err != nil {
		return nil, nil, storeError(err)
	}
	return n, account, nil
}

// NewImportRow is the row ImportCluster stores for req in network n, not
// stored yet: IMPORTED with its import DISCOVERING, the request as sent
// (input), and a spec that only says where the cluster is. The EKS
// cluster is not looked up here; Reconcile does that once
// (discoverImport) and fills in the rest. Its versions are the ones the
// cluster runs: there is nothing to resolve.
func NewImportRow(req *cloudv1.ImportClusterRequest, n *model.Network, id string) (*model.Cluster, error) {
	spec, err1 := Encode(emptyImportSpec(n.Region, n.ID))
	input, err2 := Encode(req)
	status, err3 := Encode(&cloudv1.ClusterStatus{State: cloudv1.ClusterState_CLUSTER_STATE_IMPORTED, Message: discoveringMessage,
		Import: &cloudv1.ImportStatus{Phase: cloudv1.ImportPhase_IMPORT_PHASE_DISCOVERING}})
	if err := errors.Join(err1, err2, err3); err != nil {
		return nil, fault.New(fault.Internal, err.Error())
	}
	row := &model.Cluster{
		Resource: model.Resource{Base: model.Base{ID: id}, DisplayName: req.GetDisplayName(), State: StateImported,
			Spec: spec, Status: status, Generation: 1},
		TenantID: req.GetTenantId(), Name: req.GetName(), Input: input, NetworkID: n.ID, Provider: model.ProviderAWS,
		Region: n.Region, Imported: true, ResolvedGeneration: 1,
	}
	if rid := req.GetRequestId(); rid != "" {
		row.CreateRequestID = &rid
	}
	return row, nil
}

// emptyImportSpec is the spec of an import before (or without) a plan.
func emptyImportSpec(region, networkID string) *cloudv1.ClusterSpec {
	return &cloudv1.ClusterSpec{Region: region, Network: &cloudv1.ClusterNetwork{NetworkId: networkID},
		Provider: &cloudv1.ClusterSpec_Aws{Aws: &cloudv1.AwsClusterSpec{}}}
}

// discovering reports whether row is an import whose EKS cluster was not
// looked up yet.
func discovering(row *model.Cluster) bool {
	return row.State == StateImported && StoredImport(row).GetPhase() == cloudv1.ImportPhase_IMPORT_PHASE_DISCOVERING
}

// discoverImport looks the EKS cluster of a DISCOVERING import up,
// read-only, plans the import and stores the result with the row: the
// spec derived from the cluster, the plan (status.import, PLANNED or
// BLOCKED), and what it keeps of the roles it adopts together with the
// objects planned for it (adoption), which are applied from there. It
// runs once: a row that has its plan is never looked up again. It reports
// whether the row was stored; a row a call changed meanwhile is left for
// the next pass.
//
// A cluster that does not exist, cannot be read with the account's
// credentials or cannot be imported is a fault (the cluster is FAILED,
// with the reason); a lookup that failed for another reason is tried
// again.
func (s *Reconciler) discoverImport(ctx context.Context, row *model.Cluster) (bool, error) {
	req := &cloudv1.ImportClusterRequest{}
	if err := Lenient.Unmarshal(row.Input, req); err != nil {
		return false, fmt.Errorf("cluster %s: stored import request: %w", row.ID, err)
	}
	network, account, err := s.ImportNetwork(ctx, row.TenantID, row.NetworkID)
	if err != nil {
		return false, err
	}
	records, err := s.deps.Store.ManagedResources().ListByOwner(ctx, model.OwnerNetwork, network.ID)
	if err != nil {
		return false, storeError(err)
	}
	views, err := s.deps.Observe(ctx, records)
	if err != nil {
		return false, err
	}
	found, err := s.discover(ctx, account, network.Region, req)
	if err != nil {
		return false, err
	}
	plan := corecluster.PlanImport(corecluster.ImportInput{Found: found, Name: row.Name, NodeGroup: req.GetAws().GetDefaultNodeGroupName(),
		NetworkID: network.ID, Region: network.Region, Subnets: corecluster.PrivateSubnets(views),
		Base: s.Base(row, account), Scheme: s.deps.Kube.Scheme()})
	before := *row
	if err := s.planned(row, plan); err != nil {
		*row = before
		return false, err
	}
	if err := s.deps.Store.Clusters().Update(ctx, row); err != nil {
		*row = before
		if errors.Is(err, repository.ErrConflict) || errors.Is(err, repository.ErrNotFound) {
			return false, nil
		}
		return false, storeError(err)
	}
	s.logger().InfoContext(ctx, "cluster import planned", "tenant_id", row.TenantID, "cluster_id", row.ID, "name", row.Name,
		"network_id", network.ID, "blocked", plan.Blocked(), "plan_lines", len(plan.Lines), "objects", len(plan.Objects))
	return true, nil
}

// planned writes an import's plan onto row: the derived spec, the plan
// (status), and the adoption with the planned objects; IMPORTED, or, when
// the plan is blocked, FAILED with nothing to apply.
func (s *Reconciler) planned(row *model.Cluster, plan corecluster.ImportPlan) error {
	phase := cloudv1.ImportPhase_IMPORT_PHASE_PLANNED
	row.State = StateImported
	if plan.Blocked() {
		phase, row.State = cloudv1.ImportPhase_IMPORT_PHASE_BLOCKED, StateBlocked
	}
	spec := plan.Spec
	if spec == nil {
		spec = emptyImportSpec(row.Region, row.NetworkID)
	}
	rawSpec, err1 := Encode(spec)
	status, err2 := Encode(&cloudv1.ClusterStatus{Import: &cloudv1.ImportStatus{Phase: phase, Resources: plan.Lines}})
	raw, err3 := json.Marshal(plan.Adoption)
	adoption := model.JSON(raw)
	if plan.Blocked() {
		row.AppliedGeneration, row.RenderVersion = row.Generation, crossplaneaws.RenderVersion
	} else if err3 == nil {
		adoption, err3 = WithImportObjects(adoption, plan, s.deps.Kube.Scheme())
	}
	if err := errors.Join(err1, err2, err3); err != nil {
		return fault.New(fault.Internal, err.Error())
	}
	row.Spec, row.Status, row.Adoption = rawSpec, status, adoption
	return nil
}

// discover makes the read-only AWS lookup of an import's EKS cluster.
func (s *Reconciler) discover(ctx context.Context, a *model.CloudAccount, region string, req *cloudv1.ImportClusterRequest) (*awscloud.Cluster, error) {
	if s.deps.AWSClusterDiscovery == nil {
		return nil, fault.New(fault.Unavailable, "AWS lookups are not configured on this controller")
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
		return nil, fault.New(fault.NotFound, err.Error())
	case errors.Is(err, awscloud.ErrInvalid):
		return nil, fault.New(fault.Invalid, err.Error())
	case errors.Is(err, awscloud.ErrAccessDenied):
		return nil, fault.New(fault.FailedPrecondition, "cannot read the cluster with the account's credentials: "+err.Error())
	default:
		s.logger().ErrorContext(ctx, "discover cluster", "name", req.GetName(), "error", err)
		return nil, fault.New(fault.Unavailable, "the AWS lookup failed: "+err.Error())
	}
}
