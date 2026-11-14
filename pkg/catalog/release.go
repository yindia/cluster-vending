package catalog

import (
	_ "embed"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"sigs.k8s.io/yaml"
)

//go:embed release.yaml
var releaseFile []byte

// Release is release.yaml: the versions this build ships. It is plain
// data; the controller turns it into the shipped Release and the
// ReleaseChannels.
type Release struct {
	// Kubernetes are the minors on offer, newest first.
	Kubernetes []KubernetesVersion `json:"kubernetes"`
	// Components is the chart version of each component, on every minor.
	Components map[string]string `json:"components"`
	// Channels are the channels a new management cluster gets, in order.
	Channels []Channel `json:"channels"`
}

// KubernetesVersion is one Kubernetes minor on offer.
type KubernetesVersion struct {
	// Minor, e.g. "1.37".
	Minor string `json:"minor"`
	// Default marks the minor a cluster on no channel gets when it names
	// none. A cluster on a channel gets the channel's minor (Channel).
	Default bool `json:"default,omitempty"`
	// Channel is the channel (alpha, beta or stable, one of Channels)
	// whose clusters run this minor: each channel is on exactly one minor,
	// alpha's the newest and stable's the oldest. A minor on no channel is
	// still supported; its users are warned to move. Moving a channel is
	// editing this line.
	Channel string `json:"channel,omitempty"`
	// UpgradeTo is the minor an automatic cluster moves to next.
	UpgradeTo string `json:"upgradeTo,omitempty"`
	// EndOfSupport is the day (YYYY-MM-DD, UTC) support for the minor ends.
	EndOfSupport string `json:"endOfSupport"`
	// Values are locked Helm values of a component on this minor only.
	Values map[string]map[string]any `json:"values,omitempty"`
}

// Channel is a release channel a new management cluster gets.
type Channel struct {
	// Name: alpha, beta or stable.
	Name string `json:"name"`
	// Promotion: Pinned (offers this build's release) or Automatic
	// (follows the channel before it).
	Promotion string `json:"promotion"`
}

// Channel promotions release.yaml may name.
const (
	PromotionPinned    = "Pinned"
	PromotionAutomatic = "Automatic"
)

const dayLayout = "2006-01-02"

var minorPattern = regexp.MustCompile(`^1\.[0-9]{2}$`)

// ShippedRelease is release.yaml, checked.
func ShippedRelease() (Release, error) { return ParseRelease(releaseFile) }

// ParseRelease reads a release file. A key the file may not have is an
// error, and every problem is reported together.
func ParseRelease(raw []byte) (Release, error) {
	var r Release
	if err := yaml.UnmarshalStrict(raw, &r); err != nil {
		return Release{}, fmt.Errorf("release.yaml: %w", err)
	}
	if err := r.check(); err != nil {
		return Release{}, fmt.Errorf("release.yaml: %w", err)
	}
	return r, nil
}

// EndOfSupportTime is the minor's end of support: midnight UTC of the day.
func (k KubernetesVersion) EndOfSupportTime() (time.Time, error) {
	return time.ParseInLocation(dayLayout, k.EndOfSupport, time.UTC)
}

// ComponentNames are the components with a version, sorted.
func (r Release) ComponentNames() []string {
	out := make([]string, 0, len(r.Components))
	for name := range r.Components {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func (r Release) check() error {
	var errs []error
	if len(r.Kubernetes) == 0 {
		errs = append(errs, errors.New("kubernetes: at least one minor"))
	}
	if len(r.Components) == 0 {
		errs = append(errs, errors.New("components: at least one"))
	}
	minors, defaults := map[string]bool{}, 0
	for _, k := range r.Kubernetes {
		if !minorPattern.MatchString(k.Minor) {
			errs = append(errs, fmt.Errorf("kubernetes: minor %q is not like 1.37", k.Minor))
		}
		if minors[k.Minor] {
			errs = append(errs, fmt.Errorf("kubernetes: minor %s is listed twice", k.Minor))
		}
		minors[k.Minor] = true
		if k.Default {
			defaults++
		}
		if _, err := k.EndOfSupportTime(); err != nil {
			errs = append(errs, fmt.Errorf("kubernetes %s: endOfSupport %q is not a day like 2028-01-31", k.Minor, k.EndOfSupport))
		}
		for name := range k.Values {
			if _, ok := r.Components[name]; !ok {
				errs = append(errs, fmt.Errorf("kubernetes %s: values for %s, which has no version under components", k.Minor, name))
			}
		}
	}
	if defaults != 1 && len(r.Kubernetes) > 0 {
		errs = append(errs, fmt.Errorf("kubernetes: exactly one minor is the default, %d are", defaults))
	}
	for _, k := range r.Kubernetes {
		if k.UpgradeTo != "" && !minors[k.UpgradeTo] {
			errs = append(errs, fmt.Errorf("kubernetes %s: upgradeTo %s is not on offer", k.Minor, k.UpgradeTo))
		}
	}
	errs = append(errs, r.channelProblems()...)
	for name, version := range r.Components {
		if version == "" {
			errs = append(errs, fmt.Errorf("components: %s has no version", name))
		}
	}
	names := map[string]bool{}
	for _, ch := range r.Channels {
		if ch.Name != "alpha" && ch.Name != "beta" && ch.Name != "stable" {
			errs = append(errs, fmt.Errorf("channels: %q is not alpha, beta or stable", ch.Name))
		}
		if names[ch.Name] {
			errs = append(errs, fmt.Errorf("channels: %s is listed twice", ch.Name))
		}
		names[ch.Name] = true
		if ch.Promotion != PromotionPinned && ch.Promotion != PromotionAutomatic {
			errs = append(errs, fmt.Errorf("channels: %s promotion %q is not Pinned or Automatic", ch.Name, ch.Promotion))
		}
	}
	return errors.Join(errs...)
}

// channelProblems: each channel under channels is on exactly one minor, a
// minor is on a channel the file has, and the channels go from the newest
// minor (alpha) to the oldest (stable).
func (r Release) channelProblems() []error {
	var errs []error
	listed := map[string]bool{}
	for _, ch := range r.Channels {
		listed[ch.Name] = true
	}
	on := map[string]string{}
	for _, k := range r.Kubernetes {
		switch {
		case k.Channel == "":
		case !listed[k.Channel]:
			errs = append(errs, fmt.Errorf("kubernetes %s: channel %s is not under channels", k.Minor, k.Channel))
		case on[k.Channel] != "":
			errs = append(errs, fmt.Errorf("kubernetes: channel %s is on %s and %s: one minor per channel", k.Channel, on[k.Channel], k.Minor))
		default:
			on[k.Channel] = k.Minor
		}
	}
	var present []string
	for _, name := range []string{"alpha", "beta", "stable"} {
		switch {
		case !listed[name]:
		case on[name] == "":
			errs = append(errs, fmt.Errorf("kubernetes: channel %s is on no minor: put channel: %s on one", name, name))
		default:
			present = append(present, name)
		}
	}
	for i := 1; i < len(present); i++ {
		newer, older := present[i-1], present[i]
		if minorValue(on[older]) > minorValue(on[newer]) {
			errs = append(errs, fmt.Errorf("kubernetes: channel %s (%s) is newer than %s (%s): alpha, beta and stable go from the newest minor to the oldest",
				older, on[older], newer, on[newer]))
		}
	}
	return errs
}

// minorValue is 37 for "1.37"; -1 for anything else.
func minorValue(minor string) int {
	rest, ok := strings.CutPrefix(minor, "1.")
	n, err := strconv.Atoi(rest)
	if !ok || err != nil {
		return -1
	}
	return n
}
