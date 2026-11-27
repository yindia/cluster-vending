package aws

import (
	"encoding/json"
	"slices"
	"testing"
)

// managedFieldsOfAListMap is how the API server records a kubectl edit of
// a runtime config's provider container: the containers list keyed by
// name (the CRD's list-map), its args (an atomic list) and one limit.
const managedFieldsOfAListMap = `{"f:spec":{"f:deploymentTemplate":{"f:spec":{"f:template":{"f:spec":{"f:containers":{
	"k:{\"name\":\"package-runtime\"}":{".":{},"f:args":{},"f:resources":{"f:limits":{"f:memory":{}}}}}}}}}}}`

func TestOwnedFieldsOfAListMap(t *testing.T) {
	var set map[string]any
	if err := json.Unmarshal([]byte(managedFieldsOfAListMap), &set); err != nil {
		t.Fatal(err)
	}
	var owned []string
	for _, f := range runtimeConfigFields() {
		if ownedByAny([]map[string]any{set}, f.keys, f.whole) {
			owned = append(owned, f.name)
		}
	}
	if want := []string{"args", "resources.limits.memory"}; !slices.Equal(owned, want) {
		t.Errorf("owned %v, want %v", owned, want)
	}
}

// With an atomic containers list (no list keys), whoever wrote it owns it
// whole: every field in it, and the list itself, is theirs.
func TestOwnedFieldsOfAnAtomicList(t *testing.T) {
	set := map[string]any{"f:spec": map[string]any{"f:deploymentTemplate": map[string]any{"f:spec": map[string]any{
		"f:template": map[string]any{"f:spec": map[string]any{"f:containers": map[string]any{}}}}}}}
	var owned []string
	for _, f := range runtimeConfigFields() {
		if ownedByAny([]map[string]any{set}, f.keys, f.whole) {
			owned = append(owned, f.name)
		}
	}
	want := []string{"containers", "args", "resources.requests.cpu", "resources.requests.memory", "resources.limits.cpu", "resources.limits.memory"}
	if !slices.Equal(owned, want) {
		t.Errorf("owned %v, want %v", owned, want)
	}
}

func TestDroppedFieldsLeaveTheRest(t *testing.T) {
	body := map[string]any{"spec": map[string]any{"deploymentTemplate": map[string]any{"spec": map[string]any{
		"replicas": int64(1),
		"template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"name": runtimeContainer,
			"args": []any{"--poll=1m"}, "resources": map[string]any{"limits": map[string]any{"memory": "1Gi", "cpu": "1"}}}}}}}}}}
	for _, f := range runtimeConfigFields() {
		if f.name == "args" || f.name == "resources.limits.memory" {
			f.drop(body)
		}
	}
	ctr := body["spec"].(map[string]any)["deploymentTemplate"].(map[string]any)["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
	limits := ctr["resources"].(map[string]any)["limits"].(map[string]any)
	if _, set := ctr["args"]; set || limits["memory"] != nil || limits["cpu"] != "1" {
		t.Errorf("container after drops: %v", ctr)
	}
}
