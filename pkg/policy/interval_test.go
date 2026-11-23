package policy

import (
	"testing"
	"time"
)

// The policy counts pass runs every 15 minutes (PROP-02 5.2), and its counts
// are flagged stale after two missed passes (30 minutes).
func TestCountsCadence(t *testing.T) {
	tests := []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{name: "pass interval", got: Interval, want: 15 * time.Minute},
		{name: "stale after two missed passes", got: 2 * Interval, want: 30 * time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Fatalf("got %s, want %s", tt.got, tt.want)
			}
		})
	}
}
