package cluster

import "testing"

// A cluster in a transient EKS status is looked at again later; only
// FAILED and DELETING (and ACTIVE, which needs no wait) are final.
func TestTransientStatus(t *testing.T) {
	for status, want := range map[string]bool{"CREATING": true, "UPDATING": true, "PENDING": true,
		"ACTIVE": false, "FAILED": false, "DELETING": false, "": false} {
		if got := TransientStatus(status); got != want {
			t.Errorf("TransientStatus(%q) = %t, want %t", status, got, want)
		}
	}
}
