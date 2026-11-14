package upgrade

import "testing"

func TestNextHop(t *testing.T) {
	offered := []string{"1.38", "1.37", "1.36", "1.35"}
	tests := []struct {
		name             string
		current, channel string
		offered          []string
		want             string
		ok               bool
		why              string
	}{
		{name: "two behind: one minor first", current: "1.35", channel: "1.37", offered: offered, want: "1.36", ok: true,
			why: "channel offers 1.37; upgrading one minor at a time"},
		{name: "one behind", current: "1.36", channel: "1.37", offered: offered, want: "1.37", ok: true,
			why: "channel offers 1.37; upgrading one minor at a time"},
		{name: "patch version of current", current: "1.36.4", channel: "1.37", offered: offered, want: "1.37", ok: true,
			why: "channel offers 1.37; upgrading one minor at a time"},
		{name: "at the channel", current: "1.37", channel: "1.37", offered: offered, why: "runs the channel's version 1.37"},
		{name: "ahead of the channel", current: "1.38", channel: "1.36", offered: offered,
			why: "runs 1.38, ahead of the channel's 1.36; a cluster is never downgraded"},
		{name: "next not offered", current: "1.34", channel: "1.37", offered: []string{"1.37", "1.36"},
			why: "1.35 is the next version and is not offered"},
		{name: "next offered with patch", current: "1.36", channel: "1.37", offered: []string{"1.37.1"}, want: "1.37", ok: true,
			why: "channel offers 1.37; upgrading one minor at a time"},
		{name: "unknown current", current: "", channel: "1.37", offered: offered, why: "the cluster's version is not known yet"},
		{name: "unreadable current", current: "latest", channel: "1.37", offered: offered, why: `cannot read the cluster's version "latest"`},
		{name: "no channel version", current: "1.36", channel: "", offered: offered, why: "the channel offers no Kubernetes version"},
		{name: "unreadable channel version", current: "1.36", channel: "one", offered: offered, why: `cannot read the channel's version "one"`},
		{name: "another major", current: "2.1", channel: "1.37", offered: offered, why: "runs 2.1; the channel offers 1.37"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok, why := NextHop(tt.current, tt.channel, tt.offered)
			if got != tt.want || ok != tt.ok || why != tt.why {
				t.Fatalf("NextHop(%q, %q) = %q, %v, %q; want %q, %v, %q", tt.current, tt.channel, got, ok, why, tt.want, tt.ok, tt.why)
			}
		})
	}
}
