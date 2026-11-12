package release_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yindia/cluster-vending/pkg/release"
)

var (
	now  = time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	past = now.Add(-24 * time.Hour)
	soon = now.Add(24 * time.Hour)
	far  = now.Add(365 * 24 * time.Hour)
)

// rel is a release offering 1.33, 1.34 (default) and 1.35, with one
// platform component pinned per minor.
func rel(name string, render int64) release.Release {
	return release.Release{Name: name, RenderVersion: render, Kubernetes: []release.Minor{
		{Minor: "1.33", UpgradeTo: "1.34", EndOfSupport: far, Components: []release.Component{{Name: "karpenter", Version: "1.6.3"}}},
		{Minor: "1.34", Default: true, EndOfSupport: far, Components: []release.Component{{Name: "karpenter", Version: "1.8.1"}}},
		{Minor: "1.35", EndOfSupport: far, Components: []release.Component{{Name: "karpenter", Version: "1.8.1"}}},
	}}
}

func releases(rs ...release.Release) map[string]release.Release {
	out := map[string]release.Release{}
	for _, r := range rs {
		out[r.Name] = r
	}
	return out
}

func channels(cs ...release.Channel) map[string]release.Channel {
	out := map[string]release.Channel{}
	for _, c := range cs {
		out[c.Name] = c
	}
	return out
}

var (
	old     = rel("2026-10-a", 1)
	current = rel("2026-11-b", 2)
	stable  = release.Channel{Name: "stable", Offered: "2026-11-b"}
	alpha   = release.Channel{Name: "alpha", Offered: "2026-11-b"}
)

func TestResolve(t *testing.T) {
	withdrawn := rel("2026-09-z", 1)
	withdrawn.Withdrawn = true
	oddUpgrade := rel("2026-11-c", 3)
	oddUpgrade.Kubernetes[0].UpgradeTo = "1.35"
	ended := rel("2026-11-d", 3)
	ended.Kubernetes[0].EndOfSupport = past

	tests := []struct {
		name     string
		facts    release.ClusterFacts
		releases map[string]release.Release
		channels map[string]release.Channel
		hold     *release.Hold
		want     release.Decision
		codes    []release.Code
	}{
		{
			name:  "no releases: as today",
			facts: release.ClusterFacts{Mode: release.Automatic, SpecKubernetes: "1.34", Now: now},
			want:  release.Decision{Kubernetes: "1.34", Supported: true},
			codes: []release.Code{release.NoReleases},
		},
		{
			name: "manual, newer release renderer: the renderer moves, the versions do not",
			facts: release.ClusterFacts{SpecKubernetes: "1.33", CurrentKubernetes: "1.33", CurrentRelease: "2026-10-a",
				Now: now},
			releases: releases(old, current), channels: channels(stable),
			want:  release.Decision{Kubernetes: "1.33", RenderVersion: 2, Supported: true, EndOfSupport: far},
			codes: []release.Code{release.ManualMode, release.RendererFixes},
		},
		{
			name:     "manual, new cluster without a version: the offered release's default",
			facts:    release.ClusterFacts{Mode: release.Manual, Now: now},
			releases: releases(current), channels: channels(stable),
			want:  release.Decision{Kubernetes: "1.34", RenderVersion: 2, Supported: true, EndOfSupport: far},
			codes: []release.Code{release.ManualMode, release.RendererFixes},
		},
		{
			name:     "manual, old minor: kept, reported unsupported",
			facts:    release.ClusterFacts{SpecKubernetes: "1.30", CurrentKubernetes: "1.30", Now: now},
			releases: releases(current), channels: channels(stable),
			want:  release.Decision{Kubernetes: "1.30", RenderVersion: 2},
			codes: []release.Code{release.ManualMode, release.RendererFixes, release.Unsupported},
		},
		{
			name:     "manual, past end of support: kept, warned",
			facts:    release.ClusterFacts{SpecKubernetes: "1.33", CurrentKubernetes: "1.33", Now: now},
			releases: releases(ended), channels: channels(release.Channel{Name: "stable", Offered: "2026-11-d"}),
			want:  release.Decision{Kubernetes: "1.33", RenderVersion: 3, EndOfSupport: past},
			codes: []release.Code{release.ManualMode, release.RendererFixes, release.PastEndOfSupport},
		},
		{
			name:     "manual, unknown channel: as today, no renderer target",
			facts:    release.ClusterFacts{Channel: "nightly", SpecKubernetes: "1.34", Now: now},
			releases: releases(current), channels: channels(stable),
			want:  release.Decision{Kubernetes: "1.34", Supported: true},
			codes: []release.Code{release.ManualMode, release.UnknownChannel},
		},
		{
			name:     "automatic, unset channel is stable: follows its release",
			facts:    release.ClusterFacts{Mode: release.Automatic, CurrentKubernetes: "1.34", CurrentRelease: "2026-10-a", Now: now},
			releases: releases(old, current), channels: channels(stable, release.Channel{Name: "alpha", Offered: "2026-10-a"}),
			want: release.Decision{Release: "2026-11-b", Kubernetes: "1.34", RenderVersion: 2,
				Components: map[string]string{"karpenter": "1.8.1"}, Supported: true, EndOfSupport: far},
			codes: []release.Code{release.Following},
		},
		{
			name:     "automatic, named channel",
			facts:    release.ClusterFacts{Mode: release.Automatic, Channel: "alpha", CurrentKubernetes: "1.35", Now: now},
			releases: releases(current), channels: channels(alpha),
			want: release.Decision{Release: "2026-11-b", Kubernetes: "1.35", RenderVersion: 2,
				Components: map[string]string{"karpenter": "1.8.1"}, Supported: true, EndOfSupport: far},
			codes: []release.Code{release.Following},
		},
		{
			name:     "automatic, upgradeTo moves one minor",
			facts:    release.ClusterFacts{Mode: release.Automatic, CurrentKubernetes: "1.33", Now: now},
			releases: releases(current), channels: channels(stable),
			want: release.Decision{Release: "2026-11-b", Kubernetes: "1.34", RenderVersion: 2,
				Components: map[string]string{"karpenter": "1.8.1"}, Supported: true, EndOfSupport: far},
			codes: []release.Code{release.Following},
		},
		{
			name:     "automatic, upgradeTo of two minors is ignored",
			facts:    release.ClusterFacts{Mode: release.Automatic, CurrentKubernetes: "1.33", Now: now},
			releases: releases(oddUpgrade), channels: channels(release.Channel{Name: "stable", Offered: "2026-11-c"}),
			want: release.Decision{Release: "2026-11-c", Kubernetes: "1.33", RenderVersion: 3,
				Components: map[string]string{"karpenter": "1.6.3"}, Supported: true, EndOfSupport: far},
			codes: []release.Code{release.Following, release.InvalidUpgrade},
		},
		{
			name:     "automatic, newer than every minor: never downgraded",
			facts:    release.ClusterFacts{Mode: release.Automatic, CurrentKubernetes: "1.36", Now: now},
			releases: releases(current), channels: channels(stable),
			want:  release.Decision{Release: "2026-11-b", Kubernetes: "1.36", RenderVersion: 2},
			codes: []release.Code{release.Following, release.NoDowngrade},
		},
		{
			name:     "automatic, older than every minor: one minor up",
			facts:    release.ClusterFacts{Mode: release.Automatic, CurrentKubernetes: "1.31", Now: now},
			releases: releases(current), channels: channels(stable),
			want:  release.Decision{Release: "2026-11-b", Kubernetes: "1.32", RenderVersion: 2},
			codes: []release.Code{release.Following, release.Unsupported},
		},
		{
			name:     "automatic, new cluster without a version: the default minor",
			facts:    release.ClusterFacts{Mode: release.Automatic, Now: now},
			releases: releases(current), channels: channels(stable),
			want: release.Decision{Release: "2026-11-b", Kubernetes: "1.34", RenderVersion: 2,
				Components: map[string]string{"karpenter": "1.8.1"}, Supported: true, EndOfSupport: far},
			codes: []release.Code{release.Following},
		},
		{
			name:     "automatic, new cluster with a version: the spec's",
			facts:    release.ClusterFacts{Mode: release.Automatic, SpecKubernetes: "1.35", Now: now},
			releases: releases(current), channels: channels(stable),
			want: release.Decision{Release: "2026-11-b", Kubernetes: "1.35", RenderVersion: 2,
				Components: map[string]string{"karpenter": "1.8.1"}, Supported: true, EndOfSupport: far},
			codes: []release.Code{release.Following},
		},
		{
			name:     "automatic, channel paused: stays on its release",
			facts:    release.ClusterFacts{Mode: release.Automatic, CurrentKubernetes: "1.33", CurrentRelease: "2026-10-a", Now: now},
			releases: releases(old, current), channels: channels(release.Channel{Name: "stable", Offered: "2026-11-b", Paused: true}),
			want: release.Decision{Release: "2026-10-a", Kubernetes: "1.33", RenderVersion: 1,
				Components: map[string]string{"karpenter": "1.6.3"}, Supported: true, EndOfSupport: far},
			codes: []release.Code{release.ChannelPaused},
		},
		{
			name:     "automatic, paused on a withdrawn release: no move, warned",
			facts:    release.ClusterFacts{Mode: release.Automatic, CurrentKubernetes: "1.34", CurrentRelease: "2026-09-z", Now: now},
			releases: releases(withdrawn, current), channels: channels(release.Channel{Name: "stable", Offered: "2026-11-b", Paused: true}),
			want:  release.Decision{Kubernetes: "1.34", Supported: true},
			codes: []release.Code{release.ChannelPaused, release.Withdrawn},
		},
		{
			name:     "automatic, withdrawn current release: moves to the offered one",
			facts:    release.ClusterFacts{Mode: release.Automatic, CurrentKubernetes: "1.34", CurrentRelease: "2026-09-z", Now: now},
			releases: releases(withdrawn, current), channels: channels(stable),
			want: release.Decision{Release: "2026-11-b", Kubernetes: "1.34", RenderVersion: 2,
				Components: map[string]string{"karpenter": "1.8.1"}, Supported: true, EndOfSupport: far},
			codes: []release.Code{release.Withdrawn, release.Following},
		},
		{
			name:     "automatic, channel offers a withdrawn release: stays",
			facts:    release.ClusterFacts{Mode: release.Automatic, CurrentKubernetes: "1.34", CurrentRelease: "2026-11-b", Now: now},
			releases: releases(withdrawn, current), channels: channels(release.Channel{Name: "stable", Offered: "2026-09-z"}),
			want: release.Decision{Release: "2026-11-b", Kubernetes: "1.34", RenderVersion: 2,
				Components: map[string]string{"karpenter": "1.8.1"}, Supported: true, EndOfSupport: far},
			codes: []release.Code{release.Withdrawn},
		},
		{
			name:     "automatic, unknown channel: stays where it is",
			facts:    release.ClusterFacts{Mode: release.Automatic, Channel: "nightly", CurrentKubernetes: "1.34", Now: now},
			releases: releases(current), channels: channels(stable),
			want:  release.Decision{Kubernetes: "1.34", Supported: true},
			codes: []release.Code{release.UnknownChannel},
		},
		{
			name:     "automatic, channel offers nothing published",
			facts:    release.ClusterFacts{Mode: release.Automatic, CurrentKubernetes: "1.34", Now: now},
			releases: releases(current), channels: channels(release.Channel{Name: "stable", Offered: "2027-01-a"}),
			want:  release.Decision{Kubernetes: "1.34", Supported: true},
			codes: []release.Code{release.NothingOffered},
		},
		{
			name:     "held on a release",
			facts:    release.ClusterFacts{Mode: release.Automatic, CurrentKubernetes: "1.33", CurrentRelease: "2026-10-a", Now: now},
			releases: releases(old, current), channels: channels(stable),
			hold: &release.Hold{Release: "2026-10-a", Until: soon},
			want: release.Decision{Release: "2026-10-a", Kubernetes: "1.34", RenderVersion: 1,
				Components: map[string]string{"karpenter": "1.8.1"}, Supported: true, EndOfSupport: far},
			codes: []release.Code{release.Held},
		},
		{
			name:     "held, frozen where it is",
			facts:    release.ClusterFacts{Mode: release.Automatic, CurrentKubernetes: "1.35", CurrentRelease: "2026-10-a", Now: now},
			releases: releases(old, current), channels: channels(stable),
			hold: &release.Hold{},
			want: release.Decision{Release: "2026-10-a", Kubernetes: "1.35", RenderVersion: 1,
				Components: map[string]string{"karpenter": "1.8.1"}, Supported: true, EndOfSupport: far},
			codes: []release.Code{release.Held},
		},
		{
			name:     "hold expired: follows the channel",
			facts:    release.ClusterFacts{Mode: release.Automatic, CurrentKubernetes: "1.34", CurrentRelease: "2026-10-a", Now: now},
			releases: releases(old, current), channels: channels(stable),
			hold: &release.Hold{Release: "2026-10-a", Until: past},
			want: release.Decision{Release: "2026-11-b", Kubernetes: "1.34", RenderVersion: 2,
				Components: map[string]string{"karpenter": "1.8.1"}, Supported: true, EndOfSupport: far},
			codes: []release.Code{release.HoldExpired, release.Following},
		},
		{
			name:     "a hold has no effect on a manual cluster",
			facts:    release.ClusterFacts{Mode: release.Manual, SpecKubernetes: "1.35", Now: now},
			releases: releases(old, current), channels: channels(stable),
			hold:  &release.Hold{Release: "2026-10-a"},
			want:  release.Decision{Kubernetes: "1.35", RenderVersion: 2, Supported: true, EndOfSupport: far},
			codes: []release.Code{release.ManualMode, release.RendererFixes},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := release.Resolve(tt.facts, tt.releases, tt.channels, tt.hold)
			var codes []release.Code
			for _, n := range got.Notes {
				codes = append(codes, n.Code)
				if n.Message == "" {
					t.Errorf("note %s has no message", n.Code)
				}
			}
			if !reflect.DeepEqual(codes, tt.codes) {
				t.Errorf("notes = %v, want %v (%+v)", codes, tt.codes, got.Notes)
			}
			got.Notes = nil
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("decision = %+v\nwant       %+v", got, tt.want)
			}
		})
	}
}

// The tier orders the rollout, not its target: it is not an input, and
// two clusters that differ only in tier get the same decision.
func TestTierDoesNotChangeTheTarget(t *testing.T) {
	facts := release.ClusterFacts{Mode: release.Automatic, CurrentKubernetes: "1.33", Now: now}
	if _, ok := reflect.TypeOf(facts).FieldByName("Tier"); ok {
		t.Fatal("ClusterFacts has a Tier: the tier is the channel controller's, not the resolver's")
	}
}

func TestCheckVersion(t *testing.T) {
	rs, cs := releases(current), channels(stable, alpha)
	for _, tt := range []struct {
		name               string
		releases           map[string]release.Release
		channels           map[string]release.Channel
		channel, requested string
		wantErr            bool
	}{
		{"supported minor", rs, cs, "", "1.34", false},
		{"supported patch", rs, cs, "alpha", "1.35.2", false},
		{"too old", rs, cs, "", "1.32", true},
		{"too new", rs, cs, "stable", "1.36", true},
		{"nothing requested", rs, cs, "", "", false},
		{"no releases", nil, cs, "", "1.20", false},
		{"unknown channel", rs, cs, "nightly", "1.20", false},
		{"channel offers nothing", rs, channels(release.Channel{Name: "stable"}), "", "1.20", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := release.CheckVersion(tt.releases, tt.channels, tt.channel, tt.requested)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, want error %t", err, tt.wantErr)
			}
			if err != nil && (!strings.Contains(err.Error(), tt.requested) || !strings.Contains(err.Error(), "1.33, 1.34, 1.35")) {
				t.Errorf("message %q does not name the version and the supported minors", err)
			}
		})
	}
}
