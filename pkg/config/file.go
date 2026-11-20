package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// LoadFile reads a YAML config file and returns a lookup keyed by flag
// name, for LoadGlobal, LoadServe and LoadController. Keys are flag names;
// nested mappings are joined with "-", so these are the same:
//
//	aws:
//	  management-account-id: "111122223333"   # aws-management-account-id
//
// Values are scalars (strings, numbers, booleans, durations such as 30s).
// Unknown keys, lists, null values and the "config" key itself are errors,
// except in the "nodeGroups" section (below), which has real lists.
// Keep secrets in the environment: it wins over the file.
//
// The top-level "auth" section is not a setting: it is the API server's
// authentication, read by LoadAuth. The top-level "nodeGroups" section is
// the platform admin's node group layout (nodeGroupsSection): its keys map
// onto the node group settings, so a flag or variable still overrides one
// value.
func LoadFile(path string) (LookupFunc, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config file: %w", err)
	}
	var root yaml.Node
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&root); err != nil {
		return nil, fmt.Errorf("config file %s: invalid yaml: %w", path, err)
	}
	values := map[string]string{}
	if len(root.Content) > 0 {
		if err := flatten(root.Content[0], "", values); err != nil {
			return nil, fmt.Errorf("config file %s: %w", path, err)
		}
	}
	if err := checkKeys(values); err != nil {
		return nil, fmt.Errorf("config file %s: %w", path, err)
	}
	return func(key string) (string, bool) {
		v, ok := values[key]
		return v, ok
	}, nil
}

// FileFrom loads the file named by --config or CV_CONFIG, or returns nil
// when neither is set. The file path itself cannot come from the file.
func FileFrom(flags, env LookupFunc) (LookupFunc, error) {
	path, ok := get(flags, FlagConfig)
	if !ok {
		path, ok = get(env, EnvConfig)
	}
	if !ok {
		return nil, nil
	}
	if path == "" {
		return nil, fmt.Errorf("%s is set but empty", setting(FlagConfig))
	}
	return LoadFile(path)
}

func flatten(n *yaml.Node, prefix string, out map[string]string) error {
	if n.Kind != yaml.MappingNode {
		if prefix == "" {
			return errors.New("top level must be a mapping of setting names to values")
		}
		return fmt.Errorf("%s must be a scalar or a mapping", prefix)
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		key := n.Content[i].Value
		if prefix == "" && key == SectionAuth {
			continue // not a setting: LoadAuth reads it
		}
		if prefix == "" && key == SectionNodeGroups {
			if err := flattenNodeGroups(n.Content[i+1], out); err != nil {
				return err
			}
			continue
		}
		if prefix != "" {
			key = prefix + "-" + key
		}
		switch v := n.Content[i+1]; {
		case v.Kind == yaml.MappingNode:
			if err := flatten(v, key, out); err != nil {
				return err
			}
		case v.Kind == yaml.ScalarNode && v.Tag == "!!null":
			return fmt.Errorf("%s has no value", key)
		case v.Kind == yaml.ScalarNode:
			if _, dup := out[key]; dup {
				return fmt.Errorf("%s is set twice (also in the nodeGroups section)", key)
			}
			out[key] = v.Value
		default:
			return fmt.Errorf("%s must be a scalar, got a %s", key, kindName(v.Kind))
		}
	}
	return nil
}

func checkKeys(values map[string]string) error {
	known := map[string]bool{}
	// Any command's settings: the file is read before the command is known
	// to it, and the controller's settings are not the server's.
	for _, s := range slices.Concat(globalSettings, serveSettings, controllerSettings) {
		known[s.Flag] = true
	}
	var errs []error
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		switch {
		case k == FlagConfig:
			errs = append(errs, fmt.Errorf("%s cannot be set inside the config file", FlagConfig))
		case !known[k]:
			errs = append(errs, fmt.Errorf("unknown setting %q", k))
		}
	}
	return errors.Join(errs...)
}

func kindName(k yaml.Kind) string {
	switch k {
	case yaml.SequenceNode:
		return "list"
	case yaml.AliasNode:
		return "alias"
	default:
		return "document"
	}
}

// SectionNodeGroups is the config file's section of the node group layout.
const SectionNodeGroups = "nodeGroups"

// nodeGroupsSection maps a path in the nodeGroups section to its setting,
// and whether the value is a list (joined with commas).
var nodeGroupsSection = map[string]struct {
	flag string
	list bool
}{
	"system.instanceType":              {FlagSystemNodeGroupInstanceType, false},
	"system.instanceTypes":             {FlagSystemInstanceTypes, true},
	"system.minSize":                   {FlagSystemNodeGroupMinSize, false},
	"system.maxSize":                   {FlagSystemNodeGroupMaxSize, false},
	"system.size":                      {FlagSystemNodeGroupSize, false},
	"system.capacityType":              {FlagSystemNodeGroupCapacityType, false},
	"system.diskSizeGiB":               {FlagSystemNodeGroupDiskSizeGiB, false},
	"platform.instanceFamilies":        {FlagPlatformInstanceFamilies, true},
	"platform.instanceSizes":           {FlagPlatformInstanceSizes, true},
	"platform.architectures":           {FlagPlatformArchitectures, true},
	"platform.capacityTypes":           {FlagPlatformCapacityTypes, true},
	"platform.limits.cpu":              {FlagPlatformLimitCPU, false},
	"platform.limits.memory":           {FlagPlatformLimitMemory, false},
	"platform.consolidationPolicy":     {FlagPlatformConsolidationPolicy, false},
	"platform.consolidateAfter":        {FlagPlatformConsolidateAfter, false},
	"platform.expireAfter":             {FlagPlatformExpireAfter, false},
	"platform.terminationGracePeriod":  {FlagPlatformTerminationGracePeriod, false},
	"platform.disruptionBudget":        {FlagPlatformDisruptionBudget, false},
	"platform.maxPods":                 {FlagPlatformMaxPods, false},
	"karpenter.instanceFamilies":       {FlagNodeGroupInstanceFamilies, true},
	"karpenter.instanceTypes":          {FlagNodeGroupInstanceTypes, true},
	"karpenter.defaults.architectures": {FlagNodeGroupDefaultArchitectures, true},
	"karpenter.defaults.capacityTypes": {FlagNodeGroupDefaultCapacityTypes, true},
}

// flattenNodeGroups reads the nodeGroups section into out under its
// settings' flag names. Scalars may be given as lists of one and lists as
// comma-separated text; an empty list is "none" (restricts nothing).
func flattenNodeGroups(n *yaml.Node, out map[string]string) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("%s must be a mapping", SectionNodeGroups)
	}
	return walkNodeGroups(n, SectionNodeGroups, "", out)
}

func walkNodeGroups(n *yaml.Node, where, path string, out map[string]string) error {
	for i := 0; i+1 < len(n.Content); i += 2 {
		key, v := n.Content[i].Value, n.Content[i+1]
		at, shown := key, where+"."+key
		if path != "" {
			at = path + "." + key
		}
		if v.Kind == yaml.MappingNode {
			if err := walkNodeGroups(v, shown, at, out); err != nil {
				return err
			}
			continue
		}
		entry, known := nodeGroupsSection[at]
		if !known {
			return fmt.Errorf("unknown setting %s", shown)
		}
		value, err := nodeGroupsValue(v, entry.list, shown)
		if err != nil {
			return err
		}
		if _, dup := out[entry.flag]; dup {
			return fmt.Errorf("%s is set twice (also as %s)", shown, entry.flag)
		}
		out[entry.flag] = value
	}
	return nil
}

func nodeGroupsValue(v *yaml.Node, list bool, shown string) (string, error) {
	switch {
	case v.Kind == yaml.SequenceNode && list:
		items := make([]string, 0, len(v.Content))
		for _, item := range v.Content {
			if item.Kind != yaml.ScalarNode || item.Tag == "!!null" {
				return "", fmt.Errorf("%s must be a list of scalars", shown)
			}
			items = append(items, item.Value)
		}
		if len(items) == 0 {
			return noList, nil
		}
		return strings.Join(items, ","), nil
	case v.Kind == yaml.ScalarNode && v.Tag != "!!null":
		return v.Value, nil
	case v.Kind == yaml.SequenceNode:
		return "", fmt.Errorf("%s must be a single value, not a list", shown)
	default:
		return "", fmt.Errorf("%s has no value", shown)
	}
}
