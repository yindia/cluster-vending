package convert

import (
	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

const prefixNodeGroupPhase = "NODE_GROUP_PHASE_"

// nodeGroupStatuses are the contract's node group statuses as the kind
// lists them; nil for none.
func nodeGroupStatuses(in []*cloudv1.NodeGroupStatus) []cloudv1alpha1.NodeGroupStatus {
	if len(in) == 0 {
		return nil
	}
	out := make([]cloudv1alpha1.NodeGroupStatus, 0, len(in))
	for _, g := range in {
		out = append(out, cloudv1alpha1.NodeGroupStatus{
			Name: g.GetName(), PlatformManaged: g.GetPlatformManaged(),
			Phase:   cloudv1alpha1.NodeGroupPhase(camel(g.GetPhase(), prefixNodeGroupPhase)),
			Message: g.GetMessage(), Nodes: g.GetNodes(), NodesNotice: g.GetNodesNotice(),
		})
	}
	return out
}

// NodeGroupStatusesFromAPI is the kind's node group statuses as the
// contract's; nil for none.
func NodeGroupStatusesFromAPI(in []cloudv1alpha1.NodeGroupStatus) []*cloudv1.NodeGroupStatus {
	if len(in) == 0 {
		return nil
	}
	out := make([]*cloudv1.NodeGroupStatus, 0, len(in))
	for _, g := range in {
		out = append(out, &cloudv1.NodeGroupStatus{
			Name: g.Name, PlatformManaged: g.PlatformManaged,
			Phase: cloudv1.NodeGroupPhase(uncamel(
				cloudv1.NodeGroupPhase(0).Descriptor(), prefixNodeGroupPhase, string(g.Phase))),
			Message: g.Message, Nodes: g.Nodes, NodesNotice: g.NodesNotice,
		})
	}
	return out
}
