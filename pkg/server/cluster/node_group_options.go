package cluster

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"
	"sigs.k8s.io/yaml"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
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
		InstanceFamilies: slices.Clone(s.deps.NodeGroupInstanceFamilies),
		InstanceTypes:    slices.Clone(s.deps.NodeGroupInstanceTypes),
		Architectures:    []cloudv1.Architecture{cloudv1.Architecture_ARCHITECTURE_AMD64, cloudv1.Architecture_ARCHITECTURE_ARM64},
		CapacityTypes:    []cloudv1.CapacityType{cloudv1.CapacityType_CAPACITY_TYPE_ON_DEMAND, cloudv1.CapacityType_CAPACITY_TYPE_SPOT},
	}, nil
}

// ListClusterNodeGroupManifests renders the Karpenter objects of the
// cluster's node groups with the function the controller applies them from
// (reconcilecluster.NodeGroupObjects), as deterministic YAML.
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
	byName := map[string]*cloudv1.NodeGroupManifest{}
	for _, o := range objs {
		raw, err := yaml.Marshal(o.Object)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err.Error())
		}
		m := byName[o.GetName()]
		if m == nil {
			m = &cloudv1.NodeGroupManifest{Name: o.GetName(), PlatformManaged: o.GetName() == platformGroupName}
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

// platformGroupName is the name of the platform's own node group.
const platformGroupName = "platform"

// checkNodeGroupAllowlist refuses the groups of want that ask for machines
// outside the platform admin's allowlist. Groups equal to one in have (the
// stored spec) are left alone, so a cluster keeps working when the
// allowlist is narrowed later. An empty families list restricts no family;
// a type is allowed when listed or when its family is.
func checkNodeGroupAllowlist(want, have []*cloudv1.NodeGroupSpec, families, types []string) error {
	if len(families) == 0 && len(types) == 0 {
		return nil
	}
	current := map[string]*cloudv1.NodeGroupSpec{}
	for _, g := range have {
		current[g.GetName()] = g
	}
	for _, g := range want {
		if old, ok := current[g.GetName()]; ok && proto.Equal(old, g) {
			continue
		}
		if err := checkGroupAllowed(g, families, types); err != nil {
			return connect.NewError(connect.CodeInvalidArgument, err.Error())
		}
	}
	return nil
}

func checkGroupAllowed(g *cloudv1.NodeGroupSpec, families, types []string) error {
	list := allowedText(families, types)
	if len(families) > 0 {
		for _, f := range g.GetInstanceFamilies() {
			if !slices.Contains(families, f) {
				return fmt.Errorf("node group %q: instance family %q is not allowed; %s", g.GetName(), f, list)
			}
		}
	}
	for _, t := range g.GetInstanceTypes() {
		family, _, _ := strings.Cut(t, ".")
		if !slices.Contains(types, t) && !slices.Contains(families, family) {
			return fmt.Errorf("node group %q: instance type %q is not allowed; %s", g.GetName(), t, list)
		}
	}
	if len(families) > 0 && len(g.GetInstanceFamilies()) == 0 && len(g.GetInstanceTypes()) == 0 {
		// Without either, Karpenter picks from its default categories:
		// that would sidestep the allowlist.
		return fmt.Errorf("node group %q: name instance_families or instance_types from the allowed list; %s", g.GetName(), list)
	}
	return nil
}

func allowedText(families, types []string) string {
	var parts []string
	if len(families) > 0 {
		parts = append(parts, "allowed families: "+strings.Join(families, ", "))
	}
	if len(types) > 0 {
		parts = append(parts, "allowed types: "+strings.Join(types, ", "))
	}
	return strings.Join(parts, "; ")
}
