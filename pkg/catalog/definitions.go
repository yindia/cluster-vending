package catalog

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"

	"sigs.k8s.io/yaml"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
)

// definitionsDir holds the shipped definitions: one ComponentDefinition per
// YAML file, named after it. Each definition's chart version and caveats
// are in docs/catalog.md.
const definitionsDir = "definitions"

//go:embed definitions/*.yaml
var files embed.FS

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
	paths, err := fs.Glob(files, definitionsDir+"/*.yaml")
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, path.Base(p))
	}
	sort.Strings(out)
	return out, nil
}

// Raw is a definition file as shipped.
func Raw(name string) ([]byte, error) { return files.ReadFile(path.Join(definitionsDir, name)) }

// read decodes one definition file.
func read(name string) (*cloudv1alpha1.ComponentDefinition, error) {
	raw, err := Raw(name)
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
