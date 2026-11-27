package aws

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/kube"
)

// tunedField is one field of a provider object the platform sets: its
// place in managedFields (FieldsV1 keys) and how to leave it out of an
// apply body.
type tunedField struct {
	name string
	keys []string
	drop func(obj map[string]any)
	// whole: the field counts as another manager's only when it owns it
	// as one value (an atomic list), not when it owns a part of it.
	whole bool
}

// applyResult is what applyKeeping did.
type applyResult struct {
	created, changed bool
	// kept are the fields left as another manager set them.
	kept []string
}

// applyKeeping server-side applies body (an apply body: apiVersion, kind,
// name and the fields to own) as kube.FieldOwner, forcing
// ownership, without the fields of fields that another manager owns in the
// live object: a field an operator set (kubectl edit, their own apply) is
// kept as they set it, every other one is brought to this build's value,
// and fields earlier builds wrote (kube.FieldOwner, with Create) are taken
// over. A missing object is created whole.
func applyKeeping(ctx context.Context, c client.Client, body *unstructured.Unstructured, fields []tunedField) (applyResult, error) {
	body = body.DeepCopy()
	have := &unstructured.Unstructured{}
	have.SetGroupVersionKind(body.GroupVersionKind())
	err := c.Get(ctx, client.ObjectKeyFromObject(body), have)
	switch {
	case apierrors.IsNotFound(err):
		have = nil
	case err != nil:
		return applyResult{}, fmt.Errorf("read %s %s: %w", body.GetKind(), body.GetName(), err)
	}
	var out applyResult
	if have != nil {
		others := othersFields(have.GetManagedFields())
		for _, f := range fields {
			if ownedByAny(others, f.keys, f.whole) {
				f.drop(body.Object)
				out.kept = append(out.kept, f.name)
			}
		}
	}
	if err := c.Apply(ctx, client.ApplyConfigurationFromUnstructured(body), client.FieldOwner(kube.FieldOwner), client.ForceOwnership); err != nil {
		return applyResult{}, fmt.Errorf("apply %s %s: %w", body.GetKind(), body.GetName(), err)
	}
	if have == nil {
		out.created = true
		return out, nil
	}
	after := &unstructured.Unstructured{}
	after.SetGroupVersionKind(body.GroupVersionKind())
	if err := c.Get(ctx, client.ObjectKeyFromObject(body), after); err != nil {
		return applyResult{}, fmt.Errorf("read %s %s: %w", body.GetKind(), body.GetName(), err)
	}
	out.changed = !equality.Semantic.DeepEqual(have.Object["spec"], after.Object["spec"]) ||
		!equality.Semantic.DeepEqual(have.GetLabels(), after.GetLabels()) ||
		!equality.Semantic.DeepEqual(have.GetAnnotations(), after.GetAnnotations())
	return out, nil
}

// othersFields are the FieldsV1 of every manager but the platform's.
func othersFields(entries []metav1.ManagedFieldsEntry) []map[string]any {
	var out []map[string]any
	for _, e := range entries {
		if e.Manager == kube.FieldOwner || e.FieldsV1 == nil {
			continue
		}
		fields := map[string]any{}
		if json.Unmarshal(e.FieldsV1.GetRawBytes(), &fields) == nil {
			out = append(out, fields)
		}
	}
	return out
}

// ownedByAny: one of sets owns the field at keys, or an atomic object or
// list above it (a leaf on the way). With whole, only a leaf at keys
// itself counts.
func ownedByAny(sets []map[string]any, keys []string, whole bool) bool {
	for _, set := range sets {
		if owns(set, keys, whole) {
			return true
		}
	}
	return false
}

func owns(node map[string]any, keys []string, whole bool) bool {
	for i, key := range keys {
		child, ok := node[key].(map[string]any)
		if !ok {
			return false
		}
		if len(child) == 0 {
			// A leaf: this field, or an atomic one holding it.
			return !whole || i == len(keys)-1
		}
		node = child
	}
	return !whole
}

// stripNullTimestamps removes the creationTimestamp: null the Go types
// write into every embedded ObjectMeta (a pod template's): applied, the
// platform would own a field it never sets.
func stripNullTimestamps(obj map[string]any) {
	for key, v := range obj {
		switch child := v.(type) {
		case map[string]any:
			if key == "metadata" {
				if ts, set := child["creationTimestamp"]; set && ts == nil {
					delete(child, "creationTimestamp")
				}
			}
			stripNullTimestamps(child)
		case []any:
			for _, item := range child {
				if m, ok := item.(map[string]any); ok {
					stripNullTimestamps(m)
				}
			}
		}
	}
}

// Where the tuned fields of a DeploymentRuntimeConfig are.
var (
	deploymentKeys = []string{"f:spec", "f:deploymentTemplate", "f:spec"}
	containerKeys  = append(append([]string{}, deploymentKeys...), "f:template", "f:spec", "f:containers", `k:{"name":"`+runtimeContainer+`"}`)
)

// runtimeConfigFields are the fields of a runtime config the platform
// tunes: replicas, the provider container's arguments (poll, reconcile
// rate, leader election) and resources, and the IRSA annotation.
func runtimeConfigFields() []tunedField {
	out := make([]tunedField, 0, 8)
	out = append(out, []tunedField{
		// An atomic containers list (how a schema without list keys keeps
		// it) someone else wrote is theirs as a whole.
		{name: "containers", keys: containerKeys[:len(containerKeys)-1], whole: true,
			drop: func(o map[string]any) {
				unstructured.RemoveNestedField(o, "spec", "deploymentTemplate", "spec", "template", "spec", "containers")
			}},
		{name: "replicas", keys: append(append([]string{}, deploymentKeys...), "f:replicas"),
			drop: func(o map[string]any) {
				unstructured.RemoveNestedField(o, "spec", "deploymentTemplate", "spec", "replicas")
			}},
		{name: "args", keys: append(append([]string{}, containerKeys...), "f:args"),
			drop: inContainer(func(ctr map[string]any) { delete(ctr, "args") })},
		{name: "irsa role", keys: []string{"f:spec", "f:serviceAccountTemplate", "f:metadata", "f:annotations", "f:" + irsaRoleAnnotation},
			drop: func(o map[string]any) {
				unstructured.RemoveNestedField(o, "spec", "serviceAccountTemplate", "metadata", "annotations", irsaRoleAnnotation)
			}},
	}...)
	for _, kind := range []string{"requests", "limits"} {
		for _, res := range []string{"cpu", "memory"} {
			out = append(out, tunedField{name: "resources." + kind + "." + res,
				keys: append(append([]string{}, containerKeys...), "f:resources", "f:"+kind, "f:"+res),
				drop: inContainer(func(ctr map[string]any) { unstructured.RemoveNestedField(ctr, "resources", kind, res) })})
		}
	}
	return out
}

// providerRefKeys are where a Provider's runtimeConfigRef is in
// managedFields.
var providerRefKeys = []string{"f:spec", "f:runtimeConfigRef"}

// inContainer runs edit on the provider container of a runtime config body.
func inContainer(edit func(map[string]any)) func(map[string]any) {
	return func(o map[string]any) {
		containers, _, _ := unstructured.NestedSlice(o, "spec", "deploymentTemplate", "spec", "template", "spec", "containers")
		for _, item := range containers {
			if ctr, ok := item.(map[string]any); ok && ctr["name"] == runtimeContainer {
				edit(ctr)
			}
		}
		_ = unstructured.SetNestedSlice(o, containers, "spec", "deploymentTemplate", "spec", "template", "spec", "containers")
	}
}

// keptName is "kind/name (fields)" for a log line.
func keptName(name string, kept []string) string {
	return name + " (" + strings.Join(kept, ", ") + ")"
}

// runtimeConfigBody is the apply body of a rendered runtime config.
func runtimeConfigBody(c client.Client, rc client.Object) (*unstructured.Unstructured, error) {
	body, err := kube.ApplyObject(rc, c.Scheme())
	if err != nil {
		return nil, err
	}
	stripNullTimestamps(body.Object)
	return body, nil
}
