package config

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// The platform admin's node group layout (docs/design/karpenter-node-groups.md).
// It is the `nodeGroups` section of the config file, the same for the
// server and the controller:
//
//	nodeGroups:
//	  system:                     # the default EKS managed node group
//	    instanceType: t3.medium   # new clusters only; existing ones keep theirs
//	    size: 2                   # desired = min = max; at least 2
//	    capacityType: ON_DEMAND
//	    diskSizeGiB: 0            # 0: EKS's default
//	  karpenter:                  # what users may create as their own groups
//	    instanceFamilies: [t3]
//	    instanceTypes: []
//	    defaults:                 # for a user group that leaves them unset
//	      architectures: [AMD64]
//	      capacityTypes: [ON_DEMAND]
//
// Each value is also a flag and an environment variable (below), which win
// over the file; lists are comma-separated there. An empty list (or
// "none") means no restriction.

// Flag names and environment variables of the node group layout. The two
// allowlist flags are older than the section and stay.
const (
	FlagSystemNodeGroupInstanceType = "system-node-group-instance-type"
	EnvSystemNodeGroupInstanceType  = "CV_SYSTEM_NODE_GROUP_INSTANCE_TYPE"
	FlagSystemNodeGroupSize         = "system-node-group-size"
	EnvSystemNodeGroupSize          = "CV_SYSTEM_NODE_GROUP_SIZE"
	FlagSystemNodeGroupCapacityType = "system-node-group-capacity-type"
	EnvSystemNodeGroupCapacityType  = "CV_SYSTEM_NODE_GROUP_CAPACITY_TYPE"
	FlagSystemNodeGroupDiskSizeGiB  = "system-node-group-disk-size-gib"
	EnvSystemNodeGroupDiskSizeGiB   = "CV_SYSTEM_NODE_GROUP_DISK_SIZE_GIB"

	FlagNodeGroupInstanceFamilies = "node-group-instance-families"
	EnvNodeGroupInstanceFamilies  = "CV_NODE_GROUP_INSTANCE_FAMILIES"
	FlagNodeGroupInstanceTypes    = "node-group-instance-types"
	EnvNodeGroupInstanceTypes     = "CV_NODE_GROUP_INSTANCE_TYPES"

	FlagNodeGroupDefaultArchitectures = "node-group-default-architectures"
	EnvNodeGroupDefaultArchitectures  = "CV_NODE_GROUP_DEFAULT_ARCHITECTURES"
	FlagNodeGroupDefaultCapacityTypes = "node-group-default-capacity-types"
	EnvNodeGroupDefaultCapacityTypes  = "CV_NODE_GROUP_DEFAULT_CAPACITY_TYPES"
)

// Defaults of the node group layout. The machines are t3 (x86, burstable).
const (
	DefaultSystemNodeGroupInstanceType = "t3.medium"
	// MinSystemNodeGroupSize is the least the system group may have:
	// Karpenter runs two replicas with anti-affinity.
	MinSystemNodeGroupSize     = 2
	DefaultSystemNodeGroupSize = 2
	defaultCapacityType        = "ON_DEMAND"
	defaultArchitecture        = "AMD64"
	defaultInstanceFamily      = "t3"
	maxSystemDiskSizeGiB       = 16384
	// noList is the value of a list setting that means "empty": the setting
	// is set (a flag or variable may not be empty), and restricts nothing.
	noList = "none"
)

var (
	instanceFamilyPattern = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	instanceTypePattern   = regexp.MustCompile(`^[a-z][a-z0-9-]*[.][a-z0-9]+$`)
	capacityTypes         = []string{"ON_DEMAND", "SPOT"}
	architectures         = []string{"AMD64", "ARM64"}
)

// nodeGroupSettings are the layout's settings, for the server and the
// controller alike.
var nodeGroupSettings = []Setting{
	{Flag: FlagSystemNodeGroupInstanceType, Env: EnvSystemNodeGroupInstanceType, Default: DefaultSystemNodeGroupInstanceType,
		Usage: "instance type of the default (system) managed node group of NEW clusters (nodeGroups.system.instanceType); existing clusters keep theirs: EKS cannot change it in place"},
	{Flag: FlagSystemNodeGroupSize, Env: EnvSystemNodeGroupSize, Default: strconv.Itoa(DefaultSystemNodeGroupSize),
		Usage: "size of the system managed node group, fixed: desired = min = max (nodeGroups.system.size); at least 2, Karpenter runs two replicas with anti-affinity. A change resizes existing clusters in place"},
	{Flag: FlagSystemNodeGroupCapacityType, Env: EnvSystemNodeGroupCapacityType, Default: defaultCapacityType,
		Usage: "capacity type of the system node group of NEW clusters, ON_DEMAND or SPOT (nodeGroups.system.capacityType)"},
	{Flag: FlagSystemNodeGroupDiskSizeGiB, Env: EnvSystemNodeGroupDiskSizeGiB, Default: "0",
		Usage: "disk size in GiB of the system node group of NEW clusters (nodeGroups.system.diskSizeGiB); 0: EKS's default"},
	{Flag: FlagNodeGroupInstanceFamilies, Env: EnvNodeGroupInstanceFamilies, Default: defaultInstanceFamily,
		Usage: "comma-separated instance families (t3,m7i,...) user node groups may use (nodeGroups.karpenter.instanceFamilies); none: no restriction on families"},
	{Flag: FlagNodeGroupInstanceTypes, Env: EnvNodeGroupInstanceTypes,
		Usage: "comma-separated instance types (t3.large,...) user node groups may use besides every type of an allowed family (nodeGroups.karpenter.instanceTypes); empty: none besides the families'"},
	{Flag: FlagNodeGroupDefaultArchitectures, Env: EnvNodeGroupDefaultArchitectures, Default: defaultArchitecture,
		Usage: "comma-separated architectures (AMD64,ARM64) of a user node group that sets none (nodeGroups.karpenter.defaults.architectures)"},
	{Flag: FlagNodeGroupDefaultCapacityTypes, Env: EnvNodeGroupDefaultCapacityTypes, Default: defaultCapacityType,
		Usage: "comma-separated capacity types (ON_DEMAND,SPOT) of a user node group that sets none (nodeGroups.karpenter.defaults.capacityTypes)"},
}

// withNodeGroupSettings is settings followed by the node group layout's.
func withNodeGroupSettings(settings []Setting) []Setting {
	return slices.Concat(settings, nodeGroupSettings)
}

// NodeGroups is the validated node group layout. Server and controller load
// the same struct from the same section, so their views cannot drift.
type NodeGroups struct {
	System    SystemNodeGroup
	Karpenter KarpenterNodeGroups
}

// SystemNodeGroup is the default EKS managed node group, which runs
// Karpenter, CoreDNS and every platform and tenant component.
type SystemNodeGroup struct {
	// InstanceType, CapacityType and DiskSizeGiB apply to new clusters
	// only. Size applies to every cluster, in place.
	InstanceType string
	Size         int
	CapacityType string
	DiskSizeGiB  int
}

// KarpenterNodeGroups is what users may create as their own node groups.
type KarpenterNodeGroups struct {
	// InstanceFamilies and InstanceTypes are the allowlist; empty
	// restricts nothing in its dimension.
	InstanceFamilies []string
	InstanceTypes    []string
	Defaults         NodeGroupDefaults
}

// NodeGroupDefaults are applied to a user group that leaves them unset.
type NodeGroupDefaults struct {
	Architectures []string
	CapacityTypes []string
}

// DefaultNodeGroups is the layout when nothing is configured.
func DefaultNodeGroups() NodeGroups {
	return NodeGroups{
		System: SystemNodeGroup{InstanceType: DefaultSystemNodeGroupInstanceType, Size: DefaultSystemNodeGroupSize, CapacityType: defaultCapacityType},
		Karpenter: KarpenterNodeGroups{InstanceFamilies: []string{defaultInstanceFamily},
			Defaults: NodeGroupDefaults{Architectures: []string{defaultArchitecture}, CapacityTypes: []string{defaultCapacityType}}},
	}
}

// Restricted reports whether any allowlist is set.
func (k KarpenterNodeGroups) Restricted() bool {
	return len(k.InstanceFamilies) > 0 || len(k.InstanceTypes) > 0
}

func nodeGroupsFrom(v map[string]string) (NodeGroups, []error) {
	var errs []error
	out := NodeGroups{}
	check := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	out.System.InstanceType = strings.TrimSpace(v[FlagSystemNodeGroupInstanceType])
	if !instanceTypePattern.MatchString(out.System.InstanceType) {
		errs = append(errs, fmt.Errorf("%s: %q is not an instance type like t3.large", setting(FlagSystemNodeGroupInstanceType), v[FlagSystemNodeGroupInstanceType]))
	}
	size, err := strconv.Atoi(strings.TrimSpace(v[FlagSystemNodeGroupSize]))
	switch {
	case err != nil:
		errs = append(errs, fmt.Errorf("%s: %q is not a number", setting(FlagSystemNodeGroupSize), v[FlagSystemNodeGroupSize]))
	case size < MinSystemNodeGroupSize:
		errs = append(errs, fmt.Errorf("%s must be at least %d (Karpenter runs two replicas with anti-affinity), got %d",
			setting(FlagSystemNodeGroupSize), MinSystemNodeGroupSize, size))
	}
	out.System.Size = size
	out.System.CapacityType, err = oneOf(setting(FlagSystemNodeGroupCapacityType), v[FlagSystemNodeGroupCapacityType], capacityTypes)
	check(err)
	disk, err := strconv.Atoi(strings.TrimSpace(v[FlagSystemNodeGroupDiskSizeGiB]))
	if err != nil || disk < 0 || disk > maxSystemDiskSizeGiB {
		errs = append(errs, fmt.Errorf("%s must be a number from 0 to %d, got %q", setting(FlagSystemNodeGroupDiskSizeGiB), maxSystemDiskSizeGiB, v[FlagSystemNodeGroupDiskSizeGiB]))
	}
	out.System.DiskSizeGiB = disk
	out.Karpenter.InstanceFamilies, err = parseList(setting(FlagNodeGroupInstanceFamilies), v[FlagNodeGroupInstanceFamilies], instanceFamilyPattern, "an instance family like t3")
	check(err)
	out.Karpenter.InstanceTypes, err = parseList(setting(FlagNodeGroupInstanceTypes), v[FlagNodeGroupInstanceTypes], instanceTypePattern, "an instance type like t3.large")
	check(err)
	out.Karpenter.Defaults.Architectures, err = enumList(setting(FlagNodeGroupDefaultArchitectures), v[FlagNodeGroupDefaultArchitectures], architectures)
	check(err)
	out.Karpenter.Defaults.CapacityTypes, err = enumList(setting(FlagNodeGroupDefaultCapacityTypes), v[FlagNodeGroupDefaultCapacityTypes], capacityTypes)
	check(err)
	return out, errs
}

func oneOf(s Setting, raw string, allowed []string) (string, error) {
	raw = strings.TrimSpace(raw)
	if !slices.Contains(allowed, raw) {
		return "", fmt.Errorf("%s: %q is not one of %s", s, raw, strings.Join(allowed, ", "))
	}
	return raw, nil
}

// enumList is a comma-separated list of values of allowed, without repeats;
// at least one.
func enumList(s Setting, raw string, allowed []string) ([]string, error) {
	var out []string
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if !slices.Contains(allowed, item) {
			return nil, fmt.Errorf("%s: %q is not one of %s", s, item, strings.Join(allowed, ", "))
		}
		if !slices.Contains(out, item) {
			out = append(out, item)
		}
	}
	return out, nil
}

// parseList splits a comma-separated value, trims, drops repeats and
// checks each item against pattern. Empty or "none": no items.
func parseList(s Setting, raw string, pattern *regexp.Regexp, what string) ([]string, error) {
	if raw = strings.TrimSpace(raw); raw == "" || raw == noList {
		return nil, nil
	}
	var out []string
	seen := map[string]bool{}
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if !pattern.MatchString(item) {
			return nil, fmt.Errorf("%s: %q is not %s", s, item, what)
		}
		if !seen[item] {
			seen[item] = true
			out = append(out, item)
		}
	}
	return out, nil
}
