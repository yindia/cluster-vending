// Package release holds what a release offers (its Kubernetes minors,
// each with the channel it is for and the platform components' versions)
// and the checks of a release's content (catalog.go, channels.go).
//
// It is pure: no client, no store, no Kubernetes or contract types.
// EKS add-on versions are not here: AWS decides them for the cluster's
// Kubernetes version.
package release

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Component is one platform-installed component and its version.
type Component struct {
	Name, Version string
}

// Minor is what a release offers on one Kubernetes minor.
type Minor struct {
	// Minor is "1.34".
	Minor string
	// Default marks the minor a new cluster gets when it names none.
	Default bool
	// UpgradeTo moves clusters on this minor to the next one; empty: they
	// stay. Only a move of exactly one minor is followed.
	UpgradeTo string
	// Channel is the channel (alpha, beta or stable) whose clusters this
	// minor is for; empty: none (still supported, its users are warned:
	// Ending).
	Channel string
	// EndOfSupport is when this minor stops being supported; zero: not set.
	EndOfSupport time.Time
	Components   []Component
}

// Release is a published, immutable release.
type Release struct {
	Name          string
	RenderVersion int64
	Kubernetes    []Minor
}

// minor returns the release's entry for the Kubernetes minor m.
func (r Release) minor(m string) (Minor, bool) {
	for _, k := range r.Kubernetes {
		if k.Minor == m {
			return k, true
		}
	}
	return Minor{}, false
}

// components are r's component versions on minor m.
func components(r Release, m string) map[string]string {
	k, ok := r.minor(m)
	if !ok || len(k.Components) == 0 {
		return nil
	}
	out := make(map[string]string, len(k.Components))
	for _, c := range k.Components {
		out[c.Name] = c.Version
	}
	return out
}

// minorOf is "1.34" for "1.34" or "1.34.2".
func minorOf(v string) string {
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return v
	}
	return parts[0] + "." + parts[1]
}

// parseMinor splits "1.34" into 1 and 34.
func parseMinor(v string) (int, int, error) {
	parts := strings.SplitN(minorOf(v), ".", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("version %q is not <major>.<minor>", v)
	}
	major, err1 := strconv.Atoi(parts[0])
	minor, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return 0, 0, fmt.Errorf("version %q is not <major>.<minor>", v)
	}
	return major, minor, nil
}

// compareMinor orders two minors; an unparsable one sorts first.
func compareMinor(a, b string) int {
	am, an, errA := parseMinor(a)
	bm, bn, errB := parseMinor(b)
	switch {
	case errA != nil && errB != nil:
		return strings.Compare(a, b)
	case errA != nil:
		return -1
	case errB != nil:
		return 1
	case am != bm:
		return am - bm
	default:
		return an - bn
	}
}
