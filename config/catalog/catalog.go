// Package catalog holds the component catalog this build ships: one
// ComponentDefinition per YAML file, embedded so the controller can create
// the missing ones on startup (pkg/catalog). Each definition's chart
// version and caveats are in docs/catalog.md.
package catalog

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"sort"

	"sigs.k8s.io/yaml"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
)

//go:embed *.yaml
var files embed.FS

// kustomization is the file that lists the definitions for kubectl.
const kustomization = "kustomization.yaml"

// Definitions are the shipped definitions, ordered by name. Each file holds
// exactly one, and decodes strictly: a field the kind does not have is an
// error.
func Definitions() ([]*cloudv1alpha1.ComponentDefinition, error) {
	names, err := Files()
	if err != nil {
		return nil, err
	}
	out := make([]*cloudv1alpha1.ComponentDefinition, 0, len(names))
	for _, name := range names {
		def, err := read(name)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		out = append(out, def)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Files are the names of the definition files, sorted.
func Files() ([]string, error) {
	names, err := fs.Glob(files, "*.yaml")
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(names))
	for _, name := range names {
		if name != kustomization {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// Raw is a definition file as shipped.
func Raw(name string) ([]byte, error) { return files.ReadFile(name) }

// read decodes one definition file.
func read(name string) (*cloudv1alpha1.ComponentDefinition, error) {
	raw, err := files.ReadFile(name)
	if err != nil {
		return nil, err
	}
	if bytes.Contains(raw, []byte("\n---")) {
		return nil, fmt.Errorf("holds more than one document; one definition per file")
	}
	def := &cloudv1alpha1.ComponentDefinition{}
	if err := yaml.UnmarshalStrict(raw, def); err != nil {
		return nil, err
	}
	if def.Kind != "ComponentDefinition" || def.APIVersion != cloudv1alpha1.GroupVersion.String() {
		return nil, fmt.Errorf("is %s %s, not a ComponentDefinition", def.APIVersion, def.Kind)
	}
	return def, nil
}
