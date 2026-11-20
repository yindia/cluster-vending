package cluster

import (
	"fmt"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// CheckNodeGroupAllowlist refuses the groups of want that ask for machines
// outside the platform admin's allowlist. Groups equal to one in have (the
// stored spec) are left alone, so a cluster keeps working when the
// allowlist is narrowed later. With any allowlist set, a group must name
// families or types, a family must be listed, and a type is allowed when
// listed or when its family is. Both lists empty restricts nothing.
//
// The server (CreateCluster, UpdateCluster) and the controller (a Cluster
// made or edited with kubectl) both call it.
func CheckNodeGroupAllowlist(want, have []*cloudv1.NodeGroupSpec, families, types []string) error {
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
			return err
		}
	}
	return nil
}

func checkGroupAllowed(g *cloudv1.NodeGroupSpec, families, types []string) error {
	list := allowedText(families, types)
	// A family is allowed only when listed: with a types-only allowlist no
	// family is, the group has to name allowed types.
	for _, f := range g.GetInstanceFamilies() {
		if !slices.Contains(families, f) {
			return fmt.Errorf("node group %q: instance family %q is not allowed; %s", g.GetName(), f, list)
		}
	}
	for _, t := range g.GetInstanceTypes() {
		family, _, _ := strings.Cut(t, ".")
		if !slices.Contains(types, t) && !slices.Contains(families, family) {
			return fmt.Errorf("node group %q: instance type %q is not allowed; %s", g.GetName(), t, list)
		}
	}
	if len(g.GetInstanceFamilies()) == 0 && len(g.GetInstanceTypes()) == 0 {
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
