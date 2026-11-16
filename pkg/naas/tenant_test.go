package naas

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"k8s.io/utils/ptr"

	corev1 "k8s.io/api/core/v1"
	apiextensionsinternal "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/defaulting"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/pruning"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
)

func allocationFixture() *cloud.SubTenant {
	return &cloud.SubTenant{
		ObjectMeta: metav1.ObjectMeta{Name: "00000000-0000-4000-8000-000000000021", Namespace: "tenant-acme", UID: "allocation-uid"},
		Spec: cloud.SubTenantSpec{
			TenantRef:     cloud.SubTenantTenantReference{Name: "acme", UID: "tenant-uid"},
			ClusterRef:    cloud.SubTenantClusterReference{ID: "00000000-0000-4000-8000-000000000011", Name: "parent", UID: "cluster-uid"},
			NamespaceKeys: []cloud.SubTenantNamespaceKey{"app"},
			Quota:         cloud.SubTenantQuota{NamespaceCount: 2, Hard: map[string]string{"requests.cpu": "500m", "requests.memory": "1Gi", "pods": "10"}},
			PolicyProfile: "restricted-ambient-v1",
		},
	}
}

func TestTenantRenderConstrainedIdentityAndObservedMinor(t *testing.T) {
	sub := allocationFixture()
	remote, err := RenderTenant(sub, "v1.35.2", "revision-a")
	if err != nil {
		t.Fatal(err)
	}
	if remote.GetName() != "st-00000000000040008000000000000021" || remote.GetNamespace() != "" || remote.GroupVersionKind() != TenantGVK {
		t.Fatal("unexpected remote identity", remote)
	}
	minor, _, _ := unstructured.NestedString(remote.Object, "spec", "data", "naas", "pssVersion")
	group, _, _ := unstructured.NestedString(remote.Object, "spec", "data", "naas", "developerGroup")
	if minor != "v1.35" || group != "" || remote.GetLabels()[LabelAccessReady] != "false" {
		t.Fatal("render must pin observed PSS and keep access closed")
	}
	owners, _, _ := unstructured.NestedSlice(remote.Object, "spec", "owners")
	want := []any{map[string]any{"kind": "ServiceAccount", "name": "system:serviceaccount:naas-system:provisioner", "clusterRoles": []any{"naas-workload-editor"}}}
	if !reflect.DeepEqual(owners, want) {
		t.Fatal("render widened Capsule owners", owners)
	}
	remote.SetUID("remote-uid")
	if !OwnTenant(remote, sub) {
		t.Fatal("matching immutable ownership not recognized")
	}
	for _, label := range []string{LabelSubTenantID, LabelSubTenantUID, LabelTenant, LabelTenantUID, LabelClusterUID, "cloud.cluster-vending.io/cluster-id"} {
		foreign := remote.DeepCopy()
		labels := foreign.GetLabels()
		labels[label] = "foreign"
		foreign.SetLabels(labels)
		if OwnTenant(foreign, sub) {
			t.Fatalf("accepted foreign %s", label)
		}
	}
	sub.Status.RemoteCapsuleTenantUID = "previous-uid"
	if OwnTenant(remote, sub) {
		t.Fatal("accepted replacement remote UID")
	}
}

func TestTenantRenderRejectsPrivilegeAndQuotaEscalation(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*cloud.SubTenant)
	}{
		{"identity", func(s *cloud.SubTenant) {
			s.Spec.Access = &cloud.SubTenantAccess{Bindings: []cloud.SubTenantAccessBinding{{IdentityRef: "arbitrary-admin", RoleProfile: "workload-editor"}}}
		}},
		{"profile", func(s *cloud.SubTenant) { s.Spec.PolicyProfile = "privileged" }},
		{"negative", func(s *cloud.SubTenant) { s.Spec.Quota.Hard["pods"] = "-1" }},
		{"fractional-count", func(s *cloud.SubTenant) { s.Spec.Quota.Hard["pods"] = "500m" }},
		{"unknown-resource", func(s *cloud.SubTenant) { s.Spec.Quota.Hard["count/clusterroles.rbac.authorization.k8s.io"] = "5" }},
		{"invalid-quantity", func(s *cloud.SubTenant) { s.Spec.Quota.Hard["requests.cpu"] = "unbounded" }},
		{"duplicate-key", func(s *cloud.SubTenant) { s.Spec.NamespaceKeys = []cloud.SubTenantNamespaceKey{"app", "app"} }},
		{"namespace-budget", func(s *cloud.SubTenant) { s.Spec.Quota.NamespaceCount = 0 }},
		{"long-key", func(s *cloud.SubTenant) {
			s.Spec.NamespaceKeys = []cloud.SubTenantNamespaceKey{"abcdefghijklmnopqrstuvwxyzab"}
		}},
		{"uid", func(s *cloud.SubTenant) { s.Spec.ClusterRef.UID = "" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			sub := allocationFixture()
			test.change(sub)
			if _, err := RenderTenant(sub, "1.35", "revision"); err == nil {
				t.Fatal("accepted invalid allocation")
			}
		})
	}
	for _, version := range []string{"", "unknown", "2.1", "1.0"} {
		if _, err := RenderTenant(allocationFixture(), version, "revision"); err == nil {
			t.Fatalf("accepted unobserved Kubernetes version %q", version)
		}
	}
}

func TestTenantMergePreservesServerIdentityAndClosesAccess(t *testing.T) {
	desired, err := RenderTenant(allocationFixture(), "1.35", "revision")
	if err != nil {
		t.Fatal(err)
	}
	current := desired.DeepCopy()
	current.SetUID("remote-uid")
	current.SetResourceVersion("17")
	current.SetAnnotations(map[string]string{"unrelated": "retained"})
	current.Object["status"] = map[string]any{"observedGeneration": int64(3)}
	_ = unstructured.SetNestedSlice(current.Object, []any{map[string]any{"kind": "Group", "name": "system:masters"}}, "spec", "owners")
	merged := MergeTenant(current, desired)
	if merged.GetUID() != current.GetUID() || merged.GetResourceVersion() != "17" || merged.GetAnnotations()["unrelated"] != "retained" || !reflect.DeepEqual(merged.Object["status"], current.Object["status"]) || !EqualTenantSpec(merged, desired) {
		t.Fatal("merge did not retain concurrency identity or constrain spec")
	}
	if EqualTenantSpec(current, desired) {
		t.Fatal("merge mutated source")
	}
}

func TestTenantNamespaceInventoryRetainsSuspiciousChildren(t *testing.T) {
	for _, name := range []string{"", "payments"} {
		t.Run("name="+name, func(t *testing.T) {
			sub := allocationFixture()
			sub.Spec.Name = name
			remote, err := RenderTenant(sub, "1.35", "revision")
			if err != nil {
				t.Fatal(err)
			}
			remote.SetUID("remote-uid")
			owned := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
				Name: remote.GetName() + "-app", UID: "namespace-uid",
				Labels:          map[string]string{LabelTenant: "acme", LabelSubTenantID: sub.Name, "capsule.clastix.io/tenant": remote.GetName()},
				OwnerReferences: []metav1.OwnerReference{{APIVersion: TenantGVK.GroupVersion().String(), Kind: "Tenant", Name: remote.GetName(), UID: remote.GetUID()}},
			}}
			// A stripped label or owner must not hide data from the require-empty scan.
			suspicious := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: remote.GetName() + "-untracked", UID: "suspicious-uid"}}
			unrelated := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "other-tenant-app", UID: "other-uid"}}
			scheme := runtime.NewScheme()
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(owned, suspicious, unrelated).Build()
			inventory, blockers, err := ObserveTenantNamespaces(t.Context(), c, remote, sub, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(inventory) != 2 || len(blockers) != 2 {
				t.Fatal("lost or leaked namespace inventory", inventory, blockers)
			}
			if inventory[0].Name != owned.Name || inventory[1].Name != suspicious.Name {
				t.Fatal(inventory)
			}
			for _, ns := range inventory {
				if meta.IsStatusConditionTrue(ns.Conditions, "BaselineReady") || ns.TemplateRevision != "" {
					t.Fatal("namespace presence incorrectly qualified a baseline", ns)
				}
			}
			if inventory[1].Conditions[0].Reason != "OwnershipMismatch" {
				t.Fatal(inventory[1])
			}
		})
	}
}

func TestTenantProtectedBootstrapDefaultsAndRBACDrift(t *testing.T) {
	bootstrap := mustLoad(t)
	for _, expected := range bootstrap.Objects() {
		current := expected.DeepCopy()
		if expected.GetKind() == "GlobalTenantResource" {
			current = gtrDefaults(current)
			if !payloadMatches(current, expected) {
				t.Fatal("pinned API defaults broke observation")
			}
			_ = unstructured.SetNestedField(current.Object, true, "spec", "cordoned")
			if payloadMatches(current, expected) {
				t.Fatal("cordoned template accepted as ready")
			}
		}
		if expected.GetKind() == "ClusterRole" {
			current.Object["aggregationRule"] = map[string]any{"clusterRoleSelectors": []any{map[string]any{"matchLabels": map[string]any{"wide": "true"}}}}
			if payloadMatches(current, expected) {
				t.Fatal("unexpected privilege aggregation accepted")
			}
		}
	}
}

// The optional pinned chart is read locally, never fetched. Unknown-field
// pruning and defaulting exercise the real Capsule contract, not a mock schema.
func TestTenantLocalPinnedSchema(t *testing.T) {
	chart := pinnedChart(t)
	raw, err := os.ReadFile(filepath.Join(chart, "crds", "capsule.clastix.io_tenants.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatal(err)
	}
	var schema *apiextensionsv1.JSONSchemaProps
	for _, version := range crd.Spec.Versions {
		if version.Name == "v1beta2" {
			schema = version.Schema.OpenAPIV3Schema
		}
	}
	if schema == nil {
		t.Fatal("pinned Tenant schema missing")
	}
	internal := &apiextensionsinternal.JSONSchemaProps{}
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(schema, internal, nil); err != nil {
		t.Fatal(err)
	}
	validator, _, err := validation.NewSchemaValidator(internal)
	if err != nil {
		t.Fatal(err)
	}
	structural, err := structuralschema.NewStructural(internal)
	if err != nil {
		t.Fatal(err)
	}
	remote, err := RenderTenant(allocationFixture(), "1.35", "revision")
	if err != nil {
		t.Fatal(err)
	}
	unknown := pruning.PruneWithOptions(remote.Object, structural, true, structuralschema.UnknownFieldPathOptions{TrackUnknownFieldPaths: true})
	if len(unknown) != 0 {
		t.Fatalf("Capsule would prune fields: %v", unknown)
	}
	if errs := validation.ValidateCustomResource(nil, remote.Object, validator); len(errs) != 0 {
		t.Fatal(errs.ToAggregate())
	}
	celValidator := cel.NewValidator(structural, true, celconfig.PerCallLimit)
	if errs, _ := celValidator.Validate(t.Context(), nil, structural, remote.Object, nil, celconfig.RuntimeCELCostBudget); len(errs) != 0 {
		t.Fatal(errs.ToAggregate())
	}
	defaulted := remote.DeepCopy()
	defaulting.Default(defaulted.Object, structural)
	if !EqualTenantSpec(remote, defaulted) {
		t.Fatalf("server defaults would cause repeated reconciliation: %#v", defaulted.Object["spec"])
	}
}

func TestTenantRemoteNameAndProtectionBinding(t *testing.T) {
	for _, test := range []struct {
		name, recorded string
		valid          bool
	}{
		{"payments", "", true}, {"payments", "payments", true}, {"payments", "other", false},
		{"Invalid_Name", "", false}, {"abcdefghijklmnopqrstuvwxyzabcdefghij", "", false},
		{"", "st-00000000000040008000000000000021", true}, {"", "arbitrary-legacy-name", false},
	} {
		t.Run(test.name+"/"+test.recorded, func(t *testing.T) {
			sub := allocationFixture()
			sub.Spec.Name = test.name
			sub.Status.RemoteCapsuleTenant = test.recorded
			name, err := RemoteTenantName(sub)
			if (err == nil) != test.valid {
				t.Fatal(name, err)
			}
			if test.valid && test.name != "" && name != test.name {
				t.Fatal("declared name not honored")
			}
		})
	}
	for _, value := range []*bool{nil, ptr.To(true), ptr.To(false)} {
		sub := allocationFixture()
		sub.Spec.Name = "payments"
		sub.Spec.DeletionProtection = value
		remote, err := RenderTenant(sub, "1.35", "revision")
		if err != nil {
			t.Fatal(err)
		}
		got, _, _ := unstructured.NestedBool(remote.Object, "spec", "preventDeletion")
		if got != (value == nil || *value) {
			t.Fatal("deletion protection not rendered", got)
		}
		remote.SetUID("remote-uid")
		if !OwnTenant(remote, sub) {
			t.Fatal("named Tenant ownership rejected")
		}
	}
}
