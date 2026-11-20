package cli

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

const nodeGroupsManifest = clusterHead + `spec:
  region: us-west-1
  nodeGroups:
    - name: batch
      instanceFamilies: [m7i, c7g]
      architectures: [ARCHITECTURE_ARM64]
      capacityTypes: [CAPACITY_TYPE_SPOT, CAPACITY_TYPE_ON_DEMAND]
      labels: {team: data}
      taints:
        - {key: dedicated, value: batch, effect: TAINT_EFFECT_NO_SCHEDULE}
      limits: {cpu: 200, memoryGib: 800}
    - name: web
`

func TestClusterManifestCarriesNodeGroups(t *testing.T) {
	m, err := LoadManifest(writeManifest(t, nodeGroupsManifest), KindCluster)
	if err != nil {
		t.Fatal(err)
	}
	cl, err := m.Cluster()
	if err != nil {
		t.Fatal(err)
	}
	groups := cl.GetSpec().GetNodeGroups()
	if len(groups) != 2 || groups[0].GetName() != "batch" || groups[1].GetName() != "web" {
		t.Fatalf("node groups = %v", groups)
	}
	batch := groups[0]
	if batch.GetArchitectures()[0] != cloudv1.Architecture_ARCHITECTURE_ARM64 ||
		batch.GetCapacityTypes()[0] != cloudv1.CapacityType_CAPACITY_TYPE_SPOT ||
		batch.GetLimits().GetCpu() != 200 || batch.GetLimits().MemoryGib == nil ||
		batch.GetTaints()[0].GetEffect() != cloudv1.TaintEffect_TAINT_EFFECT_NO_SCHEDULE {
		t.Errorf("batch = %v", batch)
	}
}

func TestClusterUpdateSendsNodeGroupChangesAsOneMaskPath(t *testing.T) {
	current := storedCluster()
	current.Spec.NodeGroups = []*cloudv1.NodeGroupSpec{{Name: "old"}, {Name: "web"}}
	api := &updateCluster{current: current, plan: &cloudv1.Plan{PlanId: "p1", Applicable: true, Changes: []*cloudv1.PlannedChange{
		{Kind: "NodePool", Name: "batch", Action: cloudv1.PlannedAction_PLANNED_ACTION_CREATE, Detail: "node group batch is added"},
		{Kind: "NodePool", Name: "old", Action: cloudv1.PlannedAction_PLANNED_ACTION_DELETE, Detail: "node group old is removed"},
	}}}
	var out, progress bytes.Buffer

	err := Run(t.Context(), ClusterUpdate, Clients{Cluster: api},
		Options{Input: writeManifest(t, nodeGroupsManifest), ID: inferCluster, Plan: true, Progress: &progress}, &out, OutputTable)
	if err != nil {
		t.Fatalf("%v\n%s", err, progress.String())
	}
	if got := api.updated.GetUpdateMask().GetPaths(); !slices.Equal(got, []string{"spec.node_groups"}) {
		t.Fatalf("mask = %v, want only spec.node_groups", got)
	}
	if got := api.updated.GetCluster().GetSpec().GetNodeGroups(); len(got) != 2 || got[0].GetName() != "batch" {
		t.Errorf("request node groups = %v", got)
	}
	if !api.updated.GetValidateOnly() {
		t.Error("--plan must not apply")
	}
	for _, want := range []string{"spec.node_groups:", "batch"} {
		if !strings.Contains(progress.String(), want) {
			t.Errorf("progress %q lacks %q", progress.String(), want)
		}
	}
	for _, want := range []string{"NodePool", "batch", "old", "CREATE", "DELETE"} {
		if !strings.Contains(strings.ToUpper(out.String()), strings.ToUpper(want)) {
			t.Errorf("plan output %q lacks %q", out.String(), want)
		}
	}
}

func TestClusterUpdateWithUnchangedNodeGroupsChangesNothing(t *testing.T) {
	m, err := LoadManifest(writeManifest(t, nodeGroupsManifest), KindCluster)
	if err != nil {
		t.Fatal(err)
	}
	cl, err := m.Cluster()
	if err != nil {
		t.Fatal(err)
	}
	current := storedCluster()
	current.Spec.NodeGroups = proto.CloneOf(cl).GetSpec().GetNodeGroups()
	api := &updateCluster{current: current}
	var out, progress bytes.Buffer

	err = Run(t.Context(), ClusterUpdate, Clients{Cluster: api},
		Options{Input: writeManifest(t, nodeGroupsManifest), ID: inferCluster, Progress: &progress}, &out, OutputTable)
	if err != nil || api.updated != nil || !strings.Contains(progress.String(), "nothing to change") {
		t.Errorf("err %v, updated %v, progress %q", err, api.updated, progress.String())
	}
}

func TestClusterUpdateRefusesChangesToTheDefaultNodeGroup(t *testing.T) {
	current := storedCluster()
	current.Spec.DefaultNodePool = &cloudv1.NodePoolSpec{InstanceTypes: []string{"t3.medium"},
		ScalingConfig: &cloudv1.ScalingConfig{MinSize: 2, MaxSize: 2}}
	api := &updateCluster{current: current}
	manifest := clusterHead + `spec:
  region: us-west-1
  defaultNodePool:
    instanceTypes: [t3.medium]
    scalingConfig: {minSize: 2, maxSize: 5}
    labels: {a: b}
`
	var out, progress bytes.Buffer

	err := Run(t.Context(), ClusterUpdate, Clients{Cluster: api},
		Options{Input: writeManifest(t, manifest), ID: inferCluster, Progress: &progress}, &out, OutputTable)
	if err == nil || !strings.Contains(err.Error(), "the default node group is managed by the platform") {
		t.Fatalf("err = %v, want the default node group refusal", err)
	}
	if api.updated != nil {
		t.Error("nothing may be sent")
	}
}

func TestDescribeClusterListsNodeGroups(t *testing.T) {
	c := storedCluster()
	c.Status = &cloudv1.ClusterStatus{NodeGroups: []*cloudv1.NodeGroupStatus{
		{Name: "platform", PlatformManaged: true, Phase: cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_READY, Nodes: 3},
		{Name: "batch", Phase: cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_FAILED, Message: "no capacity"},
	}}
	var out bytes.Buffer
	if err := describeCluster(&out, c); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Node Groups:", "platform", "batch", "Ready", "Failed", "no capacity"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("describe lacks %q:\n%s", want, out.String())
		}
	}
	c.Status = &cloudv1.ClusterStatus{}
	out.Reset()
	if err := describeCluster(&out, c); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "Node Groups") {
		t.Error("no node groups, no section")
	}
}
