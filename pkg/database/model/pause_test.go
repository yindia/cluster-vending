package model_test

import (
	"slices"
	"testing"
	"time"

	"github.com/yindia/cluster-vending/pkg/database/model"
)

func TestPauseAnnotationRoundTrip(t *testing.T) {
	since := time.Date(2026, 11, 14, 9, 30, 0, 0, time.UTC)
	tests := []struct {
		name  string
		value string
		want  *model.Pause
	}{
		{"none", "", nil},
		{"as written", model.Pause{By: "alice", Since: since, Reason: "provider upgrade"}.Annotation(),
			&model.Pause{By: "alice", Since: since, Reason: "provider upgrade"}},
		{"without a reason", model.Pause{By: "anonymous", Since: since}.Annotation(), &model.Pause{By: "anonymous", Since: since}},
		{"set by hand", "true", &model.Pause{By: "unknown"}},
		{"JSON without a name", `{"since":"2026-11-14T09:30:00Z"}`, &model.Pause{By: "unknown", Since: since}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := model.ParsePause(tt.value)

			switch {
			case tt.want == nil && got == nil:
			case tt.want == nil || got == nil:
				t.Fatalf("ParsePause = %+v, want %+v", got, tt.want)
			case got.By != tt.want.By || !got.Since.Equal(tt.want.Since) || got.Reason != tt.want.Reason:
				t.Fatalf("ParsePause = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// A pause records the scopes it was asked for at. Pausing again at another
// scope adds it and keeps who paused first; a resume at one scope removes
// that scope only, and the pause ends when none is left. A resume of the
// row's own scope (its network or cluster) ends it whatever else holds it.
func TestPauseScopes(t *testing.T) {
	since := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	alice := &model.Pause{By: "alice", Since: since}
	tests := []struct {
		name   string
		start  *model.Pause
		steps  func(p *model.Pause) *model.Pause
		scopes []model.PauseScope // nil: not paused at the end
		by     string
	}{
		{"tenant pause then tenant resume", nil, func(p *model.Pause) *model.Pause {
			return p.With(model.PauseTenant, alice).Without(model.PauseTenant, false)
		}, nil, ""},
		{"network and tenant: the tenant resume leaves the network's", nil, func(p *model.Pause) *model.Pause {
			return p.With(model.PauseNetwork, alice).With(model.PauseTenant, &model.Pause{By: "bob", Since: since.Add(time.Hour)}).Without(model.PauseTenant, false)
		}, []model.PauseScope{model.PauseNetwork}, "alice"},
		{"network and tenant: the network's own resume ends it", nil, func(p *model.Pause) *model.Pause {
			return p.With(model.PauseTenant, alice).With(model.PauseNetwork, alice).Without(model.PauseNetwork, true)
		}, nil, ""},
		{"both resumed", nil, func(p *model.Pause) *model.Pause {
			return p.With(model.PauseTenant, alice).With(model.PauseNetwork, alice).Without(model.PauseTenant, false).Without(model.PauseNetwork, true)
		}, nil, ""},
		{"a pause of an older build has no scopes: any resume ends it", &model.Pause{By: "carol", Since: since}, func(p *model.Pause) *model.Pause {
			return p.Without(model.PauseTenant, false)
		}, nil, ""},
		{"pausing again at the same scope changes nothing", nil, func(p *model.Pause) *model.Pause {
			return p.With(model.PauseTenant, alice).With(model.PauseTenant, &model.Pause{By: "bob"})
		}, []model.PauseScope{model.PauseTenant}, "alice"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.steps(tt.start)

			if tt.scopes == nil {
				if got != nil {
					t.Fatalf("still paused: %+v", got)
				}
				return
			}
			if got == nil || got.By != tt.by || !slices.Equal(got.Scopes, tt.scopes) {
				t.Fatalf("pause = %+v, want by %s at %v", got, tt.by, tt.scopes)
			}
			if back := model.ParsePause(got.Annotation()); !slices.Equal(back.Scopes, tt.scopes) {
				t.Errorf("annotation round trip lost the scopes: %+v", back)
			}
		})
	}
}
