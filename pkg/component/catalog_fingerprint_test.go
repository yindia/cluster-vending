package component

import (
	"testing"
	"time"
)

func fingerprintEntry() PlatformEntry {
	return PlatformEntry{
		Definition: Definition{Name: "mesh", Requires: []string{"b", "a"}, Sequence: 100,
			PlatformValues: Values{"replicas": 2, "role": "${iam.roleArn}"}},
		Install: Install{Namespace: "mesh-system", Main: Chart{Repository: "https://charts.example.com", Name: "mesh"}},
	}
}

var fingerprintVars = Vars{VarIAMRoleARN: "arn:aws:iam::1:role/mesh"}

// The fingerprint moves with what the definition renders for the cluster,
// and only with that.
func TestCatalogFingerprint(t *testing.T) {
	base := CatalogFingerprint(fingerprintEntry(), fingerprintVars)
	tests := []struct {
		name    string
		mutate  func(*PlatformEntry)
		vars    Vars
		changed bool
	}{
		{"the same definition", func(*PlatformEntry) {}, fingerprintVars, false},
		{"requires in another order", func(e *PlatformEntry) { e.Requires = []string{"a", "b"} }, fingerprintVars, false},
		{"another sequence", func(e *PlatformEntry) { e.Sequence = 300 }, fingerprintVars, false},
		{"a value", func(e *PlatformEntry) { e.PlatformValues = Values{"replicas": 3, "role": "${iam.roleArn}"} }, fingerprintVars, true},
		{"a resolved placeholder", func(*PlatformEntry) {}, Vars{VarIAMRoleARN: "arn:aws:iam::1:role/other"}, true},
		{"the chart", func(e *PlatformEntry) { e.Install.Main.Repository = "oci://registry.example.com/charts" }, fingerprintVars, true},
		{"the install timeout", func(e *PlatformEntry) { e.Install.Timeout = 20 * time.Minute }, fingerprintVars, true},
		{"requires", func(e *PlatformEntry) { e.Requires = []string{"a"} }, fingerprintVars, true},
		{"the namespace", func(e *PlatformEntry) { e.Install.Namespace = "other" }, fingerprintVars, true},
		{"a companion", func(e *PlatformEntry) {
			e.Install.Companions = []Companion{{Name: "mesh-cni", Chart: Chart{Name: "cni"}}}
		}, fingerprintVars, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := fingerprintEntry()
			tt.mutate(&e)
			got := CatalogFingerprint(e, tt.vars)
			if got == UnknownFingerprint {
				t.Fatal("unknown")
			}
			if (got != base) != tt.changed {
				t.Errorf("changed = %v, want %v", got != base, tt.changed)
			}
		})
	}
}

// A placeholder with no value yet makes the fingerprint unknown: the pass
// keeps the one recorded.
func TestCatalogFingerprintUnknown(t *testing.T) {
	if got := CatalogFingerprint(fingerprintEntry(), Vars{}); got != UnknownFingerprint {
		t.Errorf("fingerprint = %q, want unknown", got)
	}
}
