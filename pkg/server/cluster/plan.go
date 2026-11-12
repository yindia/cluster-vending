package cluster

import (
	"errors"
	"fmt"
	"slices"
	"sort"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// Defaults of a cluster spec.
const (
	defaultAuthMode     = "API"
	defaultCapacityType = "ON_DEMAND"
	defaultAMIType      = "AL2023_x86_64_STANDARD"
	minZones            = 2
)

// privateSubnet is a private subnet of the network, as observed.
type privateSubnet struct{ id, zone, name string }

// privateSubnets are the network's private subnets that exist in the cloud
// (have an ID), from the network's managed resources.
func privateSubnets(views []*cloudv1.ManagedResource) []privateSubnet {
	var out []privateSubnet
	for _, v := range views {
		if v.GetKind() == "Subnet" && v.GetRole() == "private" && v.GetExternalName() != "" {
			out = append(out, privateSubnet{id: v.GetExternalName(), zone: v.GetZoneId(), name: v.GetName()})
		}
	}
	return out
}

// chooseSubnets returns the zones the cluster uses (zone_ids, or every zone
// with a private subnet) and their private subnet IDs. EKS needs two zones.
func chooseSubnets(subnets []privateSubnet, zoneIDs []string) ([]string, []string, error) {
	available := map[string][]string{}
	for _, s := range subnets {
		available[s.zone] = append(available[s.zone], s.id)
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

// resolveSpec fills the spec's defaults (zones, auth mode, node pool
// capacity and AMI, and those of the optional settings in use:
// resolveOptions) so the stored spec says what was built. The public
// endpoint's CIDRs have no default: who may reach the API is the
// client's choice (the contract requires at least one while that endpoint
// is on).
func resolveSpec(in *cloudv1.ClusterSpec, zones []string) (*cloudv1.ClusterSpec, error) {
	spec := cloneSpec(in)
	spec.Network.ZoneIds = zones
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
	aws.Addons = mergeAddons(aws.GetAddons())
	return spec, resolveOptions(spec)
}

// errNoPublicAccessCIDRs: the spec does not say who may reach the API.
var errNoPublicAccessCIDRs = errors.New(
	"spec.control_plane.endpoint_access.public_access_cidrs needs at least one CIDR (0.0.0.0/0 for any)")

// mergeAddons is the default add-ons, each replaced by the entry of the
// same name in addons, followed by the other entries in their order.
func mergeAddons(addons []*cloudv1.AwsAddon) []*cloudv1.AwsAddon {
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

// clusterPlan maps a resolved spec onto the AWS plan. base carries the
// namespace, names and account.
func clusterPlan(spec *cloudv1.ClusterSpec, subnetIDs []string, base crossplaneaws.ClusterPlan) crossplaneaws.ClusterPlan {
	p := base
	p.Region = spec.GetRegion()
	p.KubernetesVersion = spec.GetControlPlane().GetKubernetesVersion()
	p.SubnetIDs = subnetIDs
	p.PublicAccessCIDRs = spec.GetControlPlane().GetEndpointAccess().GetPublicAccessCidrs()
	p.ServiceCIDR = spec.GetNetwork().GetServiceCidr()
	p.AdminPrincipals = spec.GetAccess().GetAdminPrincipals()
	aws := spec.GetAws()
	p.AuthenticationMode = crossplaneName(aws.GetAccessConfig().GetAuthenticationMode(), defaultAuthMode)
	for _, t := range aws.GetEnabledClusterLogTypes() {
		p.LogTypes = append(p.LogTypes, crossplaneName(t, ""))
	}
	if st := aws.GetUpgradePolicy().GetSupportType(); st != cloudv1.AwsSupportType_AWS_SUPPORT_TYPE_UNSPECIFIED {
		p.SupportType = crossplaneName(st, "")
	}
	pool := spec.GetDefaultNodePool()
	sc := pool.GetScalingConfig()
	desired := sc.GetDesiredSize()
	if sc.DesiredSize == nil {
		desired = max(sc.GetMinSize(), 1)
	}
	p.NodePool = crossplaneaws.NodePoolPlan{
		InstanceTypes: pool.GetInstanceTypes(),
		CapacityType:  crossplaneName(pool.GetCapacityType(), defaultCapacityType),
		AMIType:       crossplaneName(aws.GetDefaultNodePool().GetAmiType(), defaultAMIType),
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
			Effect: crossplaneName(t.GetEffect(), "")})
	}
	optionsPlan(spec, &p)
	return p
}

func cloneSpec(in *cloudv1.ClusterSpec) *cloudv1.ClusterSpec { return proto.CloneOf(in) }

// crossplaneName is the provider-side spelling of an enum value, from its
// (cloud.v1.crossplane) option; def when it has none.
func crossplaneName(e protoreflect.Enum, def string) string {
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
