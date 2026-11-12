package network

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect/v2"
	"github.com/google/uuid"
	awsec2 "github.com/upbound/provider-aws/v2/apis/namespaced/ec2/v1beta1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	awscloud "github.com/yindia/cluster-vending/pkg/cloud/aws"
	awscatalog "github.com/yindia/cluster-vending/pkg/constant/aws"
	"github.com/yindia/cluster-vending/pkg/crossplane"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

// ImportNetwork looks an existing VPC up once, read-only, and imports it:
// Observe-only managed resources for what the contract models, the spec
// derived from them, and the plan (status.import). Nothing changes in the
// cloud. The network is IMPORTED, or FAILED when the plan is BLOCKED (then
// nothing is imported).
func (s *NetworkService) ImportNetwork(ctx context.Context, req *cloudv1.ImportNetworkRequest) (*cloudv1.ImportNetworkResponse, error) {
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	if req.GetAws() == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, "only aws networks can be imported")
	}
	if !awscatalog.IsRegion(req.GetRegion()) {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Sprintf("region %q is not a known AWS region", req.GetRegion()))
	}
	if id := req.GetRequestId(); id != "" {
		if row, err := s.deps.Store.Networks().GetByCreateRequest(ctx, req.GetTenantId(), id); err == nil {
			n, err := s.view(ctx, row)
			return &cloudv1.ImportNetworkResponse{Network: n}, err
		} else if !errors.Is(err, repository.ErrNotFound) {
			return nil, service.ToConnect(err)
		}
	}
	account, err := s.account(ctx, req.GetTenantId(), req.GetBootstrapId())
	if err != nil {
		return nil, err
	}
	if err := s.refuseOwnedVPC(ctx, account, req.GetAws().GetVpcId()); err != nil {
		return nil, err
	}
	found, err := s.discover(ctx, account, req)
	if err != nil {
		return nil, err
	}
	row, plan, err := s.insertImport(ctx, req, account, found)
	if err != nil {
		return nil, err
	}
	if !plan.blocked() {
		err := s.applyObjects(ctx, row, account, crossplaneaws.ImportedNetworkResources(plan.resources))
		if err == nil {
			err = s.markApplied(ctx, row)
		}
		if err != nil {
			s.rollback(ctx, row)
			return nil, err
		}
	}
	s.logger().InfoContext(ctx, "network imported", "tenant_id", row.TenantID, "network_id", row.ID,
		"vpc_id", req.GetAws().GetVpcId(), "blocked", plan.blocked(), "plan_lines", len(plan.lines))
	n, err := s.view(ctx, row)
	return &cloudv1.ImportNetworkResponse{Network: n}, err
}

// discover makes the read-only AWS lookup.
func (s *NetworkService) discover(ctx context.Context, a *model.CloudAccount, req *cloudv1.ImportNetworkRequest) (*awscloud.Network, error) {
	if s.deps.AWSDiscovery == nil {
		return nil, connect.NewError(connect.CodeUnavailable, "AWS lookups are not configured on this server")
	}
	access, err := s.deps.AWSAccess(ctx, a)
	if err != nil {
		return nil, err
	}
	found, err := s.deps.AWSDiscovery.DiscoverNetwork(ctx, access, req.GetRegion(),
		req.GetAws().GetVpcId(), req.GetAws().GetSubnetIds())
	switch {
	case err == nil:
		return found, nil
	case errors.Is(err, awscloud.ErrNotFound):
		return nil, connect.NewError(connect.CodeNotFound, err.Error())
	case errors.Is(err, awscloud.ErrInvalid):
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
	case errors.Is(err, awscloud.ErrAccessDenied):
		return nil, connect.NewError(connect.CodeFailedPrecondition, "cannot read the VPC with the account's credentials: "+err.Error())
	default:
		s.logger().ErrorContext(ctx, "discover network", "vpc_id", req.GetAws().GetVpcId(), "error", err)
		return nil, connect.NewError(connect.CodeUnavailable, "the AWS lookup failed: "+err.Error())
	}
}

// insertImport plans the import and stores the network: the derived spec,
// the request as sent (input) and the plan (status), IMPORTED or, when
// blocked, FAILED. The ID is chosen first: it names the resources.
func (s *NetworkService) insertImport(ctx context.Context, req *cloudv1.ImportNetworkRequest, a *model.CloudAccount, found *awscloud.Network) (*model.Network, importPlan, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return nil, importPlan{}, connect.NewError(connect.CodeInternal, err.Error())
	}
	plan := planAWSImport(found, crossplaneaws.ImportedNetwork{
		Namespace: a.Namespace, ProviderConfigName: a.ProviderConfigName, TenantID: req.GetTenantId(),
		NetworkID: id.String(), NetworkName: req.GetName(), Region: req.GetRegion(),
		Prefix: crossplaneaws.NetworkPrefix(req.GetTenantId(), id.String(), req.GetName()),
	})
	phase, state := cloudv1.ImportPhase_IMPORT_PHASE_PLANNED, stateImported
	if plan.blocked() {
		phase, state = cloudv1.ImportPhase_IMPORT_PHASE_BLOCKED, stateBlocked
	}
	spec := &cloudv1.NetworkSpec{Region: req.GetRegion(), BootstrapId: a.ID, Layout: plan.layout}
	rawSpec, err1 := encode(spec)
	input, err2 := encode(req)
	stored, err3 := encode(&cloudv1.NetworkStatus{Import: &cloudv1.ImportStatus{Phase: phase, Resources: plan.lines}})
	if err := errors.Join(err1, err2, err3); err != nil {
		return nil, importPlan{}, connect.NewError(connect.CodeInternal, err.Error())
	}
	row := &model.Network{
		Resource: model.Resource{Base: model.Base{ID: id.String()}, DisplayName: req.GetDisplayName(),
			State: state, Spec: rawSpec, Status: stored},
		TenantID: req.GetTenantId(), Name: req.GetName(), Input: input, CloudAccountID: a.ID,
		Provider: model.ProviderAWS, Region: req.GetRegion(),
	}
	if rid := req.GetRequestId(); rid != "" {
		row.CreateRequestID = &rid
	}
	if err := s.deps.Store.Networks().Create(ctx, row); err != nil {
		return nil, importPlan{}, service.ToConnect(err)
	}
	return row, plan, nil
}

// TakeOverNetwork makes the platform the owner of an IMPORTED network: every
// managed resource switches from Observe to full management, and subnets
// get their load balancer tag (the plan's WILL_UPDATE lines). The network
// then provisions like a created one.
func (s *NetworkService) TakeOverNetwork(ctx context.Context, req *cloudv1.TakeOverNetworkRequest) (*cloudv1.TakeOverNetworkResponse, error) {
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	row, err := s.get(ctx, req.GetTenantId(), req.GetNetworkId())
	if err != nil {
		return nil, err
	}
	imp, err := takeOverAllowed(row, req)
	if err != nil {
		return nil, err
	}
	records, err := s.deps.Store.ManagedResources().ListByOwner(ctx, model.OwnerNetwork, row.ID)
	if err != nil {
		return nil, service.ToConnect(err)
	}
	for _, r := range records {
		if err := s.own(ctx, r); err != nil {
			s.logger().ErrorContext(ctx, "take over resource", "network_id", row.ID, "object", r.Name, "error", err)
			return nil, connect.NewError(connect.CodeInternal, fmt.Sprintf("could not take over %s %s", r.Kind, r.Name))
		}
	}
	imp.Phase = cloudv1.ImportPhase_IMPORT_PHASE_CONVERGING
	if row.Status, err = encode(&cloudv1.NetworkStatus{Import: imp}); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error())
	}
	row.State = stateProvisioning
	if err := s.deps.Store.Networks().Update(ctx, row); err != nil {
		return nil, service.ToConnect(err)
	}
	s.logger().InfoContext(ctx, "network taken over", "tenant_id", row.TenantID, "network_id", row.ID, "resources", len(records))
	n, err := s.view(ctx, row)
	return &cloudv1.TakeOverNetworkResponse{Network: n}, err
}

// takeOverAllowed checks state, etag, phase and the acknowledgement of
// NOT_IMPORTED resources, and returns the stored plan.
func takeOverAllowed(row *model.Network, req *cloudv1.TakeOverNetworkRequest) (*cloudv1.ImportStatus, error) {
	if row.State != stateImported {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Sprintf("network %s is not IMPORTED (only imported networks are taken over)", row.ID))
	}
	if etag := req.GetEtag(); etag != "" && etag != row.ETag {
		return nil, connect.NewError(connect.CodeAborted, "etag mismatch: the network changed; get it again and retry")
	}
	imp := storedImport(row)
	if imp.GetPhase() != cloudv1.ImportPhase_IMPORT_PHASE_PLANNED {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Sprintf("import phase is %s, want PLANNED", imp.GetPhase()))
	}
	notImported := importPlan{lines: imp.GetResources()}.notImported()
	if len(notImported) > 0 && !req.GetAcknowledgeNotImported() {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Sprintf(
			"%d resources are NOT_IMPORTED (first: %s %s: %s); give them another owner, then retry with acknowledge_not_imported",
			len(notImported), notImported[0].GetKind(), notImported[0].GetExternalName(), notImported[0].GetDetail()))
	}
	return imp, nil
}

// own switches one recorded object to full management (without Delete for
// one marked crossplaneaws.RetainAnnotation) and adds the ELB role tags the
// platform's subnets carry.
func (s *NetworkService) own(ctx context.Context, r model.ManagedResource) error {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.FromAPIVersionAndKind(r.APIVersion, r.Kind))
	// From the API server: a read-modify-write needs the current object.
	if err := s.deps.Reader().Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: r.Name}, u); err != nil {
		return err
	}
	policies := []string{"*"}
	if u.GetAnnotations()[crossplaneaws.RetainAnnotation] == "true" {
		// Shared with resources outside the network: owned, never deleted.
		policies = policies[:0]
		for _, action := range crossplaneaws.PolicyNoDelete {
			policies = append(policies, string(action))
		}
	}
	if err := unstructured.SetNestedStringSlice(u.Object, policies, "spec", "managementPolicies"); err != nil {
		return err
	}
	if r.Kind == "Subnet" {
		tag := crossplaneaws.TagInternalELB
		if u.GetLabels()[crossplane.LabelSubnetRole] == string(crossplaneaws.SubnetPublic) {
			tag = crossplaneaws.TagELB
		}
		if err := unstructured.SetNestedField(u.Object, "1", "spec", "forProvider", "tags", tag); err != nil {
			return err
		}
	}
	return s.deps.Kube.Update(ctx, u)
}

// refuseOwnedVPC fails with ALREADY_EXISTS when a network of the tenant
// (created or imported) already has the VPC: two networks would manage the
// same cloud resources.
func (s *NetworkService) refuseOwnedVPC(ctx context.Context, a *model.CloudAccount, vpcID string) error {
	vpcs := &awsec2.VPCList{}
	if err := s.deps.List(ctx, vpcs, client.InNamespace(a.Namespace)); err != nil {
		if meta.IsNoMatchError(err) {
			return nil
		}
		return connect.NewError(connect.CodeUnavailable, "cannot list the tenant's VPCs: "+err.Error())
	}
	for _, v := range vpcs.Items {
		if v.Annotations[crossplaneaws.ExternalNameAnnotation] == vpcID {
			return connect.NewError(connect.CodeAlreadyExists, fmt.Sprintf(
				"VPC %s already belongs to network %s; delete that network first, or use it", vpcID, v.Labels[crossplane.LabelNetwork]))
		}
	}
	return nil
}
