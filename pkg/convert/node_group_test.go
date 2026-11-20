package convert_test

import (
	"testing"

	"reflect"

	"google.golang.org/protobuf/proto"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/convert"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

func sampleNodeGroups() []*cloudv1.NodeGroupSpec {
	return []*cloudv1.NodeGroupSpec{
		{
			Name:             "batch",
			InstanceTypes:    []string{"m7i.large"},
			InstanceFamilies: []string{"c7g"},
			Architectures:    []cloudv1.Architecture{cloudv1.Architecture_ARCHITECTURE_AMD64, cloudv1.Architecture_ARCHITECTURE_ARM64},
			CapacityTypes:    []cloudv1.CapacityType{cloudv1.CapacityType_CAPACITY_TYPE_ON_DEMAND, cloudv1.CapacityType_CAPACITY_TYPE_SPOT},
			Labels:           map[string]string{"team": "data"},
			Taints:           []*cloudv1.Taint{{Key: "dedicated", Value: "batch", Effect: cloudv1.TaintEffect_TAINT_EFFECT_NO_SCHEDULE}},
			Limits:           &cloudv1.NodeGroupLimits{Cpu: ptr.To(int32(200)), MemoryGib: ptr.To(int32(800))},
		},
		{Name: "bare"},
	}
}

func TestClusterSpecNodeGroupsRoundTrip(t *testing.T) {
	in := &cloudv1.ClusterSpec{NodeGroups: sampleNodeGroups()}
	api, rem := convert.ClusterSpecToAPI(in, "net")
	if len(api.NodeGroups) != 2 || api.NodeGroups[0].Architectures[1] != cloudv1alpha1.ArchitectureArm64 ||
		api.NodeGroups[0].CapacityTypes[1] != cloudv1alpha1.CapacityTypeSpot ||
		api.NodeGroups[0].Taints[0].Effect != cloudv1alpha1.TaintEffect("NoSchedule") {
		t.Fatalf("to API: %+v", api.NodeGroups)
	}
	out := convert.ClusterSpecFromAPI(*api.DeepCopy(), "id", rem)
	if len(out.NodeGroups) != 2 {
		t.Fatalf("round trip: %v", out.NodeGroups)
	}
	for i := range in.NodeGroups {
		if !proto.Equal(in.NodeGroups[i], out.NodeGroups[i]) {
			t.Fatalf("round trip %d: %v != %v", i, in.NodeGroups[i], out.NodeGroups[i])
		}
	}
}

func TestClusterSpecNoNodeGroupsStaysNil(t *testing.T) {
	api, rem := convert.ClusterSpecToAPI(&cloudv1.ClusterSpec{}, "net")
	if api.NodeGroups != nil || api.DefaultNodePool != nil {
		t.Fatalf("expected nil: %+v", api)
	}
	if out := convert.ClusterSpecFromAPI(api, "id", rem); out.NodeGroups != nil || out.DefaultNodePool != nil {
		t.Fatalf("expected nil: %v", out)
	}
}

func TestNodeGroupLimitsNilVersusZeroPresence(t *testing.T) {
	in := &cloudv1.ClusterSpec{NodeGroups: []*cloudv1.NodeGroupSpec{
		{Name: "a", Limits: &cloudv1.NodeGroupLimits{Cpu: ptr.To(int32(4))}},
	}}
	api, rem := convert.ClusterSpecToAPI(in, "net")
	if api.NodeGroups[0].Limits.MemoryGiB != nil {
		t.Fatal("memory limit must stay unset")
	}
	out := convert.ClusterSpecFromAPI(api, "id", rem)
	if out.NodeGroups[0].Limits.MemoryGib != nil || out.NodeGroups[0].Limits.GetCpu() != 4 {
		t.Fatalf("limits: %v", out.NodeGroups[0].Limits)
	}
}

func TestClusterStatusIntoNodeGroups(t *testing.T) {
	p := &cloudv1.ClusterStatus{NodeGroups: []*cloudv1.NodeGroupStatus{
		{Name: "platform", PlatformManaged: true, Phase: cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_READY, Nodes: 3},
		{Name: "batch", Phase: cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_FAILED, Message: "no capacity"},
		{Name: "old", Phase: cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_DELETING},
		{Name: "new", Phase: cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_PENDING},
		{Name: "quiet", Phase: cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_READY, Nodes: 2, NodesNotice: "the node list is unavailable"},
	}}
	var into cloudv1alpha1.ClusterStatus
	convert.ClusterStatusInto(p, "", metav1.Time{}, &into)
	want := []cloudv1alpha1.NodeGroupStatus{
		{Name: "platform", PlatformManaged: true, Phase: cloudv1alpha1.NodeGroupPhaseReady, Nodes: 3},
		{Name: "batch", Phase: cloudv1alpha1.NodeGroupPhaseFailed, Message: "no capacity"},
		{Name: "old", Phase: cloudv1alpha1.NodeGroupPhaseDeleting},
		{Name: "new", Phase: cloudv1alpha1.NodeGroupPhasePending},
		{Name: "quiet", Phase: cloudv1alpha1.NodeGroupPhaseReady, Nodes: 2, NodesNotice: "the node list is unavailable"},
	}
	if !reflect.DeepEqual(want, into.NodeGroups) {
		t.Fatalf("got %+v", into.NodeGroups)
	}
	back := convert.NodeGroupStatusesFromAPI(into.NodeGroups)
	for i := range p.NodeGroups {
		if !proto.Equal(p.NodeGroups[i], back[i]) {
			t.Fatalf("from API %d: %v != %v", i, p.NodeGroups[i], back[i])
		}
	}
	convert.ClusterStatusInto(&cloudv1.ClusterStatus{}, "", metav1.Time{}, &into)
	if into.NodeGroups != nil {
		t.Fatal("status node groups must clear")
	}
}
