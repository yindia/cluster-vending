package updatemask_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/updatemask"
)

func TestDiffTreatsNodeGroupsAsOnePath(t *testing.T) {
	current := &cloudv1.Cluster{Spec: &cloudv1.ClusterSpec{NodeGroups: []*cloudv1.NodeGroupSpec{{Name: "a"}}}}
	desired := &cloudv1.Cluster{Spec: &cloudv1.ClusterSpec{NodeGroups: []*cloudv1.NodeGroupSpec{{Name: "a"}, {Name: "b"}}}}

	changes, err := updatemask.Diff(current, desired, updatemask.Cluster)

	if err != nil || !slices.Equal(updatemask.Paths(changes), []string{"spec.node_groups"}) {
		t.Fatalf("changes %v, err %v; want spec.node_groups", changes, err)
	}
	same, err := updatemask.Diff(desired, desired, updatemask.Cluster)
	if err != nil || len(same) != 0 {
		t.Fatalf("unchanged list: %v, %v", same, err)
	}
}

func TestDiffRefusesTheDefaultNodeGroupsManagedFields(t *testing.T) {
	current := &cloudv1.Cluster{Spec: &cloudv1.ClusterSpec{DefaultNodePool: &cloudv1.NodePoolSpec{
		ScalingConfig: &cloudv1.ScalingConfig{MinSize: 2, MaxSize: 2}}}}
	for name, pool := range map[string]*cloudv1.NodePoolSpec{
		"taints":  {Taints: []*cloudv1.Taint{{Key: "k", Effect: cloudv1.TaintEffect_TAINT_EFFECT_NO_SCHEDULE}}},
		"labels":  {Labels: map[string]string{"a": "b"}},
		"scaling": {ScalingConfig: &cloudv1.ScalingConfig{MinSize: 2, MaxSize: 9}},
	} {
		t.Run(name, func(t *testing.T) {
			desired := &cloudv1.Cluster{Spec: &cloudv1.ClusterSpec{DefaultNodePool: pool}}
			_, err := updatemask.Diff(current, desired, updatemask.Cluster)
			var fixed *updatemask.FixedError
			if !errors.As(err, &fixed) || !strings.Contains(err.Error(), "the default node group is managed by the platform") {
				t.Fatalf("err = %v, want a FixedError naming the platform", err)
			}
		})
	}
}
