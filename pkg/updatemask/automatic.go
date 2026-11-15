package updatemask

import (
	"slices"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// The paths of a cluster's Kubernetes version and upgrade mode.
const (
	PathKubernetesVersion = "spec.control_plane.kubernetes_version"
	PathUpgradeMode       = "spec.upgrade_mode"
)

// WithoutAutomaticVersion leaves the Kubernetes version out of an update of
// a cluster whose upgrades are automatic after the update: its release
// channel sets the version (the rollout moves it), so a manifest's version
// is not a request. The mode after the update is the requested one when
// the update sets it, else the stored one. dropped reports whether the
// version was left out.
func WithoutAutomaticVersion(stored, requested *cloudv1.ClusterSpec, paths []string) (out []string, dropped bool) {
	if !slices.Contains(paths, PathKubernetesVersion) {
		return paths, false
	}
	mode := stored.GetUpgradeMode()
	if slices.Contains(paths, PathUpgradeMode) {
		mode = requested.GetUpgradeMode()
	}
	if mode != cloudv1.UpgradeMode_UPGRADE_MODE_AUTOMATIC {
		return paths, false
	}
	return slices.DeleteFunc(slices.Clone(paths), func(p string) bool { return p == PathKubernetesVersion }), true
}
