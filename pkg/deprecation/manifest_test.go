package deprecation

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
)

// releaseRecord is the JSON Helm stores for a release with the manifest.
func releaseRecord(t *testing.T, namespace, name, manifest string) []byte {
	t.Helper()
	record, err := json.Marshal(map[string]any{
		"name":      name,
		"namespace": namespace,
		"manifest":  manifest,
		"version":   1,
	})
	if err != nil {
		t.Fatalf("marshal release: %v", err)
	}
	return record
}

// gzipped compresses data the way Helm does.
func gzipped(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		t.Fatalf("gzip: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip: %v", err)
	}
	return buf.Bytes()
}

func encoded(data []byte) []byte {
	return []byte(base64.StdEncoding.EncodeToString(data))
}

// releasePayload is the "release" key of a Helm v3 release Secret as
// client-go returns it.
func releasePayload(t *testing.T, namespace, name, manifest string) []byte {
	t.Helper()
	return encoded(gzipped(t, releaseRecord(t, namespace, name, manifest)))
}

func TestLastApplied(t *testing.T) {
	tests := []struct {
		name       string
		annotation string
		want       Object
		wantOK     bool
	}{
		{
			name:       "a namespaced object",
			annotation: `{"apiVersion":"extensions/v1beta1","kind":"Deployment","metadata":{"name":"web","namespace":"shop"},"spec":{"replicas":2}}`,
			want:       Object{APIVersion: "extensions/v1beta1", Kind: "Deployment", Namespace: "shop", Name: "web", Source: SourceLastApplied},
			wantOK:     true,
		},
		{
			name:       "a cluster-scoped object has no namespace",
			annotation: `{"apiVersion":"rbac.authorization.k8s.io/v1beta1","kind":"ClusterRole","metadata":{"name":"reader"}}`,
			want:       Object{APIVersion: "rbac.authorization.k8s.io/v1beta1", Kind: "ClusterRole", Name: "reader", Source: SourceLastApplied},
			wantOK:     true,
		},
		{name: "not JSON", annotation: "apiVersion: v1"},
		{name: "empty", annotation: ""},
		{name: "JSON that is not an object", annotation: `["apiVersion"]`},
		{name: "no apiVersion", annotation: `{"kind":"Deployment","metadata":{"name":"web"}}`},
		{name: "no kind", annotation: `{"apiVersion":"apps/v1","metadata":{"name":"web"}}`},
		{name: "metadata of the wrong type", annotation: `{"apiVersion":"apps/v1","kind":"Deployment","metadata":"web"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := lastApplied(tt.annotation)

			if ok != tt.wantOK {
				t.Fatalf("lastApplied() ok = %v, want %v", ok, tt.wantOK)
			}
			if got != tt.want {
				t.Errorf("lastApplied() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

const mixedManifest = `---
# Source: shop/templates/pdb.yaml
apiVersion: policy/v1beta1
kind: PodDisruptionBudget
metadata:
  name: web
  namespace: shop
spec:
  minAvailable: 1
---
# Source: shop/templates/empty.yaml
# nothing rendered
---

---
# Source: shop/templates/deployment.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
spec:
  template:
    metadata:
      annotations:
        note: "--- is not a separator inside a line"
---
this: [is not, valid
---
kind: NoAPIVersion
metadata:
  name: x
---
- a
- list
`

func TestReleaseObjects(t *testing.T) {
	wantObjects := []Object{
		{APIVersion: "policy/v1beta1", Kind: "PodDisruptionBudget", Namespace: "shop", Name: "web", Source: SourceHelmRelease, HelmRelease: "shop/storefront"},
		{APIVersion: "apps/v1", Kind: "Deployment", Name: "web", Source: SourceHelmRelease, HelmRelease: "shop/storefront"},
	}
	tests := []struct {
		name    string
		payload []byte
	}{
		{name: "a gzipped release", payload: releasePayload(t, "shop", "storefront", mixedManifest)},
		{name: "an un-gzipped release", payload: encoded(releaseRecord(t, "shop", "storefront", mixedManifest))},
		{name: "windows line endings", payload: releasePayload(t, "shop", "storefront", strings.ReplaceAll(mixedManifest, "\n", "\r\n"))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			release, objects, err := releaseObjects(tt.payload)

			if err != nil {
				t.Fatalf("releaseObjects() error = %v", err)
			}
			if release != "shop/storefront" {
				t.Errorf("release = %q, want %q", release, "shop/storefront")
			}
			if !slices.Equal(objects, wantObjects) {
				t.Errorf("objects = %+v, want %+v", objects, wantObjects)
			}
		})
	}
}

func TestReleaseObjectsEmptyManifest(t *testing.T) {
	release, objects, err := releaseObjects(releasePayload(t, "shop", "empty", ""))

	if err != nil {
		t.Fatalf("releaseObjects() error = %v", err)
	}
	if release != "shop/empty" || len(objects) != 0 {
		t.Errorf("release = %q, objects = %+v, want shop/empty and none", release, objects)
	}
}

func TestReleaseObjectsMalformed(t *testing.T) {
	truncated := gzipped(t, releaseRecord(t, "shop", "storefront", mixedManifest))
	truncated = truncated[:len(truncated)/2]
	tests := []struct {
		name    string
		payload []byte
	}{
		{name: "nil", payload: nil},
		{name: "not base64", payload: []byte("!!! not base64 !!!")},
		{name: "base64 of garbage", payload: encoded([]byte{0x00, 0x01, 0x02, 0xff})},
		{name: "gzip magic and nothing else", payload: encoded([]byte{0x1f, 0x8b, 0x08})},
		{name: "a truncated gzip stream", payload: encoded(truncated)},
		{name: "gzip of something that is not JSON", payload: encoded(gzipped(t, []byte("manifest: nope")))},
		{name: "a release without a name", payload: encoded([]byte(`{"namespace":"shop","manifest":""}`))},
		{name: "JSON of the wrong shape", payload: encoded([]byte(`{"name":{"a":1}}`))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			release, objects, err := releaseObjects(tt.payload)

			if err == nil {
				t.Fatalf("releaseObjects() = %q, %+v, want an error", release, objects)
			}
			if release != "" || objects != nil {
				t.Errorf("releaseObjects() = %q, %+v alongside the error, want nothing", release, objects)
			}
		})
	}
}

func TestReleaseObjectsLimit(t *testing.T) {
	record := releaseRecord(t, "shop", "storefront", mixedManifest)
	size := int64(len(record))
	tests := []struct {
		name    string
		payload []byte
		limit   int64
		wantErr bool
	}{
		{name: "gzipped, at the limit", payload: encoded(gzipped(t, record)), limit: size},
		{name: "gzipped, one byte over the limit", payload: encoded(gzipped(t, record)), limit: size - 1, wantErr: true},
		{name: "un-gzipped, at the limit", payload: encoded(record), limit: size},
		{name: "un-gzipped, one byte over the limit", payload: encoded(record), limit: size - 1, wantErr: true},
		{
			name:    "a small stream that expands far past the limit",
			payload: encoded(gzipped(t, releaseRecord(t, "shop", "bomb", strings.Repeat("#", 1<<20)))),
			limit:   1 << 10,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, objects, err := releaseObjectsLimit(tt.payload, tt.limit)

			if tt.wantErr {
				if !errors.Is(err, errReleaseTooLarge) {
					t.Fatalf("error = %v, want errReleaseTooLarge", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("releaseObjectsLimit() error = %v", err)
			}
			if len(objects) != 2 {
				t.Errorf("got %d objects, want 2", len(objects))
			}
		})
	}
}

func TestSplitDocuments(t *testing.T) {
	tests := []struct {
		name     string
		manifest string
		want     []string
	}{
		{name: "empty", manifest: "", want: nil},
		{name: "only separators", manifest: "---\n---\n", want: nil},
		{name: "one document without separator", manifest: "a: 1", want: []string{"a: 1\n"}},
		{name: "leading separator", manifest: "---\na: 1\n", want: []string{"a: 1\n"}},
		{name: "separator with trailing whitespace", manifest: "a: 1\n--- \t\nb: 2\n", want: []string{"a: 1\n", "b: 2\n"}},
		{name: "a longer dash line is not a separator", manifest: "a: 1\n----\nb: 2\n", want: []string{"a: 1\n----\nb: 2\n"}},
		{name: "an indented separator is not a separator", manifest: "a: |\n  ---\n", want: []string{"a: |\n  ---\n"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitDocuments(tt.manifest)

			if !slices.Equal(got, tt.want) {
				t.Errorf("splitDocuments() = %q, want %q", got, tt.want)
			}
		})
	}
}
