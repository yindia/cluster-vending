package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"connectrpc.com/connect/v2"
	"github.com/google/uuid"
	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	"google.golang.org/protobuf/encoding/protojson"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	awscloud "github.com/yindia/cluster-vending/pkg/cloud/aws"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

// Stored states of an imported cluster.
const (
	// stateImported: ImportCluster created Observe-only objects; nothing is
	// managed until TakeOverCluster.
	stateImported = "CLUSTER_STATE_IMPORTED"
	// stateBlocked: the import plan has BLOCKING lines; nothing was
	// imported.
	stateBlocked = "CLUSTER_STATE_FAILED"
)

// ImportCluster looks an existing EKS cluster up once, read-only, and
// imports it: the spec derived from it, a managed resource for each of its
// resources that only observes, and the plan (status.import). Nothing
// changes in the cloud. The cluster is IMPORTED, or FAILED when the plan is
// BLOCKED (then nothing is imported).
func (s *ClusterService) ImportCluster(ctx context.Context, req *cloudv1.ImportClusterRequest) (*cloudv1.ImportClusterResponse, error) {
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	if req.GetAws() == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, "only aws clusters can be imported")
	}
	if rid := req.GetRequestId(); rid != "" {
		if row, err := s.deps.Store.Clusters().GetByCreateRequest(ctx, req.GetTenantId(), rid); err == nil {
			c, err := s.view(ctx, row)
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
	plan := planClusterImport(importInput{found: found, name: req.GetName(), nodeGroup: req.GetAws().GetDefaultNodeGroupName(),
		networkID: network.ID, region: network.Region, subnets: subnets, base: s.base(row, account), scheme: s.deps.Kube.Scheme()})
	if err := s.insertImport(ctx, req, row, plan); err != nil {
		return nil, err
	}
	if !plan.blocked() {
		if err := s.applyImport(ctx, row, account, plan); err != nil {
			s.rollback(ctx, row, account.Namespace)
			return nil, err
		}
	}
	s.logger().InfoContext(ctx, "cluster imported", "tenant_id", row.TenantID, "cluster_id", row.ID, "name", row.Name,
		"network_id", network.ID, "blocked", plan.blocked(), "plan_lines", len(plan.lines), "objects", len(plan.objects))
	c, err := s.view(ctx, row)
	return &cloudv1.ImportClusterResponse{Cluster: c}, err
}

// importNetwork loads the network an imported cluster runs in, its account
// and its private subnets. The network may be imported (Observe-only) or
// owned; the takeover needs it owned.
func (s *ClusterService) importNetwork(ctx context.Context, tenantID, networkID string) (*model.Network, *model.CloudAccount, []privateSubnet, error) {
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
	return n, account, privateSubnets(views), nil
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
// as sent (input), the plan (status) and what it keeps of the roles it
// adopts, IMPORTED or, when blocked, FAILED. Its generation counts as
// applied: the loop leaves it alone until the takeover.
func (s *ClusterService) insertImport(ctx context.Context, req *cloudv1.ImportClusterRequest, row *model.Cluster, plan clusterImport) error {
	phase := cloudv1.ImportPhase_IMPORT_PHASE_PLANNED
	row.State = stateImported
	if plan.blocked() {
		phase, row.State = cloudv1.ImportPhase_IMPORT_PHASE_BLOCKED, stateBlocked
	}
	spec := plan.spec
	if spec == nil {
		spec = &cloudv1.ClusterSpec{Region: row.Region, Network: &cloudv1.ClusterNetwork{NetworkId: row.NetworkID},
			Provider: &cloudv1.ClusterSpec_Aws{Aws: &cloudv1.AwsClusterSpec{}}}
	}
	rawSpec, err1 := encode(spec)
	input, err2 := encode(req)
	status, err3 := encode(&cloudv1.ClusterStatus{Import: &cloudv1.ImportStatus{Phase: phase, Resources: plan.lines}})
	adoption, err4 := json.Marshal(plan.adoption)
	if err := errors.Join(err1, err2, err3, err4); err != nil {
		return connect.NewError(connect.CodeInternal, err.Error())
	}
	row.Spec, row.Input, row.Status, row.Adoption = rawSpec, input, status, model.JSON(adoption)
	row.Generation, row.AppliedGeneration, row.RenderVersion = 1, 1, crossplaneaws.RenderVersion
	if rid := req.GetRequestId(); rid != "" {
		row.CreateRequestID = &rid
	}
	return service.ToConnect(s.deps.Store.Clusters().Create(ctx, row))
}

// applyImport records and applies the import's objects. Pod identity
// associations get their external name (an ID only the cloud knows) set
// once, outside the apply: it is not part of any later render.
func (s *ClusterService) applyImport(ctx context.Context, row *model.Cluster, a *model.CloudAccount, plan clusterImport) error {
	owner := service.Owner{TenantID: row.TenantID, CloudAccountID: a.ID, Kind: model.OwnerCluster, ID: row.ID}
	if err := s.deps.Record(ctx, owner, plan.objects); err != nil {
		return connect.NewError(connect.CodeInternal, "could not record the cluster's resources: "+err.Error())
	}
	for _, obj := range plan.objects {
		if err := s.applyOne(ctx, obj); err != nil {
			s.logger().ErrorContext(ctx, "apply imported resource", "cluster_id", row.ID, "object", obj.GetName(), "error", err)
			return applyError(err)
		}
		id, ok := plan.podIdentities[obj.GetName()]
		if !ok {
			continue
		}
		live := &awseks.PodIdentityAssociation{}
		if err := s.deps.Reader().Get(ctx, client.ObjectKeyFromObject(obj), live); err != nil {
			return connect.NewError(connect.CodeInternal, "could not read the imported pod identity association: "+err.Error())
		}
		if live.Annotations == nil {
			live.Annotations = map[string]string{}
		}
		live.Annotations[crossplaneaws.ExternalNameAnnotation] = id
		if err := s.deps.Kube.Update(ctx, live); err != nil {
			return connect.NewError(connect.CodeInternal, "could not name the imported pod identity association: "+err.Error())
		}
	}
	return nil
}

// storedImport is the import plan kept on the row (imported clusters), or
// nil.
func storedImport(row *model.Cluster) *cloudv1.ImportStatus {
	if len(row.Status) == 0 {
		return nil
	}
	st := &cloudv1.ClusterStatus{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(row.Status, st); err != nil {
		return nil
	}
	return st.GetImport()
}

// adoption is what the cluster keeps of the resources it was imported
// from; zero for a cluster the platform created.
func adoption(row *model.Cluster) crossplaneaws.Adoption {
	var a crossplaneaws.Adoption
	if len(row.Adoption) > 0 {
		_ = json.Unmarshal(row.Adoption, &a) // written by insertImport
	}
	return a
}

// TakeOverCluster makes the platform the owner of an IMPORTED cluster. The
// objects carried over switch to full management (without Delete where
// marked); the rest is rendered from the spec like any cluster's, which
// drops their Observe-only policy, adds the platform's tags and creates
// what the plan lists as WILL_CREATE. The network must be owned by then.
func (s *ClusterService) TakeOverCluster(ctx context.Context, req *cloudv1.TakeOverClusterRequest) (*cloudv1.TakeOverClusterResponse, error) {
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
	spec, err := specOf(row)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error())
	}
	// Refuses a network that is still imported read-only.
	t, err := s.target(ctx, row.TenantID, spec)
	if err != nil {
		return nil, err
	}
	records, err := s.deps.Store.ManagedResources().ListByOwner(ctx, model.OwnerCluster, row.ID)
	if err != nil {
		return nil, service.ToConnect(err)
	}
	for _, r := range records {
		if !r.CarriedOver {
			continue // rendered: the apply below takes it over
		}
		if err := s.own(ctx, r); err != nil {
			s.logger().ErrorContext(ctx, "take over resource", "cluster_id", row.ID, "object", r.Name, "error", err)
			return nil, connect.NewError(connect.CodeInternal, fmt.Sprintf("could not take over %s %s", r.Kind, r.Name))
		}
	}
	imp.Phase = cloudv1.ImportPhase_IMPORT_PHASE_CONVERGING
	if row.Status, err = encode(&cloudv1.ClusterStatus{Import: imp}); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error())
	}
	row.State = stateUpdating
	row.Generation++
	if err := s.deps.Store.Clusters().Update(ctx, row); err != nil {
		return nil, service.ToConnect(err)
	}
	if err := s.apply(ctx, row, t.account, s.renderPlan(spec, row, t)); err != nil {
		return nil, err
	}
	if err := s.tagSubnets(ctx, row, t); err != nil {
		return nil, err
	}
	s.logger().InfoContext(ctx, "cluster taken over", "tenant_id", row.TenantID, "cluster_id", row.ID, "resources", len(records))
	c, err := s.view(ctx, row)
	return &cloudv1.TakeOverClusterResponse{Cluster: c}, err
}

// takeOverAllowed checks state, etag, phase and the acknowledgement of
// NOT_IMPORTED resources, and returns the stored plan.
func takeOverAllowed(row *model.Cluster, req *cloudv1.TakeOverClusterRequest) (*cloudv1.ImportStatus, error) {
	if row.State != stateImported {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Sprintf("cluster %s is not IMPORTED (only imported clusters are taken over)", row.ID))
	}
	if etag := req.GetEtag(); etag != "" && etag != row.ETag {
		return nil, connect.NewError(connect.CodeAborted, "etag mismatch: the cluster changed; get it again and retry")
	}
	imp := storedImport(row)
	if imp.GetPhase() != cloudv1.ImportPhase_IMPORT_PHASE_PLANNED {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Sprintf("import phase is %s, want PLANNED", imp.GetPhase()))
	}
	left := notImported(imp.GetResources())
	if len(left) > 0 && !req.GetAcknowledgeNotImported() {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Sprintf(
			"%d resources are NOT_IMPORTED (first: %s %s: %s); give them another owner, then retry with acknowledge_not_imported",
			len(left), left[0].GetKind(), left[0].GetExternalName(), left[0].GetDetail()))
	}
	return imp, nil
}

// own switches one carried-over object to full management, without Delete
// for one marked crossplaneaws.RetainAnnotation.
func (s *ClusterService) own(ctx context.Context, r model.ManagedResource) error {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.FromAPIVersionAndKind(r.APIVersion, r.Kind))
	// From the API server: a read-modify-write needs the current object.
	if err := s.deps.Reader().Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: r.Name}, u); err != nil {
		if meta.IsNoMatchError(err) {
			return nil
		}
		return err
	}
	policies := []string{"*"}
	if u.GetAnnotations()[crossplaneaws.RetainAnnotation] == "true" {
		policies = policies[:0]
		for _, action := range crossplaneaws.PolicyNoDelete {
			policies = append(policies, string(action))
		}
	}
	if err := unstructured.SetNestedStringSlice(u.Object, policies, "spec", "managementPolicies"); err != nil {
		return err
	}
	return s.deps.Kube.Update(ctx, u)
}
