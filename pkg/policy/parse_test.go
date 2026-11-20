package policy

import (
	"context"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

func obj(t *testing.T, s string) *unstructured.Unstructured {
	t.Helper()
	u := &unstructured.Unstructured{}
	if err := u.UnmarshalJSON([]byte(s)); err != nil {
		t.Fatal(err)
	}
	return u
}
func TestReportSemantics(t *testing.T) {
	p := Empty("")
	p.Summary.Incomplete = false
	p.Summary.Notices = nil
	u := obj(t, `{"apiVersion":"wgpolicyk8s.io/v1alpha2","kind":"PolicyReport","metadata":{"name":"r","namespace":"apps"},"scope":{"apiVersion":"v1","kind":"Pod","namespace":"apps","name":"pod"},"results":[{"policy":"bare","source":"kyverno","result":"fail","message":"failed"},{"policy":"apps/namespaced","source":"kyverno","result":"warn"},{"policy":"apps/cel","source":"KyvernoNamespacedValidatingPolicy","result":"error"},{"policy":"vap","source":"ValidatingAdmissionPolicy","result":"fail","resources":[{"name":"one"},{"name":"two"}]},{"result":"pass"},{"result":"skip"},{"source":"future","policy":"unknown","result":"fail"},{"result":"future-result"}]}`)
	report(p, u)
	if len(p.Violations) != 6 {
		t.Fatalf("rows = %v", p.Violations)
	}
	if p.Violations[0].Policy.Kind != "" || p.Violations[0].Policy.Namespace != "" || p.Violations[0].Enforcement != "unknown" {
		t.Fatal("guessed bare policy identity or enforcement")
	}
	if p.Violations[1].Policy.Kind != "Policy" || p.Violations[2].Policy.Kind != "NamespacedValidatingPolicy" || p.Violations[2].Result != "error" {
		t.Fatal(p.Violations)
	}
	if p.Violations[3].Engine != "vap" || p.Violations[3].Resource.Name != "one" || p.Violations[4].Resource.Name != "two" {
		t.Fatal("lost report targets")
	}
	if !p.Summary.Incomplete || len(p.Summary.Notices) == 0 {
		t.Fatal("unknown result/source not reported")
	}
}
func TestKyvernoPolicySideEvents(t *testing.T) {
	u := obj(t, `{"apiVersion":"v1","kind":"Event","metadata":{"name":"ev","namespace":"kyverno"},"reason":"PolicyViolation","source":{"component":"kyverno-admission"},"action":"Resource Blocked","involvedObject":{"kind":"ClusterPolicy","name":"p"},"related":{"apiVersion":"v1","kind":"Pod","namespace":"apps","name":"bad"},"count":3,"lastTimestamp":"2026-11-19T00:00:00Z"}`)
	e := event(u)
	if e == nil || e.Enforcement != "enforce" || e.Count != 3 || e.Resource.Name != "bad" {
		t.Fatal(e)
	}
	u.Object["action"] = "new-action"
	e = event(u)
	if e == nil || e.Enforcement != "unknown" || e.Action != "new-action" {
		t.Fatal(e)
	}
	u.Object["involvedObject"] = map[string]any{"kind": "Pod", "name": "bad"}
	if event(u) != nil {
		t.Fatal("resource duplicate retained")
	}
	u.Object["involvedObject"] = map[string]any{"kind": "Policy", "name": "p"}
	delete(u.Object, "source")
	u.Object["reportingComponent"] = "kyverno-admission"
	if event(u) == nil {
		t.Fatal("reportingComponent not accepted")
	}
	delete(u.Object, "related")
	if event(u) != nil {
		t.Fatal("event missing target accepted")
	}
}
func TestGatekeeperAndTTL(t *testing.T) {
	p := Empty("")
	constraint(p, obj(t, `{"apiVersion":"constraints.gatekeeper.sh/v1beta1","kind":"RequiredLabels","metadata":{"name":"labels"},"status":{"totalViolations":4,"violations":[{"kind":"Pod","name":"bad","enforcementAction":"new"}]}}`))
	if len(p.Violations) != 1 || !p.Summary.Truncated || !p.Summary.Incomplete || p.Violations[0].Enforcement != "unknown" || p.Violations[0].ObservedAt != nil {
		t.Fatal(p)
	}
	e := event(obj(t, `{"apiVersion":"v1","kind":"Event","metadata":{"name":"ev","annotations":{"process":"admission","constraint_kind":"RequiredLabels","constraint_name":"labels","constraint_action":"dryrun","resource_kind":"Pod","resource_name":"bad"}},"reason":"DryrunViolation","source":{"component":"gatekeeper-webhook"},"lastTimestamp":"2026-11-19T00:00:00Z"}`))
	if e == nil || e.Enforcement != "audit" || e.Policy.Name != "labels" {
		t.Fatal(e)
	}
	p.Events = []*cloudv1.ClusterViolationEvent{e}
	Expire(p, e.LastObservedAt.AsTime().Add(EventTTL+time.Second))
	if len(p.Events) != 0 {
		t.Fatal("expired event retained")
	}
}
func TestSnapshotBounds(t *testing.T) {
	p := Empty("partial")
	for range 250 {
		p.Violations = append(p.Violations, &cloudv1.ClusterViolation{Message: strings.Repeat("<", 2048)})
	}
	raw, err := Encode(p)
	if err != nil || len(raw) > MaxBytes || len(p.Violations) > MaxRows || !p.Summary.Truncated {
		t.Fatalf("size %d rows %d err %v", len(raw), len(p.Violations), err)
	}
	if !Decode([]byte("broken")).Summary.Incomplete {
		t.Fatal("broken snapshot healthy")
	}
}
func TestReaderBoundedReadOnlyAndPartial(t *testing.T) {
	gvr := schema.GroupVersionResource{Group: "wgpolicyk8s.io", Version: "v1alpha2", Resource: "policyreports"}
	kinds := map[schema.GroupVersionResource]string{gvr: "PolicyReportList",
		{Group: "wgpolicyk8s.io", Version: "v1alpha2", Resource: "clusterpolicyreports"}: "ClusterPolicyReportList",
		{Group: "openreports.io", Version: "v1alpha1", Resource: "reports"}:              "ReportList",
		{Group: "openreports.io", Version: "v1alpha1", Resource: "clusterreports"}:       "ClusterReportList",
		{Version: "v1", Resource: "events"}:                                              "EventList",
	}
	d := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), kinds)
	calls := 0
	d.PrependReactor("list", "*", func(a ktesting.Action) (bool, runtime.Object, error) {
		calls++
		if a.GetResource() != gvr {
			return true, nil, apierrors.NewForbidden(a.GetResource().GroupResource(), "", nil)
		}
		return true, &unstructured.UnstructuredList{Object: map[string]any{"metadata": map[string]any{"continue": "next"}}, Items: []unstructured.Unstructured{*obj(t, `{"apiVersion":"wgpolicyk8s.io/v1alpha2","kind":"PolicyReport","metadata":{"name":"p"},"results":[{"source":"kyverno","policy":"p","result":"fail"}]}`)}}, nil
	})
	r := &Reader{Dynamic: d, Constraints: func(context.Context) (*metav1.APIResourceList, error) {
		return nil, apierrors.NewForbidden(schema.GroupResource{}, "", nil)
	}}
	p := r.Collect(t.Context(), time.Now())
	if calls > 48 || calls != 11 || len(p.Violations) != 4 || !p.Summary.Truncated || !p.Summary.Incomplete {
		t.Fatalf("calls=%d snapshot=%v", calls, p)
	}
	seenResources := map[string]bool{}
	for _, a := range d.Actions() {
		seenResources[a.GetResource().Group+"/"+a.GetResource().Resource] = true
		if a.GetVerb() != "list" {
			t.Fatalf("workload mutation %s", a.GetVerb())
		}
	}
	for _, name := range []string{"wgpolicyk8s.io/policyreports", "wgpolicyk8s.io/clusterpolicyreports", "openreports.io/reports", "openreports.io/clusterreports", "/events"} {
		if !seenResources[name] {
			t.Errorf("source was not read: %s", name)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	before := calls
	unavailable := r.Collect(ctx, time.Now())
	if unavailable.Summary.CollectedAt != nil {
		t.Fatal("canceled collection looks available")
	}
	if calls != before {
		t.Fatal("read with canceled context")
	}
}
func TestUnsafeWorkloadCredentials(t *testing.T) {
	_, err := NewReader([]byte("apiVersion: v1\nkind: Config\nclusters:\n- name: c\n  cluster:\n    server: https://example.invalid\nusers:\n- name: u\n  user:\n    exec:\n      apiVersion: client.authentication.k8s.io/v1\n      command: forbidden-command\ncontexts:\n- name: c\n  context:\n    cluster: c\n    user: u\ncurrent-context: c\n"))
	if err == nil || !strings.Contains(err.Error(), "exec") {
		t.Fatalf("unsafe config accepted: %v", err)
	}
}
