package config

import (
	"fmt"
	"regexp"
	"strings"
)

// Flag names and environment variables of the node group allowlist, set by
// the platform admin on the server and the controller alike.
const (
	FlagNodeGroupInstanceFamilies = "node-group-instance-families"
	EnvNodeGroupInstanceFamilies  = "CV_NODE_GROUP_INSTANCE_FAMILIES"
	FlagNodeGroupInstanceTypes    = "node-group-instance-types"
	EnvNodeGroupInstanceTypes     = "CV_NODE_GROUP_INSTANCE_TYPES"
)

var (
	instanceFamilyPattern = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	instanceTypePattern   = regexp.MustCompile(`^[a-z][a-z0-9-]*[.][a-z0-9]+$`)
)

var (
	nodeGroupFamiliesSetting = Setting{Flag: FlagNodeGroupInstanceFamilies, Env: EnvNodeGroupInstanceFamilies,
		Usage: "comma-separated instance families (m7i,c7i,...) user node groups and the platform node group may use; empty: no restriction on families"}
	nodeGroupTypesSetting = Setting{Flag: FlagNodeGroupInstanceTypes, Env: EnvNodeGroupInstanceTypes,
		Usage: "comma-separated instance types (m7i.large,...) user node groups may use besides every type of an allowed family; empty: none besides the families'"}
)

// NodeGroupAllowlist is which machines user node groups may ask for. An
// empty list restricts nothing in its dimension.
type NodeGroupAllowlist struct {
	InstanceFamilies []string
	InstanceTypes    []string
}

// Restricted reports whether any list is set.
func (a NodeGroupAllowlist) Restricted() bool {
	return len(a.InstanceFamilies) > 0 || len(a.InstanceTypes) > 0
}

func nodeGroupAllowlistFrom(v map[string]string) (NodeGroupAllowlist, []error) {
	var errs []error
	families, err := parseList(setting(FlagNodeGroupInstanceFamilies), v[FlagNodeGroupInstanceFamilies], instanceFamilyPattern, "an instance family like m7i")
	if err != nil {
		errs = append(errs, err)
	}
	types, err := parseList(setting(FlagNodeGroupInstanceTypes), v[FlagNodeGroupInstanceTypes], instanceTypePattern, "an instance type like m7i.large")
	if err != nil {
		errs = append(errs, err)
	}
	return NodeGroupAllowlist{InstanceFamilies: families, InstanceTypes: types}, errs
}

// parseList splits a comma-separated value, trims, drops repeats and
// checks each item against pattern.
func parseList(s Setting, raw string, pattern *regexp.Regexp, what string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
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
