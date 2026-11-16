// Package planning compares rendered Crossplane resources without depending on a cloud provider.
package planning

import (
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/yindia/cluster-vending/pkg/crossplane/managed"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// Policy is supplied by the provider adapter. Unknown changes fail closed.
type Policy struct {
	Classify func(schema.GroupVersionKind, string, bool) cloudv1.PlannedAction
	APIField func(schema.GroupVersionKind, string) string
}

func (p Policy) action(kind schema.GroupVersionKind, path string, unset bool) cloudv1.PlannedAction {
	if p.Classify == nil {
		return cloudv1.PlannedAction_PLANNED_ACTION_REJECTED_IMMUTABLE
	}
	action := p.Classify(kind, path, unset)
	if action == cloudv1.PlannedAction_PLANNED_ACTION_UNSPECIFIED {
		return cloudv1.PlannedAction_PLANNED_ACTION_REJECTED_IMMUTABLE
	}
	return action
}
func (p Policy) field(kind schema.GroupVersionKind, path string) string {
	if p.APIField != nil {
		return p.APIField(kind, path)
	}
	return "forProvider." + path
}

// FieldChanges compares a live object with its dry-run apply, field by
// field of spec.forProvider and spec.initProvider, and with the cloud's
// state (status.atProvider): a field the update leaves alone but the cloud
// has differently was changed outside the platform, and is set back.
func FieldChanges(live, dry *unstructured.Unstructured, policy Policy) []*cloudv1.PlannedChange {
	var out []*cloudv1.PlannedChange
	change := func(path string, action cloudv1.PlannedAction, detail, diff string) {
		out = append(out, &cloudv1.PlannedChange{ApiVersion: dry.GetAPIVersion(), Kind: dry.GetKind(), Name: dry.GetName(),
			ExternalName: live.GetAnnotations()[managed.ExternalNameAnnotation], SpecPath: policy.field(dry.GroupVersionKind(), path),
			Action: action, Detail: detail, UnifiedDiff: diff})
	}
	header := fmt.Sprintf("--- %s/%s (live)\n+++ %s/%s (planned)\n", dry.GetKind(), dry.GetName(), dry.GetKind(), dry.GetName())
	was := fields(live, "spec", "forProvider")
	will := fields(dry, "spec", "forProvider")
	cloud := fields(live, "status", "atProvider")
	for _, path := range sortedKeys(was, will) {
		before, after := FieldString(was[path]), FieldString(will[path])
		if before != after {
			change(path, policy.action(dry.GroupVersionKind(), path, was[path] == nil), fmt.Sprintf("forProvider.%s: %s -> %s", path, before, after),
				fmt.Sprintf("%s-forProvider.%s: %s\n+forProvider.%s: %s\n", header, path, before, path, after))
			continue
		}
		if actual, ok := cloud[path]; ok && isScalar(actual) && isScalar(will[path]) && FieldString(actual) != after {
			change(path, policy.action(dry.GroupVersionKind(), path, false),
				fmt.Sprintf("forProvider.%s: the cloud has %s (changed outside the platform), set back to %s", path,
					FieldString(actual), after), "")
		}
	}
	wasInit, willInit := fields(live, "spec", "initProvider"), fields(dry, "spec", "initProvider")
	for _, path := range sortedKeys(wasInit, willInit) {
		before, after := FieldString(wasInit[path]), FieldString(willInit[path])
		if before != after {
			change(path, cloudv1.PlannedAction_PLANNED_ACTION_UPDATE_IN_PLACE,
				fmt.Sprintf("initProvider.%s: %s -> %s (used at creation only: no effect on the existing resource)", path, before, after), "")
		}
	}
	return out
}

func fields(u *unstructured.Unstructured, path ...string) map[string]any {
	m, _, _ := unstructured.NestedMap(u.Object, path...)
	return FlattenFields(m)
}

func sortedKeys(maps ...map[string]any) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range maps {
		for k := range m {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	sort.Strings(out)
	return out
}

func isScalar(v any) bool {
	switch v.(type) {
	case string, bool, int64, float64, int:
		return true
	default:
		return false
	}
}

func FlattenFields(m map[string]any) map[string]any {
	out := map[string]any{}
	var walk func(string, map[string]any)
	walk = func(prefix string, fields map[string]any) {
		for key, value := range fields {
			if strings.HasSuffix(key, "Ref") || strings.HasSuffix(key, "Refs") || strings.HasSuffix(key, "Selector") {
				continue
			}
			if (prefix == "tags" || strings.HasSuffix(prefix, ".tags")) && strings.HasPrefix(key, "crossplane-") {
				continue
			}
			path := key
			if prefix != "" {
				path = prefix + "." + key
			}
			if child, ok := value.(map[string]any); ok {
				walk(path, child)
			} else {
				out[path] = value
			}
		}
	}
	walk("", m)
	return out
}
func FieldString(value any) string {
	switch typed := value.(type) {
	case nil:
		return "<unset>"
	case string:
		return typed
	case []any:
		values := make([]string, 0, len(typed))
		for _, item := range typed {
			values = append(values, FieldString(item))
		}
		sort.Strings(values)
		return "[" + strings.Join(values, ", ") + "]"
	default:
		return fmt.Sprint(value)
	}
}
