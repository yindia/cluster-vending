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
	assertSystemSize(t, sc, 2, 6)
	if !reflect.DeepEqual(pool.GetLabels(), map[string]string{"node.cluster-vending.io/pool": "system"}) {
		t.Errorf("labels = %v", pool.GetLabels())
	}
	if len(pool.GetTaints()) != 1 || pool.GetTaints()[0].GetKey() != "node.cluster-vending.io/pool" ||
		pool.GetTaints()[0].GetValue() != "system" || pool.GetTaints()[0].GetEffect() != cloudv1.TaintEffect_TAINT_EFFECT_NO_SCHEDULE {
		t.Errorf("taints = %v", pool.GetTaints())
	}
}

// assertSystemSize: min and max as given, desired = min (the size at creation).
func assertSystemSize(t *testing.T, sc *cloudv1.ScalingConfig, minSize, maxSize uint32) {
	t.Helper()
	if sc.GetMinSize() != minSize || sc.GetMaxSize() != maxSize || sc.DesiredSize == nil || sc.GetDesiredSize() != minSize {
		t.Errorf("scaling = %v, want min %d max %d desired %d", sc, minSize, maxSize, minSize)
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
		AMIType: "AL2023_x86_64_STANDARD"}, 2, 6)
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

// The admin's settings apply to a new cluster: its instance type (unless
// the request names one) and its size, which is stored too.
func TestResolveSpecWithTheAdminsSystemNodeGroupSettings(t *testing.T) {
	in := minimalSpec()
	in.DefaultNodePool = nil

	spec, err := ResolveSpecWith(in, zonesAB, SystemPool{MinSize: 5, MaxSize: 9, InstanceType: "m7i.large"})
	if err != nil {
		t.Fatal(err)
	}

	if got := spec.GetDefaultNodePool().GetInstanceTypes(); !reflect.DeepEqual(got, []string{"m7i.large"}) {
		t.Errorf("instance types = %v, want the admin's", got)
	}
	assertSystemSize(t, spec.GetDefaultNodePool().GetScalingConfig(), 5, 9)

	named := minimalSpec()
	named.DefaultNodePool.InstanceTypes = []string{"c7i.xlarge"}
	spec, err = ResolveSpecWith(named, zonesAB, SystemPool{MinSize: 4, InstanceType: "m7i.large"})
	if err != nil {
		t.Fatal(err)
	}
	if got := spec.GetDefaultNodePool().GetInstanceTypes(); !reflect.DeepEqual(got, []string{"c7i.xlarge"}) {
		t.Errorf("the request's instance type was replaced: %v", got)
	}

	// Unset (or below the minimum): the defaults.
	spec, err = ResolveSpecWith(in, zonesAB, SystemPool{MinSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	assertSystemSize(t, spec.GetDefaultNodePool().GetScalingConfig(), 2, 6)
	if got := spec.GetDefaultNodePool().GetInstanceTypes(); !reflect.DeepEqual(got, []string{"t3.medium"}) {
		t.Errorf("instance types = %v", got)
	}
}

// A cluster created with the former layout stores t3.medium and a fixed 2:
// the admin's min and max move it in place (the render takes the plan's
// bounds), desired is the minimum, which the apply sends at creation only;
// its instance type, capacity and AMI stay, so the node group keeps its
// name and is not replaced, and a changed instance type default never
// reaches it.
func TestSystemNodeGroupSizeChangesInPlaceOnAnExistingCluster(t *testing.T) {
	stored := minimalSpec()
	stored.DefaultNodePool.InstanceTypes = []string{"t3.medium"}
	stored.DefaultNodePool.ScalingConfig = &cloudv1.ScalingConfig{MinSize: 2, MaxSize: 2, DesiredSize: ptrUint32(2)}
	spec := mustResolveWithout(t, stored)
	base := crossplaneawsBase()
	before := RenderPlan(spec, []string{"subnet-a"}, base)
	base.SystemPoolMin, base.SystemPoolMax = 4, 9

	after := RenderPlan(spec, []string{"subnet-a"}, base)

	if before.NodePool.MinSize != 2 || before.NodePool.MaxSize != 6 || after.NodePool.MinSize != 4 || after.NodePool.MaxSize != 9 || after.NodePool.Desired != 4 {
		t.Errorf("sizes before %+v after %+v, want the plan's bounds (default 2..6, then 4..9)", before.NodePool, after.NodePool)
	}
	if !reflect.DeepEqual(after.NodePool.InstanceTypes, []string{"t3.medium"}) || after.NodePool.CapacityType != "ON_DEMAND" || after.NodePool.AMIType != "AL2023_x86_64_STANDARD" {
		t.Errorf("the stored node group settings changed: %+v", after.NodePool)
	}
	old := crossplaneaws.NodePoolPlan{InstanceTypes: []string{"t3.medium"}, CapacityType: "ON_DEMAND", AMIType: "AL2023_x86_64_STANDARD"}
	if crossplaneaws.ReplacementNodeGroupName("p", old) != crossplaneaws.ReplacementNodeGroupName("p", after.NodePool) {
		t.Error("a size change would replace the node group")
	}
}

// The admin's capacity type and disk size are defaults of a new cluster's
// system group too; the request wins.
func TestResolveSpecWithSystemCapacityTypeAndDisk(t *testing.T) {
	in := minimalSpec()
	in.DefaultNodePool = nil
	spec, err := ResolveSpecWith(in, zonesAB, SystemPool{CapacityType: "SPOT", DiskSizeGiB: 50})
	if err != nil {
		t.Fatal(err)
	}
	if p := spec.GetDefaultNodePool(); p.GetCapacityType() != cloudv1.CapacityType_CAPACITY_TYPE_SPOT || p.GetDiskSizeGb() != 50 {
		t.Errorf("capacity %v disk %d", p.GetCapacityType(), p.GetDiskSizeGb())
	}
	named := minimalSpec()
	named.DefaultNodePool.CapacityType = cloudv1.CapacityType_CAPACITY_TYPE_ON_DEMAND
	named.DefaultNodePool.DiskSizeGb = 80
	spec, err = ResolveSpecWith(named, zonesAB, SystemPool{CapacityType: "SPOT", DiskSizeGiB: 50})
	if err != nil {
		t.Fatal(err)
	}
	if p := spec.GetDefaultNodePool(); p.GetCapacityType() != cloudv1.CapacityType_CAPACITY_TYPE_ON_DEMAND || p.GetDiskSizeGb() != 80 {
		t.Errorf("the request was replaced: %v %d", p.GetCapacityType(), p.GetDiskSizeGb())
	}
}

// The launch template owns the disk: the admin's default disk size must not
// turn a request without disk_size_gb into an invalid one.
func TestResolveSpecWithSystemDiskAndLaunchTemplate(t *testing.T) {
	withTemplate := func() *cloudv1.ClusterSpec {
		in := minimalSpec()
		in.DefaultNodePool.DiskSizeGb = 0
		in.GetAws().DefaultNodePool = &cloudv1.AwsNodePoolSpec{LaunchTemplate: &cloudv1.AwsLaunchTemplate{Id: "lt-0123456789abcdef0", Version: "1"}}
		return in
	}
	spec, err := ResolveSpecWith(withTemplate(), zonesAB, SystemPool{DiskSizeGiB: 50})
	if err != nil {
		t.Fatalf("admin disk default must not apply with a launch template: %v", err)
	}
	if got := spec.GetDefaultNodePool().GetDiskSizeGb(); got != 0 {
		t.Errorf("disk_size_gb = %d, want 0", got)
	}

	set := withTemplate()
	set.DefaultNodePool.DiskSizeGb = 80
	if _, err := ResolveSpecWith(set, zonesAB, SystemPool{DiskSizeGiB: 50}); err == nil {
		t.Error("a user-set disk_size_gb with a launch template must still be refused")
	}
}
