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

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// Defaults of a cluster spec.
const (
	DefaultAuthMode = "API"
	minZones        = 2
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

// ResolveSpec fills the spec's defaults (zones, auth mode, and those of the
// optional settings in use: ResolveOptions) so the stored spec says what
// was built. The public endpoint's CIDRs have no default: who may reach the
// API is the client's choice (the contract requires at least one while that
// endpoint is on). Node groups are not in the spec: the platform renders
// them from its config (ClusterPlan.System), or an imported cluster from
// what its import found (Adoption.NodeGroup).
func ResolveSpec(in *cloudv1.ClusterSpec, zones []string) (*cloudv1.ClusterSpec, error) {
	spec := cloneSpec(in)
	spec.Network.ZoneIds = zones
	aws := spec.GetAws()
	if aws.AccessConfig == nil {
		aws.AccessConfig = &cloudv1.AwsAccessConfig{}
	}
	if aws.AccessConfig.AuthenticationMode == cloudv1.AwsAuthenticationMode_AWS_AUTHENTICATION_MODE_UNSPECIFIED {
		aws.AccessConfig.AuthenticationMode = cloudv1.AwsAuthenticationMode_AWS_AUTHENTICATION_MODE_API
	}
	aws.Addons = MergeAddons(aws.GetAddons())
	ResolveReleaseFields(spec)
	return spec, ResolveOptions(spec)
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
	for _, a := range aws.GetAddons() {
		p.Addons = append(p.Addons, crossplaneaws.AddonPlan{Name: a.GetName(), Version: a.GetVersion(),
			ConfigurationValues: a.GetConfigurationValues(), S3BucketNames: a.GetS3BucketNames()})
	}
	optionsPlan(spec, &p)
	// A cluster the platform created has the platform's system group
	// (nodeGroups.system) and Karpenter's setup. An imported cluster (it has
	// adopted objects) keeps the node group its import found.
	if base.Imported() {
		if g := base.NodeGroup; g != nil {
			p.NodePool = *g
		}
		return p
	}
	p.NodePool = crossplaneaws.SystemNodePool(base.System)
	p.Karpenter = true
	p.ResultsBucket = true // rendered once the plan has the account (HasResultsBucket)
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
