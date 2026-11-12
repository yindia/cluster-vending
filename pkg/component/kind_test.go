package component_test

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/component"
)

func definitionKind(platform, schema string) *cloudv1alpha1.ComponentDefinition {
	def := &cloudv1alpha1.ComponentDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "karpenter"},
		Spec: cloudv1alpha1.ComponentDefinitionSpec{
			Requires:   []string{"a"},
			UserValues: "replicas: 2\n",
			CloudResources: &cloudv1alpha1.ComponentCloudResources{IAMRole: &cloudv1alpha1.ComponentIAMRole{
				ServiceAccounts:   []cloudv1alpha1.ComponentServiceAccount{{Namespace: "kube-system", Name: "karpenter"}},
				ManagedPolicyARNs: []string{"arn:aws:iam::aws:policy/ReadOnlyAccess"},
			}},
		},
	}
	if platform != "" {
		def.Spec.PlatformValues = &runtime.RawExtension{Raw: []byte(platform)}
	}
	if schema != "" {
		def.Spec.ValuesSchema = &runtime.RawExtension{Raw: []byte(schema)}
	}
	return def
}

func TestDefinitionOf(t *testing.T) {
	def := definitionKind(`{"settings":{"clusterName":"${cluster.name}"}}`, `{"type":"object"}`)
	def.Status.Conditions = []metav1.Condition{{Type: component.ConditionValid, Status: metav1.ConditionTrue}}

	got, err := component.DefinitionOf(def)

	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "karpenter" || !got.Valid || got.UserValues != "replicas: 2\n" || got.Requires[0] != "a" {
		t.Errorf("got %+v", got)
	}
	if got.PlatformValues["settings"].(map[string]any)["clusterName"] != "${cluster.name}" {
		t.Errorf("platformValues = %v", got.PlatformValues)
	}
	if got.ValuesSchema["type"] != "object" {
		t.Errorf("valuesSchema = %v", got.ValuesSchema)
	}
	if got.CloudResources.IAMRole == nil || got.CloudResources.IAMRole.Access != component.IRSA {
		t.Errorf("cloudResources = %+v", got.CloudResources)
	}
	def.Spec.Requires[0] = "changed"
	if got.Requires[0] != "a" {
		t.Error("DefinitionOf shares the input's slices")
	}
}

func TestDefinitionOfWithoutValuesOrCondition(t *testing.T) {
	got, err := component.DefinitionOf(definitionKind("", "null"))

	if err != nil || got.Valid || got.PlatformValues != nil || got.ValuesSchema != nil {
		t.Errorf("got %+v, %v", got, err)
	}
}

func TestDefinitionOfRefusesNonObjects(t *testing.T) {
	for _, tc := range []struct{ platform, schema, want string }{
		{platform: `[1]`, want: "platformValues"},
		{schema: `"x"`, want: "valuesSchema"},
		{platform: `{`, want: "platformValues"},
	} {
		_, err := component.DefinitionOf(definitionKind(tc.platform, tc.schema))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%+v: err = %v", tc, err)
		}
	}
}

// A release pin's values go over the definition's platform values and are
// locked like them.
func TestWithPinValues(t *testing.T) {
	d := component.Definition{Name: "cluster-autoscaler", PlatformValues: component.Values{
		"image": map[string]any{"repository": "registry.k8s.io/autoscaling/cluster-autoscaler"}, "replicaCount": float64(2)}}
	pin := component.Values{"image": map[string]any{"tag": "v1.36.2"}}

	got := component.WithPinValues(d, pin)

	image := got.PlatformValues["image"].(map[string]any)
	if image["tag"] != "v1.36.2" || image["repository"] == nil || got.PlatformValues["replicaCount"] != float64(2) {
		t.Errorf("platform values = %v", got.PlatformValues)
	}
	if _, changed := d.PlatformValues["image"].(map[string]any)["tag"]; changed {
		t.Error("WithPinValues changed its input")
	}
	touched := component.LockedPathsTouched(component.Values{"image": map[string]any{"tag": "v9"}}, got.PlatformValues)
	if len(touched) != 1 || touched[0] != "image.tag" {
		t.Errorf("a tenant setting the pinned key: locked paths touched = %v", touched)
	}
	if same := component.WithPinValues(d, nil); len(same.PlatformValues) != 2 {
		t.Errorf("no pin values: %v", same.PlatformValues)
	}
}

func TestPinValues(t *testing.T) {
	for _, tc := range []struct{ raw, wantErr string }{
		{raw: `{"image":{"tag":"v1.37.0"}}`},
		{raw: `{"awsRegion":"${region}"}`},
		{raw: ``},
		{raw: `["a"]`, wantErr: "must be an object"},
		{raw: `{"x":"${cluster.zone}"}`, wantErr: "${cluster.zone}"},
	} {
		var ext *runtime.RawExtension
		if tc.raw != "" {
			ext = &runtime.RawExtension{Raw: []byte(tc.raw)}
		}
		_, err := component.PinValues(ext)
		if (tc.wantErr == "") != (err == nil) || err != nil && !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%q: err = %v, want %q", tc.raw, err, tc.wantErr)
		}
	}
}
