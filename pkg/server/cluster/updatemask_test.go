package cluster

import (
	"slices"
	"testing"

	"github.com/yindia/cluster-vending/pkg/updatemask"
)

// The paths the CLI infers masks from are exactly those UpdateCluster can
// copy.
func TestUpdateMaskPathsAreTheServers(t *testing.T) {
	paths := []string{pathDisplayName, pathLabels}
	for p := range specPaths {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	want := slices.Sorted(slices.Values(updatemask.Cluster))
	if !slices.Equal(paths, want) {
		t.Errorf("server paths %v\nupdatemask.Cluster %v", paths, want)
	}
}
