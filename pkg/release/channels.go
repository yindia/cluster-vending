package release

import (
	"fmt"
	"strings"
)

// The channels a release puts minors on, newest minor first.
var channelOrder = []string{"alpha", "beta", "stable"}

// assigns reports whether r puts any minor on a channel. A release that
// puts none (published before channels were assigned per minor) has no
// channel minors: nothing is ending and no channel moves a cluster.
func assigns(r Release) bool {
	for _, k := range r.Kubernetes {
		if k.Channel != "" {
			return true
		}
	}
	return false
}

// ChannelMinor is the Kubernetes minor r puts on channel (alpha, beta or
// stable, in any case: the Cluster kind spells them Alpha, Beta, Stable).
// False when r puts no minor on it.
func ChannelMinor(r Release, channel string) (string, bool) {
	channel = strings.ToLower(channel)
	if channel == "" {
		return "", false
	}
	for _, k := range r.Kubernetes {
		if k.Channel == channel {
			return k.Minor, true
		}
	}
	return "", false
}

// Ending reports whether version's minor is offered by r on no channel:
// still supported, but no channel leads to it any more, so its users are
// warned to move. A minor r does not offer is unsupported, not ending; a
// release that assigns no channels marks nothing as ending.
func Ending(r Release, version string) bool {
	if !assigns(r) {
		return false
	}
	k, ok := r.minor(minorOf(version))
	return ok && k.Channel == ""
}

// ChannelProblems are what is wrong with how r puts minors on channels;
// none when it is right. A release puts either no minor on a channel, or
// exactly one minor on each of alpha, beta and stable, with alpha's the
// newest and stable's the oldest. Other minors stay on no channel.
func ChannelProblems(r Release) []string {
	if !assigns(r) {
		return nil
	}
	var problems []string
	on := map[string]string{}
	for _, k := range r.Kubernetes {
		switch {
		case k.Channel == "":
		case !known(k.Channel):
			problems = append(problems, fmt.Sprintf("minor %s is on channel %q: channels are alpha, beta and stable", k.Minor, k.Channel))
		case on[k.Channel] != "":
			problems = append(problems, fmt.Sprintf("channel %s is on %s and %s: one minor per channel", k.Channel, on[k.Channel], k.Minor))
		default:
			on[k.Channel] = k.Minor
		}
	}
	missing := false
	for _, ch := range channelOrder {
		if on[ch] == "" {
			missing = true
			problems = append(problems, fmt.Sprintf("no minor is on channel %s: name one minor per channel, or none at all", ch))
		}
	}
	if missing {
		return problems
	}
	for i := 1; i < len(channelOrder); i++ {
		newer, older := channelOrder[i-1], channelOrder[i]
		if compareMinor(on[older], on[newer]) > 0 {
			problems = append(problems, fmt.Sprintf("channel %s (%s) is newer than %s (%s): alpha, beta and stable go from newest to oldest",
				older, on[older], newer, on[newer]))
		}
	}
	return problems
}

func known(channel string) bool {
	for _, ch := range channelOrder {
		if ch == channel {
			return true
		}
	}
	return false
}
