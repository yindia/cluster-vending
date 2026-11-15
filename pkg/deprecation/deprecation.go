// Package deprecation finds the objects of a workload cluster that were
// written with an API version a Kubernetes release deprecates or removes:
// what `pluto detect-all-in-cluster` finds, and nothing else of Pluto.
//
// The rules are Pluto's data file (data/versions.yaml, FairwindsOps/pluto,
// Apache-2.0), read as it is. An object is found where its original
// manifest is kept: in the kubectl last-applied-configuration annotation
// of a live object, or in a Helm v3 release. Objects made any other way
// (client-side create, server-side apply, operators) keep no manifest
// and are not seen.
package deprecation

import (
	_ "embed"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"

	"sigs.k8s.io/yaml"
)

// DataSource is where data/versions.yaml comes from.
const DataSource = "github.com/FairwindsOps/pluto v5.24.5 versions.yaml"

// ComponentKubernetes is the component of the rules of Kubernetes itself:
// the only one judged against the cluster's target version. The others
// (cert-manager, istio) are judged against the versions the data file
// names for them, as Pluto does.
const ComponentKubernetes = "k8s"

//go:embed data/versions.yaml
var data []byte

// Rule is one API version of a kind and the releases (of its component)
// that deprecate and remove it. An empty Kind is every kind of the API
// version; an empty release is "not yet".
type Rule struct {
	APIVersion             string `json:"version"`
	Kind                   string `json:"kind"`
	DeprecatedIn           string `json:"deprecated-in"`
	RemovedIn              string `json:"removed-in"`
	ReplacementAPI         string `json:"replacement-api"`
	ReplacementAvailableIn string `json:"replacement-available-in"`
	Component              string `json:"component"`
}

// RuleSet is the data file: the rules, and the version each component
// other than Kubernetes is judged against.
type RuleSet struct {
	Rules          []Rule            `json:"deprecated-versions"`
	TargetVersions map[string]string `json:"target-versions"`
}

// Parse reads a Pluto versions file.
func Parse(raw []byte) (RuleSet, error) {
	var set RuleSet
	if err := yaml.UnmarshalStrict(raw, &set); err != nil {
		return RuleSet{}, fmt.Errorf("deprecation rules: %w", err)
	}
	for i, r := range set.Rules {
		if r.APIVersion == "" || r.Component == "" {
			return RuleSet{}, fmt.Errorf("deprecation rules: rule %d has no version or component", i)
		}
		for _, v := range []string{r.DeprecatedIn, r.RemovedIn} {
			if _, ok := parseVersion(v); v != "" && !ok {
				return RuleSet{}, fmt.Errorf("deprecation rules: rule %d (%s %s): %q is not a version", i, r.APIVersion, r.Kind, v)
			}
		}
	}
	return set, nil
}

var defaultRules = sync.OnceValues(func() (RuleSet, error) { return Parse(data) })

// Default is the shipped rule set.
func Default() (RuleSet, error) { return defaultRules() }

// NewestRemoval is the newest Kubernetes minor ("1.36") the rules know a
// removal in. For a newer target the rules may simply not know yet: the
// data file is refreshed with each Pluto release.
func (s RuleSet) NewestRemoval() string {
	newest := version{}
	for _, r := range s.Rules {
		if v, ok := parseVersion(r.RemovedIn); ok && r.Component == ComponentKubernetes && v.compare(newest) > 0 {
			newest = v
		}
	}
	return fmt.Sprintf("%d.%d", newest.major, newest.minor)
}

// NoKnownRemovals are the Kubernetes minors the platform offers in which
// the data file names no removal, each with what that rests on. A minor a
// release offers must have removals in the data or stand here: an empty
// result for it is then a looked-at fact, not a hole in the data. Check
// the Kubernetes deprecation guide for the minor when adding one, and
// again after `make deprecation-rules`.
var NoKnownRemovals = map[string]string{
	"1.37": "Pluto v5.24.5 names no API removed in 1.37 (its newest removals are in 1.36)",
}

// RemovalsIn is how many Kubernetes API versions the rules remove in a
// minor ("1.36").
func (s RuleSet) RemovalsIn(minor string) int {
	at, ok := parseVersion(minor)
	if !ok {
		return 0
	}
	n := 0
	for _, r := range s.Rules {
		if v, ok := parseVersion(r.RemovedIn); ok && r.Component == ComponentKubernetes && v.major == at.major && v.minor == at.minor {
			n++
		}
	}
	return n
}

// rule is the rule of an object's API version and kind; nil for none.
func (s RuleSet) rule(apiVersion, kind string) *Rule {
	for i := range s.Rules {
		if r := &s.Rules[i]; r.APIVersion == apiVersion && (r.Kind == "" || r.Kind == kind) {
			return r
		}
	}
	return nil
}

// Source is where an object's manifest was read.
type Source string

const (
	// SourceManifest is the last-applied-configuration annotation of a
	// live object.
	SourceManifest Source = "Manifest"
	// SourceHelmRelease is the manifest of a deployed Helm release.
	SourceHelmRelease Source = "HelmRelease"
)

// Object is one object as its manifest declares it.
type Object struct {
	APIVersion, Kind, Namespace, Name string
	Source                            Source
	// HelmRelease is "<namespace>/<name>" of the release, for
	// SourceHelmRelease.
	HelmRelease string
}

// Finding is an object written with a deprecated or removed API version.
type Finding struct {
	Namespace, Kind, Name, APIVersion string
	Component                         string
	// DeprecatedIn and RemovedIn are releases of Component ("v1.25.0");
	// empty for "not yet".
	DeprecatedIn, RemovedIn string
	ReplacementAPI          string
	ReplacementAvailableIn  string
	Source                  Source
	HelmRelease             string
	// Removed: the API version is gone at the target (the object cannot
	// be applied again as written); else it is only deprecated.
	Removed bool
}

// Evaluate returns the objects whose API version is deprecated or removed
// at the Kubernetes minor target ("1.37"): removed first, then by
// namespace, kind, name, API version and source. The same object from the
// same source is reported once.
func (s RuleSet) Evaluate(objects []Object, target string) ([]Finding, error) {
	at, ok := parseVersion(target)
	if !ok {
		return nil, fmt.Errorf("target version %q is not a Kubernetes version", target)
	}
	seen := map[Object]bool{}
	var out []Finding
	for _, o := range objects {
		r := s.rule(o.APIVersion, o.Kind)
		if r == nil || seen[o] {
			continue
		}
		seen[o] = true
		judged := at
		if r.Component != ComponentKubernetes {
			if judged, ok = parseVersion(s.TargetVersions[r.Component]); !ok {
				continue
			}
		}
		deprecated, removed := reached(judged, r.DeprecatedIn), reached(judged, r.RemovedIn)
		if !deprecated && !removed {
			continue
		}
		out = append(out, Finding{Namespace: o.Namespace, Kind: o.Kind, Name: o.Name, APIVersion: o.APIVersion,
			Component: r.Component, DeprecatedIn: r.DeprecatedIn, RemovedIn: r.RemovedIn,
			ReplacementAPI: r.ReplacementAPI, ReplacementAvailableIn: r.ReplacementAvailableIn,
			Source: o.Source, HelmRelease: o.HelmRelease, Removed: removed})
	}
	slices.SortStableFunc(out, compareFindings)
	return out, nil
}

func compareFindings(a, b Finding) int {
	if a.Removed != b.Removed {
		if a.Removed {
			return -1
		}
		return 1
	}
	for _, pair := range [][2]string{{a.Namespace, b.Namespace}, {a.Kind, b.Kind}, {a.Name, b.Name},
		{a.APIVersion, b.APIVersion}, {string(a.Source), string(b.Source)}, {a.HelmRelease, b.HelmRelease}} {
		if c := strings.Compare(pair[0], pair[1]); c != 0 {
			return c
		}
	}
	return 0
}

// reached: the release named by in is at or before at; never for "".
func reached(at version, in string) bool {
	v, ok := parseVersion(in)
	return ok && at.compare(v) >= 0
}

// NextMinor is the minor after a Kubernetes version: "1.36" or "1.36.2"
// gives "1.37".
func NextMinor(current string) (string, error) {
	v, ok := parseVersion(current)
	if !ok {
		return "", fmt.Errorf("%q is not a Kubernetes version", current)
	}
	return fmt.Sprintf("%d.%d", v.major, v.minor+1), nil
}

// Target is the Kubernetes minor a cluster on current is judged against:
// upgradeTo (what its release moves that minor to) when set, else the
// next minor.
func Target(current, upgradeTo string) (string, error) {
	if _, ok := parseVersion(upgradeTo); ok {
		return upgradeTo, nil
	}
	return NextMinor(current)
}

type version struct{ major, minor, patch int }

func (v version) compare(o version) int {
	for _, pair := range [][2]int{{v.major, o.major}, {v.minor, o.minor}, {v.patch, o.patch}} {
		if pair[0] != pair[1] {
			if pair[0] < pair[1] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// parseVersion reads "1.37", "v1.37" or "v1.37.2" (anything after the
// patch, like "-eks-1", is ignored).
func parseVersion(s string) (version, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) < 2 || len(parts) > 3 {
		return version{}, false
	}
	var n [3]int
	for i, p := range parts {
		x, err := strconv.Atoi(p)
		if err != nil || x < 0 {
			return version{}, false
		}
		n[i] = x
	}
	return version{n[0], n[1], n[2]}, true
}
