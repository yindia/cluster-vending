package aws

import (
	"maps"
	"strings"
)

// Tag keys an EC2NodeClass may not set (Karpenter v1 refuses them: it sets
// them itself) or that AWS reserves, and Name, which Karpenter sets to the
// node's name.
var reservedNodeTagKeys = map[string]bool{
	"Name":                           true,
	"eks:eks-cluster-name":           true,
	KarpenterNodePoolLabel:           true,
	"karpenter.sh/nodeclaim":         true,
	"karpenter.sh/managed-by":        true,
	"karpenter.k8s.aws/ec2nodeclass": true,
	"karpenter.k8s.aws/cluster":      true,
}

// reservedNodeTagPrefixes are key prefixes of the same kind.
var reservedNodeTagPrefixes = []string{"aws:", TagKubernetesClusterPrefix}

// KarpenterNodeTags are the tags the platform pool's EC2NodeClass gives the
// instances, volumes, network interfaces and launch templates Karpenter
// makes from it: the cluster's labels (less the keys Karpenter or AWS
// reserve), then the platform's own tenant and cluster tags, which a label
// cannot change. They attribute the nodes' spend to the tenant and cluster
// (cost allocation tags) like the rest of the cluster's resources.
func KarpenterNodeTags(tenantID, clusterID, clusterName string, labels map[string]string) map[string]string {
	out := make(map[string]string, len(labels)+3)
	for k, v := range labels {
		if !reservedNodeTag(k) {
			out[k] = v
		}
	}
	maps.Copy(out, map[string]string{TagTenant: tenantID, TagCluster: clusterID, TagClusterName: clusterName})
	return out
}

func reservedNodeTag(key string) bool {
	if key == "" || reservedNodeTagKeys[key] {
		return true
	}
	for _, p := range reservedNodeTagPrefixes {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

// nodeClassTags is the EC2NodeClass's spec.tags; nil without tags.
func nodeClassTags(tags map[string]string) map[string]any {
	if len(tags) == 0 {
		return nil
	}
	out := make(map[string]any, len(tags))
	for k, v := range tags {
		out[k] = v
	}
	return out
}
