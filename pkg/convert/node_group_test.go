package convert_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/convert"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

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

// A Cluster object stored before the platform owned node groups still
// carries spec.defaultNodePool, spec.nodeGroups and spec.aws.defaultNodePool:
// they are not fields of the kind any more, so they are ignored on read and
// never come back, whatever they said.
func TestLegacyNodeGroupFieldsOfAStoredClusterAreIgnored(t *testing.T) {
	stored := []byte(`{"apiVersion":"cloud.cluster-vending.io/v1alpha1","kind":"Cluster","metadata":{"name":"c"},
	"spec":{"region":"us-east-1","network":{"networkRef":{"name":"n"}},
	"defaultNodePool":{"instanceTypes":["m6i.large"],"scaling":{"minSize":1,"maxSize":3}},
	"nodeGroups":[{"name":"batch","instanceFamilies":["c7g"]}],
	"aws":{"irsa":true,"defaultNodePool":{"amiType":"CUSTOM","launchTemplate":{"id":"lt-0123456789abcdef0","version":"3"}}}}}`)
	var obj cloudv1alpha1.Cluster
	if err := json.Unmarshal(stored, &obj); err != nil {
		t.Fatal(err)
	}
	spec := convert.ClusterSpecFromAPI(obj.Spec, "net-1", nil)
	if len(spec.ProtoReflect().GetUnknown()) != 0 || len(spec.GetAws().ProtoReflect().GetUnknown()) != 0 {
		t.Fatal("legacy fields must not survive as unknown fields")
	}
	want := &cloudv1.ClusterSpec{Region: "us-east-1", Network: &cloudv1.ClusterNetwork{NetworkId: "net-1"},
		Provider: &cloudv1.ClusterSpec_Aws{Aws: &cloudv1.AwsClusterSpec{Irsa: proto.Bool(true)}}}
	if !proto.Equal(spec, want) {
		t.Fatalf("decoded %v, want %v", spec, want)
	}
	again, _ := convert.ClusterSpecToAPI(spec, "n")
	raw, err := json.Marshal(again)
	if err != nil {
		t.Fatal(err)
	}
	for _, legacy := range []string{"defaultNodePool", "nodeGroups", "launchTemplate"} {
		if strings.Contains(string(raw), legacy) {
			t.Fatalf("%s came back: %s", legacy, raw)
		}
	}
	if !proto.Equal(convert.ClusterSpecFromAPI(again, "net-1", nil), want) {
		t.Fatal("the spec does not round-trip")
	}
}
