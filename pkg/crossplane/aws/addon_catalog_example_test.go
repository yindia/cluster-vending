package aws_test

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
)

// examples/cluster.yaml lists the add-ons addons.yaml knows (commented or
// not) and its default set: they must not drift from the file.
func TestExampleClusterNamesTheCatalogsAddons(t *testing.T) {
	raw, err := os.ReadFile("../../../examples/cluster.yaml")
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	start := strings.Index(text, "    addons:")
	end := strings.Index(text, "# Defaults can also be listed")
	if start < 0 || end < start {
		t.Fatal("no addons block in examples/cluster.yaml")
	}
	known := crossplaneaws.KnownAddons()

	var listed []string
	for _, m := range regexp.MustCompile(`(?m)^\s*(?:#\s*)?-\s*name:\s*(\S+)`).FindAllStringSubmatch(text[start:end], -1) {
		listed = append(listed, m[1])
	}
	for _, name := range listed {
		if !slices.Contains(known, name) {
			t.Errorf("examples/cluster.yaml names add-on %q; addons.yaml does not know it", name)
		}
	}
	for _, name := range known {
		if !slices.Contains(listed, name) && !slices.Contains(crossplaneaws.DefaultAddons, name) {
			t.Errorf("addons.yaml knows %q; examples/cluster.yaml does not list it", name)
		}
	}

	defaults := regexp.MustCompile(`default set of pkg/crossplane/aws/addons\.yaml\s*\n\s*#\s*\(([^)]*)\)`).FindStringSubmatch(text)
	if defaults == nil {
		t.Fatal("examples/cluster.yaml does not name the default set")
	}
	if got := strings.Split(defaults[1], ", "); !slices.Equal(got, crossplaneaws.DefaultAddons) {
		t.Errorf("examples/cluster.yaml's default set %v, addons.yaml's %v", got, crossplaneaws.DefaultAddons)
	}
}
