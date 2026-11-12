package component

import (
	"reflect"
	"strings"
	"testing"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/fault"
)

// clusterWithPool is the test cluster with a default node pool.
func clusterWithPool(labels map[string]string, taints []cloudv1alpha1.Taint) *cloudv1alpha1.Cluster {
	c := testCluster(true)
	c.Spec.DefaultNodePool = &cloudv1alpha1.NodePoolSpec{Labels: labels, Taints: taints}
	return c
}

func TestExpandSystemPlacement(t *testing.T) {
	doc := map[string]any{
		"nodeSelector": "${system.nodeSelector}",
		"controller":   map[string]any{"tolerations": "${system.tolerations}", "name": "${cluster.name}"},
		"list":         []any{"${system.tolerations}"},
	}
	tests := []struct {
		name            string
		cluster         *cloudv1alpha1.Cluster
		wantSelector    map[string]any
		wantTolerations []any
	}{
		{name: "no default pool", cluster: testCluster(true), wantSelector: map[string]any{}, wantTolerations: []any{}},
		{name: "pool without labels or taints", cluster: clusterWithPool(nil, nil),
			wantSelector: map[string]any{}, wantTolerations: []any{}},
		{name: "two labels and two taints",
			cluster: clusterWithPool(map[string]string{"role": "system", "team": "platform"}, []cloudv1alpha1.Taint{
				{Key: "dedicated", Value: "system", Effect: "NoSchedule"},
				{Key: "critical", Effect: "NoExecute"}}),
			wantSelector: map[string]any{"role": "system", "team": "platform"},
			wantTolerations: []any{
				map[string]any{"key": "dedicated", "operator": "Equal", "value": "system", "effect": "NoSchedule"},
				map[string]any{"key": "critical", "operator": "Equal", "value": "", "effect": "NoExecute"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			orig := deepCopyAny(doc)

			got, err := Expand(doc, VarsFor(tt.cluster, testNetwork(), testAccount(), IAMOutputs{}))

			if err != nil {
				t.Fatalf("Expand: %v", err)
			}
			want := map[string]any{
				"nodeSelector": tt.wantSelector,
				"controller":   map[string]any{"tolerations": tt.wantTolerations, "name": "prod"},
				"list":         []any{tt.wantTolerations},
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("got  %v\nwant %v", got, want)
			}
			if !reflect.DeepEqual(doc, orig) {
				t.Error("Expand changed its input")
			}
		})
	}
}

func TestSystemPlaceholdersAreWholeValuesOnly(t *testing.T) {
	tests := []struct {
		name     string
		doc      any
		contains []string
	}{
		{name: "embedded in a string", doc: map[string]any{"a": map[string]any{"b": "x-${system.nodeSelector}"}},
			contains: []string{"a.b", "${system.nodeSelector}", "whole value"}},
		{name: "two in one string", doc: map[string]any{"t": "${system.tolerations}${system.tolerations}"},
			contains: []string{"t", "whole value"}},
		{name: "with another placeholder", doc: map[string]any{"t": "${system.tolerations}${region}"},
			contains: []string{"t", "whole value"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, err := range []error{Validate(tt.doc), expandErr(tt.doc)} {
				if !fault.Is(err, fault.Invalid) {
					t.Fatalf("want an Invalid fault, got %v", err)
				}
				for _, s := range tt.contains {
					if !strings.Contains(err.Error(), s) {
						t.Errorf("%q does not name %q", err, s)
					}
				}
			}
		})
	}
	if err := Validate(map[string]any{"a": "${system.nodeSelector}", "b": "$${system.nodeSelector} text"}); err != nil {
		t.Errorf("whole value and an escaped literal = %v", err)
	}
}

func expandErr(doc any) error {
	_, err := Expand(doc, VarsFor(testCluster(true), testNetwork(), testAccount(), IAMOutputs{}))
	return err
}

// The IAM policy and managed policy ARNs are strings: no structured values.
func TestSystemPlaceholdersAreNotForThePolicy(t *testing.T) {
	resources := role(func(r *IAMRole) {
		r.Policy = strings.Replace(testPolicy, "${region}", "${system.nodeSelector}", 1)
	})

	err := ValidateCloudResources(resources)

	if !fault.Is(err, fault.Invalid) || !strings.Contains(err.Error(), "only for platform values") {
		t.Errorf("policy with ${system.nodeSelector} = %v", err)
	}
	if _, err := ExpandString("${system.tolerations}", Vars{VarSystemTolerations: "[]"}); !fault.Is(err, fault.Invalid) {
		t.Errorf("ExpandString of a whole-value placeholder = %v", err)
	}
}
