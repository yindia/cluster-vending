package component

import "testing"

func TestInstallPhaseOf(t *testing.T) {
	tests := []struct {
		name   string
		values Values
		want   InstallPhase
	}{
		{"system placement: phase 1", Values{"nodeSelector": "${system.nodeSelector}", "tolerations": "${system.tolerations}"}, PhaseSystemGroup},
		{"platform placement, nested: phase 2", Values{"global": map[string]any{"tolerations": "${platform.tolerations}"}}, PhasePlatformPool},
		{"no placement", Values{"replicas": 2, "name": "${cluster.name}"}, PhaseUnplaced},
		{"no values", nil, PhaseUnplaced},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := InstallPhaseOf(Definition{Name: "x", PlatformValues: tt.values})
			if err != nil || got != tt.want {
				t.Errorf("InstallPhaseOf = %v, %v; want %v", got, err, tt.want)
			}
		})
	}
	both := Values{"a": "${system.nodeSelector}", "b": "${platform.tolerations}"}
	if _, err := InstallPhaseOf(Definition{Name: "x", PlatformValues: both}); err == nil {
		t.Error("a definition on both the system group and the platform pool must be refused")
	}
}
