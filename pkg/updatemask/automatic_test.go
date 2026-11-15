package updatemask

import (
	"slices"
	"testing"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// On a cluster whose upgrades are automatic the channel sets the
// Kubernetes version: an update's version is left out. On a manual one, or
// one the update switches to manual, it is kept.
func TestWithoutAutomaticVersion(t *testing.T) {
	auto, manual := cloudv1.UpgradeMode_UPGRADE_MODE_AUTOMATIC, cloudv1.UpgradeMode_UPGRADE_MODE_MANUAL
	paths := []string{PathKubernetesVersion, "spec.release_channel"}
	tests := []struct {
		name              string
		stored, requested cloudv1.UpgradeMode
		paths             []string
		want              []string
		dropped           bool
	}{
		{"automatic: the version is left out", auto, auto, paths, []string{"spec.release_channel"}, true},
		{"manual: kept", manual, manual, paths, paths, false},
		{"switched to manual in the same update: kept", auto, manual, append(slices.Clone(paths), PathUpgradeMode), append(slices.Clone(paths), PathUpgradeMode), false},
		{"switched to automatic in the same update: left out", manual, auto, []string{PathKubernetesVersion, PathUpgradeMode}, []string{PathUpgradeMode}, true},
		{"the requested mode only counts when the update sets it", auto, manual, paths, []string{"spec.release_channel"}, true},
		{"no version in the update", auto, auto, []string{"spec.release_channel"}, []string{"spec.release_channel"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stored := &cloudv1.ClusterSpec{UpgradeMode: tt.stored}
			requested := &cloudv1.ClusterSpec{UpgradeMode: tt.requested}

			got, dropped := WithoutAutomaticVersion(stored, requested, tt.paths)

			if !slices.Equal(got, tt.want) || dropped != tt.dropped {
				t.Errorf("= %v, %v; want %v, %v", got, dropped, tt.want, tt.dropped)
			}
		})
	}
}
