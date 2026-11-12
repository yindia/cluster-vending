package deprecation

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"sigs.k8s.io/yaml"
)

// maxReleaseBytes caps a decoded Helm release. The release is written by
// whoever can write Secrets in the cluster, so its compressed size says
// nothing of how far it expands.
const maxReleaseBytes = 32 << 20

// yamlDocumentSeparator is the line that divides the documents of a
// manifest.
const yamlDocumentSeparator = "---"

// errReleaseTooLarge is a release that decodes to more than the limit.
var errReleaseTooLarge = errors.New("helm release is larger than the limit")

// gzipMagic is how a gzip stream begins: its two identification bytes and
// the deflate method.
var gzipMagic = []byte{0x1f, 0x8b, 0x08}

// manifestObject is the part of a manifest that says what the object is.
type manifestObject struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
}

// object is the Object the manifest describes; false when it names no
// apiVersion or kind.
func (m manifestObject) object(source Source) (Object, bool) {
	if m.APIVersion == "" || m.Kind == "" {
		return Object{}, false
	}
	return Object{
		APIVersion: m.APIVersion,
		Kind:       m.Kind,
		Namespace:  m.Metadata.Namespace,
		Name:       m.Metadata.Name,
		Source:     source,
	}, true
}

// helmRelease is the part of a Helm v3 release record that is read.
type helmRelease struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Manifest  string `json:"manifest"`
}

// lastApplied reads the value of a
// kubectl.kubernetes.io/last-applied-configuration annotation: the JSON of
// the object as it was applied. It is false when the value is not JSON or
// names no apiVersion or kind.
func lastApplied(annotation string) (Object, bool) {
	var m manifestObject
	if err := json.Unmarshal([]byte(annotation), &m); err != nil {
		return Object{}, false
	}
	return m.object(SourceLastApplied)
}

// releaseObjects reads the objects of a Helm v3 release. payload is the
// "release" key of the release's Secret as client-go returns it: Helm's own
// base64 of a gzip stream (or, from old releases, of plain JSON) of the
// release record. release is "<namespace>/<name>".
func releaseObjects(payload []byte) (release string, objects []Object, err error) {
	return releaseObjectsLimit(payload, maxReleaseBytes)
}

// releaseObjectsLimit is releaseObjects with the cap on the decoded size
// given.
func releaseObjectsLimit(payload []byte, limit int64) (release string, objects []Object, err error) {
	record, err := decodeRelease(payload, limit)
	if err != nil {
		return "", nil, err
	}
	var rel helmRelease
	if err := json.Unmarshal(record, &rel); err != nil {
		return "", nil, fmt.Errorf("helm release: %w", err)
	}
	if rel.Name == "" {
		return "", nil, errors.New("helm release: no name")
	}
	release = rel.Namespace + "/" + rel.Name
	return release, manifestObjects(rel.Manifest, release), nil
}

// decodeRelease undoes Helm's encoding of a release record, refusing one
// that decodes to more than limit bytes.
func decodeRelease(payload []byte, limit int64) ([]byte, error) {
	raw := make([]byte, base64.StdEncoding.DecodedLen(len(payload)))
	n, err := base64.StdEncoding.Decode(raw, bytes.TrimSpace(payload))
	if err != nil {
		return nil, fmt.Errorf("helm release: base64: %w", err)
	}
	raw = raw[:n]
	if !bytes.HasPrefix(raw, gzipMagic) {
		if int64(len(raw)) > limit {
			return nil, errReleaseTooLarge
		}
		return raw, nil
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("helm release: gzip: %w", err)
	}
	// One byte past the limit tells a release at the limit from one over it.
	record, err := io.ReadAll(io.LimitReader(zr, limit+1))
	if err != nil {
		return nil, fmt.Errorf("helm release: gzip: %w", err)
	}
	if int64(len(record)) > limit {
		return nil, errReleaseTooLarge
	}
	return record, nil
}

// manifestObjects reads the objects of a multi-document YAML manifest.
// Documents that are empty, do not parse or name no apiVersion or kind are
// skipped.
func manifestObjects(manifest, release string) []Object {
	var objects []Object
	for _, doc := range splitDocuments(manifest) {
		var m manifestObject
		if err := yaml.Unmarshal([]byte(doc), &m); err != nil {
			continue
		}
		obj, ok := m.object(SourceHelmRelease)
		if !ok {
			continue
		}
		obj.HelmRelease = release
		objects = append(objects, obj)
	}
	return objects
}

// splitDocuments divides a manifest at the lines that are "---", trailing
// whitespace allowed. Documents with nothing but whitespace are dropped.
func splitDocuments(manifest string) []string {
	var docs []string
	var current strings.Builder
	flush := func() {
		if strings.TrimSpace(current.String()) != "" {
			docs = append(docs, current.String())
		}
		current.Reset()
	}
	// Without its final newline the manifest splits into its lines and no
	// empty one after the last.
	for line := range strings.SplitSeq(strings.TrimSuffix(manifest, "\n"), "\n") {
		if strings.TrimRight(line, " \t\r") == yamlDocumentSeparator {
			flush()
			continue
		}
		current.WriteString(line)
		current.WriteByte('\n')
	}
	flush()
	return docs
}
