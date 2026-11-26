package convert_test

import (
	"slices"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/convert"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// What a tenant needs for its own Karpenter pools reaches the kind's
// status: Karpenter's node role (its EC2NodeClass names it) and the tenant
// pools an upgrade left behind (a warning).
func TestClusterStatusIntoCarriesTheTenantPoolFacts(t *testing.T) {
	var status cloudv1alpha1.ClusterStatus
	arn := "arn:aws:iam::444455556666:role/prod-1a2b-karpenter-node"

	convert.ClusterStatusInto(&cloudv1.ClusterStatus{State: cloudv1.ClusterState_CLUSTER_STATE_UPDATING,
		Provider: &cloudv1.ClusterStatus_Aws{Aws: &cloudv1.AwsClusterStatus{KarpenterNodeRole: "prod-1a2b-karpenter-node", KarpenterNodeRoleArn: arn}},
		Upgrade:  &cloudv1.ClusterUpgrade{ToVersion: "1.37", TenantPoolsBehind: []string{"gpu (1.36)"}}}, "", metav1.Now(), &status)

	if aws := status.AWS; aws == nil || aws.KarpenterNodeRole != "prod-1a2b-karpenter-node" || aws.KarpenterNodeRoleARN != arn {
		t.Errorf("aws = %+v", aws)
	}
	if got := status.Upgrade.TenantPoolsBehind; !slices.Equal(got, []string{"gpu (1.36)"}) {
		t.Errorf("tenant pools behind = %v", got)
	}
}
