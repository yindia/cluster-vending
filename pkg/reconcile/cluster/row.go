package cluster

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	corecluster "github.com/yindia/cluster-vending/pkg/cluster"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/fault"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// Numbers of removed ClusterSpec fields (reserved in the contract):
// existing_cluster, default_node_pool, naas and node_groups; and of the
// removed AwsClusterSpec.default_node_pool.
const (
	existingClusterField    = 7
	defaultNodePoolField    = 4
	naasField               = 15
	nodeGroupsField         = 16
	awsDefaultNodePoolField = 4
)

// ErrNaaSRemoved is the message of a request that still sets spec.naas, the
// namespace-as-a-service (Capsule) switch removed with sub-tenants.
const ErrNaaSRemoved = "spec.naas was removed with sub-tenants: the platform no longer installs Capsule"

// ErrNodeGroupsPlatformOwned is the message of a request that still sets the
// node group fields a client built against an older contract has.
const ErrNodeGroupsPlatformOwned = "the platform owns a cluster's node groups: spec.default_node_pool, spec.aws.default_node_pool " +
	"and spec.node_groups were removed; every cluster gets the platform's system node group and Karpenter platform pool"

// supported rejects what this version cannot build.
func supported(spec *cloudv1.ClusterSpec) error {
	unknown := spec.ProtoReflect().GetUnknown()
	switch {
	case spec.GetAws() == nil:
		return fault.New(fault.Unimplemented, "only aws clusters are supported")
	case hasUnknownField(unknown, existingClusterField):
		// A client built against the old contract asks for an import:
		// creating a new cluster of the name instead would be wrong.
		return fault.New(fault.Unimplemented,
			"spec.existing_cluster is not supported; import a cluster with ImportCluster and TakeOverCluster")
	case hasUnknownField(unknown, naasField):
		return fault.New(fault.Invalid, ErrNaaSRemoved)
	}
	return NodeGroupFieldsRefused(spec)
}

// NodeGroupFieldsRefused is an Invalid fault when spec (a binary request's:
// JSON drops unknown fields) still carries a removed node group field.
func NodeGroupFieldsRefused(spec *cloudv1.ClusterSpec) error {
	if hasUnknownField(spec.ProtoReflect().GetUnknown(), defaultNodePoolField) ||
		hasUnknownField(spec.ProtoReflect().GetUnknown(), nodeGroupsField) ||
		hasUnknownField(spec.GetAws().ProtoReflect().GetUnknown(), awsDefaultNodePoolField) {
		return fault.New(fault.Invalid, ErrNodeGroupsPlatformOwned)
	}
	return nil
}

// hasUnknownField reports whether raw (a message's unknown fields, in the
// binary wire format) holds field number. JSON requests never get here:
// the JSON codec drops unknown fields.
func hasUnknownField(raw []byte, number protowire.Number) bool {
	for len(raw) > 0 {
		n, typ, size := protowire.ConsumeTag(raw)
		if size < 0 {
			return false
		}
		if n == number {
			return true
		}
		raw = raw[size:]
		size = protowire.ConsumeFieldValue(n, typ, raw)
		if size < 0 {
			return false
		}
		raw = raw[size:]
	}
	return false
}

// NewRow checks a create request (the contract's rules, what this version
// can build, the labels, the network and its subnets, the name among the
// platform's own clusters) and returns the row CreateCluster stores for
// it, not stored yet: the cluster as sent (input) and with its defaults
// (spec), PROVISIONING, its generation neither applied nor resolved yet.
// The ID is chosen here, it names the resources. The API handler and the
// controller (for a Cluster object that was created without the API) both
// build their row here.
//
// It does not look at the cloud. Whether EKS already has a cluster of the
// name, and which Kubernetes and add-on versions EKS offers, is found out
// by Reconcile before anything is rendered (resolve).
func (s *Reconciler) NewRow(ctx context.Context, req *cloudv1.CreateClusterRequest) (*model.Cluster, error) {
	if err := reconcile.Validate(req); err != nil {
		return nil, err
	}
	in := req.GetCluster()
	if err := supported(in.GetSpec()); err != nil {
		return nil, err
	}
	if err := reconcile.CheckAWSLabels(in.GetLabels()); err != nil {
		return nil, err
	}
	n, err := s.newClusterNetwork(ctx, req.GetTenantId(), in.GetSpec())
	if err != nil {
		return nil, err
	}
	t, err := s.targetIn(ctx, req.GetTenantId(), n, in.GetSpec())
	if err != nil {
		return nil, err
	}
	if err := s.NameTaken(ctx, req.GetTenantId(), in.GetName()); err != nil {
		return nil, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, fault.New(fault.Internal, err.Error())
	}
	// A cluster on a channel runs the channel's minor unless it names an
	// older one; the client's input is stored as it was sent.
	//nolint:staticcheck // Reject legacy per-cluster tier writes with migration guidance.
	if in.GetSpec().GetTier() != cloudv1.ClusterTier_CLUSTER_TIER_UNSPECIFIED {
		return nil, fault.New(fault.Invalid, "tier is inherited from the network; set spec.tier on the network")
	}
	requested := proto.CloneOf(in.GetSpec())
	if err := s.withChannelVersion(ctx, requested); err != nil {
		return nil, err
	}
	if err := s.CheckExtendedSupport(ctx, requested, nil); err != nil {
		return nil, err
	}
	if err := s.withIRSA(ctx, requested); err != nil {
		return nil, err
	}
	spec, err := corecluster.ResolveSpec(requested, t.Zones)
	if err != nil {
		return nil, fault.New(fault.Invalid, err.Error())
	}
	input, err1 := Encode(in)
	raw, err2 := Encode(spec)
	if err := errors.Join(err1, err2); err != nil {
		return nil, fault.New(fault.Internal, err.Error())
	}
	row := &model.Cluster{
		Resource: model.Resource{Base: model.Base{ID: id.String()}, DisplayName: in.GetDisplayName(),
			Labels: in.GetLabels(), State: StateProvisioning, Spec: raw},
		TenantID: req.GetTenantId(), Name: in.GetName(), Input: input, NetworkID: t.Network.ID,
		Provider: model.ProviderAWS, Region: spec.GetRegion(),
	}
	if rid := req.GetRequestId(); rid != "" {
		row.CreateRequestID = &rid
	}
	return row, nil
}

// NameTaken fails with an AlreadyExists fault when the tenant already has
// a cluster of the name: a cluster's name is unique per tenant (it is the
// Cluster object's name in the tenant's namespace), whatever its account
// and region. It reads the store only: an EKS cluster of the name made
// outside the platform is found by Reconcile (resolve).
func (s *Reconciler) NameTaken(ctx context.Context, tenantID, name string) error {
	rows, err := s.deps.Store.Clusters().ListByName(ctx, tenantID, name)
	if err != nil {
		return storeError(err)
	}
	if len(rows) > 0 {
		c := rows[0]
		return fault.New(fault.AlreadyExists, fmt.Sprintf(
			"cluster %s (%s) already has the name %q: cluster names are unique per tenant; choose another name",
			c.ID, c.State, name))
	}
	return nil
}
