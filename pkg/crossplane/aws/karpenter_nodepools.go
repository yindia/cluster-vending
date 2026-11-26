package aws

import (
	"fmt"
	"slices"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
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
	// applies (the platform pool's, and those of the tenant groups and the
	// old platform group of a cluster built before the platform owned its
	// node groups): only objects with it are reverted or deleted by the
	// controller.
	LabelManagedBy = "cloud.cluster-vending.io/managed-by"
	// ManagedByPlatform is its value.
	ManagedByPlatform = "platform"

	// defaultAMIPin is what an EC2NodeClass pins when the release has none.
	defaultAMIPin = "latest"
)

// KarpenterNodeGroupsInput is everything the cluster's Karpenter objects
// are rendered from.
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
	// PlatformPool is the platform pool (nodeGroups.platform): a class and
	// a pool named PoolPlatform, the only Karpenter group a cluster has.
	// Nil: nothing is rendered.
	PlatformPool *PlatformPoolInput
	// Tags are the EC2NodeClass's spec.tags (KarpenterNodeTags): on every
	// instance, volume and launch template Karpenter makes from it. Empty:
	// none.
	Tags map[string]string
}

// KarpenterNodeGroupObjects are the cluster's Karpenter objects, exactly
// as they are applied to the workload cluster: the platform pool's
// EC2NodeClass and NodePool, the class first; none without a platform
// pool. The same input always gives the same objects.
func KarpenterNodeGroupObjects(in KarpenterNodeGroupsInput) ([]*unstructured.Unstructured, error) {
	if in.ClusterName == "" || in.ClusterPrefix == "" {
		return nil, fmt.Errorf("cluster name and prefix are required")
	}
	if in.PlatformPool == nil {
		return nil, nil
	}
	return renderPlatformPool(in)
}

// renderNodeClassWith is the EC2NodeClass of name: the cluster's Karpenter
// node role, the subnets and the security group found by tag, the pinned
// AL2023 AMI, and the kubelet's maxPods (0: not set, Karpenter's default
// for the shape).
func renderNodeClassWith(in KarpenterNodeGroupsInput, name string, maxPods int) *unstructured.Unstructured {
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
	if maxPods > 0 {
		_ = unstructured.SetNestedField(u.Object, map[string]any{"maxPods": int64(maxPods)}, "spec", "kubelet")
	}
	if tags := nodeClassTags(in.Tags); tags != nil {
		_ = unstructured.SetNestedField(u.Object, tags, "spec", "tags")
	}
	u.SetGroupVersionKind(KarpenterNodeClassGVK)
	u.SetName(name)
	u.SetLabels(map[string]string{LabelManagedBy: ManagedByPlatform})
	return u
}

// inRequirement is a NodePool requirement that key is one of values.
func inRequirement(key string, values ...string) map[string]any {
	vs := make([]any, 0, len(values))
	for _, v := range values {
		vs = append(vs, v)
	}
	return map[string]any{"key": key, "operator": "In", "values": vs}
}

func sortedUnique(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return slices.Compact(out)
}
