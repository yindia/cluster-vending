package cluster

import (
	"context"
	"slices"

	"connectrpc.com/connect/v2"
	"sigs.k8s.io/yaml"

	corecluster "github.com/yindia/cluster-vending/pkg/cluster"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	reconcilecluster "github.com/yindia/cluster-vending/pkg/reconcile/cluster"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

// GetNodeGroupOptions is the platform admin's allowlist for user node
// groups, plus the architectures and capacity types a group may ask for.
// Clients use it to suggest and pre-check; CreateCluster and UpdateCluster
// are the authority.
func (s *ClusterService) GetNodeGroupOptions(_ context.Context, req *cloudv1.GetNodeGroupOptionsRequest) (_ *cloudv1.GetNodeGroupOptionsResponse, err error) {
	defer func() { err = rpcError(err) }()
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	return &cloudv1.GetNodeGroupOptionsResponse{
		InstanceFamilies: slices.Clone(s.deps.NodeGroups.Karpenter.InstanceFamilies),
		InstanceTypes:    slices.Clone(s.deps.NodeGroups.Karpenter.InstanceTypes),
		Architectures:    []cloudv1.Architecture{cloudv1.Architecture_ARCHITECTURE_AMD64, cloudv1.Architecture_ARCHITECTURE_ARM64},
		CapacityTypes:    []cloudv1.CapacityType{cloudv1.CapacityType_CAPACITY_TYPE_ON_DEMAND, cloudv1.CapacityType_CAPACITY_TYPE_SPOT},
	}, nil
}

// ListClusterNodeGroupManifests renders the Karpenter objects of the
// cluster's user node groups with the function the controller applies them
// from (reconcilecluster.NodeGroupObjects), as deterministic YAML. The first
// entry is the system group (platform_managed; none for a cluster the platform
// does not manage): the default EKS
// managed node group is a managed resource of the management cluster, not
// Karpenter objects, and the contract has no field for its YAML yet, so the
// entry carries no YAML.
func (s *ClusterService) ListClusterNodeGroupManifests(ctx context.Context, req *cloudv1.ListClusterNodeGroupManifestsRequest) (_ *cloudv1.ListClusterNodeGroupManifestsResponse, err error) {
	defer func() { err = rpcError(err) }()
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	row, err := s.get(ctx, req.GetTenantId(), req.GetClusterId())
	if err != nil {
		return nil, err
	}
	objs, err := s.NodeGroupObjects(ctx, row)
	if err != nil {
		s.logger().WarnContext(ctx, "render node group manifests", "cluster_id", row.ID, "error", err)
		return nil, connect.NewError(connect.CodeFailedPrecondition, "cannot render the node groups: "+err.Error())
	}
	out := &cloudv1.ListClusterNodeGroupManifestsResponse{}
	if reconcilecluster.PlatformManaged(row) {
		out.NodeGroups = append(out.NodeGroups, &cloudv1.NodeGroupManifest{Name: systemNodeGroup, PlatformManaged: true})
	}
	byName := map[string]*cloudv1.NodeGroupManifest{}
	for _, o := range objs {
		raw, err := yaml.Marshal(o.Object)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err.Error())
		}
		m := byName[o.GetName()]
		if m == nil {
			m = &cloudv1.NodeGroupManifest{Name: o.GetName(), PlatformManaged: o.GetName() == crossplaneaws.PoolPlatform}
			byName[o.GetName()] = m
			out.NodeGroups = append(out.NodeGroups, m)
		}
		if o.GetKind() == "EC2NodeClass" {
			m.Ec2NodeClassYaml = string(raw)
		} else {
			m.NodePoolYaml = string(raw)
		}
	}
	return out, nil
}

// checkNodeGroupAllowlist is corecluster.CheckNodeGroupAllowlist with the
// refusal as INVALID_ARGUMENT.
func checkNodeGroupAllowlist(want, have []*cloudv1.NodeGroupSpec, families, types []string) error {
	if err := corecluster.CheckNodeGroupAllowlist(want, have, families, types); err != nil {
		return connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	return nil
}
