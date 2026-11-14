package model_test

import (
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
