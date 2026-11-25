package aws

import (
	"strings"

	awsevents "github.com/upbound/provider-aws/v2/apis/namespaced/cloudwatchevents/v1beta1"
	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	awsiam "github.com/upbound/provider-aws/v2/apis/namespaced/iam/v1beta1"
	awssqs "github.com/upbound/provider-aws/v2/apis/namespaced/sqs/v1beta1"
)

// ClusterDeleteStage is where an object of the cluster itself (not its
// platform components, which have their own owner) goes in the cluster's
// deletion, the reverse of its creation: Karpenter's setup, then the EKS
// add-ons, then everything else. Each stage starts once the one before it
// is gone.
type ClusterDeleteStage int

const (
	// DeleteStageKarpenter: the interruption queue with its policy, the
	// EventBridge rules and targets, the node role with its policies and
	// its access entry (karpenterObjects).
	DeleteStageKarpenter ClusterDeleteStage = iota + 1
	// DeleteStageAddons: the EKS add-ons, their pod identity associations
	// and their roles and policies (setup in addons.yaml).
	DeleteStageAddons
	// DeleteStageRest: the default managed node group, the EKS cluster and
	// its IAM, the results bucket and the rest, ordered by their Usages.
	DeleteStageRest
)

// ClusterObjectDeleteStage is the stage of the object of that kind and name
// in the cluster with that prefix (ClusterPlan.Prefix).
func ClusterObjectDeleteStage(prefix, kind, name string) ClusterDeleteStage {
	switch kind {
	case awseks.Addon_Kind, awseks.PodIdentityAssociation_Kind:
		return DeleteStageAddons
	case awssqs.Queue_Kind, awssqs.QueuePolicy_Kind, awsevents.Rule_Kind, awsevents.Target_Kind:
		if strings.HasPrefix(name, prefix+"-karp") {
			return DeleteStageKarpenter
		}
	case awseks.AccessEntry_Kind:
		if name == prefix+"-karpenter-access" {
			return DeleteStageKarpenter
		}
	case awsiam.Role_Kind, awsiam.RolePolicyAttachment_Kind, awsiam.Policy_Kind:
		if strings.HasPrefix(name, prefix+"-karpenter-node") {
			return DeleteStageKarpenter
		}
		for _, setup := range addonCatalog.setups {
			if strings.HasPrefix(name, prefix+"-"+setup.short+"-") {
				return DeleteStageAddons
			}
		}
	}
	return DeleteStageRest
}

// IsKarpenterNodeIAM reports whether the object is the Karpenter node role
// or one of its policy attachments: what the Usages of karpenterUsagePairs
// keep until the EKS cluster is gone, which a deletion that removes
// Karpenter first must release (the nodes are gone by then).
func IsKarpenterNodeIAM(prefix, kind, name string) bool {
	return (kind == awsiam.Role_Kind || kind == awsiam.RolePolicyAttachment_Kind) && strings.HasPrefix(name, prefix+"-karpenter-node")
}
