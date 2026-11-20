package controller

import (
	"strings"
	"testing"

	"github.com/yindia/cluster-vending/pkg/catalog"
	"github.com/yindia/cluster-vending/pkg/config"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
)

// What this build ships, as manifests, passes `controller --check` with the
// default node groups: the rules hold for the real catalog, not only for
// the fixtures of pkg/catalog.
func TestShippedCatalogPassesTheOfflineCheck(t *testing.T) {
	defs, _ := shippedCatalog(t)
	var set catalog.Set
	for _, d := range defs {
		d = d.DeepCopy()
		d.Annotations = map[string]string{catalog.SchemaAnnotation: catalog.SchemaVersion}
		set.Definitions = append(set.Definitions, *d)
	}
	rel := DefaultRelease()
	one := int64(1)
	rel.Spec.Serial = &one
	rel.Annotations = map[string]string{catalog.SchemaAnnotation: catalog.SchemaVersion}
	set.Releases = append(set.Releases, *rel)
	for _, ch := range DefaultChannels() {
		ch.Annotations = map[string]string{catalog.SchemaAnnotation: catalog.SchemaVersion}
		set.Channels = append(set.Channels, *ch)
	}
	group, platform := config.DefaultNodeGroups().System, config.DefaultNodeGroups().Platform
	problems := catalog.Check(set, catalog.Options{RenderVersion: crossplaneaws.RenderVersion, SystemGroup: &group, PlatformPool: &platform})
	var text []string
	for _, p := range problems {
		text = append(text, p.String())
	}
	if len(problems) > 0 {
		t.Errorf("the shipped catalog fails the check:\n%s", strings.Join(text, "\n"))
	}
}
