package cluster

import (
	"reflect"
	"testing"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

var zonesAB = []string{"use1-az1", "use1-az2"}

func assertSystemPool(t *testing.T, pool *cloudv1.NodePoolSpec) {
	t.Helper()
	sc := pool.GetScalingConfig()
	if sc.GetMinSize() != 2 || sc.GetMaxSize() != 2 || sc.DesiredSize == nil || sc.GetDesiredSize() != 2 {
		t.Errorf("scaling = %v, want 2/2/2", sc)
	}
	if !reflect.DeepEqual(pool.GetLabels(), map[string]string{"node.cluster-vending.io/pool": "system"}) {
		t.Errorf("labels = %v", pool.GetLabels())
	}
	if len(pool.GetTaints()) != 1 || pool.GetTaints()[0].GetKey() != "node.cluster-vending.io/pool" ||
		pool.GetTaints()[0].GetValue() != "system" || pool.GetTaints()[0].GetEffect() != cloudv1.TaintEffect_TAINT_EFFECT_NO_SCHEDULE {
		t.Errorf("taints = %v", pool.GetTaints())
	}
}

// A spec without default_node_pool gets the platform's: the default
// instance type, on-demand, the AMI of before, and the fixed placement.
func TestResolveSpecFillsAnOmittedDefaultNodePool(t *testing.T) {
	in := minimalSpec()
	in.DefaultNodePool = nil

	spec := mustResolve(t, in, zonesAB)

	pool := spec.GetDefaultNodePool()
	if !reflect.DeepEqual(pool.GetInstanceTypes(), []string{"t3.medium"}) {
		t.Errorf("instance types = %v", pool.GetInstanceTypes())
	}
	if pool.GetCapacityType() != cloudv1.CapacityType_CAPACITY_TYPE_ON_DEMAND ||
		spec.GetAws().GetDefaultNodePool().GetAmiType() != cloudv1.AwsAmiType_AWS_AMI_TYPE_AL2023_X86_64_STANDARD {
		t.Errorf("capacity %v, AMI %v", pool.GetCapacityType(), spec.GetAws().GetDefaultNodePool().GetAmiType())
	}
	assertSystemPool(t, pool)
	if in.DefaultNodePool != nil {
		t.Error("the input was changed")
	}
}

// The placement and the size are the platform's, whatever the request
// said; what EKS cannot change in place stays the client's.
func TestResolveSpecFixesWhatThePlatformOwns(t *testing.T) {
	in := minimalSpec()
	in.DefaultNodePool.InstanceTypes = []string{"c7i.xlarge"}
	in.DefaultNodePool.CapacityType = cloudv1.CapacityType_CAPACITY_TYPE_SPOT
	in.DefaultNodePool.Labels = map[string]string{"team": "x"}
	in.DefaultNodePool.Taints = []*cloudv1.Taint{{Key: "gpu", Effect: cloudv1.TaintEffect_TAINT_EFFECT_NO_EXECUTE}}

	spec := mustResolve(t, in, zonesAB)

	assertSystemPool(t, spec.GetDefaultNodePool())
	if !reflect.DeepEqual(spec.GetDefaultNodePool().GetInstanceTypes(), []string{"c7i.xlarge"}) ||
		spec.GetDefaultNodePool().GetCapacityType() != cloudv1.CapacityType_CAPACITY_TYPE_SPOT {
		t.Errorf("instance types and capacity type must stay the client's: %v", spec.GetDefaultNodePool())
	}
}

// An existing cluster's stored spec still says its old labels, taints and
// scaling: the plan renders the platform's, in place (the node group keeps
// its name), and the cluster gets Karpenter's setup.
func TestRenderPlanFixesTheDefaultNodeGroupOfAnExistingCluster(t *testing.T) {
	stored := minimalSpec()
	stored.DefaultNodePool.Labels = map[string]string{"role": "system"}
	stored.DefaultNodePool.Taints = []*cloudv1.Taint{{Key: "dedicated", Value: "system", Effect: cloudv1.TaintEffect_TAINT_EFFECT_NO_SCHEDULE}}
	stored.DefaultNodePool.ScalingConfig.DesiredSize = ptrUint32(3)
	spec := mustResolveWithout(t, stored)
	base := crossplaneawsBase()

	plan := RenderPlan(spec, []string{"subnet-a"}, base)

	want := crossplaneaws.FixedSystemNodePool(crossplaneaws.NodePoolPlan{InstanceTypes: []string{"m6i.large"}, CapacityType: "ON_DEMAND",
		AMIType: "AL2023_x86_64_STANDARD"})
	if !reflect.DeepEqual(plan.NodePool, want) {
		t.Errorf("node pool = %+v\nwant %+v", plan.NodePool, want)
	}
	if !plan.Karpenter {
		t.Error("a platform cluster has Karpenter's setup")
	}
	old := crossplaneaws.NodePoolPlan{InstanceTypes: []string{"m6i.large"}, CapacityType: "ON_DEMAND", AMIType: "AL2023_x86_64_STANDARD"}
	if crossplaneaws.ReplacementNodeGroupName("p", old) != crossplaneaws.ReplacementNodeGroupName("p", plan.NodePool) {
		t.Error("moving to the fixed pool would replace the node group")
	}
}

// An imported cluster keeps what it was imported with: its group, its
// labels and taints, no platform setup.
func TestRenderPlanLeavesAnImportedClusterAlone(t *testing.T) {
	stored := minimalSpec()
	stored.DefaultNodePool.Labels = map[string]string{"role": "system"}
	spec := mustResolveWithout(t, stored)
	base := crossplaneawsBase()
	base.ExternalNames = map[string]string{"prod-12345678-default": "ng-1"}

	plan := RenderPlan(spec, []string{"subnet-a"}, base)

	if plan.Karpenter || !reflect.DeepEqual(plan.NodePool.Labels, map[string]string{"role": "system"}) ||
		plan.NodePool.MinSize != 2 || plan.NodePool.MaxSize != 4 || len(plan.NodePool.Taints) != 0 {
		t.Errorf("imported plan = karpenter %v, pool %+v", plan.Karpenter, plan.NodePool)
	}
}

// ResolveSpec is for new clusters; the import path derives the spec from
// the cluster and keeps its group.
func TestImportKeepsTheNodeGroupItFound(t *testing.T) {
	in := minimalSpec()
	in.DefaultNodePool.Labels = map[string]string{"role": "system"}

	spec, err := resolveSpec(in, zonesAB, false)
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(spec.GetDefaultNodePool().GetLabels(), map[string]string{"role": "system"}) || spec.GetDefaultNodePool().GetScalingConfig().GetMaxSize() != 4 {
		t.Errorf("import pool = %v", spec.GetDefaultNodePool())
	}
}

// mustResolveWithout is what a cluster created before the fixed pool has
// stored: a spec resolved without fixing the pool.
func mustResolveWithout(t *testing.T, in *cloudv1.ClusterSpec) *cloudv1.ClusterSpec {
	t.Helper()
	spec, err := resolveSpec(in, zonesAB, false)
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

func ptrUint32(v uint32) *uint32 { return &v }

// A new cluster's default group is t3.medium; a cluster whose stored spec
// says m6i.large (the old default, stored explicitly at creation) keeps it
// through every resolve and render: no node group replacement.
func TestExistingClustersKeepTheirStoredInstanceType(t *testing.T) {
	if crossplaneaws.DefaultSystemInstanceType != "t3.medium" {
		t.Fatalf("default = %s", crossplaneaws.DefaultSystemInstanceType)
	}
	stored := minimalSpec()
	stored.DefaultNodePool.InstanceTypes = []string{"m6i.large"}

	spec := mustResolve(t, stored, zonesAB)
	plan := RenderPlan(spec, []string{"subnet-a"}, crossplaneawsBase())

	if !reflect.DeepEqual(spec.GetDefaultNodePool().GetInstanceTypes(), []string{"m6i.large"}) ||
		!reflect.DeepEqual(plan.NodePool.InstanceTypes, []string{"m6i.large"}) {
		t.Errorf("stored m6i.large became %v / %v", spec.GetDefaultNodePool().GetInstanceTypes(), plan.NodePool.InstanceTypes)
	}
	old := crossplaneaws.NodePoolPlan{InstanceTypes: []string{"m6i.large"}, CapacityType: "ON_DEMAND", AMIType: "AL2023_x86_64_STANDARD"}
	if crossplaneaws.ReplacementNodeGroupName("p", old) != crossplaneaws.ReplacementNodeGroupName("p", plan.NodePool) {
		t.Error("the node group would be replaced")
	}
}
