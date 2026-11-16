// Package naas exposes offline, versioned Capsule bootstrap resources.
// It neither installs resources nor qualifies a tenant's isolation or access.
package naas

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"
)

//go:embed assets/bootstrap.yaml
var bootstrapYAML []byte

// Bootstrap is an immutable snapshot of the shared, once-installed resources.
// Its revision describes template content, not readiness or runtime enforcement.
type Bootstrap struct {
	objects  []*unstructured.Unstructured
	revision string
}

// Load reads the embedded canonical bootstrap payload. It performs no I/O other
// than decoding these bytes and makes no Kubernetes, Helm or cloud calls.
func Load() (*Bootstrap, error) { return load(bootstrapYAML) }

func load(data []byte) (*Bootstrap, error) {
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	b := &Bootstrap{}
	seen := map[string]bool{}
	for document := 1; ; document++ {
		var object map[string]any
		if err := decoder.Decode(&object); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("bootstrap document %d: %w", document, err)
		}
		if len(object) == 0 {
			continue
		}
		u := &unstructured.Unstructured{Object: object}
		if u.GetAPIVersion() == "" || u.GetKind() == "" || u.GetName() == "" {
			return nil, fmt.Errorf("bootstrap document %d: apiVersion, kind and metadata.name are required", document)
		}
		key := u.GetAPIVersion() + "/" + u.GetKind() + "/" + u.GetNamespace() + "/" + u.GetName()
		if seen[key] {
			return nil, fmt.Errorf("bootstrap document %d: duplicate object %s", document, key)
		}
		seen[key] = true
		b.objects = append(b.objects, u)
	}
	if len(b.objects) == 0 {
		return nil, fmt.Errorf("bootstrap has no resources")
	}
	// JSON normalizes map ordering and YAML presentation, while retaining document
	// order and every value (including the exact Capsule generator source).
	canonical, err := json.Marshal(b.objects)
	if err != nil {
		return nil, fmt.Errorf("bootstrap content: %w", err)
	}
	sum := sha256.Sum256(canonical)
	b.revision = "sha256:" + hex.EncodeToString(sum[:])
	return b, nil
}

// Revision is the deterministic SHA-256 of the decoded, unescaped resource set.
// Values-only template changes change it; YAML comments and map ordering do not.
func (b *Bootstrap) Revision() string { return b.revision }

// Objects returns independent unstructured copies, with Capsule expressions intact.
// The caller owns these copies; changing them cannot change this snapshot.
func (b *Bootstrap) Objects() []*unstructured.Unstructured {
	out := make([]*unstructured.Unstructured, len(b.objects))
	for i, object := range b.objects {
		out[i] = object.DeepCopy()
	}
	return out
}

// HelmExtraManifests returns independent values for Capsule 0.15.2's
// extraManifests field. That chart calls tpl(toYaml(object), root), so every
// opening Capsule delimiter must survive one Helm evaluation as literal text.
// The result replaces that Helm values list; the future installer must preserve
// any existing entries such as the platform PDB when composing its final values.
func (b *Bootstrap) HelmExtraManifests() []any {
	out := make([]any, len(b.objects))
	for i, object := range b.objects {
		out[i] = escapeHelm(runtime.DeepCopyJSON(object.Object))
	}
	return out
}

func escapeHelm(value any) any {
	switch value := value.(type) {
	case string:
		// Backtick quoting avoids interaction with YAML's own string quoting.
		// Escaping only the opener also preserves trim markers and closing braces.
		return strings.ReplaceAll(value, "{{", "{{`{{`}}")
	case map[string]any:
		out := make(map[string]any, len(value))
		for key, item := range value {
			out[strings.ReplaceAll(key, "{{", "{{`{{`}}")] = escapeHelm(item)
		}
		return out
	case []any:
		for i := range value {
			value[i] = escapeHelm(value[i])
		}
		return value
	default:
		return value
	}
}
