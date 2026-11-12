package component_test

import (
	"reflect"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/yindia/cluster-vending/pkg/component"
)

// doc decodes YAML as the API and the CLI do.
func doc(t *testing.T, s string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := yaml.Unmarshal([]byte(s), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

const valuesFile = `
# Replicas of the controller.
replicas: 2
logLevel: info
settings:
  batchMaxDuration: 10s
  featureGates:
    spotToSpotConsolidation: false
defaultNodePool:
  requirements:
  - {key: karpenter.sh/capacity-type, operator: In, values: [on-demand]}
`

func TestPruneUnchanged(t *testing.T) {
	tests := []struct {
		name string
		sent string
		want string // YAML; empty: nothing stored
	}{
		{name: "the file as it is", sent: valuesFile},
		{name: "key equal", sent: "replicas: 2\nlogLevel: info"},
		{name: "one top-level edit", sent: "replicas: 3\nlogLevel: info", want: "replicas: 3"},
		{name: "nested map: only the edited leaf",
			sent: "settings:\n  batchMaxDuration: 15s\n  featureGates:\n    spotToSpotConsolidation: false",
			want: "settings:\n  batchMaxDuration: 15s"},
		{name: "nested map: deeper edit",
			sent: "settings:\n  batchMaxDuration: 10s\n  featureGates:\n    spotToSpotConsolidation: true",
			want: "settings:\n  featureGates:\n    spotToSpotConsolidation: true"},
		{name: "list equal is dropped whole",
			sent: "defaultNodePool:\n  requirements:\n  - {key: karpenter.sh/capacity-type, operator: In, values: [on-demand]}"},
		{name: "list changed is kept whole",
			sent: "defaultNodePool:\n  requirements:\n  - {key: karpenter.sh/capacity-type, operator: In, values: [spot, on-demand]}",
			want: "defaultNodePool:\n  requirements:\n  - {key: karpenter.sh/capacity-type, operator: In, values: [spot, on-demand]}"},
		{name: "key removed from the file is not stored", sent: "replicas: 2"},
		{name: "key absent from the file is kept", sent: "replicas: 2\ncontroller: {resources: {limits: {cpu: \"1\"}}}",
			want: "controller: {resources: {limits: {cpu: \"1\"}}}"},
		{name: "explicit null removes the file's value", sent: "logLevel: null", want: "logLevel: null"},
		{name: "map where the file has a scalar", sent: "logLevel: {level: debug}", want: "logLevel: {level: debug}"},
		{name: "scalar where the file has a map", sent: "settings: none", want: "settings: none"},
		{name: "equal number, other spelling", sent: "replicas: 2.0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file, sent := doc(t, valuesFile), doc(t, tt.sent)
			fileBefore, sentBefore := doc(t, valuesFile), doc(t, tt.sent)
			got := component.PruneUnchanged(sent, file)
			var want map[string]any
			if tt.want != "" {
				want = doc(t, tt.want)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("PruneUnchanged = %v, want %v", got, want)
			}
			if !reflect.DeepEqual(file, fileBefore) || !reflect.DeepEqual(sent, sentBefore) {
				t.Error("an input was changed")
			}
		})
	}
}

func TestPruneUnchangedWithoutAFile(t *testing.T) {
	sent := map[string]any{"replicas": 3.0}
	if got := component.PruneUnchanged(sent, nil); !reflect.DeepEqual(got, sent) {
		t.Errorf("no file: %v, want everything kept", got)
	}
	if got := component.PruneUnchanged(nil, map[string]any{"replicas": 2.0}); got != nil {
		t.Errorf("nothing sent: %v, want nil", got)
	}
}
