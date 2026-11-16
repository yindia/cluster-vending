package naas

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"math/big"
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
)

type cleanupClient struct {
	client.Client
	contents  map[string][]unstructured.Unstructured
	err       error
	afterScan func()
}

func (c *cleanupClient) NamespaceContents(_ context.Context, namespace string) ([]unstructured.Unstructured, error) {
	if c.afterScan != nil {
		c.afterScan()
	}
	return c.contents[namespace], c.err
}
func cleanupFixture(t *testing.T) (*cleanupClient, *unstructured.Unstructured, *corev1.Namespace) {
	t.Helper()
	sub := allocationFixture()
	sub.Spec.Name = "payments"
	remote, err := RenderTenant(sub, "1.35", "revision")
	if err != nil {
		t.Fatal(err)
	}
	remote.SetUID("remote-uid")
	_ = unstructured.SetNestedField(remote.Object, true, "spec", "cordoned")
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "payments-app", UID: "namespace-uid", Labels: map[string]string{LabelTenant: "acme", LabelSubTenantID: sub.Name, "capsule.clastix.io/tenant": "payments", "cloud.cluster-vending.io/naas-ready": "false", "projectcapsule.dev/cordoned": "true"}, OwnerReferences: []metav1.OwnerReference{{APIVersion: TenantGVK.GroupVersion().String(), Kind: "Tenant", Name: "payments", UID: remote.GetUID()}}}, Spec: corev1.NamespaceSpec{Finalizers: []corev1.FinalizerName{corev1.FinalizerKubernetes}}}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	scheme.AddKnownTypeWithName(TenantGVK, &unstructured.Unstructured{})
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(remote, ns).Build()
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(ns), ns); err != nil {
		t.Fatal(err)
	}
	return &cleanupClient{Client: c, contents: map[string][]unstructured.Unstructured{}}, remote, ns
}
func cleanupAllocation() *cloud.SubTenant {
	sub := allocationFixture()
	sub.Spec.Name = "payments"
	sub.Spec.DeletionProtection = ptr.To(false)
	return sub
}

func TestNamespaceCleanupRejectsAllUserContent(t *testing.T) {
	for _, kind := range []struct{ api, kind, name string }{
		{"v1", "Pod", "app"}, {"apps/v1", "Deployment", "app"}, {"v1", "PersistentVolumeClaim", "data"},
		{"v1", "Secret", "token"}, {"v1", "ConfigMap", "settings"}, {"example.test/v1", "Database", "primary"},
		{"rbac.authorization.k8s.io/v1", "RoleBinding", "developer"}, {"v1", "ServiceAccount", "custom"},
	} {
		t.Run(kind.kind, func(t *testing.T) {
			c, remote, ns := cleanupFixture(t)
			c.contents[ns.Name] = []unstructured.Unstructured{{Object: map[string]any{"apiVersion": kind.api, "kind": kind.kind, "metadata": map[string]any{"name": kind.name, "namespace": ns.Name, "uid": "content-uid"}}}}
			_, blockers, err := PreflightNamespaceCleanup(t.Context(), c, remote, cleanupAllocation())
			if err != nil || len(blockers) != 1 {
				t.Fatal("user content did not block", blockers, err)
			}
			if err := DeleteEmptyNamespace(t.Context(), c, remote, cleanupAllocation(), ns); err == nil {
				t.Fatal("deleted namespace containing user content")
			}
			if err := c.Get(t.Context(), client.ObjectKeyFromObject(ns), &corev1.Namespace{}); err != nil {
				t.Fatal("namespace was deleted", err)
			}
		})
	}
}

func TestNamespaceCleanupDeletesOnlyReviewedEmptyNamespace(t *testing.T) {
	c, remote, ns := cleanupFixture(t)
	unrelated := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "someone-else", UID: "other-uid"}}
	if err := c.Create(t.Context(), unrelated); err != nil {
		t.Fatal(err)
	}
	deletes := 0
	c.Client = interceptor.NewClient(c.Client.(client.WithWatch), interceptor.Funcs{Delete: func(ctx context.Context, base client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
		if _, ok := obj.(*corev1.Namespace); !ok {
			t.Fatal("unexpected child delete")
		}
		options := &client.DeleteOptions{}
		for _, opt := range opts {
			opt.ApplyToDelete(options)
		}
		if options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID != ns.UID || options.Preconditions.ResourceVersion == nil || *options.Preconditions.ResourceVersion != ns.ResourceVersion {
			t.Fatal("missing identity/concurrency preconditions")
		}
		deletes++
		return base.Delete(ctx, obj, opts...)
	}})
	namespaces, blockers, err := PreflightNamespaceCleanup(t.Context(), c, remote, cleanupAllocation())
	if err != nil || len(namespaces) != 1 || len(blockers) > 0 {
		t.Fatal(namespaces, blockers, err)
	}
	if err := DeleteEmptyNamespace(t.Context(), c, remote, cleanupAllocation(), &namespaces[0]); err != nil {
		t.Fatal(err)
	}
	if deletes != 1 {
		t.Fatal(deletes)
	}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(ns), &corev1.Namespace{}); !apierrors.IsNotFound(err) {
		t.Fatal("namespace did not delete", err)
	}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(unrelated), &corev1.Namespace{}); err != nil {
		t.Fatal("unrelated namespace changed", err)
	}
}

func TestNamespaceCleanupFailsClosedOnIdentityDriftAndInventoryErrors(t *testing.T) {
	for _, test := range []string{"inventory-error", "namespace-rv", "foreign-owner", "readiness-open", "namespace-uncordoned", "tenant-uncordoned", "finalizer", "new-content", "during-scan"} {
		t.Run(test, func(t *testing.T) {
			c, remote, observed := cleanupFixture(t)
			current := observed.DeepCopy()
			switch test {
			case "inventory-error":
				c.err = fmt.Errorf("discovery denied")
			case "namespace-rv":
				current.Labels["changed"] = "true"
			case "foreign-owner":
				current.OwnerReferences[0].UID = "foreign"
			case "readiness-open":
				current.Labels["cloud.cluster-vending.io/naas-ready"] = "true"
			case "namespace-uncordoned":
				delete(current.Labels, "projectcapsule.dev/cordoned")
			case "tenant-uncordoned":
				_ = unstructured.SetNestedField(remote.Object, false, "spec", "cordoned")
				if err := c.Update(t.Context(), remote); err != nil {
					t.Fatal(err)
				}
			case "finalizer":
				current.Finalizers = []string{"example.test/retain"}
			case "new-content":
				c.contents[current.Name] = []unstructured.Unstructured{{Object: map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{"name": "late", "namespace": current.Name, "uid": "late-uid"}}}}
			case "during-scan":
				c.afterScan = func() {
					fresh := &corev1.Namespace{}
					if err := c.Get(t.Context(), client.ObjectKeyFromObject(current), fresh); err != nil {
						t.Fatal(err)
					}
					fresh.Labels["changed"] = "true"
					if err := c.Update(t.Context(), fresh); err != nil {
						t.Fatal(err)
					}
				}
			}
			if test == "namespace-rv" || test == "foreign-owner" || test == "readiness-open" || test == "namespace-uncordoned" || test == "finalizer" {
				if err := c.Update(t.Context(), current); err != nil {
					t.Fatal(err)
				}
			}
			if err := DeleteEmptyNamespace(t.Context(), c, remote, cleanupAllocation(), observed); err == nil {
				t.Fatal("unsafe namespace deletion succeeded")
			}
		})
	}
}

func TestNamespaceCleanupBaselineRequiresExactPayload(t *testing.T) {
	c, remote, ns := cleanupFixture(t)
	expected, err := namespaceCleanupBaseline(remote, cleanupAllocation(), ns)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range expected {
		item = item.DeepCopy()
		item.SetUID("baseline-uid")
		if policyTypes, _, _ := unstructured.NestedSlice(item.Object, "spec", "policyTypes"); item.GetKind() == "NetworkPolicy" && len(policyTypes) == 0 {
			policies := []any{"Ingress"}
			if egress, _, _ := unstructured.NestedSlice(item.Object, "spec", "egress"); len(egress) > 0 {
				policies = append(policies, "Egress")
			}
			_ = unstructured.SetNestedSlice(item.Object, policies, "spec", "policyTypes")
		}
		if !cleanupObjectAllowed(item, expected, remote) {
			t.Fatalf("rejected protected baseline %s/%s", item.GetKind(), item.GetName())
		}
		if item.GetKind() == "ServiceAccount" {
			item.Object["secrets"] = []any{map[string]any{"name": "private-token"}}
		} else {
			item.Object["spec"] = map[string]any{"arbitrary": "changed"}
		}
		if cleanupObjectAllowed(item, expected, remote) {
			t.Fatalf("accepted changed baseline %s", item.GetName())
		}
	}
	c.contents[ns.Name] = []unstructured.Unstructured{{Object: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "kube-root-ca.crt", "namespace": ns.Name, "uid": "cert-uid"}, "data": map[string]any{"ca.crt": "not a certificate", "private": "secret"}}}}
	_, blockers, err := PreflightNamespaceCleanup(t.Context(), c, remote, cleanupAllocation())
	if err != nil || len(blockers) != 1 {
		t.Fatal("unvalidated certificate data allowed", blockers, err)
	}
}

func TestNamespaceCleanupOnlyAllowsBoundCapsuleBudget(t *testing.T) {
	c, remote, ns := cleanupFixture(t)
	gvk := TenantGVK.GroupVersion().WithKind("GlobalResourceQuota")
	c.Scheme().AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
	name := remote.GetName() + "-budget"
	hard := map[string]any{}
	for key, value := range cleanupAllocation().Spec.Quota.Hard {
		hard[key] = value
	}
	global := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"quota": map[string]any{"hard": hard}, "namespaceSelectors": []any{map[string]any{"matchLabels": map[string]any{"capsule.clastix.io/tenant": remote.GetName()}}}}}}
	global.SetGroupVersionKind(gvk)
	global.SetName(name)
	global.SetUID("budget-uid")
	global.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: TenantGVK.GroupVersion().String(), Kind: "Tenant", Name: remote.GetName(), UID: remote.GetUID(), Controller: ptr.To(true)}})
	if err := c.Create(t.Context(), global); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(name))
	quota := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "ResourceQuota", "spec": map[string]any{"hard": hard}}}
	quota.SetName(fmt.Sprintf("capsule-global-quota-%x", sum[:10]))
	quota.SetNamespace(ns.Name)
	quota.SetUID("native-quota-uid")
	quota.SetLabels(map[string]string{"projectcapsule.dev/global-resource-quota": name})
	quota.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: gvk.GroupVersion().String(), Kind: gvk.Kind, Name: name, UID: global.GetUID(), Controller: ptr.To(true)}})
	if !namespaceBudgetQuota(t.Context(), c, quota, remote, cleanupAllocation()) {
		t.Fatal("exact owned budget quota rejected")
	}
	pending := cleanupAllocation()
	pending.Spec.Quota.Hard["requests.cpu"] = "4"
	pending.Spec.Quota.Hard["pods"] = "50"
	if !namespaceBudgetQuota(t.Context(), c, quota, remote, pending) {
		t.Fatal("unapplied management quota update blocked the last applied owned budget")
	}
	withdrawn := WithdrawTenantGeneration(remote)
	if !namespaceBudgetQuota(t.Context(), c, quota, withdrawn, pending) {
		t.Fatal("withdrawal lost the last applied owned budget")
	}

	wrong := quota.DeepCopy()
	_ = unstructured.SetNestedField(wrong.Object, "999", "spec", "hard", "pods")
	if namespaceBudgetQuota(t.Context(), c, wrong, remote, cleanupAllocation()) {
		t.Fatal("modified quota accepted")
	}
	wrong = quota.DeepCopy()
	owners := wrong.GetOwnerReferences()
	owners[0].UID = "foreign"
	wrong.SetOwnerReferences(owners)
	if namespaceBudgetQuota(t.Context(), c, wrong, remote, cleanupAllocation()) {
		t.Fatal("foreign budget adopted")
	}
}

func TestNamespaceCleanupCertificateAndPlatformBindingAreNarrow(t *testing.T) {
	_, remote, ns := cleanupFixture(t)
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), IsCA: true, BasicConstraintsValid: true, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	certificate := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	config := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "data": map[string]any{"ca.crt": certificate}}}
	config.SetName("kube-root-ca.crt")
	config.SetNamespace(ns.Name)
	if !publicRootCertificate(config) {
		t.Fatal("valid public certificate rejected")
	}
	_ = unstructured.SetNestedField(config.Object, certificate+"\nprivate payload", "data", "ca.crt")
	if publicRootCertificate(config) {
		t.Fatal("non-certificate payload accepted")
	}
	binding := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding", "roleRef": map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": "naas-workload-editor"}, "subjects": []any{map[string]any{"kind": "ServiceAccount", "name": "provisioner", "namespace": "naas-system"}}}}
	binding.SetName("capsule:managed:hash")
	binding.SetLabels(map[string]string{"capsule.clastix.io/role-binding": "hash", "capsule.clastix.io/managed-by": remote.GetName()})
	binding.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: TenantGVK.GroupVersion().String(), Kind: "Tenant", Name: remote.GetName(), UID: remote.GetUID(), Controller: ptr.To(true)}})
	if !platformOwnerBinding(binding, remote) {
		t.Fatal("fixed platform owner binding rejected")
	}
	_ = unstructured.SetNestedField(binding.Object, "cluster-admin", "roleRef", "name")
	if platformOwnerBinding(binding, remote) {
		t.Fatal("arbitrary RBAC allowed")
	}
}

func TestNamespaceCleanupExcludesOnlyVerifiedPrefixNeighbor(t *testing.T) {
	for _, test := range []string{"verified", "stripped-label", "foreign-uid-mismatch", "missing-foreign-tenant", "current-label-conflict", "current-recorded-namespace", "historical-recorded-unrequested-key"} {
		t.Run(test, func(t *testing.T) {
			c, remote, ns := cleanupFixture(t)
			sub := cleanupAllocation()
			sibling := allocationFixture()
			sibling.Name = "00000000-0000-4000-8000-000000000022"
			sibling.UID = "sibling-subtenant-uid"
			sibling.Spec.Name = "payments-team"
			foreign, err := RenderTenant(sibling, "1.35", "revision")
			if err != nil {
				t.Fatal(err)
			}
			foreign.SetUID("foreign-remote-uid")
			if test != "missing-foreign-tenant" {
				if err := c.Create(t.Context(), foreign); err != nil {
					t.Fatal(err)
				}
			}
			neighbor := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "payments-team-app", UID: "neighbor-ns-uid", Labels: map[string]string{LabelTenant: sibling.Spec.TenantRef.Name, LabelSubTenantID: sibling.Name, "capsule.clastix.io/tenant": foreign.GetName(), "cloud.cluster-vending.io/naas-ready": "true"}, OwnerReferences: []metav1.OwnerReference{{APIVersion: TenantGVK.GroupVersion().String(), Kind: "Tenant", Name: foreign.GetName(), UID: foreign.GetUID()}}}}
			switch test {
			case "stripped-label":
				delete(neighbor.Labels, LabelSubTenantID)
			case "foreign-uid-mismatch":
				neighbor.OwnerReferences[0].UID = "different-uid"
			case "current-label-conflict":
				neighbor.Labels[LabelSubTenantID] = sub.Name
			case "current-recorded-namespace":
				sub.Status.Namespaces = []cloud.SubTenantNamespaceStatus{{Key: "app", Name: neighbor.Name, UID: string(neighbor.UID)}}
			case "historical-recorded-unrequested-key":
				sub.Status.Namespaces = []cloud.SubTenantNamespaceStatus{{Key: "removed-key", Name: neighbor.Name, UID: "historical-uid"}}
			}
			if err := c.Create(t.Context(), neighbor); err != nil {
				t.Fatal(err)
			}
			namespaces, blockers, err := PreflightNamespaceCleanup(t.Context(), c, remote, sub)
			if err != nil {
				t.Fatal(err)
			}
			if test == "verified" {
				if len(namespaces) != 1 || namespaces[0].Name != ns.Name || len(blockers) != 0 {
					t.Fatal("valid prefix neighbor blocked cleanup", namespaces, blockers)
				}
			} else if len(namespaces) != 2 || len(blockers) == 0 {
				t.Fatal("suspicious ownership was silently excluded", namespaces, blockers)
			}
		})
	}
}

func TestNamespaceCleanupUsesAppliedBaselineDespitePendingKeys(t *testing.T) {
	_, remote, ns := cleanupFixture(t)
	before, err := namespaceCleanupBaseline(remote, cleanupAllocation(), ns)
	if err != nil {
		t.Fatal(err)
	}
	withdrawn := WithdrawTenantGeneration(remote)
	pending := cleanupAllocation()
	pending.Spec.NamespaceKeys = append(pending.Spec.NamespaceKeys, "worker")
	pending.Spec.Quota.Hard["requests.cpu"] = "4"
	after, err := namespaceCleanupBaseline(withdrawn, pending, ns)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("unapplied desired config changed the deletion baseline")
	}
}
