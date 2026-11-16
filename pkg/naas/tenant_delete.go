package naas

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const DeletionFenceAnnotation = "cloud.cluster-vending.io/deletion-gtr-fence"

// WithdrawTenantGeneration changes ONLY the owned Tenant. The shared namespace
// generator stops selecting it, but existing namespaces are never removed here.
func WithdrawTenantGeneration(remote *unstructured.Unstructured) *unstructured.Unstructured {
	next := remote.DeepCopy()
	labels := maps.Clone(next.GetLabels())
	labels[LabelAccessReady] = "false"
	labels[LabelProfile] = "terminating"
	next.SetLabels(labels)
	annotations := maps.Clone(next.GetAnnotations())
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[DeletionAnnotation] = "true"
	delete(annotations, DeletionFenceAnnotation)
	next.SetAnnotations(annotations)
	_ = unstructured.SetNestedSlice(next.Object, []any{}, "spec", "data", "naas", "namespaceKeys")
	_ = unstructured.SetNestedField(next.Object, "", "spec", "data", "naas", "developerGroup")
	_ = unstructured.SetNestedField(next.Object, true, "spec", "preventDeletion")
	next.SetGeneration(max(int64(1), remote.GetGeneration()+1))
	return next
}

// CaptureTenantDeletionFence is called in a later pass, after withdrawal. An
// older Ready snapshot cannot acknowledge that withdrawal; every relevant GTR
// must subsequently produce a fresh current-generation, deselected observation.
func CaptureTenantDeletionFence(ctx context.Context, c client.Client) (string, error) {
	objects, err := tenantGTRs(ctx, c)
	if err != nil {
		return "", err
	}
	versions := map[string]string{}
	for _, obj := range objects {
		if obj.GetResourceVersion() == "" {
			return "", fmt.Errorf("GTR %s has no observable version", obj.GetName())
		}
		versions[obj.GetName()] = obj.GetResourceVersion()
	}
	raw, err := json.Marshal(versions)
	return string(raw), err
}

func TenantDeletionAcknowledged(ctx context.Context, c client.Client, remote *unstructured.Unstructured) error {
	observed, _, err := unstructured.NestedInt64(remote.Object, "status", "observedGeneration")
	if err != nil || remote.GetGeneration() < 1 || observed < remote.GetGeneration() {
		return fmt.Errorf("waiting for Capsule to observe namespace-generation withdrawal")
	}
	versions := map[string]string{}
	if json.Unmarshal([]byte(remote.GetAnnotations()[DeletionFenceAnnotation]), &versions) != nil || len(versions) == 0 {
		return fmt.Errorf("waiting for a post-withdrawal GTR observation fence")
	}
	objects, err := tenantGTRs(ctx, c)
	if err != nil {
		return err
	}
	for _, obj := range objects {
		if versions[obj.GetName()] == "" || versions[obj.GetName()] == obj.GetResourceVersion() {
			return fmt.Errorf("waiting for fresh GTR %s acknowledgement", obj.GetName())
		}
		observed, _, err := unstructured.NestedInt64(obj.Object, "status", "observedGeneration")
		if err != nil || obj.GetGeneration() < 1 || observed < obj.GetGeneration() {
			return fmt.Errorf("GTR %s has not observed its current generation", obj.GetName())
		}
		selected, _, err := unstructured.NestedStringSlice(obj.Object, "status", "selectedTenants")
		if err != nil || slices.Contains(selected, remote.GetName()) {
			return fmt.Errorf("GTR %s still selects this Tenant", obj.GetName())
		}
		conditions, _, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
		if err != nil {
			return err
		}
		ready := false
		for _, raw := range conditions {
			condition, ok := raw.(map[string]any)
			if ok && condition["type"] == "Ready" && condition["status"] == "True" {
				ready = true
			}
		}
		if !ready {
			return fmt.Errorf("GTR %s has not confirmed a completed deselection pass", obj.GetName())
		}
	}
	return nil
}

func tenantGTRs(ctx context.Context, c client.Client) ([]*unstructured.Unstructured, error) {
	bootstrap, err := Load()
	if err != nil {
		return nil, err
	}
	var out []*unstructured.Unstructured
	for _, expected := range bootstrap.Objects() {
		if expected.GetKind() != "GlobalTenantResource" {
			continue
		}
		current := &unstructured.Unstructured{}
		current.SetGroupVersionKind(expected.GroupVersionKind())
		if err := c.Get(ctx, client.ObjectKeyFromObject(expected), current); err != nil {
			return nil, fmt.Errorf("shared GTR %s cannot be observed", expected.GetName())
		}
		if !payloadMatches(current, expected) {
			return nil, fmt.Errorf("shared GTR %s is not the current protected template", expected.GetName())
		}
		out = append(out, current)
	}
	return out, nil
}
