package model

import "testing"

// A freeze holds a resource unless it lets it through; no freeze holds
// nothing.
func TestFreezeHolds(t *testing.T) {
	tests := []struct {
		name string
		f    *Freeze
		want bool
	}{
		{"not frozen", nil, false},
		{"frozen", &Freeze{By: "alice"}, true},
		{"let through", &Freeze{By: "alice", LetThrough: true}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.f.Holds(); got != tt.want {
				t.Errorf("Holds() = %v, want %v", got, tt.want)
			}
		})
	}
}
