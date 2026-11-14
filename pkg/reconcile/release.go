package reconcile

import (
	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/release"
)

// ReleaseOf is a release's content as pkg/release takes it: its
// Kubernetes minors, with their channels, and the component pins of each.
func ReleaseOf(name string, content cloudv1alpha1.ReleaseContent) release.Release {
	out := release.Release{Name: name, RenderVersion: content.RenderVersion}
	for _, k := range content.Kubernetes {
		m := release.Minor{Minor: k.Minor, Default: k.Default, UpgradeTo: k.UpgradeTo, Channel: k.Channel}
		for _, c := range k.Components {
			m.Components = append(m.Components, release.Component{Name: c.Name, Version: c.Version})
		}
		out.Kubernetes = append(out.Kubernetes, m)
	}
	return out
}
