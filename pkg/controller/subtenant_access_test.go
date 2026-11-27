package controller

import (
	"bytes"
	"errors"
	"io"
	"testing"
	"text/template"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/config"
	"github.com/yindia/cluster-vending/pkg/crossplane"
	"github.com/yindia/cluster-vending/pkg/naas"
)

// enforcingVPCCNI is the parent cluster's vpc-cni add-on, Ready and
// Synced, with NetworkPolicy enforcement as EKS reports it.
func enforcingVPCCNI(t *testing.T, w *stWorld) *awseks.Addon {
	t.Helper()
	values := `{"enableNetworkPolicy":"true","nodeAgent":{"enabled":true}}`
	addon := &awseks.Addon{ObjectMeta: metav1.ObjectMeta{Name: "prod-addon-vpc-cni", Namespace: w.parent(t).Namespace, Labels: crossplane.ClusterLabels("acme", stClusterID)},
		Spec:   awseks.AddonSpec{ForProvider: awseks.AddonParameters{AddonName: ptr.To("vpc-cni"), ConfigurationValues: ptr.To(values)}},
		Status: awseks.AddonStatus{AtProvider: awseks.AddonObservation{AddonName: ptr.To("vpc-cni"), ConfigurationValues: ptr.To(values)}}}
	addon.SetConditions(xpv2.Available(), xpv2.ReconcileSuccess())
	return addon
}

func provisionAccessWorld(t *testing.T, configured bool) *stWorld {
	t.Helper()
	return provisionAccessWorldWith(t, configured, func(a *awseks.Addon) *awseks.Addon { return a })
}

// provisionAccessWorldWith is provisionAccessWorld with the parent's
// vpc-cni add-on as vpcCNI makes it (nil: none).
func provisionAccessWorldWith(t *testing.T, configured bool, vpcCNI func(*awseks.Addon) *awseks.Addon) *stWorld {
	t.Helper()
	w := newSubTenantWorld(t)
	if addon := vpcCNI(enforcingVPCCNI(t, w)); addon != nil {
		if err := w.management.Create(t.Context(), addon); err != nil {
			t.Fatal(err)
		}
	}
	w.deps.NaaS = config.NaaS{AllowedGroups: []string{"Admins", "Read*"}}
	parent := w.parent(t)
	parent.Spec.Region = "us-east-1"
	parent.Spec.AWS = &cloud.AWSClusterSpec{}
	if configured {
		parent.Spec.Access = &cloud.AccessSpec{OIDC: &cloud.OIDCIdentityProvider{Name: "login", IssuerURL: "https://login.example.test", ClientID: "kubernetes", GroupsClaim: "groups", GroupsPrefix: "oidc:"}}
	}
	if err := w.management.Update(t.Context(), parent); err != nil {
		t.Fatal(err)
	}
	parent.Status.AWS = &cloud.AWSClusterStatus{ClusterARN: "arn:aws:eks:us-east-1:111122223333:cluster/prod"}
	if err := w.management.Status().Update(t.Context(), parent); err != nil {
		t.Fatal(err)
	}
	sub := w.sub(t)
	sub.Spec.Access = &cloud.SubTenantAccess{AdminGroups: []string{"Admins"}, ViewerGroups: []string{"Readers"}}
	if err := w.management.Update(t.Context(), sub); err != nil {
		t.Fatal(err)
	}
	if configured {
		idp := &awseks.IdentityProviderConfig{ObjectMeta: metav1.ObjectMeta{Name: "access-idp", Namespace: parent.Namespace, UID: "association-uid", Labels: crossplane.ClusterLabels("acme", stClusterID)},
			Spec:   awseks.IdentityProviderConfigSpec{ForProvider: awseks.IdentityProviderConfigParameters{Region: ptr.To("us-east-1"), ClusterName: ptr.To("prod"), Oidc: &awseks.IdentityProviderConfigOidcParameters{IssuerURL: ptr.To("https://login.example.test"), ClientID: ptr.To("kubernetes"), GroupsClaim: ptr.To("groups"), GroupsPrefix: ptr.To("oidc:")}}},
			Status: awseks.IdentityProviderConfigStatus{AtProvider: awseks.IdentityProviderConfigObservation{Status: ptr.To("ACTIVE"), ClusterName: ptr.To("prod"), Arn: ptr.To("arn:aws:eks:us-east-1:111122223333:identityproviderconfig/prod/oidc/login/id"), Oidc: &awseks.IdentityProviderConfigOidcObservation{IssuerURL: ptr.To("https://login.example.test"), ClientID: ptr.To("kubernetes"), GroupsClaim: ptr.To("groups"), GroupsPrefix: ptr.To("oidc:")}}}}
		idp.SetConditions(xpv2.Available(), xpv2.ReconcileSuccess())
		if err := w.management.Create(t.Context(), idp); err != nil {
			t.Fatal(err)
		}
	}
	w.provision(t)
	remote := w.tenant(t)
	remote.Object["status"] = map[string]any{"observedGeneration": remote.GetGeneration(), "conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}
	if err := w.remote.Update(t.Context(), remote); err != nil {
		t.Fatal(err)
	}
	bootstrap, err := naas.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, gtr := range bootstrap.Objects() {
		if gtr.GetKind() != "GlobalTenantResource" || gtr.GetName() == "naas-developer-access" {
			continue
		}
		context := map[string]any{"tenant": remote.Object}
		if gtr.GetName() == "naas-isolation-baseline" {
			ns := &unstructured.Unstructured{}
			ns.SetAPIVersion("v1")
			ns.SetKind("Namespace")
			if err := w.remote.Get(t.Context(), client.ObjectKey{Name: remote.GetName() + "-app"}, ns); err != nil {
				t.Fatal(err)
			}
			context["namespace"] = ns.Object
		}
		resources, _, _ := unstructured.NestedSlice(gtr.Object, "spec", "resources")
		for _, raw := range resources {
			for _, rawGenerator := range raw.(map[string]any)["generators"].([]any) {
				generator := rawGenerator.(map[string]any)
				tpl, err := template.New("baseline").Option("missingkey=error").Parse(generator["template"].(string))
				if err != nil {
					t.Fatal(err)
				}
				var rendered bytes.Buffer
				if err := tpl.Execute(&rendered, context); err != nil {
					t.Fatal(err)
				}
				decoder := yaml.NewYAMLOrJSONDecoder(&rendered, 4096)
				for {
					var object map[string]any
					if err := decoder.Decode(&object); errors.Is(err, io.EOF) {
						break
					} else if err != nil {
						t.Fatal(err)
					}
					if len(object) == 0 {
						continue
					}
					obj := &unstructured.Unstructured{Object: object}
					if obj.GetKind() == "Namespace" {
						obj.SetUID("namespace-uid")
					}
					if err := w.remote.Create(t.Context(), obj); err != nil {
						t.Fatal(err)
					}
				}
			}
		}
	}
	protected := &corev1.Namespace{}
	if err := w.remote.Get(t.Context(), client.ObjectKey{Name: "naas-system"}, protected); err != nil {
		t.Fatal(err)
	}
	protected.UID = "platform-namespace-uid"
	if err := w.remote.Update(t.Context(), protected); err != nil {
		t.Fatal(err)
	}
	w.pass(t)
	return w
}

func materializeAccessBindings(t *testing.T, w *stWorld) {
	t.Helper()
	remote := w.tenant(t)
	namespace := &corev1.Namespace{}
	if err := w.remote.Get(t.Context(), client.ObjectKey{Name: remote.GetName() + "-app"}, namespace); err != nil {
		t.Fatal(err)
	}
	namespace.Labels["cloud.cluster-vending.io/naas-ready"] = "true"
	if err := w.remote.Update(t.Context(), namespace); err != nil {
		t.Fatal(err)
	}
	bindings, _, _ := unstructured.NestedSlice(remote.Object, "spec", "data", "naas", "accessBindings")
	for _, item := range bindings {
		binding := item.(map[string]any)
		role := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "naas-access-" + binding["id"].(string), Namespace: namespace.Name, Labels: map[string]string{naas.LabelSubTenantID: stID}}, RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: binding["role"].(string)}, Subjects: []rbacv1.Subject{{Kind: "Group", APIGroup: rbacv1.GroupName, Name: binding["group"].(string)}}}
		if err := w.remote.Create(t.Context(), role); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSubTenantAccessProvisioningIndependentOfOIDC(t *testing.T) {
	for _, configured := range []bool{false, true} {
		w := provisionAccessWorld(t, configured)
		sub := w.sub(t)
		// Provisioned (state Ready) while access waits; the Ready condition
		// also needs access, because groups are named (PROP-05 6.9).
		if sub.Status.State != cloud.SubTenantStateReady || !meta.IsStatusConditionTrue(sub.Status.Conditions, "ProvisioningReady") || meta.IsStatusConditionTrue(sub.Status.Conditions, "Ready") || meta.IsStatusConditionTrue(sub.Status.Conditions, "AccessReady") {
			t.Fatalf("provisioning incorrectly blocked by human access: %+v", sub.Status)
		}
		condition := meta.FindStatusCondition(sub.Status.Conditions, "AccessReady")
		want, gate := "AccessBindingsPending", "true"
		if !configured {
			want, gate = "OIDCPending", "false"
		}
		if condition.Reason != want || w.tenant(t).GetLabels()[naas.LabelAccessReady] != gate {
			t.Fatalf("configured=%t: wrong access reason or gate: %+v", configured, condition)
		}
	}
}

func TestSubTenantAccessOpensWithoutFlapping(t *testing.T) {
	w := provisionAccessWorld(t, true)
	w.pass(t)
	if w.tenant(t).GetLabels()[naas.LabelAccessReady] != "true" || meta.IsStatusConditionTrue(w.sub(t).Status.Conditions, "AccessReady") {
		t.Fatal("access was not staged through observed RBAC")
	}
	// A normal Capsule readiness-label replication lag must not revoke the gate.
	w.pass(t)
	if w.tenant(t).GetLabels()[naas.LabelAccessReady] != "true" {
		t.Fatal("access gate flapped while generated bindings were pending")
	}
	materializeAccessBindings(t, w)
	w.pass(t)
	if !meta.IsStatusConditionTrue(w.sub(t).Status.Conditions, "AccessReady") {
		t.Fatalf("observed namespace bindings not Ready: %+v", w.sub(t).Status)
	}
	writes := len(w.writes)
	w.pass(t)
	if len(w.writes) != writes {
		t.Fatal("unchanged reconciliation rewrote the access gate")
	}
	sub, parent := w.sub(t), w.parent(t)
	sub.Spec.DisplayName, sub.Generation = "Cosmetic edit", sub.Generation+1
	parent.Spec.MaintenanceWindow = &cloud.MaintenanceWindow{Days: []cloud.Weekday{"Saturday"}, Start: "02:00", Duration: "4h"}
	parent.Generation++
	if err := w.management.Update(t.Context(), sub); err != nil {
		t.Fatal(err)
	}
	if err := w.management.Update(t.Context(), parent); err != nil {
		t.Fatal(err)
	}
	w.pass(t)
	if !meta.IsStatusConditionTrue(w.sub(t).Status.Conditions, "AccessReady") || len(w.writes) != writes {
		t.Fatal("display name or maintenance window edit unnecessarily revoked access")
	}
}

// Access opens only once the cluster enforces NetworkPolicy: the
// sub-tenant's deny-by-default policies isolate nothing before.
func TestSubTenantAccessWaitsForNetworkPolicyEnforcement(t *testing.T) {
	w := provisionAccessWorldWith(t, true, func(a *awseks.Addon) *awseks.Addon {
		a.Status.AtProvider.ConfigurationValues = nil // asked for, EKS not there yet
		return a
	})
	condition := meta.FindStatusCondition(w.sub(t).Status.Conditions, "AccessReady")
	policy := meta.FindStatusCondition(w.sub(t).Status.Conditions, "NetworkPolicyReady")
	if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != "NetworkPolicyPending" ||
		policy == nil || policy.Status != metav1.ConditionFalse || w.tenant(t).GetLabels()[naas.LabelAccessReady] != "false" {
		t.Fatalf("access opened before NetworkPolicy enforcement: %+v", w.sub(t).Status.Conditions)
	}

	addon := &awseks.Addon{}
	if err := w.management.Get(t.Context(), client.ObjectKey{Namespace: w.parent(t).Namespace, Name: "prod-addon-vpc-cni"}, addon); err != nil {
		t.Fatal(err)
	}
	addon.Status.AtProvider.ConfigurationValues = addon.Spec.ForProvider.ConfigurationValues
	if err := w.management.Update(t.Context(), addon); err != nil { // no status subresource in the fake
		t.Fatal(err)
	}
	w.pass(t)

	if !meta.IsStatusConditionTrue(w.sub(t).Status.Conditions, "NetworkPolicyReady") || w.tenant(t).GetLabels()[naas.LabelAccessReady] != "true" {
		t.Fatalf("access did not open once NetworkPolicy was enforced: %+v", w.sub(t).Status.Conditions)
	}
}

// Access already open stays open while the vpc-cni add-on is updated (or
// before an older render gains enforcement): AccessReady reports the wait,
// the gate is not pulled and the sub-tenant does not go Degraded.
func TestSubTenantOpenAccessSurvivesAVPCCNIUpdate(t *testing.T) {
	w := provisionAccessWorld(t, true)
	materializeAccessBindings(t, w)
	w.pass(t)
	if !meta.IsStatusConditionTrue(w.sub(t).Status.Conditions, "AccessReady") {
		t.Fatalf("access not open: %+v", w.sub(t).Status)
	}
	addon := &awseks.Addon{}
	if err := w.management.Get(t.Context(), client.ObjectKey{Namespace: w.parent(t).Namespace, Name: "prod-addon-vpc-cni"}, addon); err != nil {
		t.Fatal(err)
	}
	addon.SetConditions(xpv2.Creating())
	if err := w.management.Update(t.Context(), addon); err != nil { // no status subresource in the fake
		t.Fatal(err)
	}

	w.pass(t)

	sub := w.sub(t)
	condition := meta.FindStatusCondition(sub.Status.Conditions, "AccessReady")
	if condition.Status != metav1.ConditionFalse || condition.Reason != "NetworkPolicyPending" ||
		w.tenant(t).GetLabels()[naas.LabelAccessReady] != "true" || sub.Status.State != cloud.SubTenantStateReady {
		t.Fatalf("a vpc-cni update revoked open access or went unreported: state %s, %+v, gate %s",
			sub.Status.State, condition, w.tenant(t).GetLabels()[naas.LabelAccessReady])
	}
}

// Only the platform's own access bindings are tracked: a RoleBinding someone
// else made is left alone, and one of ours that is no longer wanted closes
// access.
func TestSubTenantAccessTracksOnlyOwnBindings(t *testing.T) {
	w := provisionAccessWorld(t, true)
	w.pass(t)
	materializeAccessBindings(t, w)
	w.pass(t)
	namespace := w.tenant(t).GetName() + "-app"
	foreign := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "team-extra", Namespace: namespace},
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "view"},
		Subjects: []rbacv1.Subject{{Kind: "Group", APIGroup: rbacv1.GroupName, Name: "someone-else"}}}
	if err := w.remote.Create(t.Context(), foreign); err != nil {
		t.Fatal(err)
	}
	w.pass(t)
	if !meta.IsStatusConditionTrue(w.sub(t).Status.Conditions, "AccessReady") {
		t.Fatalf("a RoleBinding the platform did not create closed access: %+v", w.sub(t).Status.Conditions)
	}
	stale := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "naas-access-stale", Namespace: namespace, Labels: map[string]string{naas.LabelSubTenantID: stID}},
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: naas.WorkloadAdminRole},
		Subjects: []rbacv1.Subject{{Kind: "Group", APIGroup: rbacv1.GroupName, Name: "removed-group"}}}
	if err := w.remote.Create(t.Context(), stale); err != nil {
		t.Fatal(err)
	}
	w.pass(t)
	if condition := meta.FindStatusCondition(w.sub(t).Status.Conditions, "AccessReady"); condition.Status == metav1.ConditionTrue || condition.Reason != "AccessBindingMismatch" {
		t.Fatalf("a stale platform binding did not close access: %+v", condition)
	}
}
