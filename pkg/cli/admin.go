package cli

import (
	"fmt"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/apimachinery/pkg/util/duration"
)

func parseUntil(s string) (*timestamppb.Timestamp, error) {
	if s == "" {
		return nil, nil
	}
	for _, layout := range []string{time.RFC3339, time.DateOnly} {
		if t, err := time.Parse(layout, s); err == nil {
			return timestamppb.New(t), nil
		}
	}
	return nil, fmt.Errorf("--until must be a time (2026-12-31T18:00:00Z) or a date (2026-12-31), got %q", s)
}

func untilText(t *timestamppb.Timestamp) string {
	if t == nil || !t.IsValid() {
		return none
	}
	left := t.AsTime().Sub(clock())
	if left <= 0 {
		return "ended"
	}
	return "in " + strings.TrimSpace(duration.HumanDuration(left))
}
