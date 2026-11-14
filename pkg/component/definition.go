package component

import (
	"bytes"
	"encoding/json"
	"fmt"

	"sigs.k8s.io/yaml"
)

// ParseUserValues decodes a definition's userValues file: YAML that must
// be an object. A file of comments only is no values.
func ParseUserValues(file string) (Values, error) {
	raw, err := yaml.YAMLToJSON([]byte(file))
	if err != nil {
		return nil, fmt.Errorf("not YAML: %w", err)
	}
	if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("not YAML: %w", err)
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("must be an object, got %s", typeName(v))
	}
	return obj, nil
}

// CheckDefinition reports what makes a definition's values unusable, for
// its Valid condition, every problem at once; nil when they are usable:
//   - the values schema uses what the subset does not support (CheckSchema);
//   - platformValues name a placeholder that is not on the list, or write
//     one badly (Validate);
//   - the cloud resources are unusable (ValidateCloudResources: the IAM
//     policy is no policy document, or uses an unknown placeholder);
//   - a Platform component has userValues or a valuesSchema;
//   - a bundle (includes) has an install, values, a schema or cloud
//     resources of its own, or is internal;
//   - userValues does not parse as a YAML object;
//   - userValues and platformValues share a path (the same leaf, or one
//     inside the other): the file would offer a key the tenant may not
//     change;
//   - userValues does not validate against the values schema, or there is
//     no schema (no overrides accepted) and the file sets something.
func CheckDefinition(d Definition) []string {
	var out []string
	var schemaProblems []string
	if d.ValuesSchema != nil {
		schemaProblems = CheckSchema(d.ValuesSchema)
	}
	for _, p := range schemaProblems {
		out = append(out, "valuesSchema: "+p)
	}
	if err := Validate(d.PlatformValues); err != nil {
		out = append(out, "platformValues: "+err.Error())
	}
	out = append(out, providerProblems(d)...)
	if err := ValidateCloudResources(d.CloudResources); err != nil {
		out = append(out, err.Error())
	}
	if d.IsPlatform() && (d.UserValues != "" || d.ValuesSchema != nil) {
		out = append(out, "a Platform component takes no tenant values: remove userValues and valuesSchema")
	}
	out = append(out, bundleProblems(d)...)
	user, err := ParseUserValues(d.UserValues)
	if err != nil {
		return append(out, "userValues: "+err.Error())
	}
	for _, path := range OverlappingPaths(user, d.PlatformValues) {
		out = append(out, "userValues sets "+path+", which platformValues locks")
	}
	switch {
	case len(user) == 0:
	case d.ValuesSchema == nil:
		out = append(out, "userValues sets values, but there is no valuesSchema, so tenants may change nothing")
	case len(schemaProblems) == 0:
		for _, msg := range ValidateSchema(d.ValuesSchema, user) {
			out = append(out, "userValues: "+msg)
		}
	}
	return out
}

// bundleProblems are what a bundle may not have: it installs nothing
// itself, so everything about a chart belongs to its members.
func bundleProblems(d Definition) []string {
	if !d.IsBundle() {
		return nil
	}
	var out []string
	if d.HasInstall {
		out = append(out, "a bundle (includes) has no install of its own")
	}
	if d.Internal {
		out = append(out, "a bundle is not internal: bundles do not nest")
	}
	if len(d.PlatformValues) > 0 || d.UserValues != "" || d.ValuesSchema != nil || d.CloudResources.IAMRole != nil {
		out = append(out, "a bundle has no values, schema or cloud resources: they belong to its members")
	}
	return out
}
