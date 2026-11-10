package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"

	"go.yaml.in/yaml/v3"
)

// LoadFile reads a YAML config file and returns a lookup keyed by flag
// name, for LoadGlobal / LoadServe. Keys are flag names; nested mappings
// are joined with "-", so these are the same:
//
//	database:
//	  driver: postgres         # database-driver
//	aws:
//	  management-account-id: "111122223333"   # aws-management-account-id
//
// Values are scalars (strings, numbers, booleans, durations such as 30s).
// Unknown keys, lists, null values and the "config" key itself are errors.
// Keep secrets (e.g. a password in database-dsn) in the environment: it
// wins over the file.
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
			out[key] = v.Value
		default:
			return fmt.Errorf("%s must be a scalar, got a %s", key, kindName(v.Kind))
		}
	}
	return nil
}

func checkKeys(values map[string]string) error {
	known := map[string]bool{}
	for _, s := range slices.Concat(globalSettings, serveSettings) {
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
