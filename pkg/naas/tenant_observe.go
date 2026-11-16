package naas

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"
	"text/template"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
)

// VerifyTenantBootstrap reads the shared installation; it never installs it.
// Exact protected payload comparison binds observations to this build's revision.
// GTR Ready alone is deliberately not treated as isolation qualification.
func VerifyTenantBootstrap(ctx context.Context, c client.Client) (*Bootstrap, error) {
	b, err := Load()
	if err != nil {
		return nil, err
	}
	for _, expected := range b.Objects() {
		current := &unstructured.Unstructured{}
		current.SetGroupVersionKind(expected.GroupVersionKind())
		if err := c.Get(ctx, client.ObjectKeyFromObject(expected), current); err != nil {
			return nil, fmt.Errorf("bootstrap %s %s is unavailable: %w", expected.GetKind(), expected.GetName(), err)
		}
		if !payloadMatches(current, expected) {
			return nil, fmt.Errorf("bootstrap %s %s does not match the current profile revision", expected.GetKind(), expected.GetName())
		}
	}
	provisioner := &corev1.ServiceAccount{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: "naas-system", Name: "provisioner"}, provisioner); err != nil {
		return nil, fmt.Errorf("trusted Capsule provisioner service account is unavailable: %w", err)
	}
	return b, nil
}

func payloadMatches(current, expected *unstructured.Unstructured) bool {
	if expected.GetAPIVersion() == "networking.k8s.io/v1" && expected.GetKind() == "NetworkPolicy" {
		current = networkPolicyDefaults(current)
		expected = networkPolicyDefaults(expected)
	}
	if expected.GetKind() == "GlobalTenantResource" {
		current = gtrDefaults(current)
		expected = gtrDefaults(expected)
	}
	if expected.GetKind() == "ClusterRole" && current.Object["aggregationRule"] != nil && expected.Object["aggregationRule"] == nil {
		return false
	}
	for key, value := range expected.Object {
		if key == "metadata" || key == "status" || key == "apiVersion" || key == "kind" {
			continue
		}
		if !reflect.DeepEqual(current.Object[key], value) {
			return false
		}
	}
	for key, value := range expected.GetLabels() {
		if current.GetLabels()[key] != value {
			return false
		}
	}
	return true
}

// Normalize only API representations/defaults at known NetworkPolicy fields.
// In particular, [] permits no traffic while [{}] permits all traffic; selectors,
// policyTypes and explicit protocols must retain their exact policy meaning.
func networkPolicyDefaults(input *unstructured.Unstructured) *unstructured.Unstructured {
	out := input.DeepCopy()
	spec, ok := out.Object["spec"].(map[string]any)
	if !ok {
		return out
	}
	for _, direction := range []string{"ingress", "egress"} {
		if spec[direction] == nil {
			delete(spec, direction)
			continue
		}
		rules, ok := spec[direction].([]any)
		if !ok {
			continue
		}
		if len(rules) == 0 {
			delete(spec, direction)
			continue
		}
		for _, raw := range rules {
			rule, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			ports, _ := rule["ports"].([]any)
			for _, raw := range ports {
				port, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				if port["protocol"] == nil {
					port["protocol"] = "TCP"
				}
				for _, field := range []string{"port", "endPort"} {
					// The YAML/JSON template decoder can yield float64 while the
					// typed Kubernetes converter uses int64. Never coerce names,
					// fractional numbers or values outside the API's port range.
					if value, ok := port[field].(float64); ok && value >= 1 && value <= 65535 && value == float64(int64(value)) {
						port[field] = int64(value)
					}
				}
			}
		}
	}
	return out
}

// ObserveTenantNamespaces includes owned or suspiciously matching namespaces,
// including unexpected keys and terminating objects. Deletion must not overlook
// a namespace merely because someone removed or changed one ownership label.
func ObserveTenantNamespaces(ctx context.Context, c client.Client, remote *unstructured.Unstructured, sub *cloud.SubTenant, b *Bootstrap) ([]cloud.SubTenantNamespaceStatus, []string, error) {
	list := &corev1.NamespaceList{}
	if err := c.List(ctx, list); err != nil {
		return nil, nil, err
	}
	name, err := RemoteTenantName(sub)
	if err != nil {
		return nil, nil, err
	}
	uid := sub.Status.RemoteCapsuleTenantUID
	if remote != nil {
		uid = string(remote.GetUID())
	}
	var inventory []cloud.SubTenantNamespaceStatus
	var blockers []string
	found := map[string]bool{}
	for _, ns := range list.Items {
		owned := false
		for _, owner := range ns.OwnerReferences {
			if owner.APIVersion == TenantGVK.GroupVersion().String() && owner.Kind == "Tenant" && owner.Name == name && uid != "" && string(owner.UID) == uid {
				owned = true
			}
		}
		matches := owned || ns.Labels["capsule.clastix.io/tenant"] == name || ns.Labels[LabelSubTenantID] == sub.Name || strings.HasPrefix(ns.Name, name+"-")
		if !matches {
			continue
		}
		foreign, err := NamespaceBelongsToForeignAllocation(ctx, c, sub, &ns)
		if err != nil {
			return nil, nil, err
		}
		if foreign {
			continue
		}
		key := strings.TrimPrefix(ns.Name, name+"-")
		if strings.HasPrefix(ns.Name, name+"-") && len(key) <= 27 {
			found[key] = true
		} else {
			sum := sha256.Sum256([]byte(ns.Name))
			key = "observed-" + hex.EncodeToString(sum[:8])
		}
		entry := cloud.SubTenantNamespaceStatus{Key: cloud.SubTenantNamespaceKey(key), Name: ns.Name, UID: string(ns.UID)}
		baseline := metav1.Condition{Type: "BaselineReady", Status: metav1.ConditionFalse, Reason: "OwnershipMismatch", Message: "Namespace ownership does not match the current Capsule Tenant"}
		if owned && ns.Labels[LabelSubTenantID] == sub.Name && ns.Labels[LabelTenant] == sub.Spec.TenantRef.Name {
			baseline.Reason, baseline.Message = "BaselinePending", "Waiting for current shared namespace and isolation templates"
			if b != nil && remote != nil && ns.DeletionTimestamp.IsZero() {
				if err := verifyNamespace(ctx, c, remote, &ns, b); err == nil {
					baseline.Status, baseline.Reason, baseline.Message = metav1.ConditionTrue, "BaselineObserved", "Current baseline objects were observed; runtime isolation qualification is still required"
					entry.TemplateRevision = b.Revision()
				} else {
					baseline.Message = err.Error()
				}
			}
		}
		entry.Conditions = []metav1.Condition{baseline}
		inventory = append(inventory, entry)
		blockers = append(blockers, "Namespace "+ns.Name+" still exists; require-empty deletion never garbage-collects namespace data")
	}
	for _, key := range sub.Spec.NamespaceKeys {
		if !found[string(key)] {
			inventory = append(inventory, cloud.SubTenantNamespaceStatus{Key: key, Conditions: []metav1.Condition{{Type: "BaselineReady", Status: metav1.ConditionFalse, Reason: "NamespacePending", Message: "Waiting for Capsule namespace generation"}}})
		}
	}
	slices.SortFunc(inventory, func(a, b cloud.SubTenantNamespaceStatus) int { return strings.Compare(string(a.Key), string(b.Key)) })
	slices.Sort(blockers)
	if len(blockers) > 64 {
		blockers = append(blockers[:63], fmt.Sprintf("and %d more namespace dependencies", len(blockers)-63))
	}
	return inventory, blockers, nil
}

func verifyNamespace(ctx context.Context, c client.Client, remote *unstructured.Unstructured, ns *corev1.Namespace, b *Bootstrap) error {
	rawNS, err := runtime.DefaultUnstructuredConverter.ToUnstructured(ns)
	if err != nil {
		return err
	}
	data := map[string]any{"tenant": remote.Object, "namespace": rawNS}
	for _, gtr := range b.Objects() {
		if gtr.GetKind() != "GlobalTenantResource" || gtr.GetName() == "naas-developer-access" {
			continue
		}
		resources, _, err := unstructured.NestedSlice(gtr.Object, "spec", "resources")
		if err != nil {
			return err
		}
		for _, raw := range resources {
			resource, ok := raw.(map[string]any)
			if !ok {
				return fmt.Errorf("invalid shared resource template")
			}
			generators, _, err := unstructured.NestedSlice(resource, "generators")
			if err != nil {
				return err
			}
			for _, raw := range generators {
				generator, ok := raw.(map[string]any)
				if !ok {
					return fmt.Errorf("invalid generator")
				}
				source, ok := generator["template"].(string)
				if !ok {
					return fmt.Errorf("missing baseline generator")
				}
				parsed, err := template.New("baseline").Option("missingkey=error").Parse(source)
				if err != nil {
					return err
				}
				var rendered bytes.Buffer
				if err := parsed.Execute(&rendered, data); err != nil {
					return err
				}
				decoder := yaml.NewYAMLOrJSONDecoder(&rendered, 4096)
				for {
					obj := map[string]any{}
					err := decoder.Decode(&obj)
					if errors.Is(err, io.EOF) {
						break
					}
					if err != nil {
						return err
					}
					if len(obj) == 0 {
						continue
					}
					expected := &unstructured.Unstructured{Object: obj}
					// The tenant-scoped namespace generator emits every requested namespace.
					if expected.GetKind() == "Namespace" && expected.GetName() != ns.Name {
						continue
					}
					current := &unstructured.Unstructured{}
					current.SetGroupVersionKind(expected.GroupVersionKind())
					if err := c.Get(ctx, client.ObjectKeyFromObject(expected), current); err != nil {
						return fmt.Errorf("baseline %s %s is unavailable", expected.GetKind(), expected.GetName())
					}
					if !ProvisioningPayloadMatches(current, expected) {
						return fmt.Errorf("baseline %s %s differs from the current protected template", expected.GetKind(), expected.GetName())
					}
				}
			}
		}
	}
	return nil
}

// These are the pinned Capsule v0.15.2 structural defaults. Normalize both
// payloads so defaulting cannot cause an endless mismatch/update loop; differing
// non-default values still fail closed rather than being ignored.
func gtrDefaults(input *unstructured.Unstructured) *unstructured.Unstructured {
	out := input.DeepCopy()
	for _, entry := range []struct {
		path  []string
		value bool
	}{
		{[]string{"spec", "cordoned"}, false},
		{[]string{"spec", "pruningOnDelete"}, true},
		{[]string{"spec", "settings", "adopt"}, false},
		{[]string{"spec", "settings", "force"}, false},
	} {
		if _, found, _ := unstructured.NestedFieldNoCopy(out.Object, entry.path...); !found {
			_ = unstructured.SetNestedField(out.Object, entry.value, entry.path...)
		}
	}
	return out
}
