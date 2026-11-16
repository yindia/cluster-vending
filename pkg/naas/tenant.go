package naas

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/google/uuid"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/version"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
)

var TenantGVK = schema.GroupVersionKind{Group: "capsule.clastix.io", Version: "v1beta2", Kind: "Tenant"}

const LabelSubTenantID = "cloud.cluster-vending.io/subtenant-id"
const LabelSubTenantUID = "cloud.cluster-vending.io/subtenant-uid"
const LabelTenantUID = "cloud.cluster-vending.io/tenant-uid"
const LabelClusterUID = "cloud.cluster-vending.io/cluster-uid"
const LabelTenant = "cloud.cluster-vending.io/tenant"
const LabelProfile = "cloud.cluster-vending.io/naas-profile"
const LabelAccessReady = "cloud.cluster-vending.io/access-ready"
const RevisionAnnotation = "cloud.cluster-vending.io/template-revision"
const SpecAnnotation = "cloud.cluster-vending.io/subtenant-spec-hash"
const DeletionAnnotation = "cloud.cluster-vending.io/deletion-requested"

func TenantName(id string) (string, error) {
	parsed, err := uuid.Parse(id)
	if err != nil || parsed.String() != id {
		return "", fmt.Errorf("SubTenant name must be a canonical UUID")
	}
	return "st-" + strings.ReplaceAll(id, "-", ""), nil
}

// RemoteTenantName binds every workload operation to the immutable declared
// name. Legacy allocations may retain only their deterministic UUID-derived name.
func RemoteTenantName(sub *cloud.SubTenant) (string, error) {
	if sub.Spec.Name != "" {
		if len(sub.Spec.Name) > 35 || len(validation.IsDNS1123Label(sub.Spec.Name)) != 0 {
			return "", fmt.Errorf("capsule Tenant name must be a DNS label of at most 35 characters")
		}
		if recorded := sub.Status.RemoteCapsuleTenant; recorded != "" && recorded != sub.Spec.Name {
			return "", fmt.Errorf("declared Capsule Tenant name differs from its recorded identity")
		}
		return sub.Spec.Name, nil
	}
	legacy, err := TenantName(sub.Name)
	if err != nil {
		return "", err
	}
	if recorded := sub.Status.RemoteCapsuleTenant; recorded != "" && recorded != legacy {
		return "", fmt.Errorf("legacy Capsule Tenant name differs from its deterministic identity")
	}
	return legacy, nil
}

func DeletionProtectionEnabled(value *bool) bool { return value == nil || *value }

func ownership(sub *cloud.SubTenant) map[string]string {
	return map[string]string{LabelSubTenantID: sub.Name, LabelSubTenantUID: string(sub.UID), LabelTenant: sub.Spec.TenantRef.Name, LabelTenantUID: sub.Spec.TenantRef.UID, LabelClusterUID: sub.Spec.ClusterRef.UID, "cloud.cluster-vending.io/cluster-id": sub.Spec.ClusterRef.ID}
}

// OwnTenant checks immutable bindings, including the last observed remote UID.
// Matching a convenient name or an allocation UUID alone never permits adoption.
func OwnTenant(remote *unstructured.Unstructured, sub *cloud.SubTenant) bool {
	name, err := RemoteTenantName(sub)
	if err != nil || remote.GetName() != name || remote.GetNamespace() != "" || remote.GroupVersionKind() != TenantGVK {
		return false
	}
	if remote.GetUID() == "" {
		return false
	}
	if known := sub.Status.RemoteCapsuleTenantUID; known != "" && known != string(remote.GetUID()) {
		return false
	}
	for key, value := range ownership(sub) {
		if value == "" || remote.GetLabels()[key] != value {
			return false
		}
	}
	return true
}

// RenderTenant emits exactly one Capsule Tenant. It emits no namespaces,
// policies, service accounts or RBAC children, and cannot open developer access.
func RenderTenant(sub *cloud.SubTenant, currentVersion, revision string) (*unstructured.Unstructured, error) {
	name, err := RemoteTenantName(sub)
	if err != nil {
		return nil, err
	}
	if sub.UID == "" || sub.Spec.TenantRef.UID == "" || sub.Spec.ClusterRef.UID == "" {
		return nil, fmt.Errorf("immutable registration UIDs are required")
	}
	if sub.Spec.PolicyProfile != "restricted-ambient-v1" {
		return nil, fmt.Errorf("only restricted-ambient-v1 is supported")
	}
	if sub.Spec.Access != nil && len(sub.Spec.Access.Bindings) > 0 {
		return nil, fmt.Errorf("identity references require a trusted platform resolver; no resolver is configured")
	}
	keys := sub.Spec.NamespaceKeys
	if len(keys) < 1 || len(keys) > 64 || int(sub.Spec.Quota.NamespaceCount) < len(keys) || sub.Spec.Quota.NamespaceCount > 64 {
		return nil, fmt.Errorf("namespace allocation exceeds its 1–64 namespace budget")
	}
	seen := map[string]bool{}
	names := make([]any, 0, len(keys))
	for _, key := range keys {
		value := string(key)
		if len(value) > 27 || len(validation.IsDNS1123Label(value)) > 0 || seen[value] {
			return nil, fmt.Errorf("namespace keys must be unique DNS labels of at most 27 characters")
		}
		seen[value] = true
		names = append(names, value)
	}
	if len(sub.Spec.Quota.Hard) < 1 || len(sub.Spec.Quota.Hard) > 12 {
		return nil, fmt.Errorf("quota requires 1–12 approved resource limits")
	}
	hard := map[string]any{}
	for key, value := range sub.Spec.Quota.Hard {
		count := false
		switch key {
		case "requests.cpu", "requests.memory", "limits.cpu", "limits.memory", "requests.ephemeral-storage", "limits.ephemeral-storage", "requests.storage":
		case "persistentvolumeclaims", "pods", "services", "secrets", "configmaps":
			count = true
		default:
			return nil, fmt.Errorf("unsupported quota resource %q", key)
		}
		quantity, err := resource.ParseQuantity(value)
		if len(value) > 64 || err != nil || quantity.Sign() < 0 {
			return nil, fmt.Errorf("invalid nonnegative Kubernetes quota quantity for %s", key)
		}
		if count && quantity.Cmp(*resource.NewQuantity(quantity.Value(), resource.DecimalSI)) != 0 {
			return nil, fmt.Errorf("quota %s requires an integral count", key)
		}
		hard[key] = value
	}
	v, err := version.ParseGeneric(currentVersion)
	if err != nil || v.Major() != 1 || v.Minor() == 0 {
		return nil, fmt.Errorf("an observed Kubernetes minor is required to pin PSS")
	}
	pss := fmt.Sprintf("v%d.%d", v.Major(), v.Minor())
	labels := ownership(sub)
	labels[LabelProfile] = "restricted-ambient-v1"
	labels[LabelAccessReady] = "false"
	protected := map[string]any{"istio.io/dataplane-mode": "ambient"}
	for _, mode := range []string{"enforce", "audit", "warn"} {
		protected["pod-security.kubernetes.io/"+mode] = "restricted"
		protected["pod-security.kubernetes.io/"+mode+"-version"] = pss
	}
	metadataRules := map[string]any{}
	for key, value := range protected {
		metadataRules[key] = map[string]any{"managed": value, "required": true, "values": []any{map[string]any{"exact": []any{value}}}}
	}
	spec := map[string]any{
		"preventDeletion": DeletionProtectionEnabled(sub.Spec.DeletionProtection), "forceTenantPrefix": true, "cordoned": false,
		"permissions":      map[string]any{"allowOwnerPromotion": false},
		"owners":           []any{map[string]any{"kind": "ServiceAccount", "name": "system:serviceaccount:naas-system:provisioner", "clusterRoles": []any{"naas-workload-editor"}}},
		"namespaceOptions": map[string]any{"quota": int64(sub.Spec.Quota.NamespaceCount), "managedMetadataOnly": false},
		"rules": []any{
			map[string]any{"quota": []any{map[string]any{"name": "budget", "hard": hard}}},
			map[string]any{"enforce": map[string]any{"action": "allow", "services": map[string]any{"types": []any{"ClusterIP"}}}},
			map[string]any{"enforce": map[string]any{"action": "deny", "services": map[string]any{"externalIPs": map[string]any{}}}},
			map[string]any{"enforce": map[string]any{"action": "allow", "metadata": []any{map[string]any{"apiGroups": []any{"v1"}, "kinds": []any{"Namespace"}, "labels": metadataRules}}}},
		},
		"data": map[string]any{"naas": map[string]any{"namespaceKeys": names, "developerGroup": "", "pssVersion": pss}},
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	obj := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	obj.SetGroupVersionKind(TenantGVK)
	obj.SetName(name)
	obj.SetLabels(labels)
	obj.SetAnnotations(map[string]string{RevisionAnnotation: revision, SpecAnnotation: hex.EncodeToString(sum[:])})
	return obj, nil
}

// MergeTenant changes only this controller's fields and retains server metadata,
// status and unrelated annotations. UID/resource-version preconditions are kept.
func MergeTenant(current, desired *unstructured.Unstructured) *unstructured.Unstructured {
	next := current.DeepCopy()
	labels := maps.Clone(current.GetLabels())
	if labels == nil {
		labels = map[string]string{}
	}
	for key, value := range desired.GetLabels() {
		labels[key] = value
	}
	next.SetLabels(labels)
	annotations := maps.Clone(current.GetAnnotations())
	if annotations == nil {
		annotations = map[string]string{}
	}
	for key, value := range desired.GetAnnotations() {
		annotations[key] = value
	}
	next.SetAnnotations(annotations)
	// The complete Tenant spec is platform-owned; arbitrary access/rules are not merged.
	next.Object["spec"] = desired.DeepCopy().Object["spec"]
	return next
}

func EqualTenantSpec(a, b *unstructured.Unstructured) bool {
	left, _ := json.Marshal(a.Object["spec"])
	right, _ := json.Marshal(b.Object["spec"])
	return slices.Equal(left, right)
}
