package catalog_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/catalog"
)

// spec is a small definition spec; description tells versions apart.
func spec(description string) cloudv1alpha1.ComponentDefinitionSpec {
	return cloudv1alpha1.ComponentDefinitionSpec{Description: description, Namespace: "lb",
		Install: &cloudv1alpha1.ComponentInstall{Helm: &cloudv1alpha1.HelmInstall{
			HelmChartSource: cloudv1alpha1.HelmChartSource{Repository: "https://example.com/charts", Chart: "lb"}}}}
}

func definition(name string, s cloudv1alpha1.ComponentDefinitionSpec) *cloudv1alpha1.ComponentDefinition {
	return &cloudv1alpha1.ComponentDefinition{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: s}
}

// shipped is what an earlier build left: labelled, with the hash of what it
// shipped.
func shipped(name string, s cloudv1alpha1.ComponentDefinitionSpec) *cloudv1alpha1.ComponentDefinition {
	d := definition(name, s)
	d.Labels = map[string]string{cloudv1alpha1.LabelShipped: "true"}
	d.Annotations = map[string]string{catalog.ShippedHashAnnotation: catalog.SpecHash(s)}
	return d
}

func get(t *testing.T, c client.Client, name string) *cloudv1alpha1.ComponentDefinition {
	t.Helper()
	d := &cloudv1alpha1.ComponentDefinition{}
	if err := c.Get(t.Context(), client.ObjectKey{Name: name}, d); err != nil {
		return nil
	}
	return d
}

// What a new build does with each definition on the management cluster.
func TestSyncShippedDefinitions(t *testing.T) {
	v1, v2 := spec("version one"), spec("version two")
	edited := spec("the platform team's")
	disabledV1 := v1
	disabledV1.Disabled = true
	withDrop := v1
	withDrop.Requires = []string{"dropped"} // a field the new file no longer has

	tests := []struct {
		name     string
		existing []client.Object
		build    []*cloudv1alpha1.ComponentDefinition
		// want is each definition's description afterwards; "" when gone.
		want map[string]string
		// result is what Sync reports.
		result catalog.Result
		check  func(*testing.T, client.Client)
	}{
		{name: "missing: created, labelled",
			build: []*cloudv1alpha1.ComponentDefinition{definition("lb", v1)},
			want:  map[string]string{"lb": "version one"}, result: catalog.Result{Created: []string{"lb"}},
			check: func(t *testing.T, c client.Client) {
				d := get(t, c, "lb")
				if d.Labels[cloudv1alpha1.LabelShipped] != "true" || d.Annotations[catalog.ShippedHashAnnotation] != catalog.SpecHash(v1) {
					t.Errorf("created without the shipped label and hash: %v %v", d.Labels, d.Annotations)
				}
			}},
		{name: "unedited and the build changed it: replaced",
			existing: []client.Object{shipped("lb", withDrop)}, build: []*cloudv1alpha1.ComponentDefinition{definition("lb", v2)},
			want: map[string]string{"lb": "version two"}, result: catalog.Result{Updated: []string{"lb"}},
			check: func(t *testing.T, c client.Client) {
				d := get(t, c, "lb")
				if len(d.Spec.Requires) != 0 {
					t.Errorf("a field the new build dropped stayed: %v", d.Spec.Requires)
				}
				if d.Annotations[catalog.ShippedHashAnnotation] != catalog.SpecHash(v2) {
					t.Error("the hash was not updated")
				}
			}},
		{name: "unedited and the same: left",
			existing: []client.Object{shipped("lb", v1)}, build: []*cloudv1alpha1.ComponentDefinition{definition("lb", v1)},
			want: map[string]string{"lb": "version one"}, result: catalog.Result{Existing: []string{"lb"}}},
		{name: "edited: the platform team's, left",
			existing: []client.Object{func() client.Object { d := shipped("lb", v1); d.Spec = edited; return d }()},
			build:    []*cloudv1alpha1.ComponentDefinition{definition("lb", v2)},
			want:     map[string]string{"lb": "the platform team's"}, result: catalog.Result{Edited: []string{"lb"}}},
		{name: "disabled by the platform team: updated, still disabled",
			existing: []client.Object{func() client.Object { d := shipped("lb", v1); d.Spec = disabledV1; return d }()},
			build:    []*cloudv1alpha1.ComponentDefinition{definition("lb", v2)},
			want:     map[string]string{"lb": "version two"}, result: catalog.Result{Updated: []string{"lb"}},
			check: func(t *testing.T, c client.Client) {
				if !get(t, c, "lb").Spec.Disabled {
					t.Error("the platform team's disabled flag was lost")
				}
			}},
		{name: "no label, same as this build: adopted",
			existing: []client.Object{definition("lb", v2)}, build: []*cloudv1alpha1.ComponentDefinition{definition("lb", v2)},
			want: map[string]string{"lb": "version two"}, result: catalog.Result{Adopted: []string{"lb"}},
			check: func(t *testing.T, c client.Client) {
				if get(t, c, "lb").Labels[cloudv1alpha1.LabelShipped] != "true" {
					t.Error("not adopted")
				}
			}},
		{name: "no label, different: left",
			existing: []client.Object{definition("lb", v1)}, build: []*cloudv1alpha1.ComponentDefinition{definition("lb", v2)},
			want: map[string]string{"lb": "version one"}, result: catalog.Result{Foreign: []string{"lb"}}},
		{name: "shipped, unedited, gone from the build: deleted",
			existing: []client.Object{shipped("gateway-api-crds", v1)}, build: nil,
			want: map[string]string{"gateway-api-crds": ""}, result: catalog.Result{Deleted: []string{"gateway-api-crds"}}},
		{name: "shipped, edited, gone from the build: left",
			existing: []client.Object{func() client.Object { d := shipped("gateway-api-crds", v1); d.Spec = edited; return d }()},
			want:     map[string]string{"gateway-api-crds": "the platform team's"}, result: catalog.Result{Edited: []string{"gateway-api-crds"}}},
		{name: "the platform team's own, not in the build: left",
			existing: []client.Object{definition("external-dns", v1)},
			want:     map[string]string{"external-dns": "version one"}, result: catalog.Result{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newClient(t, tt.existing...)

			res, err := catalog.Sync(t.Context(), c, tt.build)

			if err != nil {
				t.Fatalf("Sync: %v", err)
			}
			if !equalResult(res, tt.result) {
				t.Errorf("result = %+v, want %+v", res, tt.result)
			}
			for name, want := range tt.want {
				d := get(t, c, name)
				switch {
				case want == "" && d != nil:
					t.Errorf("%s still exists", name)
				case want != "" && (d == nil || d.Spec.Description != want):
					t.Errorf("%s = %v, want description %q", name, d, want)
				}
			}
			if tt.check != nil {
				tt.check(t, c)
			}
		})
	}
}

func equalResult(a, b catalog.Result) bool {
	return slices.Equal(a.Created, b.Created) && slices.Equal(a.Updated, b.Updated) && slices.Equal(a.Existing, b.Existing) &&
		slices.Equal(a.Adopted, b.Adopted) && slices.Equal(a.Edited, b.Edited) && slices.Equal(a.Foreign, b.Foreign) &&
		slices.Equal(a.Deleted, b.Deleted)
}

// The hash is of the spec alone, the same for the same spec, other for
// another, and ignores the platform team's disabled flag.
func TestSpecHash(t *testing.T) {
	a, b := spec("one"), spec("two")
	off := a
	off.Disabled = true
	if catalog.SpecHash(a) != catalog.SpecHash(spec("one")) || catalog.SpecHash(a) == catalog.SpecHash(b) {
		t.Error("the hash does not follow the spec")
	}
	if catalog.SpecHash(off) != catalog.SpecHash(a) {
		t.Error("disabled changes the hash")
	}
}

// The API server fills a CRD default into every stored definition: a field
// added later with a default (sequence, 100) appears on definitions an
// earlier build shipped and hashed without it. That is not an edit: the
// definition is still updated to this build's spec, and one no longer in
// the build is still deleted. A real edit is still left alone.
func TestSyncTreatsCRDDefaultsAsUnedited(t *testing.T) {
	old := spec("version one") // hashed before sequence existed
	stored := func(name string) *cloudv1alpha1.ComponentDefinition {
		d := shipped(name, old)
		d.Spec.Sequence = ptrTo(int32(100)) // what the API server returns now
		return d
	}
	first := spec("version two")
	first.Sequence = ptrTo(int32(0)) // this build moves karpenter first
	c := newClient(t, stored("karpenter"), stored("cluster-autoscaler"))

	res, err := catalog.Sync(t.Context(), c, []*cloudv1alpha1.ComponentDefinition{definition("karpenter", first)})
	if err != nil {
		t.Fatal(err)
	}

	if got := get(t, c, "karpenter"); got == nil || got.Spec.Description != "version two" || got.Spec.Sequence == nil || *got.Spec.Sequence != 0 {
		t.Errorf("karpenter = %+v, want this build's spec with sequence 0", got)
	}
	if got := get(t, c, "cluster-autoscaler"); got != nil && got.DeletionTimestamp.IsZero() {
		t.Errorf("cluster-autoscaler, no longer in the build, is kept: %+v (result %+v)", got.Spec, res)
	}
	if slices.Contains(res.Edited, "karpenter") || slices.Contains(res.Edited, "cluster-autoscaler") {
		t.Errorf("a CRD default counted as an edit: %+v", res)
	}
	if catalog.SpecHash(old) != catalog.SpecHash(stored("x").Spec) {
		t.Error("a spec and the same spec with sequence at its default hash differently")
	}
	realEdit := old
	realEdit.Sequence = ptrTo(int32(5))
	if catalog.SpecHash(old) == catalog.SpecHash(realEdit) {
		t.Error("a sequence other than the default no longer counts as an edit")
	}
}

func ptrTo[T any](v T) *T { return &v }

// A build before the provider field hashed a definition without the key;
// the API server fills in the CRD's default (aws) since. Such a
// definition is unedited: this build's spec replaces it. One edited since
// is still left alone.
func TestSyncTreatsTheProviderDefaultAsUnedited(t *testing.T) {
	old := spec("version one")
	// What a build without the provider field recorded: the spec's JSON
	// without the key.
	raw, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	legacy := strings.Replace(string(raw), `,"provider":""`, "", 1)
	if legacy == string(raw) {
		t.Fatalf("no provider key in %s", raw)
	}
	sum := sha256.Sum256([]byte(legacy))
	stored := func(name, description string) *cloudv1alpha1.ComponentDefinition {
		s := old
		s.Description = description
		s.Provider = cloudv1alpha1.CloudProviderAWS // what the API server returns now
		d := shipped(name, s)
		d.Annotations[catalog.ShippedHashAnnotation] = hex.EncodeToString(sum[:])
		return d
	}
	c := newClient(t, stored("kyverno", "version one"), stored("edited", "the platform team's"))
	next := spec("version two")
	next.Provider = cloudv1alpha1.CloudProviderAWS

	res, err := catalog.Sync(t.Context(), c, []*cloudv1alpha1.ComponentDefinition{definition("kyverno", next), definition("edited", next)})
	if err != nil {
		t.Fatal(err)
	}

	if got := get(t, c, "kyverno"); got == nil || got.Spec.Description != "version two" {
		t.Errorf("kyverno = %+v, want this build's spec (result %+v)", got, res)
	}
	if got := get(t, c, "edited"); got == nil || got.Spec.Description != "the platform team's" || !slices.Contains(res.Edited, "edited") {
		t.Errorf("an edited definition was not left alone: %+v (result %+v)", got, res)
	}
}

// management defaults to Tenant in the CRD: a tenant definition the build
// ships without it comes back from the API server with Tenant set. That is
// not an edit, and a definition not labelled shipped with that one
// difference is the build's (adopted).
func TestSyncTreatsTheManagementDefaultAsUnedited(t *testing.T) {
	v1, v2 := spec("version one"), spec("version two")
	stored := shipped("gpu-operator", v1)
	stored.Spec.Management = cloudv1alpha1.ComponentManagementTenant
	unlabelled := definition("kyverno-policies", v2)
	unlabelled.Spec.Management = cloudv1alpha1.ComponentManagementTenant
	c := newClient(t, stored, unlabelled)

	res, err := catalog.Sync(t.Context(), c, []*cloudv1alpha1.ComponentDefinition{definition("gpu-operator", v2), definition("kyverno-policies", v2)})
	if err != nil {
		t.Fatal(err)
	}

	if got := get(t, c, "gpu-operator"); got == nil || got.Spec.Description != "version two" {
		t.Errorf("gpu-operator = %+v, want this build's spec (result %+v)", got, res)
	}
	if !slices.Contains(res.Adopted, "kyverno-policies") {
		t.Errorf("kyverno-policies, equal but for the management default, was not adopted: %+v", res)
	}
	if catalog.SpecHash(v1) != catalog.SpecHash(stored.Spec) {
		t.Error("a spec and the same spec with management at its default hash differently")
	}
}

// spec.role and spec.sizing came after definitions were hashed. Both are
// omitempty with no CRD default, so a definition stored without them hashes
// as before (not edited), and this build's role and sizing replace an
// unedited Karpenter that predates them: the role, not sequence 0, puts it
// first now, and the API server's sequence default is still no edit.
func TestSyncTakesRoleAndSizingOverDefinitionsThatPredateThem(t *testing.T) {
	old := spec("version one")
	old.Sequence = ptrTo(int32(0))
	raw, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"role"`) || strings.Contains(string(raw), `"sizing"`) {
		t.Fatalf("a spec without a role or sizing marshals them: %s", raw)
	}
	c := newClient(t, shipped("karpenter", old))
	next := spec("version two")
	next.Role = cloudv1alpha1.ComponentRoleNodeProvisioner
	next.Sizing = &cloudv1alpha1.ComponentSizing{Deployments: []cloudv1alpha1.SizedDeployment{
		{Name: "karpenter", Replicas: "replicas", Requests: "controller.resources.requests"}}}

	res, err := catalog.Sync(t.Context(), c, []*cloudv1alpha1.ComponentDefinition{definition("karpenter", next)})
	if err != nil {
		t.Fatal(err)
	}
	got := get(t, c, "karpenter")
	if !slices.Contains(res.Updated, "karpenter") || got.Spec.Role != cloudv1alpha1.ComponentRoleNodeProvisioner || got.Spec.Sizing == nil {
		t.Fatalf("karpenter = %+v, want this build's role and sizing (result %+v)", got.Spec, res)
	}

	got.Spec.Sequence = ptrTo(cloudv1alpha1.DefaultComponentSequence) // what the API server returns now
	if err := c.Update(t.Context(), got); err != nil {
		t.Fatal(err)
	}
	again, err := catalog.Sync(t.Context(), c, []*cloudv1alpha1.ComponentDefinition{definition("karpenter", next)})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(again.Existing, "karpenter") {
		t.Errorf("the next sync of the same build = %+v, want karpenter as it is", again)
	}
}
