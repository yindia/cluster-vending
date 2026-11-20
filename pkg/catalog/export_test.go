package catalog_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/catalog"
	"github.com/yindia/cluster-vending/pkg/kube"
)

var exportNow = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

// live is a management cluster as the API server hands the objects back:
// uid, resourceVersion, managed fields, finalizers, a status, the shipped
// label and hash of the build that created them, and no serial on the
// releases.
func live(t *testing.T, mutate func(*catalog.Set)) client.WithWatch {
	t.Helper()
	set := goodSet()
	set.Releases = append(set.Releases, mkRelease("2026-11-b", 2, nil), mkRelease("2026-11-c", 3, nil))
	if mutate != nil {
		mutate(&set)
	}
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	b := fake.NewClientBuilder().WithScheme(scheme)
	var objs []client.Object
	noise := func(m *metav1.ObjectMeta, age time.Duration) {
		delete(m.Annotations, catalog.SchemaAnnotation) // the cluster never had it
		m.UID = types.UID("uid-" + m.Name)
		m.Generation = 3
		m.Finalizers = []string{"cloud.cluster-vending.io/release-in-use"}
		m.CreationTimestamp = metav1.NewTime(exportNow.Add(-age))
		m.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: "kubectl", Operation: metav1.ManagedFieldsOperationUpdate, APIVersion: "cloud.cluster-vending.io/v1alpha1", FieldsType: "FieldsV1", FieldsV1: metav1.NewFieldsV1("{}")}}
		m.Labels = map[string]string{cloudv1alpha1.LabelShipped: "true", "team": "platform"}
		m.Annotations = map[string]string{catalog.ShippedHashAnnotation: "abc", "kubectl.kubernetes.io/last-applied-configuration": "{}"}
	}
	for i := range set.Definitions {
		d := &set.Definitions[i]
		noise(&d.ObjectMeta, time.Duration(i+1)*time.Hour)
		d.Status.ObservedGeneration = 3
		objs = append(objs, d)
	}
	for i := range set.Releases {
		r := &set.Releases[i]
		noise(&r.ObjectMeta, time.Duration(10-i)*time.Hour)
		r.Spec.Serial = nil // predates serials
		r.Status.Channels = []string{"alpha"}
		objs = append(objs, r)
	}
	for i := range set.Channels {
		c := &set.Channels[i]
		noise(&c.ObjectMeta, time.Hour)
		c.Status.Release = "2026-11-a"
		objs = append(objs, c)
	}
	return b.WithObjects(objs...).Build()
}

func TestExportIsCleanAndPassesTheCheck(t *testing.T) {
	set, err := catalog.Export(t.Context(), live(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	if problems := catalog.Check(set, checkOptions); len(problems) > 0 {
		t.Fatalf("export of a live cluster fails the check: %v", problemStrings(problems))
	}
	var out bytes.Buffer
	if err := set.WriteStream(&out); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, banned := range []string{"status:", "uid", "resourceVersion", "managedFields", "creationTimestamp", "finalizers", "generation",
		cloudv1alpha1.LabelShipped, catalog.ShippedHashAnnotation, "last-applied", "null"} {
		if strings.Contains(text, banned) {
			t.Errorf("the export holds %q:\n%s", banned, text)
		}
	}
	for _, want := range []string{catalog.SchemaAnnotation + ": \"1\"", "team: platform", "kind: ReleaseChannel", "serial: 1\n"} {
		if !strings.Contains(text, want) {
			t.Errorf("the export lacks %q:\n%s", want, text)
		}
	}
}

// Serials are backfilled in creation order: the releases were created
// oldest first by name here, with the newest object listed first.
func TestExportBackfillsSerialsInCreationOrder(t *testing.T) {
	set, err := catalog.Export(t.Context(), live(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, r := range set.Releases {
		got[r.Name] = *r.Spec.Serial
	}
	// noise ages: a is 10h, b 9h, c 8h old: a is the oldest.
	if got["2026-11-a"] != 1 || got["2026-11-b"] != 2 || got["2026-11-c"] != 3 {
		t.Errorf("serials = %v, want a=1 b=2 c=3", got)
	}
}

func TestExportOrderIsStable(t *testing.T) {
	var first string
	for i := 0; i < 4; i++ {
		set, err := catalog.Export(t.Context(), live(t, nil))
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err := set.WriteStream(&out); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = out.String()
		} else if out.String() != first {
			t.Fatalf("export %d differs from the first", i)
		}
	}
	// Definitions by name, then releases, then channels alpha, beta, stable.
	order := []string{"name: karpenter", "name: mesh", "name: tool", "name: 2026-11-a", "name: 2026-11-b", "name: 2026-11-c", "name: alpha", "name: beta", "name: stable"}
	at := -1
	for _, w := range order {
		i := strings.Index(first, w+"\n")
		if i < at {
			t.Fatalf("%q out of order in:\n%s", w, first)
		}
		at = i
	}
}

// The export only reads: no create, update, patch or delete.
func TestExportWritesNothing(t *testing.T) {
	writes := 0
	count := func() { writes++ }
	base := live(t, nil)
	c := interceptor.NewClient(base, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			count()
			return c.Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			count()
			return c.Update(ctx, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			count()
			return c.Patch(ctx, obj, patch, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			count()
			return c.Delete(ctx, obj, opts...)
		},
	})
	if _, err := catalog.Export(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	if writes != 0 {
		t.Errorf("the export made %d writes", writes)
	}
}

// Export, apply to another cluster, export again: no diff, and the second
// cluster's objects carry the first export's specs.
func TestExportRoundTrip(t *testing.T) {
	first, err := catalog.Export(t.Context(), live(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	var stream bytes.Buffer
	if err := first.WriteStream(&stream); err != nil {
		t.Fatal(err)
	}
	decoded, problems := catalog.Decode(&stream)
	if len(problems) > 0 {
		t.Fatal(problems)
	}
	if problems := catalog.Check(decoded, checkOptions); len(problems) > 0 {
		t.Fatalf("the exported stream fails the check: %v", problemStrings(problems))
	}

	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	other := fake.NewClientBuilder().WithScheme(scheme).Build()
	for i := range decoded.Definitions {
		mustCreate(t, other, &decoded.Definitions[i])
	}
	for i := range decoded.Releases {
		mustCreate(t, other, &decoded.Releases[i])
	}
	for i := range decoded.Channels {
		mustCreate(t, other, &decoded.Channels[i])
	}
	second, err := catalog.Export(t.Context(), other)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		var a, b bytes.Buffer
		_ = first.WriteStream(&a)
		_ = second.WriteStream(&b)
		t.Errorf("a second export differs.\nfirst:\n%s\nsecond:\n%s", a.String(), b.String())
	}
}

func mustCreate(t *testing.T, c client.Client, obj client.Object) {
	t.Helper()
	if err := c.Create(t.Context(), obj); err != nil {
		t.Fatalf("apply %s: %v", obj.GetName(), err)
	}
}

func TestWriteDirLayout(t *testing.T) {
	set, err := catalog.Export(t.Context(), live(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := set.WriteDir(dir); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"kustomization.yaml", "definitions/karpenter.yaml", "definitions/mesh.yaml", "releases/2026-11-a.yaml",
		"releases/2026-11-c.yaml", "channels/alpha.yaml", "channels/beta.yaml", "channels/stable.yaml"} {
		if _, err := os.Stat(filepath.Join(dir, path)); err != nil {
			t.Errorf("%s: %v", path, err)
		}
	}
	kustomization, err := os.ReadFile(filepath.Join(dir, "kustomization.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(kustomization), "- definitions/karpenter.yaml") || !strings.Contains(string(kustomization), "- channels/stable.yaml") {
		t.Errorf("kustomization lists:\n%s", kustomization)
	}
	// Each file holds one object, which reads back and checks.
	var all bytes.Buffer
	for _, sub := range []string{"definitions", "releases", "channels"} {
		entries, err := os.ReadDir(filepath.Join(dir, sub))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			data, err := os.ReadFile(filepath.Join(dir, sub, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(string(data), "\nkind: ")+boolInt(strings.HasPrefix(string(data), "kind: ")) != 1 {
				t.Errorf("%s/%s holds more or less than one object", sub, e.Name())
			}
			all.WriteString("---\n")
			all.Write(data)
		}
	}
	read, problems := catalog.Decode(&all)
	if len(problems) > 0 {
		t.Fatal(problems)
	}
	if ps := catalog.Check(read, checkOptions); len(ps) > 0 {
		t.Errorf("the files fail the check: %v", problemStrings(ps))
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// The export tells what differs from this build's embedded catalog.
func TestDifferencesFromTheBuild(t *testing.T) {
	shipped, err := catalog.Definitions()
	if err != nil {
		t.Fatal(err)
	}
	var set catalog.Set
	for _, d := range shipped {
		set.Definitions = append(set.Definitions, *d.DeepCopy())
	}
	if diff, err := catalog.Differences(set); err != nil || len(diff) != 0 {
		t.Fatalf("an unchanged catalog differs: %v, %v", diff, err)
	}
	for i := range set.Definitions {
		switch set.Definitions[i].Name {
		case "karpenter":
			set.Definitions[i].Spec.Description = "edited"
		case "kyverno":
			set.Definitions[i].Spec.Disabled = true
		}
	}
	set.Definitions = append(set.Definitions[1:], mkDef("mine", nil))
	gone := shipped[0].Name
	diff, err := catalog.Differences(set)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(diff, "\n")
	for _, want := range []string{"ComponentDefinition/karpenter: spec differs from this build's (spec.description)",
		"ComponentDefinition/kyverno: spec.disabled is true", "ComponentDefinition/mine: not in this build's catalog",
		"ComponentDefinition/" + gone + ": in this build's catalog, missing from the cluster"} {
		if !strings.Contains(text, want) {
			t.Errorf("differences =\n%s\nwant %q", text, want)
		}
	}
}

func TestDecode(t *testing.T) {
	doc := `
---
# a comment-only document is no object

---
apiVersion: v1
kind: ConfigMap
metadata: {name: ignored}
---
apiVersion: cloud.cluster-vending.io/v1alpha1
kind: ReleaseChannel
metadata: {name: beta}
spec: {promotion: Automatic}
---
apiVersion: cloud.cluster-vending.io/v1alpha1
kind: ReleaseChannel
metadata: {name: stable}
spec: {promotion: Automatic, noSuchField: 1}
`
	set, problems := catalog.Decode(strings.NewReader(doc))
	if len(set.Channels) != 1 || set.Channels[0].Name != "beta" {
		t.Errorf("channels = %v, want only beta", set.Channels)
	}
	if len(problems) != 1 || !strings.Contains(problems[0].String(), "ReleaseChannel/stable: cannot be read") || !strings.Contains(problems[0].Message, "noSuchField") {
		t.Errorf("problems = %v", problems)
	}
	if _, problems := catalog.Decode(strings.NewReader("{{{")); len(problems) == 0 {
		t.Error("garbage is read without a problem")
	}
}

// RunExport writes the stream to out, or the files to a directory, and
// reports the differences from the build on stderr.
func TestRunExport(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := catalog.RunExport(t.Context(), live(t, nil), "", &out, &errOut); err != nil {
		t.Fatal(err)
	}
	set, problems := catalog.Decode(&out)
	if len(problems) > 0 || len(set.Releases) != 3 || len(set.Channels) != 3 {
		t.Fatalf("stream: %v %+v", problems, set)
	}
	if !strings.Contains(errOut.String(), "differs from the build: ComponentDefinition/mesh: not in this build's catalog") {
		t.Errorf("stderr = %q, want the difference listed", errOut.String())
	}
	dir := t.TempDir()
	out.Reset()
	if err := catalog.RunExport(t.Context(), live(t, nil), dir, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want nothing when writing a directory", out.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "definitions", "karpenter.yaml")); err != nil {
		t.Error(err)
	}
}
