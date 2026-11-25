package cluster

import (
	"context"

	"connectrpc.com/connect/v2"
	"sigs.k8s.io/yaml"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	reconcilecluster "github.com/yindia/cluster-vending/pkg/reconcile/cluster"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

// ListClusterNodeGroupManifests renders the Karpenter objects of the
// cluster's platform pool with the function the controller applies them
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
