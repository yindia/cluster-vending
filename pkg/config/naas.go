package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
	"unicode"

	"go.yaml.in/yaml/v3"
)

// SectionNaaS is the settings file's key of the sub-tenant (NaaS) rules
// (PROP-05 6.2). Like "auth" it holds lists, so it has no flags and no
// environment variables; LoadFile leaves it to LoadNaaS. The server and the
// controller read the same section: the API refuses a group outside it, and
// the controller closes access for one that left it.
const SectionNaaS = "naas"

// maxGroupPatterns bounds each list of the section.
const maxGroupPatterns = 256

// NaaS is the "naas" section of the settings file.
//
// AllowedGroups are the OIDC group claim values a sub-tenant may name in its
// access groups: exact values, or a prefix ending in "*". Empty (the
// default) allows no group: access stays closed until an operator sets it.
// DeniedGroups may never be named; they are checked first.
//
// QualifierImage is the image of the naas-qualifier Job (the
// access-qualification runner, PROP-05 6.1). Empty: the controller starts
// no Job, and clusters are qualified only by a record an operator writes.
// ProbeImage is the image of the qualifier's test pods (a shell with nc,
// wget and nslookup); empty is DefaultQualifierProbeImage.
type NaaS struct {
	AllowedGroups  []string `yaml:"allowedGroups"`
	DeniedGroups   []string `yaml:"deniedGroups"`
	QualifierImage string   `yaml:"qualifierImage"`
	ProbeImage     string   `yaml:"probeImage"`
}

// DefaultQualifierProbeImage is the qualifier's test pod image when the
// section does not name one.
const DefaultQualifierProbeImage = "busybox:1.36.1"

// ErrGroupNotAllowed is the error of a group outside naas.allowedGroups or
// inside naas.deniedGroups.
var ErrGroupNotAllowed = errors.New("group is not allowed by the platform's naas.allowedGroups")

// NaaSFrom reads the "naas" section of the file named by --config or
// CV_CONFIG. Without a file, or without the section, the zero NaaS: no
// group is allowed.
func NaaSFrom(flags, env LookupFunc) (NaaS, error) {
	path, ok := get(flags, FlagConfig)
	if !ok {
		path, ok = get(env, EnvConfig)
	}
	if !ok {
		return NaaS{}, nil
	}
	return LoadNaaS(path)
}

// LoadNaaS reads the "naas" section of a settings file. Unknown keys and
// malformed patterns are errors, reported together.
func LoadNaaS(path string) (NaaS, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return NaaS{}, fmt.Errorf("config file: %w", err)
	}
	var file map[string]yaml.Node
	if err := yaml.Unmarshal(raw, &file); err != nil {
		return NaaS{}, fmt.Errorf("config file %s: invalid yaml: %w", path, err)
	}
	var cfg NaaS
	section, ok := file[SectionNaaS]
	if !ok {
		return cfg, nil
	}
	encoded, err := yaml.Marshal(&section)
	if err != nil {
		return NaaS{}, fmt.Errorf("config file %s: %s: %w", path, SectionNaaS, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(encoded))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return NaaS{}, fmt.Errorf("config file %s: %s: %w", path, SectionNaaS, err)
	}
	if err := cfg.Validate(); err != nil {
		return NaaS{}, fmt.Errorf("config file %s: %w", path, err)
	}
	return cfg, nil
}

// Validate checks every pattern of both lists.
func (n NaaS) Validate() error {
	var errs []error
	for _, list := range []struct {
		name     string
		patterns []string
	}{{"allowedGroups", n.AllowedGroups}, {"deniedGroups", n.DeniedGroups}} {
		if len(list.patterns) > maxGroupPatterns {
			errs = append(errs, fmt.Errorf("%s.%s has more than %d entries", SectionNaaS, list.name, maxGroupPatterns))
		}
		for i, p := range list.patterns {
			if err := validGroupPattern(p); err != nil {
				errs = append(errs, fmt.Errorf("%s.%s[%d] %q: %w", SectionNaaS, list.name, i, p, err))
			}
		}
	}
	if strings.ContainsFunc(n.QualifierImage, unicode.IsSpace) || strings.ContainsFunc(n.ProbeImage, unicode.IsSpace) {
		errs = append(errs, fmt.Errorf("%s image references must not contain whitespace", SectionNaaS))
	}
	return errors.Join(errs...)
}

func validGroupPattern(p string) error {
	switch {
	case p == "":
		return errors.New("is empty")
	case p != strings.TrimSpace(p) || strings.ContainsFunc(p, unicode.IsControl):
		return errors.New("has edge whitespace or control characters")
	case len(p) > 256:
		return errors.New("is longer than 256 characters")
	case strings.Contains(strings.TrimSuffix(p, "*"), "*") || strings.Contains(p, "?"):
		return errors.New(`only a single trailing "*" is a wildcard`)
	}
	return nil
}

// matchGroup reports whether group matches pattern: equal, or starting with
// the prefix of a pattern ending in "*".
func matchGroup(pattern, group string) bool {
	if prefix, ok := strings.CutSuffix(pattern, "*"); ok {
		return strings.HasPrefix(group, prefix)
	}
	return pattern == group
}

// GroupAllowed reports whether a sub-tenant may name group: it matches no
// denied pattern (checked first) and some allowed one.
func (n NaaS) GroupAllowed(group string) bool {
	for _, p := range n.DeniedGroups {
		if matchGroup(p, group) {
			return false
		}
	}
	for _, p := range n.AllowedGroups {
		if matchGroup(p, group) {
			return true
		}
	}
	return false
}

// CheckGroups returns an error wrapping ErrGroupNotAllowed that names the
// first group of lists outside the section and the allowed patterns; nil
// when every group is allowed.
func (n NaaS) CheckGroups(lists ...[]string) error {
	for _, groups := range lists {
		for _, group := range groups {
			if !n.GroupAllowed(group) {
				return fmt.Errorf("%w: %q (allowed: %s)", ErrGroupNotAllowed, group, n.AllowedPatterns())
			}
		}
	}
	return nil
}

// AllowedPatterns is the allowed list as messages show it.
func (n NaaS) AllowedPatterns() string {
	if len(n.AllowedGroups) == 0 {
		return "none; the platform operator has not set naas.allowedGroups"
	}
	return strings.Join(n.AllowedGroups, ", ")
}

// QualifierProbeImage is ProbeImage or its default.
func (n NaaS) QualifierProbeImage() string {
	if n.ProbeImage == "" {
		return DefaultQualifierProbeImage
	}
	return n.ProbeImage
}
