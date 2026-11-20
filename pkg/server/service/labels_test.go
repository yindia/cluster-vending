package service

import (
	"testing"

	"connectrpc.com/connect/v2"
)

func TestCheckAWSLabels(t *testing.T) {
	if err := CheckAWSLabels(map[string]string{"team": "x", "CostCenter": "1"}); err != nil {
		t.Errorf("ordinary labels refused: %v", err)
	}
	for _, key := range []string{"aws:foo", "AWS:Foo"} {
		if err := CheckAWSLabels(map[string]string{key: "x"}); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("%s: %v, want INVALID_ARGUMENT", key, err)
		}
	}
	// Keys the platform tags its resources with would be silently overridden.
	for _, key := range []string{"cluster-vending.io/tenant", "Cluster-Vending.io/x", "karpenter.sh/discovery",
		"kubernetes.io/cluster/prod", "k8s.io/cluster-autoscaler/enabled"} {
		if err := CheckAWSLabels(map[string]string{key: "x"}); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("%s: %v, want INVALID_ARGUMENT", key, err)
		}
	}
	for _, key := range []string{"Name", "name", "kubernetes.io/role", "team/Name"} {
		if err := CheckAWSLabels(map[string]string{key: "x"}); err != nil {
			t.Errorf("%s refused: %v", key, err)
		}
	}
}
