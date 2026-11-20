package config

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// The platform admin's node group layout (docs/architecture.md#node-layout).
// It is the `nodeGroups` section of the config file, the same for the
// server and the controller:
//
//	nodeGroups:
//	  system:                     # the bootstrap group: the default EKS managed node group
//	    instanceTypes: [t3.medium] # new clusters only; existing ones keep theirs
//	    minSize: 2                # at least 2; the fixed size (maxSize is deprecated)
//	    capacityType: ON_DEMAND
//	    diskSizeGiB: 0            # 0: EKS's default
//	  platform: {...}             # the platform pool (node_groups_platform.go)
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
	// DefaultSystemNodeGroupMinSize is the size at creation; the default
	// maximum is how far cluster-autoscaler may grow the group (or the
	// minimum, when that is higher).
	DefaultSystemNodeGroupMinSize = 2
	// DefaultSystemNodeGroupMaxSize is the old cluster-autoscaler ceiling,
	// kept until cluster-autoscaler leaves the default set (PROP-03 phase 4).
	DefaultSystemNodeGroupMaxSize = 6
	defaultCapacityType           = "ON_DEMAND"
	defaultArchitecture           = "AMD64"
	defaultInstanceFamily         = "t3"
	maxSystemDiskSizeGiB          = 16384
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
	{Flag: FlagSystemInstanceTypes, Env: EnvSystemInstanceTypes,
		Usage: "comma-separated instance types of the bootstrap (system) managed node group of NEW clusters, one architecture and one vCPU/memory class (nodeGroups.system.instanceTypes); default t3.medium; existing clusters keep theirs: EKS cannot change it in place"},
	{Flag: FlagSystemNodeGroupInstanceType, Env: EnvSystemNodeGroupInstanceType,
		Usage: "deprecated: one instance type, the old form of --" + FlagSystemInstanceTypes + " (nodeGroups.system.instanceType)"},
	{Flag: FlagSystemNodeGroupSize, Env: EnvSystemNodeGroupSize,
		Usage: "deprecated alias of --" + FlagSystemNodeGroupMinSize + " (nodeGroups.system.size)"},
	{Flag: FlagSystemNodeGroupMinSize, Env: EnvSystemNodeGroupMinSize,
		Usage: "minimum size of the system managed node group, and its size at creation (nodeGroups.system.minSize); default 2, at least 2: Karpenter runs two replicas with anti-affinity. A change applies to existing clusters in place, without touching the node count"},
	{Flag: FlagSystemNodeGroupMaxSize, Env: EnvSystemNodeGroupMaxSize,
		Usage: "deprecated: maximum size of the system managed node group, which cluster-autoscaler grows it to until it leaves the default set (nodeGroups.system.maxSize); default 6, at least the minimum"},
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
	return slices.Concat(settings, nodeGroupSettings, platformSettings)
}

// NodeGroups is the validated node group layout. Server and controller load
// the same struct from the same section, so their views cannot drift.
type NodeGroups struct {
	System    SystemNodeGroup
	Platform  PlatformNodePool
	Karpenter KarpenterNodeGroups
	// Warnings are keys that still work but are on their way out; the
	// commands log them at start.
	Warnings []string
}

// SystemNodeGroup is the default EKS managed node group, which runs
// Karpenter, CoreDNS and every platform and tenant component.
type SystemNodeGroup struct {
	// InstanceTypes, CapacityType and DiskSizeGiB apply to new clusters
	// only. MinSize applies to every cluster, in place.
	InstanceTypes []string
	MinSize       int
	// MaxSize is the ceiling cluster-autoscaler still grows the group to,
	// until it leaves the default set (PROP-03 phase 4); the key is
	// deprecated and warned about.
	MaxSize      int
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
		System:   SystemNodeGroup{InstanceTypes: []string{DefaultSystemNodeGroupInstanceType}, MinSize: DefaultSystemNodeGroupMinSize, MaxSize: DefaultSystemNodeGroupMaxSize, CapacityType: defaultCapacityType},
		Platform: defaultPlatformNodePool(),
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
	var warnings []string
	out.System.InstanceTypes, errs, warnings = systemInstanceTypes(v, errs)
	if v[FlagSystemNodeGroupMaxSize] != "" {
		warnings = append(warnings, "nodeGroups.system.maxSize is deprecated: the bootstrap group has a fixed size (minSize). It still sets the cluster-autoscaler ceiling until cluster-autoscaler leaves the default set; remove it")
	}
	out.Warnings = warnings
	out.System.MinSize, out.System.MaxSize, errs = systemSizes(v, errs)
	var err error
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
	var platformErrs []error
	out.Platform, platformErrs = platformFrom(v)
	errs = append(errs, platformErrs...)
	return out, errs
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

// systemSizes reads the system group's minimum and maximum. size is the
// old fixed-size setting: it means the minimum (it may also be set to the
// same number). An unset minimum is DefaultSystemNodeGroupMinSize, an unset
// maximum DefaultSystemNodeGroupMaxSize or the minimum when that is higher.
func systemSizes(v map[string]string, errs []error) (minSize, maxSize int, _ []error) {
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
	maxSize, hasMax := number(FlagSystemNodeGroupMaxSize)
	switch {
	case hasSize && hasMin && size != minSize:
		errs = append(errs, fmt.Errorf("%s and %s are different (%d and %d): size is the old name of the minimum, set one",
			setting(FlagSystemNodeGroupSize), setting(FlagSystemNodeGroupMinSize), size, minSize))
	case hasSize && !hasMin:
		minSize, hasMin = size, true
	}
	if !hasMin {
		minSize = DefaultSystemNodeGroupMinSize
	}
	if minSize < MinSystemNodeGroupSize {
		errs = append(errs, fmt.Errorf("the system node group's minimum size (%s) must be at least %d (Karpenter runs two replicas with anti-affinity), got %d",
			setting(FlagSystemNodeGroupMinSize), MinSystemNodeGroupSize, minSize))
	}
	if !hasMax {
		maxSize = max(DefaultSystemNodeGroupMaxSize, minSize)
	}
	if maxSize < minSize {
		errs = append(errs, fmt.Errorf("%s must be at least the minimum size (%d), got %d", setting(FlagSystemNodeGroupMaxSize), minSize, maxSize))
	}
	return minSize, maxSize, errs
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
