package model_test

import (
	"slices"
	"testing"
	"time"

	"github.com/yindia/cluster-vending/pkg/database/model"
)

// A pause asked with an until ends by itself then: Active drops what has
// passed, and a pause whose every scope passed is no pause. Scopes keep
// their own until, who and why: a tier's pause that ended leaves the
// cluster's own.
func TestPauseUntil(t *testing.T) {
	since := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	until := since.Add(2 * time.Hour)
	tier := model.ChannelScope("beta", "0")
	own := (*model.Pause)(nil).With(model.PauseCluster, &model.Pause{By: "alice", Since: since, Reason: "fixing by hand"})
	both := own.With(tier, &model.Pause{By: "bob", Since: since, Reason: "freeze", Until: &until})
	onlyTier := (*model.Pause)(nil).With(tier, &model.Pause{By: "bob", Since: since, Reason: "freeze", Until: &until})
	tests := []struct {
		name   string
		pause  *model.Pause
		at     time.Time
		scopes []model.PauseScope // nil: not paused
		by     string
	}{
		{"before until", onlyTier, since.Add(time.Hour), []model.PauseScope{tier}, "bob"},
		{"at until it ended", onlyTier, until, nil, ""},
		{"the tier's ended, the cluster's own stays", both, until.Add(time.Minute), []model.PauseScope{model.PauseCluster}, "alice"},
		{"no until never ends", own, since.Add(1000 * time.Hour), []model.PauseScope{model.PauseCluster}, "alice"},
		{"an older build's pause without scopes", &model.Pause{By: "old", Since: since}, until, []model.PauseScope{}, "old"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := model.ParsePause(tt.pause.Annotation()).Active(tt.at)

			switch {
			case tt.scopes == nil && got == nil:
			case tt.scopes == nil || got == nil:
				t.Fatalf("Active = %+v, want scopes %v", got, tt.scopes)
			case len(got.Scopes) != len(tt.scopes) || !slices.Equal(got.Scopes, tt.scopes) && len(tt.scopes) > 0, got.By != tt.by:
				t.Fatalf("Active = %+v, want scopes %v by %s", got, tt.scopes, tt.by)
			}
		})
	}
}

// Each scope keeps who paused at it, why and until when: a pause list shows
// each, not only the first pauser's.
func TestPauseEntries(t *testing.T) {
	since := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	until := since.Add(time.Hour)
	tier := model.ChannelScope("beta", "")
	p := (*model.Pause)(nil).With(model.PauseTenant, &model.Pause{By: "alice", Since: since, Reason: "provider upgrade"}).
		With(tier, &model.Pause{By: "bob", Since: since.Add(time.Minute), Reason: "freeze", Until: &until})

	got := model.ParsePause(p.Annotation()).Entries

	if len(got) != 2 || got[0].Scope != model.PauseTenant || got[0].By != "alice" ||
		got[1].Scope != tier || got[1].By != "bob" || got[1].Reason != "freeze" || got[1].Until == nil || !got[1].Until.Equal(until) {
		t.Fatalf("entries = %+v", got)
	}
	if e, ok := p.Entry(tier); !ok || e.By != "bob" {
		t.Fatalf("Entry(%s) = %+v, %v", tier, e, ok)
	}
	if rest := p.Without(tier, false); len(rest.Entries) != 1 || rest.Entries[0].Scope != model.PauseTenant {
		t.Fatalf("Without(%s) = %+v", tier, rest)
	}
}

// A pause of an older build has scopes, no entries: each scope reads as an
// entry of the pause's own who, when and why.
func TestPauseEntriesOfAnOlderBuild(t *testing.T) {
	got := model.ParsePause(`{"by":"alice","since":"2026-11-14T09:00:00Z","reason":"r","scopes":["tenant","cluster"]}`)

	if len(got.Entries) != 2 || got.Entries[1].Scope != model.PauseCluster || got.Entries[1].By != "alice" || got.Entries[1].Reason != "r" {
		t.Fatalf("entries = %+v", got.Entries)
	}
}

func TestChannelScope(t *testing.T) {
	tests := []struct {
		channel, tier     string
		want              model.PauseScope
		channelOf, tierOf string
	}{
		{"beta", "", "channel:beta", "beta", ""},
		{"Stable", "0", "channel:stable/tier:0", "stable", "0"},
	}
	for _, tt := range tests {
		got := model.ChannelScope(tt.channel, tt.tier)
		ch, tier, ok := got.Channel()
		if got != tt.want || !ok || ch != tt.channelOf || tier != tt.tierOf {
			t.Errorf("ChannelScope(%q, %q) = %q (%q %q %v)", tt.channel, tt.tier, got, ch, tier, ok)
		}
	}
	if _, _, ok := model.PauseCluster.Channel(); ok {
		t.Error("the cluster scope is no channel's")
	}
}
