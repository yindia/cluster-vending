package config

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
)

// The platform pool (docs/proposals/PROP-03-component-placement.md, 4.3 and
// 4.4): the Karpenter NodePool and EC2NodeClass `platform-pool`. In the
// file:
//
//	nodeGroups:
//	  platform:
//	    instanceFamilies: [m7i, m6i]
//	    instanceSizes: [large, xlarge, 2xlarge]
//	    architectures: [AMD64]          # one per pool
//	    capacityTypes: [ON_DEMAND]      # SPOT is refused in this version
//	    limits: {cpu: 128, memory: 512Gi}
//	    consolidationPolicy: WhenEmpty  # or WhenEmptyOrUnderutilized
//	    consolidateAfter: 5m
//	    expireAfter: 336h
//	    terminationGracePeriod: 1h
//	    disruptionBudget: 1             # nodes disrupted at once
//	    maxPods: 0                      # 0: Karpenter's default for the shape
const (
	FlagSystemInstanceTypes = "system-instance-types"
	EnvSystemInstanceTypes  = "CV_SYSTEM_INSTANCE_TYPES"

	FlagPlatformInstanceFamilies       = "platform-instance-families"
	EnvPlatformInstanceFamilies        = "CV_PLATFORM_INSTANCE_FAMILIES"
	FlagPlatformInstanceSizes          = "platform-instance-sizes"
	EnvPlatformInstanceSizes           = "CV_PLATFORM_INSTANCE_SIZES"
	FlagPlatformArchitectures          = "platform-architectures"
	EnvPlatformArchitectures           = "CV_PLATFORM_ARCHITECTURES"
	FlagPlatformCapacityTypes          = "platform-capacity-types"
	EnvPlatformCapacityTypes           = "CV_PLATFORM_CAPACITY_TYPES"
	FlagPlatformLimitCPU               = "platform-limit-cpu"
	EnvPlatformLimitCPU                = "CV_PLATFORM_LIMIT_CPU"
	FlagPlatformLimitMemory            = "platform-limit-memory"
	EnvPlatformLimitMemory             = "CV_PLATFORM_LIMIT_MEMORY"
	FlagPlatformConsolidationPolicy    = "platform-consolidation-policy"
	EnvPlatformConsolidationPolicy     = "CV_PLATFORM_CONSOLIDATION_POLICY"
	FlagPlatformConsolidateAfter       = "platform-consolidate-after"
	EnvPlatformConsolidateAfter        = "CV_PLATFORM_CONSOLIDATE_AFTER"
	FlagPlatformExpireAfter            = "platform-expire-after"
	EnvPlatformExpireAfter             = "CV_PLATFORM_EXPIRE_AFTER"
	FlagPlatformTerminationGracePeriod = "platform-termination-grace-period"
	EnvPlatformTerminationGracePeriod  = "CV_PLATFORM_TERMINATION_GRACE_PERIOD"
	FlagPlatformDisruptionBudget       = "platform-disruption-budget"
	EnvPlatformDisruptionBudget        = "CV_PLATFORM_DISRUPTION_BUDGET"
	FlagPlatformMaxPods                = "platform-max-pods"
	EnvPlatformMaxPods                 = "CV_PLATFORM_MAX_PODS"
)

// Defaults of the platform pool, and the consolidation policies.
const (
	ConsolidateWhenEmpty                = "WhenEmpty"
	ConsolidateWhenEmptyOrUnderutilized = "WhenEmptyOrUnderutilized"

	defaultPlatformFamilies = "m7i,m6i"
	defaultPlatformSizes    = "large,xlarge,2xlarge"
	// MaxPlatformMaxPods is the most pods a node may hold with prefix
	// delegation (the VPC CNI's limit for the shapes the pool uses).
	MaxPlatformMaxPods = 110
)

var (
	platformFamilyPattern = regexp.MustCompile(`^[a-z]+[0-9]+[a-z]*$`)
	platformSizePattern   = regexp.MustCompile(`^(nano|micro|small|medium|large|[0-9]*xlarge|metal)$`)
	// armFamilyPattern: the Graviton families carry a g after the
	// generation digits (m7g, c6gd, x2gd, im4gn, is4gen); a1 is the first.
	armFamilyPattern    = regexp.MustCompile(`^[a-z]+[0-9]+[a-z]*g[a-z]*$`)
	consolidationPolicy = []string{ConsolidateWhenEmpty, ConsolidateWhenEmptyOrUnderutilized}
)

// PlatformNodePool is the settings of the Karpenter NodePool and
// EC2NodeClass `platform-pool`.
type PlatformNodePool struct {
	InstanceFamilies []string
	InstanceSizes    []string
	// Architectures has exactly one item (AMD64 or ARM64): one per pool.
	Architectures []string
	CapacityTypes []string
	Limits        PlatformLimits
	// ConsolidationPolicy is WhenEmpty or WhenEmptyOrUnderutilized.
	ConsolidationPolicy    string
	ConsolidateAfter       time.Duration
	ExpireAfter            time.Duration
	TerminationGracePeriod time.Duration
	// DisruptionBudget is how many nodes Karpenter may disrupt at once.
	DisruptionBudget int
	// MaxPods is the kubelet's maxPods; 0 leaves Karpenter's default.
	MaxPods int
}

// PlatformLimits caps what the pool may launch. Memory is a Kubernetes
// quantity such as 512Gi.
type PlatformLimits struct {
	CPU    int64
	Memory string
}

// ArchitectureOfInstanceType is the CPU architecture an instance type or
// family name implies (AMD64 or ARM64), from the name alone.
func ArchitectureOfInstanceType(name string) string {
	family, _, _ := strings.Cut(name, ".")
	if family == "a1" || armFamilyPattern.MatchString(family) {
		return "ARM64"
	}
	return "AMD64"
}

func defaultPlatformNodePool() PlatformNodePool {
	return PlatformNodePool{
		InstanceFamilies: strings.Split(defaultPlatformFamilies, ","), InstanceSizes: strings.Split(defaultPlatformSizes, ","),
		Architectures: []string{defaultArchitecture}, CapacityTypes: []string{defaultCapacityType},
		Limits:              PlatformLimits{CPU: 128, Memory: "512Gi"},
		ConsolidationPolicy: ConsolidateWhenEmpty, ConsolidateAfter: 5 * time.Minute,
		ExpireAfter: 336 * time.Hour, TerminationGracePeriod: time.Hour, DisruptionBudget: 1,
	}
}

// platformSettings are the platform pool's flags and variables.
var platformSettings = []Setting{
	{Flag: FlagPlatformInstanceFamilies, Env: EnvPlatformInstanceFamilies, Default: defaultPlatformFamilies,
		Usage: "comma-separated instance families of the platform pool (nodeGroups.platform.instanceFamilies)"},
	{Flag: FlagPlatformInstanceSizes, Env: EnvPlatformInstanceSizes, Default: defaultPlatformSizes,
		Usage: "comma-separated instance sizes of the platform pool (nodeGroups.platform.instanceSizes)"},
	{Flag: FlagPlatformArchitectures, Env: EnvPlatformArchitectures, Default: defaultArchitecture,
		Usage: "architecture of the platform pool, AMD64 or ARM64: one (nodeGroups.platform.architectures)"},
	{Flag: FlagPlatformCapacityTypes, Env: EnvPlatformCapacityTypes, Default: defaultCapacityType,
		Usage: "capacity types of the platform pool; SPOT is refused in this version (nodeGroups.platform.capacityTypes)"},
	{Flag: FlagPlatformLimitCPU, Env: EnvPlatformLimitCPU, Default: "128",
		Usage: "CPU cores the platform pool may launch (nodeGroups.platform.limits.cpu)"},
	{Flag: FlagPlatformLimitMemory, Env: EnvPlatformLimitMemory, Default: "512Gi",
		Usage: "memory the platform pool may launch, a quantity such as 512Gi (nodeGroups.platform.limits.memory)"},
	{Flag: FlagPlatformConsolidationPolicy, Env: EnvPlatformConsolidationPolicy, Default: ConsolidateWhenEmpty,
		Usage: "consolidation of the platform pool, WhenEmpty or WhenEmptyOrUnderutilized (nodeGroups.platform.consolidationPolicy)"},
	{Flag: FlagPlatformConsolidateAfter, Env: EnvPlatformConsolidateAfter, Default: "5m",
		Usage: "how long a node must be idle before Karpenter consolidates it (nodeGroups.platform.consolidateAfter)"},
	{Flag: FlagPlatformExpireAfter, Env: EnvPlatformExpireAfter, Default: "336h",
		Usage: "node lifetime of the platform pool (nodeGroups.platform.expireAfter)"},
	{Flag: FlagPlatformTerminationGracePeriod, Env: EnvPlatformTerminationGracePeriod, Default: "1h",
		Usage: "longest a drain of a platform pool node may take (nodeGroups.platform.terminationGracePeriod)"},
	{Flag: FlagPlatformDisruptionBudget, Env: EnvPlatformDisruptionBudget, Default: "1",
		Usage: "nodes of the platform pool Karpenter may disrupt at once (nodeGroups.platform.disruptionBudget)"},
	{Flag: FlagPlatformMaxPods, Env: EnvPlatformMaxPods, Default: "0",
		Usage: "kubelet maxPods of the platform pool, 0 to 110; 0 leaves Karpenter's default (nodeGroups.platform.maxPods)"},
}

// platformFrom reads and validates the platform pool; every problem is
// returned.
func platformFrom(v map[string]string) (PlatformNodePool, []error) {
	var errs []error
	out := PlatformNodePool{}
	list := func(flag string, pattern *regexp.Regexp, what string) []string {
		items, err := parseList(nodeSetting(flag), v[flag], pattern, what)
		if err != nil {
			errs = append(errs, err)
		} else if len(items) == 0 {
			errs = append(errs, fmt.Errorf("%s must name at least one value", nodeSetting(flag)))
		}
		return items
	}
	out.InstanceFamilies = list(FlagPlatformInstanceFamilies, platformFamilyPattern, "an instance family like m7i")
	out.InstanceSizes = list(FlagPlatformInstanceSizes, platformSizePattern, "an instance size like xlarge")
	archs, err := enumList(nodeSetting(FlagPlatformArchitectures), v[FlagPlatformArchitectures], architectures)
	if err != nil {
		errs = append(errs, err)
	}
	if len(archs) > 1 {
		errs = append(errs, fmt.Errorf("%s: the pool has one architecture, got %s", nodeSetting(FlagPlatformArchitectures), strings.Join(archs, ", ")))
	}
	out.Architectures = archs
	out.CapacityTypes, err = enumList(nodeSetting(FlagPlatformCapacityTypes), v[FlagPlatformCapacityTypes], capacityTypes)
	if err != nil {
		errs = append(errs, err)
	}
	if slices.Contains(out.CapacityTypes, "SPOT") {
		errs = append(errs, fmt.Errorf("%s: SPOT is refused in this version: the platform pool is on-demand", nodeSetting(FlagPlatformCapacityTypes)))
	}
	if len(archs) == 1 {
		for _, f := range out.InstanceFamilies {
			if got := ArchitectureOfInstanceType(f); got != archs[0] {
				errs = append(errs, fmt.Errorf("%s: family %q is %s, which does not match the pool's architecture %s (%s)",
					nodeSetting(FlagPlatformInstanceFamilies), f, got, archs[0], nodeSetting(FlagPlatformArchitectures)))
			}
		}
	}
	if mixed(out.InstanceFamilies) {
		errs = append(errs, fmt.Errorf("%s mixes architectures: a pool has one", nodeSetting(FlagPlatformInstanceFamilies)))
	}
	errs = append(errs, platformLimitsFrom(v, &out)...)
	out.ConsolidationPolicy, err = oneOf(nodeSetting(FlagPlatformConsolidationPolicy), v[FlagPlatformConsolidationPolicy], consolidationPolicy)
	if err != nil {
		errs = append(errs, err)
	}
	for _, d := range []struct {
		flag      string
		dst       *time.Duration
		allowZero bool
	}{
		{FlagPlatformConsolidateAfter, &out.ConsolidateAfter, true},
		{FlagPlatformExpireAfter, &out.ExpireAfter, false},
		{FlagPlatformTerminationGracePeriod, &out.TerminationGracePeriod, false},
	} {
		got, err := time.ParseDuration(strings.TrimSpace(v[d.flag]))
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("%s: %q is not a duration like 5m or 336h", nodeSetting(d.flag), v[d.flag]))
		case got < 0 || (got == 0 && !d.allowZero):
			errs = append(errs, fmt.Errorf("%s must be positive, got %s", nodeSetting(d.flag), got))
		default:
			*d.dst = got
		}
	}
	out.DisruptionBudget = intSetting(v, FlagPlatformDisruptionBudget, &errs)
	if out.DisruptionBudget < 1 {
		errs = append(errs, fmt.Errorf("%s must be at least 1, got %d", nodeSetting(FlagPlatformDisruptionBudget), out.DisruptionBudget))
	}
	out.MaxPods = intSetting(v, FlagPlatformMaxPods, &errs)
	if out.MaxPods < 0 || out.MaxPods > MaxPlatformMaxPods {
		errs = append(errs, fmt.Errorf("%s must be 0 to %d (above 110 needs more than prefix delegation gives), got %d", nodeSetting(FlagPlatformMaxPods), MaxPlatformMaxPods, out.MaxPods))
	}
	return out, errs
}

func platformLimitsFrom(v map[string]string, out *PlatformNodePool) []error {
	var errs []error
	cpu, err := strconv.ParseInt(strings.TrimSpace(v[FlagPlatformLimitCPU]), 10, 64)
	if err != nil || cpu <= 0 {
		errs = append(errs, fmt.Errorf("%s (limits.cpu) must be a positive number of cores, got %q", nodeSetting(FlagPlatformLimitCPU), v[FlagPlatformLimitCPU]))
	}
	out.Limits.CPU = cpu
	mem := strings.TrimSpace(v[FlagPlatformLimitMemory])
	if q, err := resource.ParseQuantity(mem); err != nil || q.Sign() <= 0 {
		errs = append(errs, fmt.Errorf("%s (limits.memory) must be a positive quantity like 512Gi, got %q", nodeSetting(FlagPlatformLimitMemory), v[FlagPlatformLimitMemory]))
	}
	out.Limits.Memory = mem
	return errs
}

// nodeSetting is the setting of a flag with its place in the config file's
// nodeGroups section, for the messages.
func nodeSetting(flag string) Setting {
	out := setting(flag)
	for path, e := range nodeGroupsSection {
		if e.flag == flag {
			out.Key = SectionNodeGroups + "." + path
		}
	}
	return out
}

// intSetting reads a whole number; a bad one is a problem and reads as 0.
func intSetting(v map[string]string, flag string, errs *[]error) int {
	n, err := strconv.Atoi(strings.TrimSpace(v[flag]))
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s: %q is not a number", nodeSetting(flag), v[flag]))
		return 0
	}
	return n
}

// mixed reports whether the names (families or instance types) imply more
// than one architecture.
func mixed(names []string) bool {
	seen := map[string]bool{}
	for _, n := range names {
		seen[ArchitectureOfInstanceType(n)] = true
	}
	return len(seen) > 1
}
