package aws

import (
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// The workload cluster's Karpenter kinds, applied as unstructured: the
// platform does not import Karpenter's API.
var (
	KarpenterNodePoolGVK  = schema.GroupVersionKind{Group: "karpenter.sh", Version: "v1", Kind: "NodePool"}
	KarpenterNodeClaimGVK = schema.GroupVersionKind{Group: "karpenter.sh", Version: "v1", Kind: "NodeClaim"}
	KarpenterNodeClassGVK = schema.GroupVersionKind{Group: "karpenter.k8s.aws", Version: "v1", Kind: "EC2NodeClass"}
)

const (
	// KarpenterNodePoolLabel is the label Karpenter puts on its nodes: the
	// NodePool's name.
	KarpenterNodePoolLabel = "karpenter.sh/nodepool"
	karpenterAPIGroup      = "karpenter.k8s.aws"
	eksClusterNameTag      = "aws:eks:cluster-name"

	// LabelManagedBy marks the NodePools and EC2NodeClasses the platform
	// applies (platform and user groups alike): only objects with it are
	// reverted or deleted by the controller.
	LabelManagedBy = "cloud.cluster-vending.io/managed-by"
	// ManagedByPlatform is its value.
	ManagedByPlatform = "platform"

	// platformCPULimit and platformMemoryLimit are the platform pool's
	// (generous) limits.
	platformCPULimit    = "1000"
	platformMemoryLimit = "4000Gi"
	// defaultAMIPin is what an EC2NodeClass pins when the release has none.
	defaultAMIPin = "latest"
)

// KarpenterNodeGroupsInput is everything the EC2NodeClasses and NodePools
// of a cluster are rendered from.
type KarpenterNodeGroupsInput struct {
	// ClusterName is the EKS cluster name; ClusterPrefix the cluster's
	// object prefix (ClusterPrefix(...), the node role's).
	ClusterName, ClusterPrefix string
	// AMIPin is the AL2023 AMI release the nodes pin ("v20261107"); empty:
	// latest.
	AMIPin string
	// Discovery selects subnets with karpenter.sh/discovery=<cluster>;
	// false: with kubernetes.io/cluster/<cluster> (another cluster of the
	// network holds the discovery tag).
	Discovery bool
	// NodeGroups are the spec's user node groups (spec.node_groups); the
	// platform group is always rendered.
	NodeGroups []*cloudv1.NodeGroupSpec
}

// KarpenterNodeGroupObjects are the objects of the cluster's Karpenter
// node groups, exactly as they are applied to the workload cluster:
// first the platform group's EC2NodeClass and NodePool, then each user
// group's, a class before its pool, in the spec's order. The same input
// always gives the same objects. A name that is empty, repeated, reserved
// ("platform", "default", "system") or not a DNS label is an error.
func KarpenterNodeGroupObjects(in KarpenterNodeGroupsInput) ([]*unstructured.Unstructured, error) {
	if in.ClusterName == "" || in.ClusterPrefix == "" {
		return nil, fmt.Errorf("cluster name and prefix are required")
	}
	groups := make([]karpenterGroup, 1, 1+len(in.NodeGroups))
	groups[0] = karpenterGroup{Name: PoolPlatform, PlatformPool: true}
	seen := map[string]bool{}
	for _, g := range in.NodeGroups {
		name := g.GetName()
		if errs := validation.IsDNS1123Label(name); len(errs) > 0 {
			return nil, fmt.Errorf("node group name %q: %s", name, strings.Join(errs, "; "))
		}
		if name == PoolPlatform || name == "default" || name == PoolSystem || seen[name] {
			return nil, fmt.Errorf("node group name %q is reserved or repeated", name)
		}
		seen[name] = true
		groups = append(groups, karpenterGroup{Name: name, Spec: g})
	}
	out := make([]*unstructured.Unstructured, 0, 2*len(groups))
	for _, g := range groups {
		out = append(out, renderNodeClass(in, g.Name), renderNodePool(g))
	}
	return out, nil
}

// karpenterGroup is a node group as rendered: the platform's or a user's.
type karpenterGroup struct {
	Name         string
	PlatformPool bool
	Spec         *cloudv1.NodeGroupSpec // nil for the platform's
}

// renderNodeClass is the group's EC2NodeClass: the cluster's Karpenter
// node role, the subnets and the security group found by tag, and the
// pinned AL2023 AMI.
func renderNodeClass(in KarpenterNodeGroupsInput, name string) *unstructured.Unstructured {
	subnetTag := map[string]any{TagKarpenterDiscovery: in.ClusterName}
	if !in.Discovery {
		subnetTag = map[string]any{TagKubernetesClusterPrefix + in.ClusterName: "*"}
	}
	pin := in.AMIPin
	if pin == "" {
		pin = defaultAMIPin
	}
	u := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{
		"role":                       KarpenterNodeRoleName(in.ClusterPrefix),
		"amiSelectorTerms":           []any{map[string]any{"alias": "al2023@" + pin}},
		"subnetSelectorTerms":        []any{map[string]any{"tags": subnetTag}},
		"securityGroupSelectorTerms": []any{map[string]any{"tags": map[string]any{eksClusterNameTag: in.ClusterName}}},
	}}}
	u.SetGroupVersionKind(KarpenterNodeClassGVK)
	u.SetName(name)
	u.SetLabels(map[string]string{LabelManagedBy: ManagedByPlatform})
	return u
}

func requirement(key, operator string, values ...string) map[string]any {
	r := map[string]any{"key": key, "operator": operator}
	if len(values) > 0 {
		vs := make([]any, 0, len(values))
		for _, v := range values {
			vs = append(vs, v)
		}
		r["values"] = vs
	}
	return r
}

var (
	archNames     = map[cloudv1.Architecture]string{cloudv1.Architecture_ARCHITECTURE_AMD64: "amd64", cloudv1.Architecture_ARCHITECTURE_ARM64: "arm64"}
	capacityNames = map[cloudv1.CapacityType]string{cloudv1.CapacityType_CAPACITY_TYPE_ON_DEMAND: "on-demand", cloudv1.CapacityType_CAPACITY_TYPE_SPOT: "spot"}
	effectNames   = map[cloudv1.TaintEffect]string{cloudv1.TaintEffect_TAINT_EFFECT_NO_SCHEDULE: "NoSchedule",
		cloudv1.TaintEffect_TAINT_EFFECT_PREFER_NO_SCHEDULE: "PreferNoSchedule", cloudv1.TaintEffect_TAINT_EFFECT_NO_EXECUTE: "NoExecute"}
)

// categoryRequirements are the defaults when a group names no instance
// type or family: compute, general and memory optimised, newer than gen 5.
func categoryRequirements() []any {
	return []any{
		requirement("karpenter.k8s.aws/instance-category", "In", "c", "m", "r"),
		requirement("karpenter.k8s.aws/instance-generation", "Gt", "5"),
	}
}

// uniqueNames maps enum values to their names, sorted, skipping unknown
// values and duplicates; def when none is left.
func uniqueNames[T comparable](values []T, names map[T]string, def string) []string {
	var out []string
	for _, v := range values {
		if n, ok := names[v]; ok && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	if len(out) == 0 {
		return []string{def}
	}
	slices.Sort(out)
	return out
}

// renderNodePool is the group's NodePool, pointing at the EC2NodeClass of
// its name. The platform's is on-demand, amd64 and tainted for the
// platform's own pods; a user's follows its spec, with the pool label and
// the user's labels and taints.
func renderNodePool(g karpenterGroup) *unstructured.Unstructured {
	labels := map[string]any{PoolLabelKey: g.Name}
	var taints []any
	var reqs []any
	limits := map[string]any{}
	if g.PlatformPool {
		taint := PoolTaint(PoolPlatform)
		taints = append(taints, map[string]any{"key": taint.Key, "value": taint.Value, "effect": "NoSchedule"})
		// The platform's own choice (PlatformInstanceFamilies/Sizes); the
		// admin allowlist applies to user groups only.
		reqs = append(reqs,
			requirement("karpenter.k8s.aws/instance-family", "In", PlatformInstanceFamilies...),
			requirement("karpenter.k8s.aws/instance-size", "In", PlatformInstanceSizes...),
			requirement("kubernetes.io/arch", "In", "amd64"),
			requirement("kubernetes.io/os", "In", "linux"),
			requirement("karpenter.sh/capacity-type", "In", "on-demand"))
		limits["cpu"], limits["memory"] = platformCPULimit, platformMemoryLimit
	} else {
		s := g.Spec
		for k, v := range s.GetLabels() {
			if k != PoolLabelKey {
				labels[k] = v
			}
		}
		for _, t := range s.GetTaints() {
			taints = append(taints, map[string]any{"key": t.GetKey(), "value": t.GetValue(),
				"effect": orEffect(t.GetEffect())})
		}
		if len(s.GetInstanceTypes()) > 0 {
			reqs = append(reqs, requirement("node.kubernetes.io/instance-type", "In", sortedUnique(s.GetInstanceTypes())...))
		}
		if len(s.GetInstanceFamilies()) > 0 {
			reqs = append(reqs, requirement("karpenter.k8s.aws/instance-family", "In", sortedUnique(s.GetInstanceFamilies())...))
		}
		if len(s.GetInstanceTypes()) == 0 && len(s.GetInstanceFamilies()) == 0 {
			reqs = append(reqs, categoryRequirements()...)
		}
		reqs = append(reqs,
			requirement("kubernetes.io/arch", "In", uniqueNames(s.GetArchitectures(), archNames, "amd64")...),
			requirement("kubernetes.io/os", "In", "linux"),
			requirement("karpenter.sh/capacity-type", "In", uniqueNames(s.GetCapacityTypes(), capacityNames, "on-demand")...))
		if cpu := s.GetLimits().GetCpu(); s.GetLimits() != nil && s.GetLimits().Cpu != nil {
			limits["cpu"] = fmt.Sprintf("%d", cpu)
		}
		if s.GetLimits() != nil && s.GetLimits().MemoryGib != nil {
			limits["memory"] = fmt.Sprintf("%dGi", s.GetLimits().GetMemoryGib())
		}
	}
	templateSpec := map[string]any{
		"nodeClassRef": map[string]any{"group": karpenterAPIGroup, "kind": KarpenterNodeClassGVK.Kind, "name": g.Name},
		"requirements": reqs,
	}
	if len(taints) > 0 {
		templateSpec["taints"] = taints
	}
	spec := map[string]any{
		"template":   map[string]any{"metadata": map[string]any{"labels": labels}, "spec": templateSpec},
		"disruption": map[string]any{"consolidationPolicy": "WhenEmptyOrUnderutilized", "consolidateAfter": "1m"},
	}
	if len(limits) > 0 {
		spec["limits"] = limits
	}
	u := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	u.SetGroupVersionKind(KarpenterNodePoolGVK)
	u.SetName(g.Name)
	u.SetLabels(map[string]string{LabelManagedBy: ManagedByPlatform})
	return u
}

func orEffect(e cloudv1.TaintEffect) string {
	if n, ok := effectNames[e]; ok {
		return n
	}
	return "NoSchedule"
}

func sortedUnique(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return slices.Compact(out)
}
