package naas

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/kube"
)

func qualificationFixture() (QualificationBinding, AccessQualification) {
	now := time.Now().UTC().Truncate(time.Second)
	binding := QualificationBinding{AllocationUID: "allocation-uid", TenantUID: "tenant-uid", ClusterUID: "cluster-uid", ClusterID: "cluster-id", RemoteTenantUID: "remote-uid",
		NamespaceUIDs: map[string]string{"payments-app": "namespace-uid"}, BootstrapRevision: "bootstrap-revision", SpecHash: "security-spec", OIDCRevision: "oidc-revision", AssociationUID: "association-uid"}
	qualification := AccessQualification{Version: "v1", Binding: binding, IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(20 * time.Minute), Evidence: "https://evidence.example.test/run/qualified", Checks: map[string]bool{}}
	for _, check := range RequiredQualificationChecks() {
		qualification.Checks[check] = true
	}
	return binding, qualification
}

func TestAccessQualificationProtectedCurrentBoundAndExpiring(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*AccessQualification, *corev1.ConfigMap, *corev1.Namespace)
		ok     bool
	}{
		{name: "current trusted evidence", ok: true},
		{name: "expired", mutate: func(q *AccessQualification, _ *corev1.ConfigMap, _ *corev1.Namespace) {
			q.ExpiresAt = time.Now().Add(-time.Second)
		}},
		{name: "excessive validity", mutate: func(q *AccessQualification, _ *corev1.ConfigMap, _ *corev1.Namespace) {
			q.ExpiresAt = q.IssuedAt.Add(2 * time.Hour)
		}},
		{name: "future issue", mutate: func(q *AccessQualification, _ *corev1.ConfigMap, _ *corev1.Namespace) {
			q.IssuedAt = time.Now().Add(10 * time.Minute)
		}},
		{name: "different association", mutate: func(q *AccessQualification, _ *corev1.ConfigMap, _ *corev1.Namespace) {
			q.Binding.AssociationUID = "replacement"
		}},
		{name: "different bootstrap", mutate: func(q *AccessQualification, _ *corev1.ConfigMap, _ *corev1.Namespace) {
			q.Binding.BootstrapRevision = "replacement"
		}},
		{name: "different namespace UID", mutate: func(q *AccessQualification, _ *corev1.ConfigMap, _ *corev1.Namespace) {
			q.Binding.NamespaceUIDs["payments-app"] = "replacement"
		}},
		{name: "different security intent", mutate: func(q *AccessQualification, _ *corev1.ConfigMap, _ *corev1.Namespace) {
			q.Binding.SpecHash = "replacement"
		}},
		{name: "unqualified policy", mutate: func(q *AccessQualification, _ *corev1.ConfigMap, _ *corev1.Namespace) {
			q.Checks["admission-guards"] = false
		}},
		{name: "missing evidence", mutate: func(q *AccessQualification, _ *corev1.ConfigMap, _ *corev1.Namespace) { q.Evidence = "" }},
		{name: "tenant namespace record", mutate: func(_ *AccessQualification, cm *corev1.ConfigMap, _ *corev1.Namespace) { cm.Namespace = "payments-app" }},
		{name: "tenant owned authority namespace", mutate: func(_ *AccessQualification, _ *corev1.ConfigMap, ns *corev1.Namespace) {
			ns.Labels = map[string]string{LabelSubTenantID: "tenant-owned"}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			binding, qualification := qualificationFixture()
			// Deep-copy maps to keep expected bindings independent of mutations.
			raw, _ := json.Marshal(qualification)
			qualification = AccessQualification{}
			if err := json.Unmarshal(raw, &qualification); err != nil {
				t.Fatal(err)
			}
			cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: QualificationNamespace, Name: QualificationName(binding.AllocationUID)}}
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: QualificationNamespace, UID: "platform-namespace-uid"}}
			if test.mutate != nil {
				test.mutate(&qualification, cm, ns)
			}
			raw, _ = json.Marshal(qualification)
			cm.Data = map[string]string{QualificationKey: string(raw)}
			scheme, err := kube.Scheme()
			if err != nil {
				t.Fatal(err)
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ns, cm).Build()
			if err := VerifyAccessQualification(t.Context(), c, binding, time.Now()); (err == nil) != test.ok {
				t.Fatalf("ok=%t err=%v", test.ok, err)
			}
		})
	}
}

func TestAccessQualificationRejectsMalformedAndUntrustedPayloads(t *testing.T) {
	binding, qualification := qualificationFixture()
	raw, _ := json.Marshal(qualification)
	for _, payload := range []string{"", "{}", "{", string(raw) + " {}", string(raw[:len(raw)-1]) + `,"unknown":true}`, string(make([]byte, 65537))} {
		scheme, err := kube.Scheme()
		if err != nil {
			t.Fatal(err)
		}
		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: QualificationName(binding.AllocationUID), Namespace: QualificationNamespace}, Data: map[string]string{QualificationKey: payload}}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: QualificationNamespace, UID: "protected"}}, cm).Build()
		if err := VerifyAccessQualification(t.Context(), c, binding, time.Now()); err == nil {
			t.Fatal("accepted invalid qualification")
		}
	}
}

func TestQualificationBindingIgnoresCosmeticAndScalingGenerations(t *testing.T) {
	sub := allocationFixture()
	sub.Spec.Access = nil
	remote, err := RenderTenant(sub, "1.35", "revision")
	if err != nil {
		t.Fatal(err)
	}
	remote.SetUID("remote-uid")
	parent := &cloud.Cluster{ObjectMeta: metav1.ObjectMeta{UID: "cluster-uid", Generation: 1}}
	inventory := []cloud.SubTenantNamespaceStatus{{Key: "app", Name: remote.GetName() + "-app", UID: "namespace-uid"}}
	access := &ResolvedAccess{OIDCRevision: "mapping-revision"}
	before, err := BindAccessQualification(sub, parent, remote, inventory, "revision", access, "association-uid")
	if err != nil {
		t.Fatal(err)
	}
	sub.Spec.DisplayName, sub.Generation = "Cosmetic rename", sub.Generation+1
	parent.Generation++
	parent.Spec.DefaultNodePool = &cloud.NodePoolSpec{Scaling: &cloud.ScalingConfig{MinSize: 2, MaxSize: 8}}
	after, err := BindAccessQualification(sub, parent, remote, inventory, "revision", access, "association-uid")
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("unrelated edits invalidated qualification", err)
	}
	// A real namespace replacement must invalidate the exact binding.
	inventory[0].UID = "replacement"
	replaced, err := BindAccessQualification(sub, parent, remote, inventory, "revision", access, "association-uid")
	if err != nil || reflect.DeepEqual(before, replaced) {
		t.Fatal("namespace UID replacement reused qualification", err)
	}
}
