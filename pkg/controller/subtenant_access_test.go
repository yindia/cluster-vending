package controller

import (
	"bytes"
	"errors"
	"io"
	"testing"
	"text/template"
	"time"

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

func provisionAccessWorld(t *testing.T, configured bool) *stWorld {
	t.Helper()
	w := newSubTenantWorld(t)
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
	if err := w.remote.Get(t.Context(), client.ObjectKey{Name: naas.QualificationNamespace}, protected); err != nil {
		t.Fatal(err)
	}
	protected.UID = "platform-namespace-uid"
	if err := w.remote.Update(t.Context(), protected); err != nil {
		t.Fatal(err)
	}
	w.pass(t)
	return w
}

// qualifyAccessWorld writes the parent cluster's record, as the qualification
// controller does after a passed run (or an operator by hand).
func qualifyAccessWorld(t *testing.T, w *stWorld) *corev1.ConfigMap {
	t.Helper()
	return writeClusterRecord(t, w, func(*naas.ClusterQualification) {})
}

func writeClusterRecord(t *testing.T, w *stWorld, mutate func(*naas.ClusterQualification)) *corev1.ConfigMap {
	t.Helper()
	request, err := naas.CurrentClusterQualificationRequest(t.Context(), w.management, w.remote, "tenant-acme", "parent")
	if err != nil {
		t.Fatal(err)
	}
	issued := time.Now().Add(-time.Minute)
	qualification := &naas.ClusterQualification{Version: naas.ClusterQualificationVersion, Key: request.Key, IssuedAt: issued, ExpiresAt: issued.Add(naas.MaxQualificationValidity),
		Evidence: "https://qualification.example.test/run/test", Checks: map[string]bool{}}
	for _, check := range request.RequiredChecks {
		qualification.Checks[check] = true
	}
	mutate(qualification)
	desired, err := naas.ClusterQualificationConfigMap(qualification)
	if err != nil {
		t.Fatal(err)
	}
	current := &corev1.ConfigMap{}
	if err := w.remote.Get(t.Context(), client.ObjectKeyFromObject(desired), current); err == nil {
		current.Data = desired.Data
		if err := w.remote.Update(t.Context(), current); err != nil {
			t.Fatal(err)
		}
		return current
	}
	if err := w.remote.Create(t.Context(), desired); err != nil {
		t.Fatal(err)
	}
	return desired
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

func TestSubTenantAccessProvisioningIndependentOfOIDCAndQualification(t *testing.T) {
	for _, configured := range []bool{false, true} {
		w := provisionAccessWorld(t, configured)
		sub := w.sub(t)
		// Provisioned (state Ready) while access waits; the Ready condition
		// also needs access, because groups are named (PROP-05 6.9).
		if sub.Status.State != cloud.SubTenantStateReady || !meta.IsStatusConditionTrue(sub.Status.Conditions, "ProvisioningReady") || meta.IsStatusConditionTrue(sub.Status.Conditions, "Ready") || meta.IsStatusConditionTrue(sub.Status.Conditions, "AccessReady") {
			t.Fatalf("provisioning incorrectly blocked by human access: %+v", sub.Status)
		}
		condition := meta.FindStatusCondition(sub.Status.Conditions, "AccessReady")
		want := "QualificationRequired"
		if !configured {
			want = "OIDCPending"
		}
		if condition.Reason != want || w.tenant(t).GetLabels()[naas.LabelAccessReady] != "false" {
			t.Fatalf("configured=%t: wrong closed access reason: %+v", configured, condition)
		}
	}
}

func TestSubTenantAccessQualifiesWithoutFlappingAndRevokesExpiredEvidence(t *testing.T) {
	w := provisionAccessWorld(t, true)
	cm := qualifyAccessWorld(t, w)
	w.pass(t)
	if w.tenant(t).GetLabels()[naas.LabelAccessReady] != "true" || meta.IsStatusConditionTrue(w.sub(t).Status.Conditions, "AccessReady") {
		t.Fatal("qualification did not stage access through observed RBAC")
	}
	// A normal Capsule readiness-label replication lag must not revoke the gate.
	w.pass(t)
	if w.tenant(t).GetLabels()[naas.LabelAccessReady] != "true" {
		t.Fatal("access gate flapped while generated bindings were pending")
	}
	materializeAccessBindings(t, w)
	w.pass(t)
	if !meta.IsStatusConditionTrue(w.sub(t).Status.Conditions, "AccessReady") {
		t.Fatalf("qualified, observed namespace bindings not Ready: %+v", w.sub(t).Status)
	}
	writes := len(w.writes)
	w.pass(t)
	if len(w.writes) != writes {
		t.Fatal("unchanged qualified reconciliation rewrote the access gate")
	}
	sub, parent := w.sub(t), w.parent(t)
	sub.Spec.DisplayName, sub.Generation = "Cosmetic edit", sub.Generation+1
	parent.Spec.DefaultNodePool = &cloud.NodePoolSpec{Scaling: &cloud.ScalingConfig{MinSize: 2, MaxSize: 8}}
	parent.Generation++
	if err := w.management.Update(t.Context(), sub); err != nil {
		t.Fatal(err)
	}
	if err := w.management.Update(t.Context(), parent); err != nil {
		t.Fatal(err)
	}
	w.pass(t)
	if !meta.IsStatusConditionTrue(w.sub(t).Status.Conditions, "AccessReady") || len(w.writes) != writes {
		t.Fatal("display name or scaling edit unnecessarily revoked qualified access")
	}
	if cm.Name != naas.ClusterQualificationName {
		t.Fatalf("record %s is not the cluster's", cm.Name)
	}
	writeClusterRecord(t, w, func(q *naas.ClusterQualification) {
		q.IssuedAt, q.ExpiresAt = time.Now().Add(-31*24*time.Hour), time.Now().Add(-time.Second)
	})
	w.pass(t)
	if w.tenant(t).GetLabels()[naas.LabelAccessReady] != "false" || meta.IsStatusConditionTrue(w.sub(t).Status.Conditions, "AccessReady") || !meta.IsStatusConditionTrue(w.sub(t).Status.Conditions, "ProvisioningReady") {
		t.Fatal("expired evidence did not revoke access independently of provisioning")
	}
}
