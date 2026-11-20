package convert

import (
	"slices"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

const (
	prefixArchitecture   = "ARCHITECTURE_"
	prefixNodeGroupPhase = "NODE_GROUP_PHASE_"
)

// nodeGroupsToAPI is the contract's user node groups as the kind lists
// them; nil for none.
func nodeGroupsToAPI(in []*cloudv1.NodeGroupSpec) []cloudv1alpha1.NodeGroupSpec {
	if len(in) == 0 {
		return nil
	}
	out := make([]cloudv1alpha1.NodeGroupSpec, 0, len(in))
	for _, g := range in {
		n := cloudv1alpha1.NodeGroupSpec{
			Name:             g.GetName(),
			InstanceTypes:    slices.Clone(g.GetInstanceTypes()),
			InstanceFamilies: slices.Clone(g.GetInstanceFamilies()),
		}
		for _, a := range g.GetArchitectures() {
			n.Architectures = append(n.Architectures, cloudv1alpha1.Architecture(camel(a, prefixArchitecture)))
		}
		for _, c := range g.GetCapacityTypes() {
			n.CapacityTypes = append(n.CapacityTypes, cloudv1alpha1.CapacityType(camel(c, prefixCapacity)))
		}
		if len(g.GetLabels()) > 0 {
			n.Labels = make(map[string]string, len(g.GetLabels()))
			for k, v := range g.GetLabels() {
				n.Labels[k] = v
			}
		}
		for _, t := range g.GetTaints() {
			n.Taints = append(n.Taints, cloudv1alpha1.Taint{Key: t.GetKey(), Value: t.GetValue(),
				Effect: cloudv1alpha1.TaintEffect(camel(t.GetEffect(), prefixTaintEffect))})
		}
		if l := g.GetLimits(); l != nil {
			n.Limits = &cloudv1alpha1.NodeGroupLimits{CPU: l.Cpu, MemoryGiB: l.MemoryGib}
		}
		out = append(out, n)
	}
	return out
}

// nodeGroupsFromAPI is nodeGroupsToAPI the other way.
func nodeGroupsFromAPI(in []cloudv1alpha1.NodeGroupSpec) []*cloudv1.NodeGroupSpec {
	if len(in) == 0 {
		return nil
	}
	out := make([]*cloudv1.NodeGroupSpec, 0, len(in))
	for _, g := range in {
		n := &cloudv1.NodeGroupSpec{
			Name:             g.Name,
			InstanceTypes:    slices.Clone(g.InstanceTypes),
			InstanceFamilies: slices.Clone(g.InstanceFamilies),
		}
		for _, a := range g.Architectures {
			n.Architectures = append(n.Architectures, cloudv1.Architecture(
				uncamel(cloudv1.Architecture(0).Descriptor(), prefixArchitecture, string(a))))
		}
		for _, c := range g.CapacityTypes {
			n.CapacityTypes = append(n.CapacityTypes, cloudv1.CapacityType(
				uncamel(cloudv1.CapacityType(0).Descriptor(), prefixCapacity, string(c))))
		}
		if len(g.Labels) > 0 {
			n.Labels = make(map[string]string, len(g.Labels))
			for k, v := range g.Labels {
				n.Labels[k] = v
			}
		}
		for _, t := range g.Taints {
			n.Taints = append(n.Taints, &cloudv1.Taint{Key: t.Key, Value: t.Value, Effect: cloudv1.TaintEffect(
				uncamel(cloudv1.TaintEffect(0).Descriptor(), prefixTaintEffect, string(t.Effect)))})
		}
		if l := g.Limits; l != nil {
			n.Limits = &cloudv1.NodeGroupLimits{Cpu: copyInt32(l.CPU), MemoryGib: copyInt32(l.MemoryGiB)}
		}
		out = append(out, n)
	}
	return out
}

func copyInt32(p *int32) *int32 {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

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
