package controller

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/component"
	"github.com/yindia/cluster-vending/pkg/naas"
)

func TestShippedCapsuleBootstrapIsLockedIntoEachMinor(t *testing.T) {
	content := defaultContent()
	bootstrap, err := naas.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, minor := range content.Kubernetes {
		found := false
		for _, pin := range minor.Components {
			if pin.Name != "capsule" {
				continue
			}
			found = true
			if pin.Version != "0.15.2" {
				t.Fatal("bootstrap must be requalified against another chart version")
			}
			values, err := component.PinValues(pin.Values)
			if err != nil {
				t.Fatal(err)
			}
			extra, _ := values["extraManifests"].([]any)
			if len(extra) != len(bootstrap.Objects())+1 {
				t.Fatal("shipped pin lost the PDB or bootstrap")
			}
			if !strings.Contains(string(pin.Values.Raw), bootstrap.Revision()) {
				t.Fatal("shipped pin has no bootstrap revision")
			}
		}
		if !found {
			t.Fatalf("minor %s has no Capsule bootstrap pin", minor.Minor)
		}
	}
	// A template-only edit, with the chart version unchanged, ships a different
	// immutable release. Existing published Releases are never recomposed.
	changed := *content.DeepCopy()
	for i := range changed.Kubernetes[0].Components {
		pin := &changed.Kubernetes[0].Components[i]
		if pin.Name != "capsule" {
			continue
		}
		values, err := component.PinValues(pin.Values)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range values["extraManifests"].([]any) {
			object := item.(map[string]any)
			if object["kind"] != "GlobalTenantResource" {
				continue
			}
			resource := object["spec"].(map[string]any)["resources"].([]any)[0].(map[string]any)
			generator := resource["generators"].([]any)[0].(map[string]any)
			generator["template"] = generator["template"].(string) + "\n# new template content\n"
			break
		}
		pin.Values.Raw, err = json.Marshal(values)
		if err != nil {
			t.Fatal(err)
		}
	}
	if ShippedReleaseName(changed) == ShippedReleaseName(content) {
		t.Fatal("values-only template update did not change shipped release identity")
	}
	if ShippedReleaseName(defaultContent()) != ShippedReleaseName(content) {
		t.Fatal("editing a pin mutated canonical shipped content")
	}
}

func TestCapsuleBootstrapDoesNotRewritePublishedRelease(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := cloudv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	old := DefaultRelease()
	old.Name = "shipped-before-bootstrap"
	old.Spec.Content.Kubernetes[0].Components = []cloudv1alpha1.ComponentPin{{Name: "capsule", Version: "0.15.2"}}
	before, _ := json.Marshal(old.Spec)
	alpha := &cloudv1alpha1.ReleaseChannel{ObjectMeta: metav1.ObjectMeta{Name: cloudv1alpha1.ChannelAlpha},
		Spec: cloudv1alpha1.ReleaseChannelSpec{Promotion: cloudv1alpha1.PromotionPinned, Release: old.Name}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(old, alpha).Build()
	for range 2 {
		if err := CreateReleaseDefaults(t.Context(), c); err != nil {
			t.Fatal(err)
		}
	}
	preserved := &cloudv1alpha1.Release{}
	if err := c.Get(t.Context(), client.ObjectKey{Name: old.Name}, preserved); err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(preserved.Spec)
	if !bytes.Equal(before, after) {
		t.Fatal("new bootstrap mutated an existing published release")
	}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(alpha), alpha); err != nil {
		t.Fatal(err)
	}
	if alpha.Spec.Release != DefaultReleaseName {
		t.Fatal("new bootstrap bypassed normal shipped-release channel handling")
	}
}
