package config

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// The platform admin's node group layout (docs/architecture.md#node-layout).
// It is the `nodeGroups` section of the config file, the same for the
// server and the controller. A cluster has exactly these two groups:
// tenants define none.
//
//	nodeGroups:
//	  system:                     # the default EKS managed node group: Karpenter and the EKS add-ons
//	    instanceTypes: [t3.medium] # one architecture
//	    minSize: 2                # at least 2; the static size (maxSize is ignored)
//	    capacityType: ON_DEMAND
//	    amiType: AL2023_x86_64_STANDARD
//	    diskSizeGiB: 0            # 0: EKS's default
//	  platform: {...}             # the platform pool (node_groups_platform.go)
//
// Every cluster the platform created runs them; EKS cannot change a node
// group's instance types, capacity type, AMI type or disk in place, so a
// change of one of them replaces each cluster's system group (the new one
// first, the old one retired once the new one is Ready), rolled out like a
// code change. Each value is also a flag and an environment variable
// (below), which win over the file; lists are comma-separated there.
//
// The old karpenter section (what tenants could create as their own groups)
// is ignored with a warning, so old configs still start.

// Flag names and environment variables of the node group layout. The two
// allowlist flags are older than the section and stay.
const (
	// FlagSystemNodeGroupInstanceType is the old single type; it stays
	// accepted (with a warning) as a list of one.
	FlagSystemNodeGroupInstanceType = "system-node-group-instance-type"
	EnvSystemNodeGroupInstanceType  = "CV_SYSTEM_NODE_GROUP_INSTANCE_TYPE"
	FlagSystemNodeGroupMinSize      = "system-node-group-min-size"
	EnvSystemNodeGroupMinSize       = "CV_SYSTEM_NODE_GROUP_MIN_SIZE"
	FlagSystemNodeGroupMaxSize      = "system-node-group-max-size"
	EnvSystemNodeGroupMaxSize       = "CV_SYSTEM_NODE_GROUP_MAX_SIZE"
	// FlagSystemNodeGroupSize is the setting of the fixed-size group, which
	// stays accepted as an alias of the minimum.
	FlagSystemNodeGroupSize         = "system-node-group-size"
	EnvSystemNodeGroupSize          = "CV_SYSTEM_NODE_GROUP_SIZE"
	FlagSystemNodeGroupCapacityType = "system-node-group-capacity-type"
	EnvSystemNodeGroupCapacityType  = "CV_SYSTEM_NODE_GROUP_CAPACITY_TYPE"
	FlagSystemNodeGroupDiskSizeGiB  = "system-node-group-disk-size-gib"
	EnvSystemNodeGroupDiskSizeGiB   = "CV_SYSTEM_NODE_GROUP_DISK_SIZE_GIB"
	FlagSystemNodeGroupAMIType      = "system-node-group-ami-type"
	EnvSystemNodeGroupAMIType       = "CV_SYSTEM_NODE_GROUP_AMI_TYPE"

	// The tenant node groups' allowlist and defaults are gone with the
	// groups: still accepted, ignored with a warning.

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
	MinSystemNodeGroupSize = 2
	// DefaultSystemNodeGroupMinSize is the group's static size unless the
	// admin sets another: its min, max and desired size.
	DefaultSystemNodeGroupMinSize = 2
	// DefaultSystemNodeGroupAMIType is the system group's AMI type unless
	// the admin sets another.
	DefaultSystemNodeGroupAMIType = "AL2023_x86_64_STANDARD"
	defaultCapacityType           = "ON_DEMAND"
	defaultArchitecture           = "AMD64"
	maxSystemDiskSizeGiB          = 16384
	// noList is the value of a list setting that means "empty": the setting
	// is set (a flag or variable may not be empty), and restricts nothing.
	noList = "none"
)

var (
	instanceTypePattern = regexp.MustCompile(`^[a-z][a-z0-9-]*[.][a-z0-9]+$`)
	capacityTypes       = []string{"ON_DEMAND", "SPOT"}
	architectures       = []string{"AMD64", "ARM64"}
	// systemAMITypes are the AMI types the system group may run, by
	// architecture: Linux ones Karpenter and the EKS add-ons run on.
	systemAMITypes = map[string]string{
		"AL2023_x86_64_STANDARD": "AMD64", "AL2023_ARM_64_STANDARD": "ARM64",
		"BOTTLEROCKET_x86_64": "AMD64", "BOTTLEROCKET_ARM_64": "ARM64",
	}
	// ignoredTenantSettings are the old tenant node group settings.
	ignoredTenantSettings = []string{FlagNodeGroupInstanceFamilies, FlagNodeGroupInstanceTypes,
		FlagNodeGroupDefaultArchitectures, FlagNodeGroupDefaultCapacityTypes}
)

// nodeGroupSettings are the layout's settings, for the server and the
// controller alike.
var nodeGroupSettings = []Setting{
	{Flag: FlagSystemInstanceTypes, Env: EnvSystemInstanceTypes,
		Usage: "comma-separated instance types of the system managed node group, one architecture and one vCPU/memory class (nodeGroups.system.instanceTypes); default t3.medium. A change replaces every cluster's system group (EKS cannot change it in place)"},
	{Flag: FlagSystemNodeGroupInstanceType, Env: EnvSystemNodeGroupInstanceType,
		Usage: "deprecated: one instance type, the old form of --" + FlagSystemInstanceTypes + " (nodeGroups.system.instanceType)"},
	{Flag: FlagSystemNodeGroupSize, Env: EnvSystemNodeGroupSize,
		Usage: "deprecated alias of --" + FlagSystemNodeGroupMinSize + " (nodeGroups.system.size)"},
	{Flag: FlagSystemNodeGroupMinSize, Env: EnvSystemNodeGroupMinSize,
		Usage: "static size of the system managed node group: its min, max and desired size (nodeGroups.system.minSize); default 2, at least 2: Karpenter runs two replicas with anti-affinity. A change applies to existing clusters in place, and sets their node count"},
	{Flag: FlagSystemNodeGroupMaxSize, Env: EnvSystemNodeGroupMaxSize,
		Usage: "ignored: the system managed node group has one static size, --" + FlagSystemNodeGroupMinSize + " (nodeGroups.system.maxSize); accepted with a warning so old configs still start"},
	{Flag: FlagSystemNodeGroupCapacityType, Env: EnvSystemNodeGroupCapacityType, Default: defaultCapacityType,
		Usage: "capacity type of the system node group, ON_DEMAND or SPOT (nodeGroups.system.capacityType). A change replaces every cluster's system group"},
	{Flag: FlagSystemNodeGroupAMIType, Env: EnvSystemNodeGroupAMIType, Default: DefaultSystemNodeGroupAMIType,
		Usage: "EKS AMI type of the system node group, of the instance types' architecture: AL2023_x86_64_STANDARD, AL2023_ARM_64_STANDARD, BOTTLEROCKET_x86_64 or BOTTLEROCKET_ARM_64 (nodeGroups.system.amiType). A change replaces every cluster's system group"},
	{Flag: FlagSystemNodeGroupDiskSizeGiB, Env: EnvSystemNodeGroupDiskSizeGiB, Default: "0",
		Usage: "disk size in GiB of the system node group (nodeGroups.system.diskSizeGiB); 0: EKS's default. A change replaces every cluster's system group"},
	{Flag: FlagNodeGroupInstanceFamilies, Env: EnvNodeGroupInstanceFamilies,
		Usage: "ignored: tenants define no node groups (nodeGroups.karpenter.instanceFamilies); accepted with a warning so old configs still start"},
	{Flag: FlagNodeGroupInstanceTypes, Env: EnvNodeGroupInstanceTypes,
		Usage: "ignored: tenants define no node groups (nodeGroups.karpenter.instanceTypes); accepted with a warning"},
	{Flag: FlagNodeGroupDefaultArchitectures, Env: EnvNodeGroupDefaultArchitectures,
		Usage: "ignored: tenants define no node groups (nodeGroups.karpenter.defaults.architectures); accepted with a warning"},
	{Flag: FlagNodeGroupDefaultCapacityTypes, Env: EnvNodeGroupDefaultCapacityTypes,
		Usage: "ignored: tenants define no node groups (nodeGroups.karpenter.defaults.capacityTypes); accepted with a warning"},
}

// withNodeGroupSettings is settings followed by the node group layout's.
func withNodeGroupSettings(settings []Setting) []Setting {
	return slices.Concat(settings, nodeGroupSettings, platformSettings)
}

// NodeGroups is the validated node group layout. Server and controller load
// the same struct from the same section, so their views cannot drift.
type NodeGroups struct {
	System   SystemNodeGroup
	Platform PlatformNodePool
	// Warnings are keys that still work but are on their way out; the
	// commands log them at start.
	Warnings []string
}

// SystemNodeGroup is the default EKS managed node group, which runs
// Karpenter and the EKS add-ons, on every cluster the platform created.
type SystemNodeGroup struct {
	// MinSize (nodeGroups.system.minSize) is the static size, the group's
	// min, max and desired size, changed in place: nothing autoscales the
	// group. The old maxSize key is ignored. A change of InstanceTypes,
	// CapacityType, AMIType or DiskSizeGiB replaces the group.
	InstanceTypes []string
	MinSize       int
	CapacityType  string
	// AMIType is the EKS AMI type (AL2023_x86_64_STANDARD, ...), of the
	// instance types' architecture.
	AMIType     string
	DiskSizeGiB int
}

// DefaultNodeGroups is the layout when nothing is configured.
func DefaultNodeGroups() NodeGroups {
	return NodeGroups{
		System: SystemNodeGroup{InstanceTypes: []string{DefaultSystemNodeGroupInstanceType}, MinSize: DefaultSystemNodeGroupMinSize,
			CapacityType: defaultCapacityType, AMIType: DefaultSystemNodeGroupAMIType},
		Platform: defaultPlatformNodePool(),
	}
}

func nodeGroupsFrom(v map[string]string) (NodeGroups, []error) {
	var errs []error
	out := NodeGroups{}
	check := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	var warnings []string
	out.System.InstanceTypes, errs, warnings = systemInstanceTypes(v, errs)
	if v[FlagSystemNodeGroupMaxSize] != "" {
		warnings = append(warnings, "nodeGroups.system.maxSize is ignored: the system group has one static size, minSize, and nothing autoscales it; remove it")
	}
	out.Warnings = warnings
	out.System.MinSize, errs = systemSize(v, errs)
	var err error
	out.System.CapacityType, err = oneOf(setting(FlagSystemNodeGroupCapacityType), v[FlagSystemNodeGroupCapacityType], capacityTypes)
	check(err)
	disk, err := strconv.Atoi(strings.TrimSpace(v[FlagSystemNodeGroupDiskSizeGiB]))
	if err != nil || disk < 0 || disk > maxSystemDiskSizeGiB {
		errs = append(errs, fmt.Errorf("%s must be a number from 0 to %d, got %q", setting(FlagSystemNodeGroupDiskSizeGiB), maxSystemDiskSizeGiB, v[FlagSystemNodeGroupDiskSizeGiB]))
	}
	out.System.DiskSizeGiB = disk
	out.System.AMIType, err = systemAMIType(v[FlagSystemNodeGroupAMIType], out.System.InstanceTypes)
	check(err)
	for _, flag := range ignoredTenantSettings {
		if v[flag] != "" {
			out.Warnings = append(out.Warnings, "nodeGroups.karpenter is ignored: tenants define no node groups, every cluster runs the system group and the platform pool; remove it")
			break
		}
	}
	var platformErrs []error
	out.Platform, platformErrs = platformFrom(v)
	errs = append(errs, platformErrs...)
	return out, errs
}

// systemAMIType reads the system group's AMI type: one of systemAMITypes,
// of the instance types' architecture.
func systemAMIType(raw string, types []string) (string, error) {
	raw = strings.TrimSpace(raw)
	arch, ok := systemAMITypes[raw]
	if !ok {
		allowed := slices.Sorted(maps.Keys(systemAMITypes))
		return "", fmt.Errorf("%s: %q is not one of %s", nodeSetting(FlagSystemNodeGroupAMIType), raw, strings.Join(allowed, ", "))
	}
	if len(types) > 0 && !mixed(types) && ArchitectureOfInstanceType(types[0]) != arch {
		return "", fmt.Errorf("%s %s is %s, but the instance types (%s) are not: the AMI must match their architecture",
			nodeSetting(FlagSystemNodeGroupAMIType), raw, arch, strings.Join(types, ", "))
	}
	return raw, nil
}

// systemInstanceTypes reads the bootstrap group's types: a non-empty list
// of one architecture. The old single-type key is a list of one, with a
// warning, and may not be set together with the list.
func systemInstanceTypes(v map[string]string, errs []error) ([]string, []error, []string) {
	var warnings []string
	raw, legacy := strings.TrimSpace(v[FlagSystemInstanceTypes]), strings.TrimSpace(v[FlagSystemNodeGroupInstanceType])
	switch {
	case raw != "" && legacy != "":
		errs = append(errs, fmt.Errorf("%s and %s are both set: instanceType is the old name of instanceTypes, set one",
			nodeSetting(FlagSystemNodeGroupInstanceType), nodeSetting(FlagSystemInstanceTypes)))
	case legacy != "":
		raw = legacy
		warnings = append(warnings, "nodeGroups.system.instanceType is deprecated: use instanceTypes, a list")
	case v[FlagSystemInstanceTypes] == "":
		raw = DefaultSystemNodeGroupInstanceType
	}
	types, err := parseList(nodeSetting(FlagSystemInstanceTypes), raw, instanceTypePattern, "an instance type like t3.large")
	switch {
	case err != nil:
		errs = append(errs, err)
	case len(types) == 0:
		errs = append(errs, fmt.Errorf("%s must name at least one instance type", nodeSetting(FlagSystemInstanceTypes)))
	case mixed(types):
		errs = append(errs, fmt.Errorf("%s mixes architectures (%s): every type of the bootstrap group must have one", nodeSetting(FlagSystemInstanceTypes), strings.Join(types, ", ")))
	}
	return types, errs, warnings
}

// systemSize reads the system group's static size, its minSize. size is
// the old fixed-size setting, the same number (both may be set when they
// are equal). Unset, it is DefaultSystemNodeGroupMinSize. The old maxSize
// is not read: it is ignored (nodeGroupsFrom warns about it).
func systemSize(v map[string]string, errs []error) (int, []error) {
	number := func(flag string) (int, bool) {
		raw := strings.TrimSpace(v[flag])
		if raw == "" {
			return 0, false
		}
		n, err := strconv.Atoi(raw)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %q is not a number", setting(flag), v[flag]))
			return 0, false
		}
		return n, true
	}
	size, hasSize := number(FlagSystemNodeGroupSize)
	minSize, hasMin := number(FlagSystemNodeGroupMinSize)
	switch {
	case hasSize && hasMin && size != minSize:
		errs = append(errs, fmt.Errorf("%s and %s are different (%d and %d): size is the old name of minSize, set one",
			setting(FlagSystemNodeGroupSize), setting(FlagSystemNodeGroupMinSize), size, minSize))
	case hasSize && !hasMin:
		minSize, hasMin = size, true
	}
	if !hasMin {
		minSize = DefaultSystemNodeGroupMinSize
	}
	if minSize < MinSystemNodeGroupSize {
		errs = append(errs, fmt.Errorf("the system node group's size (%s) must be at least %d (Karpenter runs two replicas with anti-affinity), got %d",
			setting(FlagSystemNodeGroupMinSize), MinSystemNodeGroupSize, minSize))
	}
	return minSize, errs
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

// LeftoverNodeGroupVariables names the environment variables that still
// set a node group key, in the settings' order. They keep working (a
// variable wins over the file) but the file is where the layout lives
// (docs/architecture.md#node-groups-configuration), so the commands warn at
// start to make a forgotten one visible.
func LeftoverNodeGroupVariables(env LookupFunc) []string {
	var set []string
	for _, s := range slices.Concat(nodeGroupSettings, platformSettings) {
		if _, ok := get(env, s.Env); ok {
			set = append(set, s.Env)
		}
	}
	return set
}
