package controller

import "testing"

// The per-object controllers run Deps.Workers reconciles at once; an unset
// count is one, controller-runtime's own default.
func TestObjectWorkers(t *testing.T) {
	for _, tc := range []struct{ workers, want int }{{0, 1}, {-3, 1}, {1, 1}, {8, 8}} {
		if got := objectOptions(Deps{Workers: tc.workers}).MaxConcurrentReconciles; got != tc.want {
			t.Errorf("Workers %d: MaxConcurrentReconciles = %d, want %d", tc.workers, got, tc.want)
		}
	}
}
