package release_test

import (
	"reflect"
	"testing"

	"github.com/yindia/cluster-vending/pkg/release"
)

// onChannels is a release offering 1.35 to 1.37 with the given channel per
// minor ("" leaves the minor on none).
func onChannels(c135, c136, c137 string) release.Release {
	return release.Release{Name: "r", Kubernetes: []release.Minor{
		{Minor: "1.37", Default: true, Channel: c137},
		{Minor: "1.36", Channel: c136},
		{Minor: "1.35", Channel: c135},
	}}
}

func TestChannelMinor(t *testing.T) {
	r := onChannels("stable", "beta", "alpha")
	for _, tc := range []struct {
		channel, want string
		ok            bool
	}{
		{"alpha", "1.37", true},
		{"beta", "1.36", true},
		{"stable", "1.35", true},
		{"Stable", "1.35", true}, // the Cluster kind spells channels capitalised
		{"rapid", "", false},
		{"", "", false},
	} {
		got, ok := release.ChannelMinor(r, tc.channel)
		if got != tc.want || ok != tc.ok {
			t.Errorf("ChannelMinor(%q) = %q, %v; want %q, %v", tc.channel, got, ok, tc.want, tc.ok)
		}
	}
	if got, ok := release.ChannelMinor(onChannels("", "", ""), "stable"); ok || got != "" {
		t.Errorf("a release without channels: ChannelMinor(stable) = %q, %v; want none", got, ok)
	}
}

func TestEnding(t *testing.T) {
	r := onChannels("", "stable", "alpha") // 1.35 on no channel; beta on none
	for _, tc := range []struct {
		minor string
		want  bool
	}{
		{"1.35", true},  // offered, on no channel: its users are warned
		{"1.36", false}, // stable
		{"1.37", false}, // alpha
		{"1.34", false}, // not offered at all: that is unsupported, not ending
		{"1.35.2", true},
	} {
		if got := release.Ending(r, tc.minor); got != tc.want {
			t.Errorf("Ending(%s) = %v, want %v", tc.minor, got, tc.want)
		}
	}
	if release.Ending(onChannels("", "", ""), "1.35") {
		t.Error("a release that assigns no channels marks nothing as ending")
	}
}

func TestChannelProblems(t *testing.T) {
	for _, tc := range []struct {
		name string
		r    release.Release
		want []string
	}{
		{"each channel once, newest alpha", onChannels("stable", "beta", "alpha"), nil},
		{"no channels at all (an older release)", onChannels("", "", ""), nil},
		{"one channel-less minor", release.Release{Kubernetes: []release.Minor{
			{Minor: "1.38", Channel: "alpha"}, {Minor: "1.37", Channel: "beta"}, {Minor: "1.36", Channel: "stable"}, {Minor: "1.35"},
		}}, nil},
		{"a channel missing", onChannels("", "beta", "alpha"),
			[]string{"no minor is on channel stable: name one minor per channel, or none at all"}},
		{"a channel twice", onChannels("beta", "beta", "alpha"),
			[]string{"channel beta is on 1.36 and 1.35: one minor per channel", "no minor is on channel stable: name one minor per channel, or none at all"}},
		{"stable newer than beta", onChannels("beta", "stable", "alpha"),
			[]string{"channel stable (1.36) is newer than beta (1.35): alpha, beta and stable go from newest to oldest"}},
		{"beta newer than alpha", onChannels("stable", "alpha", "beta"),
			[]string{"channel beta (1.37) is newer than alpha (1.36): alpha, beta and stable go from newest to oldest"}},
		{"unknown channel", onChannels("stable", "beta", "rapid"),
			[]string{"minor 1.37 is on channel \"rapid\": channels are alpha, beta and stable", "no minor is on channel alpha: name one minor per channel, or none at all"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := release.ChannelProblems(tc.r); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ChannelProblems =\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
}
