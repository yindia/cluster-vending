package network

import (
	"slices"
	"testing"

	"github.com/yindia/cluster-vending/pkg/updatemask"
)

// The paths the CLI infers masks from are exactly those UpdateNetwork
// accepts.
func TestUpdateMaskPathsAreTheServers(t *testing.T) {
	if want := []string{pathDisplayName, pathLabels, pathFlowLogs}; !slices.Equal(updatemask.Network, want) {
		t.Errorf("updatemask.Network = %v, want %v", updatemask.Network, want)
	}
}
