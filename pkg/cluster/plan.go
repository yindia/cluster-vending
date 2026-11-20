// Package cluster is the cluster logic that needs no API and no database:
// a spec's defaults and its render plan (ResolveSpec, RenderPlan), the
// plan of importing an existing EKS cluster (PlanImport), and what an
// update changes on rendered objects (FieldChanges). It takes plain
// values and returns plain errors; pkg/server/cluster maps them to the
// API.
package cluster

import (
	"errors"
	"fmt"
	"slices"
	"sort"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"k8s.io/utils/ptr"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// Defaults of a cluster spec.
const (
	DefaultAuthMode     = "API"
	DefaultCapacityType = "ON_DEMAND"
	DefaultAMIType      = "AL2023_x86_64_STANDARD"
	minZones            = 2
)

// PrivateSubnet is a private subnet of the network, as observed: its
// cloud ID, its zone ID and the name of its Subnet object.
type PrivateSubnet struct{ ID, Zone, Name string }

// PrivateSubnets are the network's private subnets that exist in the cloud
// (have an ID), from the network's managed resources.
func PrivateSubnets(views []*cloudv1.ManagedResource) []PrivateSubnet {
	var out []PrivateSubnet
	for _, v := range views {
		if v.GetKind() == "Subnet" && v.GetRole() == "private" && v.GetExternalName() != "" {
			out = append(out, PrivateSubnet{ID: v.GetExternalName(), Zone: v.GetZoneId(), Name: v.GetName()})
		}
	}
	return out
}

// SubnetObjectNames are the Subnet object names of ids, in order.
func SubnetObjectNames(subnets []PrivateSubnet, ids []string) []string {
	byID := make(map[string]string, len(subnets))
	for _, s := range subnets {
		byID[s.ID] = s.Name
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, byID[id])
	}
	return out
}

// ChooseSubnets returns the zones the cluster uses (zone_ids, or every zone
// with a private subnet) and their private subnet IDs. EKS needs two zones.
func ChooseSubnets(subnets []PrivateSubnet, zoneIDs []string) ([]string, []string, error) {
	available := map[string][]string{}
	for _, s := range subnets {
		available[s.Zone] = append(available[s.Zone], s.ID)
	}
	zones := slices.Clone(zoneIDs)
	if len(zones) == 0 {
		for z := range available {
			zones = append(zones, z)
		}
		sort.Strings(zones)
	}
	var ids []string
	for _, z := range zones {
		if len(available[z]) == 0 {
			return nil, nil, fmt.Errorf("the network has no private subnet in zone %s (yet)", z)
		}
		ids = append(ids, available[z]...)
	}
	if len(zones) < minZones {
		return nil, nil, fmt.Errorf("EKS needs private subnets in at least %d zones, got %d", minZones, len(zones))
	}
	return zones, ids, nil
}

// ResolveSpec fills the spec's defaults (zones, auth mode, node pool
// capacity and AMI, and those of the optional settings in use:
// ResolveOptions) so the stored spec says what was built. The public
// endpoint's CIDRs have no default: who may reach the API is the
// client's choice (the contract requires at least one while that endpoint
// is on).
//
// The default node pool is the platform's: omitted, it is filled with the
// default instance type and capacity; its labels, taints and size are
// always the fixed ones (crossplaneaws.FixedSystemNodePool), whatever the
// request said; the minimum and maximum size are the platform admin's (a
// render takes the admin's current ones, ClusterPlan.SystemPoolMin and
// SystemPoolMax; desired is the minimum at creation only). Instance types, capacity type, AMI, disk and launch
// template stay the client's.
func ResolveSpec(in *cloudv1.ClusterSpec, zones []string) (*cloudv1.ClusterSpec, error) {
	return ResolveSpecWith(in, zones, SystemPool{})
}

// SystemPool is the platform admin's settings of the default node group of
// a new cluster (--system-node-group-min-size and -max-size, --system-node-
// group-instance-type). Zero values, and a minimum below
// crossplaneaws.MinSystemPoolSize, are the defaults.
type SystemPool struct {
	MinSize, MaxSize int
	InstanceType     string
	// CapacityType ("ON_DEMAND", "SPOT") and DiskSizeGiB are used when the
	// request names none; empty and 0: ON_DEMAND and EKS's default.
	CapacityType string
	DiskSizeGiB  int
}

func (p SystemPool) instanceType() string {
	if p.InstanceType == "" {
		return crossplaneaws.DefaultSystemInstanceType
	}
	return p.InstanceType
}

// ResolveSpecWith is ResolveSpec with the admin's settings of the default
// node group: they apply to this (new) cluster only, never to one that
// exists.
func ResolveSpecWith(in *cloudv1.ClusterSpec, zones []string, pool SystemPool) (*cloudv1.ClusterSpec, error) {
	return resolveSpecWith(in, zones, true, pool)
}

// resolveSpec is ResolveSpec; fixPool false leaves the default node pool's
// placement and size as they are (an imported cluster's: PlanImport).
func resolveSpec(in *cloudv1.ClusterSpec, zones []string, fixPool bool) (*cloudv1.ClusterSpec, error) {
	return resolveSpecWith(in, zones, fixPool, SystemPool{})
}

func resolveSpecWith(in *cloudv1.ClusterSpec, zones []string, fixPool bool, system SystemPool) (*cloudv1.ClusterSpec, error) {
	spec := cloneSpec(in)
	spec.Network.ZoneIds = zones
	if fixPool {
		spec.DefaultNodePool = fixedNodePool(spec.DefaultNodePool, system)
	}
	pool := spec.DefaultNodePool
	if pool.CapacityType == cloudv1.CapacityType_CAPACITY_TYPE_UNSPECIFIED {
		pool.CapacityType = cloudv1.CapacityType_CAPACITY_TYPE_ON_DEMAND
	}
	aws := spec.GetAws()
	if aws.AccessConfig == nil {
		aws.AccessConfig = &cloudv1.AwsAccessConfig{}
	}
	if aws.AccessConfig.AuthenticationMode == cloudv1.AwsAuthenticationMode_AWS_AUTHENTICATION_MODE_UNSPECIFIED {
		aws.AccessConfig.AuthenticationMode = cloudv1.AwsAuthenticationMode_AWS_AUTHENTICATION_MODE_API
	}
	if aws.DefaultNodePool == nil {
		aws.DefaultNodePool = &cloudv1.AwsNodePoolSpec{}
	}
	if aws.DefaultNodePool.AmiType == cloudv1.AwsAmiType_AWS_AMI_TYPE_UNSPECIFIED {
		aws.DefaultNodePool.AmiType = cloudv1.AwsAmiType_AWS_AMI_TYPE_AL2023_X86_64_STANDARD
	}
	aws.Addons = MergeAddons(aws.GetAddons())
	ResolveReleaseFields(spec)
	return spec, ResolveOptions(spec)
}

// fixedNodePool is pool (nil: an empty one) with the platform's placement
// and size, and the admin's (default) instance type when it names none.
func fixedNodePool(pool *cloudv1.NodePoolSpec, system SystemPool) *cloudv1.NodePoolSpec {
	if pool == nil {
		pool = &cloudv1.NodePoolSpec{}
	}
	if len(pool.InstanceTypes) == 0 {
		pool.InstanceTypes = []string{system.instanceType()}
	}
	if pool.CapacityType == cloudv1.CapacityType_CAPACITY_TYPE_UNSPECIFIED && system.CapacityType == "SPOT" {
		pool.CapacityType = cloudv1.CapacityType_CAPACITY_TYPE_SPOT
	}
	if pool.DiskSizeGb == 0 && system.DiskSizeGiB > 0 {
		pool.DiskSizeGb = uint32(system.DiskSizeGiB) //nolint:gosec // validated: at most 16384
	}
	fixed := crossplaneaws.FixedSystemNodePool(crossplaneaws.NodePoolPlan{}, system.MinSize, system.MaxSize)
	pool.Labels = fixed.Labels
	pool.Taints = nil
	for _, t := range fixed.Taints {
		pool.Taints = append(pool.Taints, &cloudv1.Taint{Key: t.Key, Value: t.Value, Effect: cloudv1.TaintEffect_TAINT_EFFECT_NO_SCHEDULE})
	}
	pool.ScalingConfig = &cloudv1.ScalingConfig{MinSize: uint32(fixed.MinSize), MaxSize: uint32(fixed.MaxSize), //nolint:gosec // small, validated settings
		DesiredSize: ptr.To(uint32(fixed.Desired))} //nolint:gosec // the size at creation: the minimum
	return pool
}

// ResolveReleaseFields stores the defaults of the fields that say how
// platform releases treat the cluster. Tier is inherited from its network;
// an unset upgrade mode is manual (releases never move it). An unset release
// channel stays unset: it only means STABLE once the cluster is automatic.
func ResolveReleaseFields(spec *cloudv1.ClusterSpec) {
	spec.ProtoReflect().Clear(spec.ProtoReflect().Descriptor().Fields().ByName("tier"))
	if spec.GetUpgradeMode() == cloudv1.UpgradeMode_UPGRADE_MODE_UNSPECIFIED {
		spec.UpgradeMode = cloudv1.UpgradeMode_UPGRADE_MODE_MANUAL
	}
}

// ErrNoPublicAccessCIDRs: the spec does not say who may reach the API.
var ErrNoPublicAccessCIDRs = errors.New(
	"spec.control_plane.endpoint_access.public_access_cidrs needs at least one CIDR (0.0.0.0/0 for any)")

// MergeAddons is the default add-ons, each replaced by the entry of the
// same name in addons, followed by the other entries in their order.
func MergeAddons(addons []*cloudv1.AwsAddon) []*cloudv1.AwsAddon {
	byName := make(map[string]*cloudv1.AwsAddon, len(addons))
	for _, a := range addons {
		byName[a.GetName()] = a
	}
	out := make([]*cloudv1.AwsAddon, 0, len(crossplaneaws.DefaultAddons)+len(addons))
	for _, name := range crossplaneaws.DefaultAddons {
		if a, ok := byName[name]; ok {
			out = append(out, proto.CloneOf(a))
			delete(byName, name)
			continue
		}
		out = append(out, &cloudv1.AwsAddon{Name: name})
	}
	for _, a := range addons {
		if _, ok := byName[a.GetName()]; ok {
			out = append(out, proto.CloneOf(a))
		}
	}
	return out
}

// RenderPlan maps a resolved spec onto the AWS plan. base carries the
// namespace, names and account.
func RenderPlan(spec *cloudv1.ClusterSpec, subnetIDs []string, base crossplaneaws.ClusterPlan) crossplaneaws.ClusterPlan {
	p := base
	p.Region = spec.GetRegion()
	p.KubernetesVersion = spec.GetControlPlane().GetKubernetesVersion()
	p.SubnetIDs = subnetIDs
	p.PublicAccessCIDRs = spec.GetControlPlane().GetEndpointAccess().GetPublicAccessCidrs()
	p.ServiceCIDR = spec.GetNetwork().GetServiceCidr()
	p.AdminPrincipals = spec.GetAccess().GetAdminPrincipals()
	aws := spec.GetAws()
	p.AuthenticationMode = CrossplaneName(aws.GetAccessConfig().GetAuthenticationMode(), DefaultAuthMode)
	for _, t := range aws.GetEnabledClusterLogTypes() {
		p.LogTypes = append(p.LogTypes, CrossplaneName(t, ""))
	}
	if st := aws.GetUpgradePolicy().GetSupportType(); st != cloudv1.AwsSupportType_AWS_SUPPORT_TYPE_UNSPECIFIED {
		p.SupportType = CrossplaneName(st, "")
	}
	pool := spec.GetDefaultNodePool()
	sc := pool.GetScalingConfig()
	desired := sc.GetDesiredSize()
	if sc.DesiredSize == nil {
		desired = max(sc.GetMinSize(), 1)
	}
	p.NodePool = crossplaneaws.NodePoolPlan{
		InstanceTypes: pool.GetInstanceTypes(),
		CapacityType:  CrossplaneName(pool.GetCapacityType(), DefaultCapacityType),
		AMIType:       CrossplaneName(aws.GetDefaultNodePool().GetAmiType(), DefaultAMIType),
		DiskSizeGB:    int(pool.GetDiskSizeGb()),
		MinSize:       int(sc.GetMinSize()), MaxSize: int(sc.GetMaxSize()), Desired: int(desired),
		Labels: pool.GetLabels(),
	}
	for _, a := range aws.GetAddons() {
		p.Addons = append(p.Addons, crossplaneaws.AddonPlan{Name: a.GetName(), Version: a.GetVersion(),
			ConfigurationValues: a.GetConfigurationValues(), S3BucketNames: a.GetS3BucketNames()})
	}
	for _, t := range pool.GetTaints() {
		p.NodePool.Taints = append(p.NodePool.Taints, crossplaneaws.TaintPlan{Key: t.GetKey(), Value: t.GetValue(),
			Effect: CrossplaneName(t.GetEffect(), "")})
	}
	optionsPlan(spec, &p)
	// A cluster the platform created has the fixed default node group and
	// Karpenter's setup, whatever its stored spec says (a cluster created
	// before them is moved in place: nothing EKS cannot change changes,
	// min and max are the admin's current ones, desired is not touched). An imported cluster (it has
	// adopted objects) keeps what it has.
	if !base.Imported() {
		p.NodePool = crossplaneaws.FixedSystemNodePool(p.NodePool, base.SystemPoolMin, base.SystemPoolMax)
		p.Karpenter = true
		p.ResultsBucket = true // rendered once the plan has the account (HasResultsBucket)
	}
	return p
}

func cloneSpec(in *cloudv1.ClusterSpec) *cloudv1.ClusterSpec { return proto.CloneOf(in) }

// CrossplaneName is the provider-side spelling of an enum value, from its
// (cloud.v1.crossplane) option; def when it has none.
func CrossplaneName(e protoreflect.Enum, def string) string {
	value := e.Descriptor().Values().ByNumber(e.Number())
	if value == nil {
		return def
	}
	names, ok := proto.GetExtension(value.Options(), cloudv1.E_Crossplane).(*cloudv1.CrossplaneValues)
	if !ok || names.GetAws() == "" {
		return def
	}
	return names.GetAws()
}
