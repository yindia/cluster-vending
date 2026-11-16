package crd_test

import (
	"encoding/json"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestClusterNaaSAcceptsOnlyBooleans(t *testing.T) {
	v := newValidator(t, "Cluster")
	sample := sampleDocuments(t, "cloud_v1alpha1_cluster.yaml")[0]
	for _, test := range []struct {
		name      string
		value     any
		wantError bool
	}{
		{"enabled", true, false}, {"disabled", false, false}, {"string", "true", true}, {"number", float64(1), true},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := map[string]any{}
			if err := yaml.Unmarshal(sample, &body); err != nil {
				t.Fatal(err)
			}
			body["spec"].(map[string]any)["naas"] = test.value
			raw, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			invalid := false
			for _, failure := range v.validate(t, raw) {
				if strings.HasPrefix(failure.Field, "spec.naas") {
					invalid = true
				}
			}
			if invalid != test.wantError {
				t.Fatalf("naas=%v invalid=%v want=%v", test.value, invalid, test.wantError)
			}
		})
	}
}
