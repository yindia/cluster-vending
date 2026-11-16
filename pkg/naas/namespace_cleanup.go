package naas

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"
	"text/template"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
)

// NamespaceContentReader must discover and list every preferred, namespaced,
// listable/deletable API resource, including custom resources. Partial discovery
// or an unreadable resource is an error, never evidence of emptiness.
type NamespaceContentReader interface {
	NamespaceContents(context.Context, string) ([]unstructured.Unstructured, error)
}

// PreflightNamespaceCleanup does not delete or mutate anything. Namespace
// existence alone is not a blocker; ownership, unknown content or unavailable
// inventory is. The caller must stop generation and await fresh Capsule/GTR
// acknowledgements before calling DeleteEmptyNamespace.
func PreflightNamespaceCleanup(ctx context.Context, c client.Client, remote *unstructured.Unstructured, sub *cloud.SubTenant) ([]corev1.Namespace, []string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	name, err := RemoteTenantName(sub)
	if err != nil {
		return nil, nil, err
	}
	list := &corev1.NamespaceList{}
	if err := c.List(ctx, list); err != nil {
		return nil, nil, err
	}
	if list.Continue != "" {
		return nil, nil, fmt.Errorf("namespace list is incomplete")
	}
	var namespaces []corev1.Namespace
	var blockers []string
	for _, ns := range list.Items {
		matches := strings.HasPrefix(ns.Name, name+"-") || ns.Labels["capsule.clastix.io/tenant"] == name || ns.Labels[LabelSubTenantID] == sub.Name
		for _, owner := range ns.OwnerReferences {
			if owner.APIVersion == TenantGVK.GroupVersion().String() && owner.Kind == "Tenant" && owner.Name == name {
				matches = true
			}
		}
		for _, recorded := range sub.Status.Namespaces {
			if recorded.Name == ns.Name {
				matches = true
			}
		}
		if !matches {
			continue
		}
		namespaces = append(namespaces, ns)
		if err := namespaceOwnership(remote, sub, &ns); err != nil {
			blockers = append(blockers, fmt.Sprintf("Namespace %s: %s", ns.Name, err))
			continue
		}
		if !ns.DeletionTimestamp.IsZero() {
			continue
		}
		content, err := namespaceContentBlockers(ctx, c, remote, sub, &ns)
		if err != nil {
			return namespaces, blockers, err
		}
		blockers = append(blockers, content...)
	}
	slices.SortFunc(namespaces, func(a, b corev1.Namespace) int { return strings.Compare(a.Name, b.Name) })
	slices.Sort(blockers)
	if len(blockers) > 64 {
		blockers = append(blockers[:63], fmt.Sprintf("and %d more content blockers", len(blockers)-63))
	}
	return namespaces, blockers, nil
}

// DeleteEmptyNamespace is the sole offboarding exception to the Tenant-only
// workload writer. It never deletes a child object or strips any finalizer.
// The controller owns deletion-protection, freeze and fresh withdrawal-ack gates.
// Observed cordoning is required here because UID/RV alone cannot serialize
// namespace deletion against new child writes. Discovery still cannot prove
// atomic absence of requests already admitted before cordoning.
func DeleteEmptyNamespace(ctx context.Context, c client.Client, remote *unstructured.Unstructured, sub *cloud.SubTenant, observed *corev1.Namespace) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if DeletionProtectionEnabled(sub.Spec.DeletionProtection) {
		return fmt.Errorf("SubTenant deletion protection is enabled")
	}
	if observed == nil {
		return fmt.Errorf("namespace deletion requires a reviewed namespace snapshot")
	}
	currentRemote := &unstructured.Unstructured{}
	currentRemote.SetGroupVersionKind(TenantGVK)
	if remote == nil {
		return fmt.Errorf("owned Capsule Tenant observation is required")
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(remote), currentRemote); err != nil {
		return err
	}
	if !OwnTenant(currentRemote, sub) || currentRemote.GetUID() != remote.GetUID() {
		return fmt.Errorf("capsule Tenant ownership changed")
	}
	cordoned, _, _ := unstructured.NestedBool(currentRemote.Object, "spec", "cordoned")
	if !cordoned {
		return fmt.Errorf("waiting for Capsule Tenant cordoning before namespace cleanup")
	}
	current := &corev1.Namespace{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(observed), current); err != nil {
		return client.IgnoreNotFound(err)
	}
	if current.UID != observed.UID || current.ResourceVersion != observed.ResourceVersion {
		return fmt.Errorf("namespace changed since preflight; fresh review is required")
	}
	if err := namespaceOwnership(currentRemote, sub, current); err != nil {
		return err
	}
	if !current.DeletionTimestamp.IsZero() {
		return nil
	}
	if current.Labels["projectcapsule.dev/cordoned"] != "true" {
		return fmt.Errorf("waiting for namespace cordoning observation")
	}
	blockers, err := namespaceContentBlockers(ctx, c, currentRemote, sub, current)
	if err != nil {
		return err
	}
	if len(blockers) > 0 {
		return fmt.Errorf("namespace is not empty: %s", strings.Join(blockers, "; "))
	}
	// Re-read after discovery to catch namespace ownership/lifecycle changes.
	fresh := &corev1.Namespace{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(current), fresh); err != nil {
		return client.IgnoreNotFound(err)
	}
	if fresh.UID != current.UID || fresh.ResourceVersion != current.ResourceVersion {
		return fmt.Errorf("namespace changed during content inspection")
	}
	uid, rv := current.UID, current.ResourceVersion
	return client.IgnoreNotFound(c.Delete(ctx, current, client.Preconditions{UID: &uid, ResourceVersion: &rv}))
}

func namespaceOwnership(remote *unstructured.Unstructured, sub *cloud.SubTenant, ns *corev1.Namespace) error {
	if remote == nil || !OwnTenant(remote, sub) {
		return fmt.Errorf("owned Capsule Tenant UID cannot be verified")
	}
	if remote.GetLabels()[LabelAccessReady] != "false" || ns.Labels["cloud.cluster-vending.io/naas-ready"] != "false" {
		return fmt.Errorf("developer access/readiness gates must be closed and observed")
	}
	key := strings.TrimPrefix(ns.Name, remote.GetName()+"-")
	if ns.UID == "" || ns.ResourceVersion == "" || ns.Name != remote.GetName()+"-"+key || !slices.Contains(sub.Spec.NamespaceKeys, cloud.SubTenantNamespaceKey(key)) {
		return fmt.Errorf("namespace is not a current, identifiable allocation key")
	}
	if ns.Labels[LabelTenant] != sub.Spec.TenantRef.Name || ns.Labels[LabelSubTenantID] != sub.Name || ns.Labels["capsule.clastix.io/tenant"] != remote.GetName() {
		return fmt.Errorf("namespace tenant/allocation labels do not match")
	}
	if len(ns.OwnerReferences) != 1 || !tenantOwner(ns.OwnerReferences[0], remote) {
		return fmt.Errorf("namespace Capsule Tenant owner UID does not match")
	}
	if len(ns.Finalizers) > 0 {
		return fmt.Errorf("namespace has unresolved metadata finalizers")
	}
	for _, finalizer := range ns.Spec.Finalizers {
		if finalizer != corev1.FinalizerKubernetes {
			return fmt.Errorf("namespace has an unknown lifecycle finalizer")
		}
	}
	return nil
}

func tenantOwner(owner metav1.OwnerReference, remote *unstructured.Unstructured) bool {
	return owner.APIVersion == TenantGVK.GroupVersion().String() && owner.Kind == "Tenant" && owner.Name == remote.GetName() && owner.UID == remote.GetUID()
}

func namespaceContentBlockers(ctx context.Context, c client.Client, remote *unstructured.Unstructured, sub *cloud.SubTenant, ns *corev1.Namespace) ([]string, error) {
	reader, ok := c.(NamespaceContentReader)
	if !ok {
		return nil, fmt.Errorf("complete namespace discovery inventory is unavailable; deletion is blocked")
	}
	objects, err := reader.NamespaceContents(ctx, ns.Name)
	if err != nil {
		return nil, err
	}
	expected, err := namespaceCleanupBaseline(remote, sub, ns)
	if err != nil {
		return nil, err
	}
	var blockers []string
	for _, object := range objects {
		if object.GetNamespace() != ns.Name || object.GetName() == "" || object.GetUID() == "" {
			return nil, fmt.Errorf("namespace inventory has unverifiable object identity")
		}
		if object.GroupVersionKind().Group == "" && object.GetKind() == "ResourceQuota" && namespaceBudgetQuota(ctx, c, &object, remote, sub) {
			continue
		}
		if !cleanupObjectAllowed(&object, expected, remote) {
			blockers = append(blockers, fmt.Sprintf("Namespace %s contains %s %s", ns.Name, object.GroupVersionKind().GroupKind(), object.GetName()))
		}
	}
	return blockers, nil
}

// The native quota projected by Capsule's fixed budget is baseline enforcement,
// not tenant content. Require its exact budget and immutable controller-owner
// chain; manual quotas and projections that have not converged stay blockers.
func namespaceBudgetQuota(ctx context.Context, c client.Client, obj, remote *unstructured.Unstructured, sub *cloud.SubTenant) bool {
	quota := &corev1.ResourceQuota{}
	if runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, quota) != nil || len(quota.Finalizers) > 0 || !quota.DeletionTimestamp.IsZero() || len(quota.OwnerReferences) != 1 {
		return false
	}
	globalName := remote.GetName() + "-budget"
	sum := sha256.Sum256([]byte(globalName))
	if quota.Name != fmt.Sprintf("capsule-global-quota-%x", sum[:10]) || quota.Labels["projectcapsule.dev/global-resource-quota"] != globalName {
		return false
	}
	owner := quota.OwnerReferences[0]
	if owner.APIVersion != TenantGVK.GroupVersion().String() || owner.Kind != "GlobalResourceQuota" || owner.Name != globalName || owner.UID == "" || owner.Controller == nil || !*owner.Controller {
		return false
	}
	global := &unstructured.Unstructured{}
	global.SetAPIVersion(TenantGVK.GroupVersion().String())
	global.SetKind("GlobalResourceQuota")
	if c.Get(ctx, client.ObjectKey{Name: globalName}, global) != nil || global.GetUID() != owner.UID || len(global.GetOwnerReferences()) != 1 || !tenantOwner(global.GetOwnerReferences()[0], remote) {
		return false
	}
	want := corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{}}
	for key, value := range sub.Spec.Quota.Hard {
		q, err := resource.ParseQuantity(value)
		if err != nil {
			return false
		}
		want.Hard[corev1.ResourceName(key)] = q
	}
	if !equality.Semantic.DeepEqual(quota.Spec, want) {
		return false
	}
	raw, ok, err := unstructured.NestedMap(global.Object, "spec", "quota")
	if err != nil || !ok {
		return false
	}
	actual := &corev1.ResourceQuotaSpec{}
	if runtime.DefaultUnstructuredConverter.FromUnstructured(raw, actual) != nil || !equality.Semantic.DeepEqual(*actual, want) {
		return false
	}
	selectors, _, _ := unstructured.NestedSlice(global.Object, "spec", "namespaceSelectors")
	return reflect.DeepEqual(selectors, []any{map[string]any{"matchLabels": map[string]any{"capsule.clastix.io/tenant": remote.GetName()}}})
}

func namespaceCleanupBaseline(remote *unstructured.Unstructured, sub *cloud.SubTenant, ns *corev1.Namespace) (map[string]*unstructured.Unstructured, error) {
	pss, _, err := unstructured.NestedString(remote.Object, "spec", "data", "naas", "pssVersion")
	if err != nil {
		return nil, err
	}
	// Reconstruct the registered keys after withdrawal cleared the remote keys.
	tenant, err := RenderTenant(sub, pss, "cleanup")
	if err != nil {
		return nil, err
	}
	tenant.SetUID(remote.GetUID())
	rawNS, err := runtime.DefaultUnstructuredConverter.ToUnstructured(ns)
	if err != nil {
		return nil, err
	}
	data := map[string]any{"tenant": tenant.Object, "namespace": rawNS}
	b, err := Load()
	if err != nil {
		return nil, err
	}
	result := map[string]*unstructured.Unstructured{}
	for _, gtr := range b.Objects() {
		if gtr.GetKind() != "GlobalTenantResource" || gtr.GetName() == "naas-developer-access" {
			continue
		}
		resources, _, err := unstructured.NestedSlice(gtr.Object, "spec", "resources")
		if err != nil {
			return nil, err
		}
		for _, raw := range resources {
			resource, ok := raw.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("invalid baseline resource")
			}
			generators, _, err := unstructured.NestedSlice(resource, "generators")
			if err != nil {
				return nil, err
			}
			for _, raw := range generators {
				generator, ok := raw.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("invalid baseline generator")
				}
				source, ok := generator["template"].(string)
				if !ok {
					return nil, fmt.Errorf("invalid baseline template")
				}
				parsed, err := template.New("cleanup-baseline").Option("missingkey=error").Parse(source)
				if err != nil {
					return nil, err
				}
				var rendered bytes.Buffer
				if err := parsed.Execute(&rendered, data); err != nil {
					return nil, err
				}
				decoder := yaml.NewYAMLOrJSONDecoder(&rendered, 4096)
				for {
					obj := map[string]any{}
					err := decoder.Decode(&obj)
					if errors.Is(err, io.EOF) {
						break
					}
					if err != nil {
						return nil, err
					}
					if len(obj) == 0 {
						continue
					}
					item := &unstructured.Unstructured{Object: obj}
					if item.GetNamespace() == ns.Name {
						result[cleanupObjectKey(item)] = item
					}
				}
			}
		}
	}
	return result, nil
}

func cleanupObjectKey(obj *unstructured.Unstructured) string {
	return obj.GroupVersionKind().GroupKind().String() + "/" + obj.GetName()
}

func cleanupObjectAllowed(obj *unstructured.Unstructured, expected map[string]*unstructured.Unstructured, remote *unstructured.Unstructured) bool {
	if len(obj.GetFinalizers()) > 0 || !obj.GetDeletionTimestamp().IsZero() {
		return false
	}
	group := obj.GroupVersionKind().Group
	if obj.GetKind() == "Event" && (group == "" || group == "events.k8s.io") {
		return true
	}
	if group == "" && obj.GetKind() == "ConfigMap" && obj.GetName() == "kube-root-ca.crt" {
		return publicRootCertificate(obj)
	}
	if group == "rbac.authorization.k8s.io" && obj.GetKind() == "RoleBinding" {
		return platformOwnerBinding(obj, remote)
	}
	baseline := expected[cleanupObjectKey(obj)]
	if baseline == nil {
		if group == "" && obj.GetKind() == "ServiceAccount" && obj.GetName() == "default" {
			return emptyServiceAccount(obj, false)
		}
		return false
	}
	for key, value := range baseline.GetLabels() {
		if obj.GetLabels()[key] != value {
			return false
		}
	}
	switch obj.GetKind() {
	case "ServiceAccount":
		return emptyServiceAccount(obj, true)
	case "NetworkPolicy":
		current, want := &networkingv1.NetworkPolicy{}, &networkingv1.NetworkPolicy{}
		if runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, current) != nil || runtime.DefaultUnstructuredConverter.FromUnstructured(baseline.Object, want) != nil {
			return false
		}
		if len(want.Spec.PolicyTypes) == 0 {
			want.Spec.PolicyTypes = []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}
			if len(want.Spec.Egress) > 0 {
				want.Spec.PolicyTypes = append(want.Spec.PolicyTypes, networkingv1.PolicyTypeEgress)
			}
		}
		return equality.Semantic.DeepEqual(current.Spec, want.Spec)
	case "LimitRange":
		current, want := &corev1.LimitRange{}, &corev1.LimitRange{}
		return runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, current) == nil && runtime.DefaultUnstructuredConverter.FromUnstructured(baseline.Object, want) == nil && equality.Semantic.DeepEqual(current.Spec, want.Spec)
	default:
		return payloadMatches(obj, baseline) && reflect.DeepEqual(obj.Object["spec"], baseline.Object["spec"])
	}
}

func emptyServiceAccount(obj *unstructured.Unstructured, requireDisabled bool) bool {
	account := &corev1.ServiceAccount{}
	if runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, account) != nil {
		return false
	}
	return len(account.Secrets) == 0 && len(account.ImagePullSecrets) == 0 && (account.AutomountServiceAccountToken == nil && !requireDisabled || account.AutomountServiceAccountToken != nil && !*account.AutomountServiceAccountToken)
}

func publicRootCertificate(obj *unstructured.Unstructured) bool {
	config := &corev1.ConfigMap{}
	if runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, config) != nil || len(config.BinaryData) > 0 || len(config.Data) != 1 {
		return false
	}
	raw := []byte(config.Data["ca.crt"])
	certificates := 0
	for len(bytes.TrimSpace(raw)) > 0 {
		block, rest := pem.Decode(raw)
		if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) > 0 || !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("-----BEGIN CERTIFICATE-----")) {
			return false
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			return false
		}
		certificates++
		raw = rest
	}
	return certificates > 0
}

func platformOwnerBinding(obj *unstructured.Unstructured, remote *unstructured.Unstructured) bool {
	binding := &rbacv1.RoleBinding{}
	if runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, binding) != nil {
		return false
	}
	hash := strings.TrimPrefix(binding.Name, "capsule:managed:")
	if hash == binding.Name || hash == "" || binding.Labels["capsule.clastix.io/role-binding"] != hash || binding.Labels["capsule.clastix.io/managed-by"] != remote.GetName() || len(binding.OwnerReferences) != 1 || !tenantOwner(binding.OwnerReferences[0], remote) {
		return false
	}
	if binding.RoleRef != (rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "naas-workload-editor"}) {
		return false
	}
	return reflect.DeepEqual(binding.Subjects, []rbacv1.Subject{{Kind: "ServiceAccount", Name: "provisioner", Namespace: "naas-system"}})
}
