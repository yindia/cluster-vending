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

// importedClusterWithPool is a cluster that was imported: its default node
// pool is what it had, not the platform's.
func importedClusterWithPool(labels map[string]string, taints []cloudv1alpha1.Taint) *cloudv1alpha1.Cluster {
	c := clusterWithPool(labels, taints)
	c.Spec.ImportFrom = &cloudv1alpha1.ClusterImport{}
	return c
}

var (
	systemSelector = map[string]any{"node.cluster-vending.io/pool": "system"}
	systemTaints   = []any{map[string]any{"key": "node.cluster-vending.io/pool", "operator": "Equal", "value": "system", "effect": "NoSchedule"}}
	poolSelector   = map[string]any{"node.cluster-vending.io/pool": "platform-pool"}
	poolTaints     = []any{map[string]any{"key": "node.cluster-vending.io/pool", "operator": "Equal", "value": "platform-pool", "effect": "NoSchedule"}}
)

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
		{name: "no default pool: the platform's", cluster: testCluster(true), wantSelector: systemSelector, wantTolerations: systemTaints},
		{name: "a stored pool's old labels and taints do not count",
			cluster:      clusterWithPool(map[string]string{"role": "old"}, []cloudv1alpha1.Taint{{Key: "old", Effect: "NoSchedule"}}),
			wantSelector: systemSelector, wantTolerations: systemTaints},
		{name: "imported, pool without labels or taints", cluster: importedClusterWithPool(nil, nil),
			wantSelector: map[string]any{}, wantTolerations: []any{}},
		{name: "imported, two labels and two taints",
			cluster: importedClusterWithPool(map[string]string{"role": "system", "team": "platform"}, []cloudv1alpha1.Taint{
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

// ${platform.*} is Karpenter's platform pool (platform-pool) on a platform
// cluster: every component but the phase-1 one (Karpenter) runs there. An imported cluster's components follow
// its default pool, as before.
func TestExpandPlatformPlacement(t *testing.T) {
	doc := map[string]any{"nodeSelector": "${platform.nodeSelector}", "tolerations": "${platform.tolerations}"}
	tests := []struct {
		name         string
		cluster      *cloudv1alpha1.Cluster
		wantSelector map[string]any
		wantTol      []any
	}{
		{name: "a platform cluster: the platform pool", cluster: testCluster(true), wantSelector: poolSelector, wantTol: poolTaints},
		{name: "imported: its default pool", cluster: importedClusterWithPool(map[string]string{"role": "system"}, nil),
			wantSelector: map[string]any{"role": "system"}, wantTol: []any{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Expand(doc, VarsFor(tt.cluster, testNetwork(), testAccount(), IAMOutputs{}))

			if err != nil {
				t.Fatal(err)
			}
			if want := map[string]any{"nodeSelector": tt.wantSelector, "tolerations": tt.wantTol}; !reflect.DeepEqual(got, want) {
				t.Errorf("got %v want %v", got, want)
			}
		})
	}
	for _, name := range []string{"platform.nodeSelector", "platform.tolerations"} {
		if err := Validate(map[string]any{"a": "x-${" + name + "}"}); !fault.Is(err, fault.Invalid) || !strings.Contains(err.Error(), "whole value") {
			t.Errorf("${%s} inside a string = %v", name, err)
		}
		resources := role(func(r *IAMRole) { r.Policy = strings.Replace(testPolicy, "${region}", "${"+name+"}", 1) })
		if err := ValidateCloudResources(resources); !fault.Is(err, fault.Invalid) || !strings.Contains(err.Error(), "only for platform values") {
			t.Errorf("policy with ${%s} = %v", name, err)
		}
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
