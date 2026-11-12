// Package deprecation finds the objects of a Kubernetes cluster that were
// written with an API version Kubernetes deprecated and later stopped
// serving, the way kube-no-trouble (kubent) does.
//
// The API server converts an object to whatever version it is asked for,
// so the version an object was written with is not on the object. It is
// read from the two places that keep it: the
// kubectl.kubernetes.io/last-applied-configuration annotation and the
// manifests of deployed Helm releases. Objects written another way
// (server-side apply, a client library) are not found.
//
// Rules is the table of deprecated versions; Scanner reads a cluster and
// matches what it finds against it. The package knows nothing of the
// platform: it works on any cluster it is given a client for.
package deprecation

import (
	"fmt"
	"strconv"
	"strings"
)

// Version is a Kubernetes minor version: 31 is "1.31".
type Version int

// ParseVersion reads a Kubernetes version as its minor version. It takes
// what clusters and people write: "1.31", "v1.31.2", "v1.31.2-eks-7f9249a"
// and the "31+" a /version endpoint reports as its minor (with major "1").
func ParseVersion(s string) (Version, error) {
	rest := strings.TrimPrefix(strings.TrimSpace(s), "v")
	major, minor, found := strings.Cut(rest, ".")
	if !found {
		return 0, fmt.Errorf("kubernetes version %q: want 1.<minor>", s)
	}
	if major != "1" {
		return 0, fmt.Errorf("kubernetes version %q: major version %q is not 1", s, major)
	}
	return parseMinor(s, minor)
}

// ParseMinor reads the minor version a /version endpoint reports, e.g.
// "31" or "31+".
func ParseMinor(minor string) (Version, error) { return parseMinor(minor, minor) }

// parseMinor reads the leading digits of minor; whole is what the error
// quotes.
func parseMinor(whole, minor string) (Version, error) {
	end := 0
	for end < len(minor) && minor[end] >= '0' && minor[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, fmt.Errorf("kubernetes version %q: no minor version", whole)
	}
	n, err := strconv.Atoi(minor[:end])
	if err != nil {
		return 0, fmt.Errorf("kubernetes version %q: %w", whole, err)
	}
	return Version(n), nil
}

// String is "1.<minor>".
func (v Version) String() string { return "1." + strconv.Itoa(int(v)) }

// Source is where an object's written version was found.
type Source int

// Sources.
const (
	// SourceLastApplied is the object's
	// kubectl.kubernetes.io/last-applied-configuration annotation.
	SourceLastApplied Source = iota + 1
	// SourceHelmRelease is the manifest of a deployed Helm release.
	SourceHelmRelease
)

// Object is an object as it was written: the apiVersion and kind of its
// manifest, not of what the API server serves.
type Object struct {
	APIVersion string
	Kind       string
	// Namespace is empty for cluster-scoped kinds, and for an object of a
	// Helm manifest that names none (it is installed into the release's).
	Namespace string
	Name      string
	Source    Source
	// HelmRelease is "<namespace>/<name>" of the release whose manifest
	// holds the object; set for SourceHelmRelease.
	HelmRelease string
}

// Rule is one deprecated version of a kind.
type Rule struct {
	// APIVersion is the deprecated "<group>/<version>".
	APIVersion string
	Kind       string
	// Replacement is the apiVersion to move to; empty when the kind was
	// removed without one.
	Replacement string
	// DeprecatedIn is the release that deprecated APIVersion; RemovedIn the
	// first release that does not serve it.
	DeprecatedIn Version
	RemovedIn    Version
}

// Finding is an object written with a deprecated version, and the rule
// that says so.
type Finding struct {
	Object Object
	Rule   Rule
}

// Result is what a scan of a cluster found.
type Result struct {
	// Version is the version the cluster runs.
	Version Version
	// Findings are ordered by Rule.RemovedIn, then the object's kind,
	// namespace, name and source: what breaks first comes first.
	Findings []Finding
}
