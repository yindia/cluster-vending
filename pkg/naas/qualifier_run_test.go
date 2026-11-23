package naas

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/kube"
)

// qualifierWorld is a workload cluster where Capsule already generated what
// the two test Tenants produce, and the impersonated identities are refused
// what RBAC and admission refuse on a qualified cluster.
type qualifierWorld struct {
	admin client.WithWatch
	// What the fake cluster gets wrong, by test.
	openCrossRBAC, admitLoadBalancer, admitPrivileged bool
	webhookPool                                       string
	foreignTenant                                     bool
	probe                                             func(namespace, script string) bool
}

func newQualifierWorld(t *testing.T, mutate func(*qualifierWorld)) *qualifierWorld {
	t.Helper()
	w := &qualifierWorld{webhookPool: crossplaneaws.PoolSystem}
	w.probe = func(namespace, script string) bool {
		switch {
		case strings.Contains(script, "1.1.1.1"):
			return false
		case strings.Contains(script, "wget"):
			return strings.Contains(script, "."+namespace+".svc")
		default:
			return true
		}
	}
	if mutate != nil {
		mutate(w)
	}
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	scheme.AddKnownTypeWithName(TenantGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(TenantGVK.GroupVersion().WithKind("TenantList"), &unstructured.UnstructuredList{})
	peer := schema.GroupVersionKind{Group: "security.istio.io", Version: "v1", Kind: "PeerAuthentication"}
	scheme.AddKnownTypeWithName(peer, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(peer.GroupVersion().WithKind("PeerAuthenticationList"), &unstructured.UnstructuredList{})
	objects := []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: QualificationNamespace, UID: "naas-system-uid"}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: CapsuleWebhookService, Namespace: CapsuleNamespace}, Spec: corev1.ServiceSpec{Selector: map[string]string{"app": "capsule"}}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "capsule-0", Namespace: CapsuleNamespace, Labels: map[string]string{"app": "capsule"}}, Spec: corev1.PodSpec{NodeName: "node-1"},
			Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1", Labels: map[string]string{crossplaneaws.PoolLabelKey: w.webhookPool}}},
	}
	for _, side := range QualifierSides() {
		objects = append(objects,
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: side.Namespace, Labels: map[string]string{"capsule.clastix.io/tenant": side.Tenant, "istio.io/dataplane-mode": "ambient"}}},
			&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "naas-access-" + side.Group, Namespace: side.Namespace},
				RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: WorkloadAdminRole},
				Subjects: []rbacv1.Subject{{Kind: rbacv1.GroupKind, APIGroup: rbacv1.GroupName, Name: side.Group}}},
			&corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: "capsule-" + side.Tenant + "-0", Namespace: side.Namespace},
				Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{corev1.ResourcePods: resource.MustParse(qualifierQuotaPods)}}},
		)
		pa := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"mtls": map[string]any{"mode": "STRICT"}}}}
		pa.SetGroupVersionKind(peer)
		pa.SetName("naas-strict-mtls")
		pa.SetNamespace(side.Namespace)
		objects = append(objects, pa)
	}
	if w.foreignTenant {
		tenant := &unstructured.Unstructured{}
		tenant.SetGroupVersionKind(TenantGVK)
		tenant.SetName(QualifierSides()[0].Tenant)
		objects = append(objects, tenant)
	}
	w.admin = fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	return w
}

func forbidden(what string) error {
	return apierrors.NewForbidden(schema.GroupResource{Resource: what}, "x", nil)
}

// as is the impersonated client of side: the shared fake, with the refusals of
// RBAC and admission.
func (w *qualifierWorld) as(side QualifierSide) (client.Client, error) {
	return interceptor.NewClient(w.admin, interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			options := &client.ListOptions{}
			options.ApplyOptions(opts)
			if _, cms := list.(*corev1.ConfigMapList); cms && options.Namespace != side.Namespace && !w.openCrossRBAC {
				return forbidden("configmaps")
			}
			return c.List(ctx, list, opts...)
		},
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if obj.GetNamespace() != side.Namespace {
				return forbidden("any")
			}
			switch o := obj.(type) {
			case *corev1.Service:
				if (o.Spec.Type != "" && o.Spec.Type != corev1.ServiceTypeClusterIP || len(o.Spec.ExternalIPs) > 0) && !w.admitLoadBalancer {
					return forbidden("services")
				}
			case *corev1.Pod:
				if c := o.Spec.Containers[0].SecurityContext; c != nil && c.Privileged != nil && *c.Privileged && !w.admitPrivileged {
					return forbidden("pods")
				}
				if o.Name == qualifierServerName {
					o.Status = corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}
				}
			case *rbacv1.RoleBinding:
				return forbidden("rolebindings")
			}
			return c.Create(ctx, obj, opts...)
		},
	}), nil
}

func (w *qualifierWorld) runner() *QualifierRunner {
	return &QualifierRunner{
		Clients: QualifierClients{Admin: w.admin, As: w.as}, Key: qualificationKeyFixture(), JobUID: "job-uid", ProbeImage: "busybox",
		Poll: time.Millisecond, SetupTimeout: 2 * time.Second, ProbeTimeout: time.Second,
		Now: func() time.Time { return qualificationNow },
		Probe: func(_ context.Context, _ client.Client, namespace, _ string, script string) (bool, error) {
			return w.probe(namespace, script), nil
		},
	}
}

func TestQualifierRunnerChecks(t *testing.T) {
	all := RequiredQualificationChecks()
	tests := []struct {
		name   string
		mutate func(*qualifierWorld)
		failed []string
	}{
		{name: "qualified cluster passes all eleven"},
		{name: "egress open", mutate: func(w *qualifierWorld) {
			base := w.probe
			w.probe = func(ns, s string) bool { return strings.Contains(s, "1.1.1.1") || base(ns, s) }
		}, failed: []string{CheckEgressDeny}},
		{name: "cross sub-tenant traffic open", mutate: func(w *qualifierWorld) {
			w.probe = func(_, s string) bool { return !strings.Contains(s, "1.1.1.1") }
		}, failed: []string{CheckCrossSubTenant}},
		{name: "broken probe image cannot pass deny checks", mutate: func(w *qualifierWorld) {
			w.probe = func(string, string) bool { return false }
		}, failed: []string{CheckAmbientMTLS, CheckCrossSubTenant, CheckDNS, CheckEgressDeny}},
		{name: "cross-namespace RBAC open", mutate: func(w *qualifierWorld) { w.openCrossRBAC = true }, failed: []string{CheckCrossNamespaceRBAC}},
		{name: "load balancer admitted", mutate: func(w *qualifierWorld) { w.admitLoadBalancer = true }, failed: []string{CheckAdmissionGuards}},
		{name: "privileged pod admitted", mutate: func(w *qualifierWorld) { w.admitPrivileged = true }, failed: []string{CheckRestrictedPSS}},
		{name: "webhook on the platform pool", mutate: func(w *qualifierWorld) { w.webhookPool = crossplaneaws.PoolPlatform }, failed: []string{CheckAdmissionPoolOutage}},
		{name: "foreign tenant fails setup", mutate: func(w *qualifierWorld) { w.foreignTenant = true }, failed: all},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newQualifierWorld(t, tt.mutate)
			result, err := w.runner().Run(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			var failed []string
			for _, check := range all {
				if !result.Checks[check] {
					failed = append(failed, check)
					if result.Failures[check] == "" {
						t.Errorf("%s failed without a reason", check)
					}
				}
			}
			if !slices.Equal(failed, tt.failed) {
				t.Fatalf("failed = %v, want %v (failures %v)", failed, tt.failed, result.Failures)
			}
			if result.JobUID != "job-uid" || result.Key != qualificationKeyFixture() || len(result.Checks) != len(all) {
				t.Fatalf("result = %+v", result)
			}
			// Test Tenants are removed; a foreign one is never touched.
			for i, side := range QualifierSides() {
				tenant := &unstructured.Unstructured{}
				tenant.SetGroupVersionKind(TenantGVK)
				err := w.admin.Get(t.Context(), client.ObjectKey{Name: side.Tenant}, tenant)
				if w.foreignTenant && i == 0 {
					if err != nil || tenant.GetLabels()[LabelQualifier] != "" {
						t.Fatalf("foreign tenant changed: %v %v", err, tenant.GetLabels())
					}
					continue
				}
				if !apierrors.IsNotFound(err) {
					t.Fatalf("test Tenant %s left: %v", side.Tenant, err)
				}
			}
			if len(result.LeftOver) != 0 {
				t.Fatalf("left over: %v", result.LeftOver)
			}
		})
	}
}

func TestQualifierRunnerCreatesTestTenantsThroughTheRenderer(t *testing.T) {
	w := newQualifierWorld(t, nil)
	r := w.runner()
	r.defaults()
	if _, err := r.setup(t.Context()); err != nil {
		t.Fatal(err)
	}
	tenant := &unstructured.Unstructured{}
	tenant.SetGroupVersionKind(TenantGVK)
	side := QualifierSides()[0]
	if err := w.admin.Get(t.Context(), client.ObjectKey{Name: side.Tenant}, tenant); err != nil {
		t.Fatal(err)
	}
	bindings, _, _ := unstructured.NestedSlice(tenant.Object, "spec", "data", "naas", "accessBindings")
	if tenant.GetLabels()[LabelAccessReady] != "true" || tenant.GetLabels()[LabelProfile] != "restricted-ambient-v1" || len(bindings) != 1 ||
		bindings[0].(map[string]any)["group"] != side.Group || bindings[0].(map[string]any)["role"] != WorkloadAdminRole {
		t.Fatalf("test Tenant = %v", tenant.Object)
	}
	if prevent, _, _ := unstructured.NestedBool(tenant.Object, "spec", "preventDeletion"); prevent {
		t.Fatal("test Tenant cannot be deleted")
	}
}

func TestQualifierResultRoundTripAndRecord(t *testing.T) {
	w := newQualifierWorld(t, func(w *qualifierWorld) { w.webhookPool = crossplaneaws.PoolPlatform })
	result, err := w.runner().Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteQualifierResult(t.Context(), w.admin, result); err != nil {
		t.Fatal(err)
	}
	if err := WriteQualifierResult(t.Context(), w.admin, result); err != nil {
		t.Fatal("rewrite:", err)
	}
	read, err := ReadQualifierResult(t.Context(), w.admin)
	if err != nil || read.JobUID != "job-uid" {
		t.Fatalf("read = %+v, %v", read, err)
	}
	record := read.Record(QualifierJobRef{Name: "naas-qualifier-x", UID: "job-uid"}, qualificationNow.Add(time.Minute))
	raw, _ := json.Marshal(record)
	decoded, err := DecodeClusterQualification(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decoded.Current(qualificationKeyFixture(), qualificationNow.Add(time.Hour)); err == nil || !strings.Contains(err.Error(), CheckAdmissionPoolOutage) {
		t.Fatalf("recorded failure = %v", err)
	}
	if decoded.Failures[CheckAdmissionPoolOutage] == "" || decoded.ExpiresAt.Sub(decoded.IssuedAt) != MaxQualificationValidity {
		t.Fatalf("record = %+v", decoded)
	}
	crashed := FailedRunRecord(qualificationKeyFixture(), QualifierJobRef{Name: "j", UID: "u"}, "job failed", qualificationNow)
	if len(crashed.FailedChecks()) != len(RequiredQualificationChecks()) {
		t.Fatalf("crashed run passed checks: %+v", crashed)
	}
	if none, err := ReadQualifierResult(t.Context(), fake.NewClientBuilder().Build()); none != nil || err != nil {
		t.Fatalf("no result = %+v, %v", none, err)
	}
}

func TestPodProbeWaitsForThePodToFinish(t *testing.T) {
	for _, phase := range []corev1.PodPhase{corev1.PodSucceeded, corev1.PodFailed} {
		base := fake.NewClientBuilder().Build()
		var created *corev1.Pod
		c := interceptor.NewClient(base, interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				created = obj.(*corev1.Pod).DeepCopy()
				obj.(*corev1.Pod).Status.Phase = phase
				return c.Create(ctx, obj, opts...)
			},
		})
		ok, err := PodProbe("busybox", time.Millisecond)(t.Context(), c, "ns", "probe", "true")
		if err != nil || ok != (phase == corev1.PodSucceeded) {
			t.Fatalf("%s: ok=%t err=%v", phase, ok, err)
		}
		sc := created.Spec.Containers[0].SecurityContext
		if created.Spec.SecurityContext.RunAsNonRoot == nil || !*created.Spec.SecurityContext.RunAsNonRoot || *sc.AllowPrivilegeEscalation || sc.Capabilities.Drop[0] != "ALL" {
			t.Fatal("probe pod is not restricted-PSS")
		}
		if err := base.Get(t.Context(), client.ObjectKey{Namespace: "ns", Name: "probe"}, &corev1.Pod{}); !apierrors.IsNotFound(err) {
			t.Fatal("probe pod not deleted", err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, err := PodProbe("busybox", time.Millisecond)(ctx, fake.NewClientBuilder().Build(), "ns", "stuck", "sleep 1000"); err == nil {
		t.Fatal("an unfinished probe passed")
	}
}

func TestRunQualifierJobWritesTheResult(t *testing.T) {
	w := newQualifierWorld(t, nil)
	raw, _ := json.Marshal(qualificationKeyFixture())
	if _, err := RunQualifierJob(t.Context(), w.runner(), string(raw), "job-uid"); err != nil {
		t.Fatal(err)
	}
	result, err := ReadQualifierResult(t.Context(), w.admin)
	if err != nil || result == nil || result.JobUID != "job-uid" || len(result.Checks) != len(RequiredQualificationChecks()) {
		t.Fatalf("result = %+v, %v", result, err)
	}
	for _, bad := range []struct{ key, uid string }{{`{"clusterUID":"x","extra":1}`, "u"}, {"{", "u"}, {string(raw), ""}, {`{}`, "u"}} {
		if _, err := RunQualifierJob(t.Context(), w.runner(), bad.key, bad.uid); err == nil {
			t.Fatalf("accepted key %q uid %q", bad.key, bad.uid)
		}
	}
}
