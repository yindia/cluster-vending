package component_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/yindia/cluster-vending/pkg/component"
)

// object is a values literal written as JSON.
func object(t *testing.T, raw string) component.Values {
	t.Helper()
	var v component.Values
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	return v
}

const karpenterSchema = `{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "replicas": {"type": "integer", "minimum": 1, "maximum": 3},
    "logLevel": {"type": "string", "enum": ["error", "info", "debug"]},
    "settings": {"type": "object", "properties": {"batchMaxDuration": {"type": "string"}}}
  }
}`

const karpenterPlatform = `{"controller": {"resources": {"requests": {"cpu": "250m"}}}, "settings": {"isolatedVPC": false}}`

func TestCheckDefinition(t *testing.T) {
	definition := func(userValues string) component.Definition {
		return component.Definition{
			Name: "karpenter", UserValues: userValues,
			PlatformValues: object(t, karpenterPlatform), ValuesSchema: object(t, karpenterSchema),
		}
	}
	tests := []struct {
		name   string
		d      component.Definition
		wantIn []string // substrings of the problems; none: valid
	}{
		{name: "valid, with comments", d: definition("# Controller replicas.\nreplicas: 2\nlogLevel: info # error, info or debug\nsettings:\n  batchMaxDuration: 10s\n")},
		{name: "no file", d: definition("")},
		{name: "only comments", d: definition("# nothing to change yet\n")},
		{name: "no platform values", d: component.Definition{UserValues: "replicas: 2\n", ValuesSchema: object(t, karpenterSchema)}},
		{name: "nothing at all", d: component.Definition{}},

		{name: "a long install timeout", d: component.Definition{HasInstall: true, InstallTimeout: 15 * time.Minute}},
		{name: "no install timeout", d: component.Definition{HasInstall: true}},
		{name: "an install timeout under a minute", d: component.Definition{HasInstall: true, InstallTimeout: 30 * time.Second},
			wantIn: []string{"install.timeout 30s must be between 1m and 60m"}},
		{name: "an install timeout past an hour", d: component.Definition{HasInstall: true, InstallTimeout: 61 * time.Minute},
			wantIn: []string{"install.timeout 1h1m0s must be between 1m and 60m"}},
		{name: "userValues not YAML", d: definition("replicas: [2\n"), wantIn: []string{"userValues: not YAML"}},
		{name: "userValues a list", d: definition("- replicas\n"), wantIn: []string{"must be an object, got array"}},
		{name: "userValues a scalar", d: definition("replicas\n"), wantIn: []string{"must be an object, got string"}},

		{name: "same leaf as platformValues", d: definition("settings:\n  isolatedVPC: true\n"),
			wantIn: []string{"userValues sets settings.isolatedVPC, which platformValues locks"}},
		{name: "a parent of a platformValues leaf", d: definition("controller: null\n"),
			wantIn: []string{"userValues sets controller,"}},
		{name: "below a platformValues leaf", d: component.Definition{
			UserValues: "settings:\n  batchMaxDuration: 10s\n", PlatformValues: object(t, `{"settings": "locked"}`),
			ValuesSchema: object(t, karpenterSchema)},
			wantIn: []string{"userValues sets settings.batchMaxDuration"}},
		{name: "every overlap named", d: definition("controller: {resources: {}}\nsettings: {isolatedVPC: true}\n"),
			wantIn: []string{"controller.resources", "settings.isolatedVPC"}},

		{name: "fails valuesSchema: unknown key", d: definition("replicaCount: 2\n"), wantIn: []string{"userValues: replicaCount"}},
		{name: "fails valuesSchema: out of range", d: definition("replicas: 9\n"), wantIn: []string{"userValues: replicas: must be at most 3"}},
		{name: "fails valuesSchema: wrong type", d: definition("logLevel: 3\n"), wantIn: []string{"userValues: logLevel"}},
		{name: "no valuesSchema, values set", d: component.Definition{UserValues: "replicas: 2\n"},
			wantIn: []string{"no valuesSchema"}},
		{name: "valuesSchema with an unsupported keyword", d: component.Definition{UserValues: "replicas: 2\n",
			ValuesSchema: object(t, `{"type": "object", "oneOf": []}`)}, wantIn: []string{`valuesSchema: schema: keyword "oneOf" is not supported`}},

		{name: "platformValues with placeholders", d: component.Definition{
			PlatformValues: object(t, `{"settings": {"clusterName": "${cluster.name}"}, "sa": "${iam.roleArn}"}`)}},
		{name: "platformValues with an unknown placeholder", d: component.Definition{
			PlatformValues: object(t, `{"settings": {"queue": "${karpenter.queueName}"}}`)},
			wantIn: []string{"platformValues: settings.queue: unknown placeholder ${karpenter.queueName}"}},
		{name: "an IAM role with a bad policy", d: component.Definition{CloudResources: component.CloudResources{
			IAMRole: &component.IAMRole{ServiceAccounts: []component.ServiceAccount{{Namespace: "ns", Name: "sa"}}, Policy: "{}"}}},
			wantIn: []string{"policy has no Version"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := tt.d
			d.Provider = "aws" // TestCheckDefinitionNeedsAProvider covers it
			problems := component.CheckDefinition(d)
			if len(tt.wantIn) == 0 && len(problems) > 0 {
				t.Fatalf("problems %v, want none", problems)
			}
			if len(tt.wantIn) > 0 && len(problems) == 0 {
				t.Fatal("no problem found")
			}
			all := strings.Join(problems, "\n")
			for _, want := range tt.wantIn {
				if !strings.Contains(all, want) {
					t.Errorf("problems %q, want one containing %q", all, want)
				}
			}
		})
	}
}

func TestParseUserValues(t *testing.T) {
	v, err := component.ParseUserValues("# a comment\nreplicas: 2 # inline\nsettings: {a: true}\n")
	if err != nil || v["replicas"] != 2.0 || v["settings"].(map[string]any)["a"] != true {
		t.Errorf("ParseUserValues = %v, %v", v, err)
	}
	if v, err := component.ParseUserValues("# only comments\n"); v != nil || err != nil {
		t.Errorf("comments only = %v, %v; want no values", v, err)
	}
}
