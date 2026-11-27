package buildserial

import (
	"strconv"
	"strings"
	"testing"
)

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// A stored serial above this build's is newer: this build must not write
// over it. Equal, older or none (written before serials) is not.
func TestNewer(t *testing.T) {
	for _, tc := range []struct {
		annotations map[string]string
		newer       bool
	}{
		{nil, false},
		{map[string]string{}, false},
		{map[string]string{Annotation: "0"}, false},
		{map[string]string{Annotation: itoa(Serial)}, false},
		{map[string]string{Annotation: itoa(Serial + 1)}, true},
		{map[string]string{Annotation: "not-a-number"}, false},
	} {
		if got, _ := Newer(tc.annotations); got != tc.newer {
			t.Errorf("Newer(%v) = %v, want %v", tc.annotations, got, tc.newer)
		}
	}
}

// Stamp returns a copy with this build's serial; the input is not changed.
func TestStamp(t *testing.T) {
	in := map[string]string{"a": "b"}
	out := Stamp(in)
	if out[Annotation] != itoa(Serial) || out["a"] != "b" {
		t.Fatalf("Stamp = %v", out)
	}
	if _, changed := in[Annotation]; changed {
		t.Fatal("Stamp changed its input")
	}
}

// A refused write is remembered: the ready check fails, naming it.
func TestRefusalsFailTheReadyCheck(t *testing.T) {
	t.Cleanup(ResetForTest)
	if err := Check(nil); err != nil {
		t.Fatalf("no refusal: %v", err)
	}
	Refuse("CustomResourceDefinition clusters.cloud.cluster-vending.io", Serial+1)
	err := Check(nil)
	if err == nil || !strings.Contains(err.Error(), "clusters.cloud.cluster-vending.io") || !strings.Contains(err.Error(), itoa(Serial+1)) {
		t.Fatalf("Check = %v; want it to name the refused object and its serial", err)
	}
}
