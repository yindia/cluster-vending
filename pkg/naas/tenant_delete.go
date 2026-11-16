package naas

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const DeletionFenceAnnotation = "cloud.cluster-vending.io/deletion-gtr-fence"
const DeletionAppliedSpecAnnotation = "cloud.cluster-vending.io/deletion-applied-spec"

type appliedTenantSpec struct {
	TenantUID string         `json:"tenantUID"`
	Spec      map[string]any `json:"spec"`
}

type tenantWithdrawalFence struct {
	TenantName        string                   `json:"tenantName"`
	TenantUID         string                   `json:"tenantUID"`
	Generation        int64                    `json:"generation"`
	AppliedSpecDigest string                   `json:"appliedSpecDigest"`
	GTRs              map[string]withdrawalGTR `json:"gtrs"`
}
type withdrawalGTR struct {
	UID        string `json:"uid"`
	Generation int64  `json:"generation"`
}

// DeletionAppliedTenant preserves the last remote configuration, not a desired
// management update which may never have been applied to Capsule. Older in-flight
// deletions without a snapshot retain their remote rules; only legacy cleared
// namespace keys need the caller's existing compatibility fallback.
func DeletionAppliedTenant(remote *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	if remote == nil {
		return nil, fmt.Errorf("owned Tenant configuration is unavailable")
	}
	result := remote.DeepCopy()
	raw := remote.GetAnnotations()[DeletionAppliedSpecAnnotation]
	if raw == "" {
		return result, nil
	}
	snapshot := appliedTenantSpec{}
	if err := json.Unmarshal([]byte(raw), &snapshot); err != nil || snapshot.TenantUID == "" || snapshot.TenantUID != string(remote.GetUID()) || snapshot.Spec == nil {
		return nil, fmt.Errorf("applied Tenant configuration snapshot is invalid or belongs to another UID")
	}
	// Unstructured integer fields must retain int64, not JSON float64.
	decoded := &unstructured.Unstructured{}
	specJSON, err := json.Marshal(map[string]any{"apiVersion": TenantGVK.GroupVersion().String(), "kind": TenantGVK.Kind, "spec": snapshot.Spec})
	if err != nil {
		return nil, err
	}
	if err := decoded.UnmarshalJSON(specJSON); err != nil {
		return nil, err
	}
	result.Object["spec"] = decoded.Object["spec"]
	return result, nil
}

func appliedSpecDigest(remote *unstructured.Unstructured) string {
	sum := sha256.Sum256([]byte(remote.GetAnnotations()[DeletionAppliedSpecAnnotation]))
	return hex.EncodeToString(sum[:])
}

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
	if annotations[DeletionAppliedSpecAnnotation] == "" {
		spec, _ := remote.DeepCopy().Object["spec"].(map[string]any)
		raw, err := json.Marshal(appliedTenantSpec{TenantUID: string(remote.GetUID()), Spec: spec})
		if err == nil {
			annotations[DeletionAppliedSpecAnnotation] = string(raw)
		}
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

// CaptureTenantDeletionFence binds the withdrawal to a specific Tenant UID,
// applied configuration and installed GTR identities. Capsule does not publish a
// per-Tenant observed generation on GTRs: resourceVersion movement is neither
// necessary for an unchanged resync nor evidence of this Tenant's cleanup.
func CaptureTenantDeletionFence(ctx context.Context, c client.Client, remote *unstructured.Unstructured) (string, error) {
	if err := validateTenantWithdrawal(remote); err != nil {
		return "", err
	}
	if _, err := DeletionAppliedTenant(remote); err != nil {
		return "", err
	}
	objects, err := tenantGTRs(ctx, c)
	if err != nil {
		return "", err
	}
	fence := tenantWithdrawalFence{TenantName: remote.GetName(), TenantUID: string(remote.GetUID()), Generation: remote.GetGeneration(), AppliedSpecDigest: appliedSpecDigest(remote), GTRs: map[string]withdrawalGTR{}}
	for _, obj := range objects {
		if obj.GetUID() == "" || obj.GetGeneration() < 1 {
			return "", fmt.Errorf("GTR %s has no observable identity", obj.GetName())
		}
		fence.GTRs[obj.GetName()] = withdrawalGTR{UID: string(obj.GetUID()), Generation: obj.GetGeneration()}
	}
	raw, err := json.Marshal(fence)
	return string(raw), err
}

func validateTenantWithdrawal(remote *unstructured.Unstructured) error {
	if remote == nil || remote.GetUID() == "" || remote.GetName() == "" || remote.GetGeneration() < 1 {
		return fmt.Errorf("owned Tenant withdrawal identity is unavailable")
	}
	keys, found, err := unstructured.NestedSlice(remote.Object, "spec", "data", "naas", "namespaceKeys")
	if err != nil || !found || len(keys) != 0 || remote.GetLabels()[LabelAccessReady] != "false" || remote.GetLabels()[LabelProfile] != "terminating" || remote.GetAnnotations()[DeletionAnnotation] != "true" {
		return fmt.Errorf("tenant namespace generation and access have not been withdrawn")
	}
	return nil
}

// TenantDeletionFenceCurrent also recognizes legacy RV-only fences as needing
// migration. Changes to installed GTR identities remain checked by acknowledgement.
func TenantDeletionFenceCurrent(remote *unstructured.Unstructured) bool {
	if remote == nil {
		return false
	}
	fence := tenantWithdrawalFence{}
	if json.Unmarshal([]byte(remote.GetAnnotations()[DeletionFenceAnnotation]), &fence) != nil || fence.TenantUID != string(remote.GetUID()) || fence.TenantName != remote.GetName() || fence.Generation < 1 || fence.Generation > remote.GetGeneration() || fence.AppliedSpecDigest != appliedSpecDigest(remote) || len(fence.GTRs) == 0 {
		return false
	}
	for _, binding := range fence.GTRs {
		if binding.UID == "" || binding.Generation < 1 {
			return false
		}
	}
	return true
}

func TenantDeletionAcknowledged(ctx context.Context, c client.Client, remote *unstructured.Unstructured) error {
	if err := validateTenantWithdrawal(remote); err != nil {
		return err
	}
	observed, _, err := unstructured.NestedInt64(remote.Object, "status", "observedGeneration")
	if err != nil || observed < remote.GetGeneration() {
		return fmt.Errorf("waiting for Capsule to observe the current Tenant withdrawal generation")
	}
	fence := tenantWithdrawalFence{}
	if json.Unmarshal([]byte(remote.GetAnnotations()[DeletionFenceAnnotation]), &fence) != nil || fence.TenantUID != string(remote.GetUID()) || fence.TenantName != remote.GetName() || fence.Generation < 1 || fence.Generation > remote.GetGeneration() || fence.AppliedSpecDigest != appliedSpecDigest(remote) || len(fence.GTRs) == 0 {
		return fmt.Errorf("waiting for an identity-bound Tenant withdrawal observation")
	}
	if _, err := DeletionAppliedTenant(remote); err != nil {
		return err
	}
	objects, err := tenantGTRs(ctx, c)
	if err != nil {
		return err
	}
	if len(objects) != len(fence.GTRs) {
		return fmt.Errorf("shared GTR inventory changed during withdrawal")
	}
	for _, obj := range objects {
		binding, ok := fence.GTRs[obj.GetName()]
		if !ok || binding.UID != string(obj.GetUID()) || binding.Generation != obj.GetGeneration() {
			return fmt.Errorf("GTR %s identity/generation changed; withdrawal requires review", obj.GetName())
		}
		observed, _, err := unstructured.NestedInt64(obj.Object, "status", "observedGeneration")
		if err != nil || observed < obj.GetGeneration() {
			return fmt.Errorf("GTR %s has not observed its current generation", obj.GetName())
		}
		selected, _, err := unstructured.NestedStringSlice(obj.Object, "status", "selectedTenants")
		if err != nil || slices.Contains(selected, remote.GetName()) {
			return fmt.Errorf("GTR %s still selects this Tenant", obj.GetName())
		}
		items, _, err := unstructured.NestedSlice(obj.Object, "status", "processedItems")
		if err != nil {
			return fmt.Errorf("GTR %s cleanup inventory cannot be read", obj.GetName())
		}
		for _, raw := range items {
			item, ok := raw.(map[string]any)
			if !ok {
				return fmt.Errorf("GTR %s has malformed cleanup inventory", obj.GetName())
			}
			tenant, ok := item["tenant"].(string)
			if !ok || tenant == "" {
				return fmt.Errorf("GTR %s contains an unattributed resource; cleanup cannot be established", obj.GetName())
			}
			if tenant == remote.GetName() {
				return fmt.Errorf("GTR %s still tracks resources for Tenant %s", obj.GetName(), remote.GetName())
			}
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
			return fmt.Errorf("GTR %s has not confirmed completed resource cleanup", obj.GetName())
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
