package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
)

// Directories of the exported catalog, as config/catalog lays them out
// (PROP-01 4.2).
const (
	DefinitionsDir = "definitions"
	ReleasesDir    = "releases"
	ChannelsDir    = "channels"
)

// kubectlLastApplied is what `kubectl apply` records on an object; it is
// not part of the catalog.
const kubectlLastApplied = "kubectl.kubernetes.io/last-applied-configuration"

// Export reads the ComponentDefinitions, Releases and ReleaseChannels of
// the management cluster and returns them as clean manifests: no status, no
// uid, resourceVersion, managed fields, finalizers or timestamps; the
// shipped label and shipped-hash annotation stripped; the schema annotation
// added; and a serial on every release that has none (BackfillSerials:
// creation order, ties by name). The objects are in their stable order
// (Set.Sort). It only reads.
func Export(ctx context.Context, c client.Reader) (Set, error) {
	defs := &cloudv1alpha1.ComponentDefinitionList{}
	if err := c.List(ctx, defs); err != nil {
		return Set{}, fmt.Errorf("list component definitions: %w", err)
	}
	rels := &cloudv1alpha1.ReleaseList{}
	if err := c.List(ctx, rels); err != nil {
		return Set{}, fmt.Errorf("list releases: %w", err)
	}
	chans := &cloudv1alpha1.ReleaseChannelList{}
	if err := c.List(ctx, chans); err != nil {
		return Set{}, fmt.Errorf("list release channels: %w", err)
	}
	out := Set{Releases: cloudv1alpha1.BackfillSerials(rels.Items)}
	for i := range defs.Items {
		d := defs.Items[i].DeepCopy()
		d.TypeMeta = typeMeta("ComponentDefinition")
		d.ObjectMeta = cleanMeta(d.ObjectMeta)
		d.Status = cloudv1alpha1.ComponentDefinitionStatus{}
		out.Definitions = append(out.Definitions, *d)
	}
	for i := range out.Releases {
		r := &out.Releases[i]
		r.TypeMeta = typeMeta("Release")
		r.ObjectMeta = cleanMeta(r.ObjectMeta)
		r.Status = cloudv1alpha1.ReleaseStatus{}
	}
	for i := range chans.Items {
		ch := chans.Items[i].DeepCopy()
		ch.TypeMeta = typeMeta("ReleaseChannel")
		ch.ObjectMeta = cleanMeta(ch.ObjectMeta)
		ch.Status = cloudv1alpha1.ReleaseChannelStatus{}
		out.Channels = append(out.Channels, *ch)
	}
	return out.Sort(), nil
}

func typeMeta(kind string) metav1.TypeMeta {
	return metav1.TypeMeta{APIVersion: cloudv1alpha1.GroupVersion.String(), Kind: kind}
}

// cleanMeta keeps what a manifest says: the name, the labels and the
// annotations the platform team set, and the schema annotation.
func cleanMeta(in metav1.ObjectMeta) metav1.ObjectMeta {
	out := metav1.ObjectMeta{Name: in.Name}
	for k, v := range in.Labels {
		if k != cloudv1alpha1.LabelShipped {
			if out.Labels == nil {
				out.Labels = map[string]string{}
			}
			out.Labels[k] = v
		}
	}
	out.Annotations = map[string]string{SchemaAnnotation: SchemaVersion}
	for k, v := range in.Annotations {
		if k != ShippedHashAnnotation && k != kubectlLastApplied && k != SchemaAnnotation {
			out.Annotations[k] = v
		}
	}
	return out
}

// document is one object as YAML: its fields in the order sigs.k8s.io/yaml
// gives (keys sorted), without the empty status and creation time a typed
// object marshals.
func document(obj any) ([]byte, error) {
	raw, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	m := map[string]any{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	delete(m, "status")
	if meta, ok := m["metadata"].(map[string]any); ok {
		delete(meta, "creationTimestamp")
	}
	return yaml.Marshal(m)
}

// file is a manifest file of the export: its path under the output
// directory and its content.
type file struct {
	path string
	data []byte
}

// files are the manifests of the set, one per object, in the layout of
// config/catalog, and the kustomization that lists them.
func (s Set) files() ([]file, error) {
	s = s.Sort()
	var out []file
	var resources []string
	add := func(dir, name string, obj any) error {
		data, err := document(obj)
		if err != nil {
			return fmt.Errorf("%s/%s: %w", dir, name, err)
		}
		path := dir + "/" + name + ".yaml"
		out = append(out, file{path: path, data: data})
		resources = append(resources, path)
		return nil
	}
	for i := range s.Definitions {
		if err := add(DefinitionsDir, s.Definitions[i].Name, &s.Definitions[i]); err != nil {
			return nil, err
		}
	}
	for i := range s.Releases {
		if err := add(ReleasesDir, s.Releases[i].Name, &s.Releases[i]); err != nil {
			return nil, err
		}
	}
	for i := range s.Channels {
		if err := add(ChannelsDir, s.Channels[i].Name, &s.Channels[i]); err != nil {
			return nil, err
		}
	}
	kustomization, err := yaml.Marshal(map[string]any{
		"apiVersion": "kustomize.config.k8s.io/v1beta1", "kind": "Kustomization", "resources": resources})
	if err != nil {
		return nil, err
	}
	return append([]file{{path: "kustomization.yaml", data: kustomization}}, out...), nil
}

// WriteDir writes the set under dir as config/catalog lays it out:
// definitions/, releases/ and channels/ with one file per object, and a
// kustomization.yaml that lists them. The directory is created; a file
// that exists is replaced.
func (s Set) WriteDir(dir string) error {
	files, err := s.files()
	if err != nil {
		return err
	}
	for _, f := range files {
		path := filepath.Join(dir, filepath.FromSlash(f.path))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return err
		}
		if err := os.WriteFile(path, f.data, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// WriteStream writes the set as one multi-document YAML stream, objects in
// their stable order: what `kustomize build` prints, and what Decode reads.
func (s Set) WriteStream(w io.Writer) error {
	files, err := s.files()
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	for _, f := range files[1:] { // not the kustomization
		buf.WriteString("---\n")
		buf.Write(f.data)
	}
	_, err = w.Write(buf.Bytes())
	return err
}

// Differences lists, one line each, how the live definitions differ from
// the ones this build ships (definitions/): a spec that changed since (a
// description edited, a chart repinned), a flag the platform team set, a
// definition the build does not have, and one it has that is missing. For
// the operator reviewing an export: what is intended moves into an overlay.
func Differences(live Set) ([]string, error) {
	shipped, err := Definitions()
	if err != nil {
		return nil, err
	}
	byName := map[string]*cloudv1alpha1.ComponentDefinition{}
	for _, d := range shipped {
		byName[d.Name] = d
	}
	var out []string
	seen := map[string]bool{}
	for _, d := range live.Definitions {
		seen[d.Name] = true
		want, ok := byName[d.Name]
		switch {
		case !ok:
			out = append(out, fmt.Sprintf("ComponentDefinition/%s: not in this build's catalog", d.Name))
		case SpecHash(d.Spec) != SpecHash(want.Spec):
			out = append(out, fmt.Sprintf("ComponentDefinition/%s: spec differs from this build's (%s)", d.Name, changedFields(want.Spec, d.Spec)))
		case d.Spec.Disabled != want.Spec.Disabled:
			out = append(out, fmt.Sprintf("ComponentDefinition/%s: spec.disabled is %t, this build's is %t", d.Name, d.Spec.Disabled, want.Spec.Disabled))
		}
	}
	for name := range byName {
		if !seen[name] {
			out = append(out, fmt.Sprintf("ComponentDefinition/%s: in this build's catalog, missing from the cluster", name))
		}
	}
	slices.Sort(out)
	return out, nil
}

// changedFields names the top-level spec fields that differ.
func changedFields(want, got cloudv1alpha1.ComponentDefinitionSpec) string {
	a, b := map[string]any{}, map[string]any{}
	wantRaw, _ := json.Marshal(want)
	gotRaw, _ := json.Marshal(got)
	_ = json.Unmarshal(wantRaw, &a)
	_ = json.Unmarshal(gotRaw, &b)
	var fields []string
	for k := range a {
		if k == "disabled" {
			continue
		}
		if !jsonEqual(a[k], b[k]) {
			fields = append(fields, k)
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok && k != "disabled" && !(k == "sequence" && jsonEqual(b[k], float64(cloudv1alpha1.DefaultComponentSequence))) {
			fields = append(fields, k)
		}
	}
	slices.Sort(fields)
	fields = slices.Compact(fields)
	if len(fields) == 0 {
		return "defaults"
	}
	return "spec." + joinComma(fields)
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

func joinComma(s []string) string {
	var buf bytes.Buffer
	for i, v := range s {
		if i > 0 {
			buf.WriteString(", spec.")
		}
		buf.WriteString(v)
	}
	return buf.String()
}
