package aws_test

import (
	"reflect"
	"slices"
	"testing"

	protectionv1beta1 "github.com/crossplane/crossplane/apis/v2/protection/v1beta1"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
)

// Every object of a cluster has a deletion stage: Karpenter's queue, rules,
// targets, node role (and its policies and access entry) go with Karpenter;
// the EKS add-ons with their pod identity associations and roles go before
// the rest; everything else (cluster, default node group, cluster IAM,
// results bucket, ...) is the last stage.
func TestClusterDeleteStage(t *testing.T) {
	p := karpenterPlan()
	prefix := p.Prefix
	byStage := map[crossplaneaws.ClusterDeleteStage][]string{}
	for _, o := range crossplaneaws.ClusterResources(p) {
		if _, usage := o.(*protectionv1beta1.Usage); usage {
			continue
		}
		kind := reflect.TypeOf(o).Elem().Name()
		stage := crossplaneaws.ClusterObjectDeleteStage(prefix, kind, o.GetName())
		byStage[stage] = append(byStage[stage], kind+"/"+o.GetName())
	}
	for _, want := range []string{
		"Queue/" + prefix + "-karpenter", "QueuePolicy/" + prefix + "-karpenter-queue-policy",
		"Role/" + prefix + "-karpenter-node", "RolePolicyAttachment/" + prefix + "-karpenter-node-worker",
		"AccessEntry/" + prefix + "-karpenter-access", "Rule/" + prefix + "-karp-spot", "Target/" + prefix + "-karp-spot-target",
	} {
		if !slices.Contains(byStage[crossplaneaws.DeleteStageKarpenter], want) {
			t.Errorf("%s is not in the Karpenter stage: %v", want, byStage[crossplaneaws.DeleteStageKarpenter])
		}
	}
	for _, want := range []string{
		"Addon/" + prefix + "-addon-vpc-cni", "Addon/" + prefix + "-addon-aws-ebs-csi-driver",
		"Role/" + prefix + "-ebs-csi-role", "RolePolicyAttachment/" + prefix + "-ebs-csi-policy",
		"PodIdentityAssociation/" + prefix + "-ebs-csi-identity",
	} {
		if !slices.Contains(byStage[crossplaneaws.DeleteStageAddons], want) {
			t.Errorf("%s is not in the add-ons stage: %v", want, byStage[crossplaneaws.DeleteStageAddons])
		}
	}
	for _, want := range []string{"Cluster/" + prefix, "Role/" + prefix + "-cluster-role", "Role/" + prefix + "-node-role",
		"NodeGroup/" + crossplaneaws.DefaultNodeGroupName(prefix)} {
		if !slices.Contains(byStage[crossplaneaws.DeleteStageRest], want) {
			t.Errorf("%s is not in the last stage: %v", want, byStage[crossplaneaws.DeleteStageRest])
		}
	}
	for stage, names := range byStage {
		if stage == crossplaneaws.DeleteStageRest {
			continue
		}
		for _, n := range names {
			if n == "Cluster/"+prefix || n == "NodeGroup/"+crossplaneaws.DefaultNodeGroupName(prefix) {
				t.Errorf("%s is in stage %d", n, stage)
			}
		}
	}
}
