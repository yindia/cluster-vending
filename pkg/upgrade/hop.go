package upgrade

import (
	"fmt"
	"strconv"
	"strings"
)

// minor is a Kubernetes version's major and minor; patch versions do not
// matter to an upgrade between minors.
type minor struct{ major, minor int }

func (m minor) String() string { return fmt.Sprintf("%d.%d", m.major, m.minor) }

// parseMinor reads "1.36" or "1.36.4" (a leading "v" is allowed).
func parseMinor(v string) (minor, bool) {
	parts := strings.Split(strings.TrimPrefix(strings.TrimSpace(v), "v"), ".")
	if len(parts) < 2 || len(parts) > 3 {
		return minor{}, false
	}
	nums := make([]int, len(parts))
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return minor{}, false
		}
		nums[i] = n
	}
	return minor{major: nums[0], minor: nums[1]}, true
}

// NextHop is the minor a cluster running current moves to next on a
// channel that offers channelMinor, among the offered minors: the minor
// after current, when current is below the channel's. ok is false when
// the cluster does not move; why says, in words for the cluster's users,
// why it moves or stays.
func NextHop(current, channelMinor string, offered []string) (next string, ok bool, why string) {
	if strings.TrimSpace(current) == "" {
		return "", false, "the cluster's version is not known yet"
	}
	cur, okCur := parseMinor(current)
	if !okCur {
		return "", false, fmt.Sprintf("cannot read the cluster's version %q", current)
	}
	if strings.TrimSpace(channelMinor) == "" {
		return "", false, "the channel offers no Kubernetes version"
	}
	target, okTarget := parseMinor(channelMinor)
	if !okTarget {
		return "", false, fmt.Sprintf("cannot read the channel's version %q", channelMinor)
	}
	switch {
	case cur.major != target.major:
		return "", false, fmt.Sprintf("runs %s; the channel offers %s", cur, target)
	case cur.minor == target.minor:
		return "", false, fmt.Sprintf("runs the channel's version %s", target)
	case cur.minor > target.minor:
		return "", false, fmt.Sprintf("runs %s, ahead of the channel's %s; a cluster is never downgraded", cur, target)
	}
	hop := minor{major: cur.major, minor: cur.minor + 1}
	if !offers(offered, hop) {
		return "", false, fmt.Sprintf("%s is the next version and is not offered", hop)
	}
	return hop.String(), true, fmt.Sprintf("channel offers %s; upgrading one minor at a time", target)
}

func offers(offered []string, m minor) bool {
	for _, o := range offered {
		if got, ok := parseMinor(o); ok && got == m {
			return true
		}
	}
	return false
}
