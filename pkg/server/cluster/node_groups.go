package cluster

import (
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// nodePoolAPIVersion is the Karpenter API a user node group becomes.
const nodePoolAPIVersion = "karpenter.sh/v1"

// nodeGroupChanges are the plan's entries for the user's node groups: the
// groups the update adds (CREATE), changes (UPDATE_IN_PLACE) and removes
// (DELETE), in name order. They are NodePools of the workload cluster, not
// managed resources of the management cluster, so the Crossplane
// comparison does not see them. have is the stored spec, want the new one.
func nodeGroupChanges(have, want *cloudv1.ClusterSpec) []*cloudv1.PlannedChange {
	current := map[string]*cloudv1.NodeGroupSpec{}
	for _, g := range have.GetNodeGroups() {
		current[g.GetName()] = g
	}
	desired := map[string]*cloudv1.NodeGroupSpec{}
	for _, g := range want.GetNodeGroups() {
		desired[g.GetName()] = g
	}
	var out []*cloudv1.PlannedChange
	add := func(name string, action cloudv1.PlannedAction, detail string) {
		out = append(out, &cloudv1.PlannedChange{ApiVersion: nodePoolAPIVersion, Kind: "NodePool", Name: name,
			SpecPath: pathNodeGroups, Action: action, Detail: detail})
	}
	for name, g := range desired {
		old, ok := current[name]
		switch {
		case !ok:
			add(name, cloudv1.PlannedAction_PLANNED_ACTION_CREATE, "node group "+name+" is added")
		case !proto.Equal(old, g):
			add(name, cloudv1.PlannedAction_PLANNED_ACTION_UPDATE_IN_PLACE, "changed: "+strings.Join(changedFields(old, g), ", "))
		}
	}
	for name := range current {
		if _, ok := desired[name]; !ok {
			add(name, cloudv1.PlannedAction_PLANNED_ACTION_DELETE, "node group "+name+" is removed; Karpenter drains its nodes")
		}
	}
	slices.SortFunc(out, func(a, b *cloudv1.PlannedChange) int { return strings.Compare(a.GetName(), b.GetName()) })
	return out
}

// changedFields are the names of the fields in which a and b differ.
func changedFields(a, b *cloudv1.NodeGroupSpec) []string {
	var out []string
	fields := a.ProtoReflect().Descriptor().Fields()
	for i := range fields.Len() {
		fd := fields.Get(i)
		if !equalField(a.ProtoReflect(), b.ProtoReflect(), fd) {
			out = append(out, string(fd.Name()))
		}
	}
	return out
}

func equalField(a, b protoreflect.Message, fd protoreflect.FieldDescriptor) bool {
	return a.Has(fd) == b.Has(fd) && a.Get(fd).Equal(b.Get(fd))
}
