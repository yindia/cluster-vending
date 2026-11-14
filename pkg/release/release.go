// Package release decides what a cluster should run, given the releases an
// operator published, the channels that offer them, and an optional hold:
// the release it follows, its Kubernetes minor, the versions of the
// platform's own components, the renderer version, whether its minor is
// still supported, and why (docs/design/release-admin.md, section 5).
//
// A manual cluster keeps its Kubernetes and component versions, but still
// gets platform fixes: its renderer version is that of the release its
// channel offers. Manual protects versions, not bugs.
//
// It is pure: no client, no store, no Kubernetes or contract types. The
// callers (the cluster reconciler, the channel controller, the API's
// checks) read the release objects and the cluster and pass plain values.
//
// What it deliberately does not decide:
//   - When a cluster moves. The rollout order (channels alpha, beta,
//     stable; within a channel tier 2, then 1, then 0), concurrency, soak
//     and failure budgets belong to the channel controller. The tier is
//     therefore not an input here: it orders the rollout, not its target.
//   - EKS add-on versions. AWS decides those for the cluster's Kubernetes
//     version; a release lists only components the platform installs
//     itself, each with a version.
package release

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// UpgradeMode is how a cluster's versions change.
type UpgradeMode string

// Upgrade modes.
const (
	// Manual: the tenant's versions stand; channels do not apply. The
	// default: an empty mode is Manual.
	Manual UpgradeMode = "Manual"
	// Automatic: the cluster follows the release its channel offers.
	Automatic UpgradeMode = "Automatic"
)

// DefaultChannel is the channel of a cluster that names none.
const DefaultChannel = "stable"

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
	// Withdrawn: never offered again; clusters on it move to their
	// channel's release.
	Withdrawn bool
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

// defaultMinor is the minor a new cluster gets.
func (r Release) defaultMinor() string {
	for _, k := range r.Kubernetes {
		if k.Default {
			return k.Minor
		}
	}
	return ""
}

// Channel is one release channel as the operator set it.
type Channel struct {
	Name string
	// Offered is the release the channel offers now; empty: none.
	Offered string
	// Paused: clusters on the channel do not move.
	Paused bool
}

// Hold keeps one cluster where it is or on one release; only the operator
// sets it.
type Hold struct {
	// Release keeps the cluster on that release; empty: frozen where it
	// is.
	Release string
	// Until ends the hold; zero: no end.
	Until time.Time
}

// ClusterFacts is what the decision needs to know about the cluster.
type ClusterFacts struct {
	// Mode; empty is Manual.
	Mode UpgradeMode
	// Channel; empty is DefaultChannel.
	Channel string
	// CurrentKubernetes is the running minor; empty for a new cluster.
	CurrentKubernetes string
	// SpecKubernetes is what the tenant wrote; may be empty.
	SpecKubernetes string
	// CurrentRelease is the release last applied; may be empty.
	CurrentRelease string
	Now            time.Time
}

// Code says why a decision was taken, or what to warn about.
type Code string

// Codes of Decision.Notes.
const (
	NoReleases       Code = "NoReleases"       // nothing published: the cluster behaves as without releases
	ManualMode       Code = "Manual"           // the tenant's versions stand
	RendererFixes    Code = "RendererFixes"    // a manual cluster still gets the offered release's renderer
	Following        Code = "Following"        // the channel's release
	Held             Code = "Held"             // a hold decides
	HoldExpired      Code = "HoldExpired"      // a hold ended and is ignored
	ChannelPaused    Code = "ChannelPaused"    // the channel does not move clusters
	UnknownChannel   Code = "UnknownChannel"   // the channel does not exist
	NothingOffered   Code = "NothingOffered"   // the channel offers no usable release
	Withdrawn        Code = "Withdrawn"        // a release involved was withdrawn
	NoDowngrade      Code = "NoDowngrade"      // newer than the release's minors: kept
	Unsupported      Code = "Unsupported"      // the minor is not among the release's
	PastEndOfSupport Code = "PastEndOfSupport" // the minor's end of support has passed
	InvalidUpgrade   Code = "InvalidUpgrade"   // an upgradeTo of more than one minor: ignored
)

// Note is a reason or a warning.
type Note struct {
	Code    Code
	Message string
}

// Decision is what the cluster should run, and why.
type Decision struct {
	// Release the cluster follows; empty for a manual cluster or when no
	// release applies.
	Release string
	// Kubernetes is the target minor; empty: leave it as it is (EKS
	// chooses for a new cluster).
	Kubernetes string
	// Components are the platform components' target versions, by name;
	// nil for a manual cluster (its versions are not moved).
	Components map[string]string
	// RenderVersion clusters are rendered with: the followed release's, or
	// for a manual cluster the offered release's; 0: none (the running
	// build's).
	RenderVersion int64
	// Supported reports whether the cluster's minor is among the release's
	// and before its end of support. True when no release applies.
	Supported bool
	// EndOfSupport of the cluster's minor; zero when unknown.
	EndOfSupport time.Time
	Notes        []Note
}

func (d *Decision) note(code Code, format string, args ...any) {
	d.Notes = append(d.Notes, Note{Code: code, Message: fmt.Sprintf(format, args...)})
}

// Has reports whether the decision carries a note of code c.
func (d Decision) Has(c Code) bool {
	return slices.ContainsFunc(d.Notes, func(n Note) bool { return n.Code == c })
}

// Resolve decides what the cluster should run:
//
//   - No release published: the cluster behaves as without releases.
//   - Manual: the tenant's versions stand (spec, else running, else the
//     offered release's default minor for a new cluster) and component
//     versions are not moved; no release is followed. Manual protects
//     versions, not bugs: the renderer version is the one of the release
//     the cluster's channel offers (stable when unset), so platform fixes
//     still arrive. Support and end of support are reported against that
//     release.
//   - Automatic: the channel's offered release, unless a hold applies (its
//     release, or frozen on the current one), or the channel is paused,
//     unknown or offers nothing usable (the current release and the
//     running minor stay: nothing moves). A
//     withdrawn current release is left for the offered one, except while
//     the channel is paused: a pause stops every move.
//   - Kubernetes, on a followed release: a minor the release has stays, or
//     moves to its upgradeTo when that is exactly one minor up; a minor
//     newer than all of the release's stays (never downgraded); one older
//     than all moves up one minor; a new cluster gets the release's
//     default minor, or the spec's when it named one.
func Resolve(facts ClusterFacts, releases map[string]Release, channels map[string]Channel, hold *Hold) Decision {
	if len(releases) == 0 {
		d := Decision{Kubernetes: first(facts.SpecKubernetes, facts.CurrentKubernetes), Supported: true}
		d.note(NoReleases, "no release is published")
		return d
	}
	if facts.Mode != Automatic {
		return resolveManual(facts, releases, channels)
	}
	return resolveAutomatic(facts, releases, channels, hold)
}

// resolveManual: the tenant's versions; the offered release only for
// support and the default minor of a new cluster.
func resolveManual(facts ClusterFacts, releases map[string]Release, channels map[string]Channel) Decision {
	d := Decision{Kubernetes: first(facts.SpecKubernetes, facts.CurrentKubernetes), Supported: true}
	d.note(ManualMode, "upgrade mode is manual: the tenant's versions stand")
	offered, ok := offeredRelease(facts, releases, channels, &d)
	if !ok {
		return d
	}
	if d.Kubernetes == "" {
		d.Kubernetes = offered.defaultMinor()
	}
	// Manual protects versions, not bugs: platform fixes still arrive.
	d.RenderVersion = offered.RenderVersion
	d.note(RendererFixes, "renderer version %d of release %q applies: manual keeps versions, not bugs",
		offered.RenderVersion, offered.Name)
	support(&d, offered, d.Kubernetes, facts.Now)
	return d
}

// resolveAutomatic: the release the cluster follows, then its versions.
func resolveAutomatic(facts ClusterFacts, releases map[string]Release, channels map[string]Channel, hold *Hold) Decision {
	d := Decision{Supported: true}
	target, moving, ok := followed(facts, releases, channels, hold, &d)
	if !ok {
		d.Kubernetes = first(facts.CurrentKubernetes, facts.SpecKubernetes)
		return d
	}
	d.Release, d.RenderVersion = target.Name, target.RenderVersion
	if moving {
		d.Kubernetes = kubernetesTarget(facts, target, &d)
	} else {
		// Staying (paused, frozen, nothing to move to): not even the
		// minor moves.
		d.Kubernetes = first(facts.CurrentKubernetes, facts.SpecKubernetes)
	}
	d.Components = components(target, d.Kubernetes)
	support(&d, target, d.Kubernetes, facts.Now)
	return d
}

// followed is the release an automatic cluster follows, if any, and
// whether it moves on it (false: it stays where it is).
func followed(facts ClusterFacts, releases map[string]Release, channels map[string]Channel, hold *Hold, d *Decision) (Release, bool, bool) {
	current, hasCurrent := releases[facts.CurrentRelease]
	if hold != nil {
		if hold.Until.IsZero() || facts.Now.Before(hold.Until) {
			return heldRelease(hold, current, hasCurrent, releases, d)
		}
		d.note(HoldExpired, "the hold ended at %s and is ignored", hold.Until.Format(time.RFC3339))
	}
	stay := func() (Release, bool, bool) {
		if hasCurrent && !current.Withdrawn {
			return current, false, true
		}
		return Release{}, false, false
	}
	name := first(facts.Channel, DefaultChannel)
	ch, ok := channels[name]
	if !ok {
		d.note(UnknownChannel, "channel %q does not exist: the cluster stays where it is", name)
		return stay()
	}
	if ch.Paused {
		d.note(ChannelPaused, "channel %q is paused: the cluster stays where it is", name)
		if hasCurrent && current.Withdrawn {
			d.note(Withdrawn, "release %q is withdrawn; it is left once the channel resumes", current.Name)
		}
		return stay()
	}
	offered, ok := releases[ch.Offered]
	switch {
	case !ok:
		d.note(NothingOffered, "channel %q offers no published release: the cluster stays where it is", name)
		return stay()
	case offered.Withdrawn:
		d.note(Withdrawn, "channel %q offers withdrawn release %q: the cluster stays where it is", name, offered.Name)
		return stay()
	}
	if hasCurrent && current.Withdrawn && current.Name != offered.Name {
		d.note(Withdrawn, "release %q is withdrawn: the cluster moves to %q", current.Name, offered.Name)
	}
	d.note(Following, "channel %q offers release %q", name, offered.Name)
	return offered, true, true
}

// heldRelease is the release a hold keeps the cluster on, whether it
// moves on it (a hold that names a release: as that release says), and
// whether there is one.
func heldRelease(hold *Hold, current Release, hasCurrent bool, releases map[string]Release, d *Decision) (Release, bool, bool) {
	if hold.Release == "" {
		d.note(Held, "held: frozen where it is")
		return current, false, hasCurrent
	}
	r, ok := releases[hold.Release]
	switch {
	case !ok:
		d.note(Held, "held on release %q, which is not published: frozen where it is", hold.Release)
		return current, false, hasCurrent
	case r.Withdrawn:
		d.note(Held, "held on release %q, which is withdrawn: frozen where it is", hold.Release)
		return current, false, hasCurrent
	}
	d.note(Held, "held on release %q", hold.Release)
	return r, true, true
}

// offeredRelease is the release the cluster's channel offers, for the
// checks a manual cluster still gets.
func offeredRelease(facts ClusterFacts, releases map[string]Release, channels map[string]Channel, d *Decision) (Release, bool) {
	name := first(facts.Channel, DefaultChannel)
	ch, ok := channels[name]
	if !ok {
		d.note(UnknownChannel, "channel %q does not exist", name)
		return Release{}, false
	}
	r, ok := releases[ch.Offered]
	if !ok || r.Withdrawn {
		d.note(NothingOffered, "channel %q offers no usable release", name)
		return Release{}, false
	}
	return r, true
}

// kubernetesTarget is the minor an automatic cluster moves to on release
// r.
func kubernetesTarget(facts ClusterFacts, r Release, d *Decision) string {
	base := first(facts.CurrentKubernetes, facts.SpecKubernetes)
	if base == "" {
		return r.defaultMinor()
	}
	if m, ok := r.minor(base); ok {
		if m.UpgradeTo == "" {
			return base
		}
		if next, err := nextMinor(base); err == nil && next == m.UpgradeTo {
			return m.UpgradeTo
		}
		d.note(InvalidUpgrade, "release %q moves %s to %s, not one minor up: ignored", r.Name, base, m.UpgradeTo)
		return base
	}
	lowest, highest, ok := bounds(r)
	if !ok {
		return base
	}
	switch {
	case compareMinor(base, highest) > 0:
		d.note(NoDowngrade, "%s is newer than release %q's minors: never downgraded", base, r.Name)
		return base
	case compareMinor(base, lowest) < 0:
		next, err := nextMinor(base)
		if err != nil {
			return base
		}
		d.note(Unsupported, "%s is older than release %q supports: moved up one minor to %s", base, r.Name, next)
		return next
	default:
		d.note(Unsupported, "%s is not offered by release %q: it stays", base, r.Name)
		return base
	}
}

// support fills Supported and EndOfSupport for minor m on release r.
func support(d *Decision, r Release, m string, now time.Time) {
	if m == "" {
		return
	}
	k, ok := r.minor(m)
	if !ok {
		d.Supported = false
		if !d.Has(Unsupported) && !d.Has(NoDowngrade) {
			d.note(Unsupported, "%s is not among release %q's supported minors", m, r.Name)
		}
		return
	}
	d.EndOfSupport = k.EndOfSupport
	if !k.EndOfSupport.IsZero() && !now.Before(k.EndOfSupport) {
		d.Supported = false
		d.note(PastEndOfSupport, "%s reached end of support on %s", m, k.EndOfSupport.Format("2006-01-02"))
	}
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

// CheckVersion reports, as an error whose message suits an
// INVALID_ARGUMENT answer, a requested Kubernetes version that the release
// offered on channel (empty: DefaultChannel) does not support. Nil when
// nothing is requested, nothing is published, or the channel offers no
// usable release (there is nothing to check against). A patch version
// ("1.34.2") is checked by its minor.
func CheckVersion(releases map[string]Release, channels map[string]Channel, channel, requested string) error {
	if requested == "" || len(releases) == 0 {
		return nil
	}
	name := first(channel, DefaultChannel)
	ch, ok := channels[name]
	if !ok {
		return nil
	}
	r, ok := releases[ch.Offered]
	if !ok || r.Withdrawn {
		return nil
	}
	if _, ok := r.minor(minorOf(requested)); ok {
		return nil
	}
	offered := make([]string, 0, len(r.Kubernetes))
	for _, k := range r.Kubernetes {
		offered = append(offered, k.Minor)
	}
	return fmt.Errorf("kubernetes version %s is not supported: release %s on channel %s supports %s",
		requested, r.Name, name, strings.Join(offered, ", "))
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

// nextMinor is the minor after v ("1.34" → "1.35").
func nextMinor(v string) (string, error) {
	major, minor, err := parseMinor(v)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%d.%d", major, minor+1), nil
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

// bounds are the lowest and highest minors of r.
func bounds(r Release) (string, string, bool) {
	if len(r.Kubernetes) == 0 {
		return "", "", false
	}
	lowest, highest := r.Kubernetes[0].Minor, r.Kubernetes[0].Minor
	for _, k := range r.Kubernetes[1:] {
		if compareMinor(k.Minor, lowest) < 0 {
			lowest = k.Minor
		}
		if compareMinor(k.Minor, highest) > 0 {
			highest = k.Minor
		}
	}
	return lowest, highest, true
}

// first is the first non-empty value.
func first(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
