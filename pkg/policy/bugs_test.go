package policy

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

func baseKinds() map[schema.GroupVersionResource]string {
	return map[schema.GroupVersionResource]string{
		{Group: "wgpolicyk8s.io", Version: "v1alpha2", Resource: "policyreports"}:        "PolicyReportList",
		{Group: "wgpolicyk8s.io", Version: "v1alpha2", Resource: "clusterpolicyreports"}: "ClusterPolicyReportList",
		{Group: "openreports.io", Version: "v1alpha1", Resource: "reports"}:              "ReportList",
		{Group: "openreports.io", Version: "v1alpha1", Resource: "clusterreports"}:       "ClusterReportList",
		{Version: "v1", Resource: "events"}:                                              "EventList",
	}
}

func gatekeeperKinds(n int) *metav1.APIResourceList {
	l := &metav1.APIResourceList{}
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("kind%02d", i)
		l.APIResources = append(l.APIResources, metav1.APIResource{Name: name}, metav1.APIResource{Name: name + "/status"})
	}
	return l
}

func TestGatekeeperKindCapCountsOnlyRealKinds(t *testing.T) {
	kinds := baseKinds()
	for i := 0; i < 40; i++ {
		kinds[schema.GroupVersionResource{Group: "constraints.gatekeeper.sh", Version: "v1beta1", Resource: fmt.Sprintf("kind%02d", i)}] = "XList"
	}
	d := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), kinds)
	read := map[string]bool{}
	d.PrependReactor("list", "*", func(a ktesting.Action) (bool, runtime.Object, error) {
		read[a.GetResource().Resource] = true
		return true, &unstructured.UnstructuredList{}, nil
	})
	r := &Reader{Dynamic: d, Constraints: func(context.Context) (*metav1.APIResourceList, error) { return gatekeeperKinds(40), nil }}
	p := r.Collect(t.Context(), time.Now())
	n := 0
	for k := range read {
		if strings.HasPrefix(k, "kind") {
			n++
		}
	}
	if n != 32 || !p.Summary.Truncated {
		t.Fatalf("read %d kinds truncated=%v", n, p.Summary.Truncated)
	}
	d2 := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), kinds)
	r2 := &Reader{Dynamic: d2, Constraints: func(context.Context) (*metav1.APIResourceList, error) { return gatekeeperKinds(32), nil }}
	if p := r2.Collect(t.Context(), time.Now()); p.Summary.Truncated {
		t.Fatal("exactly 32 kinds must not be truncated")
	}
}

func TestEventsAreNotStarvedByGatekeeperBudget(t *testing.T) {
	kinds := baseKinds()
	for i := 0; i < 32; i++ {
		kinds[schema.GroupVersionResource{Group: "constraints.gatekeeper.sh", Version: "v1beta1", Resource: fmt.Sprintf("kind%02d", i)}] = "XList"
	}
	d := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), kinds)
	eventCalls := 0
	d.PrependReactor("list", "*", func(a ktesting.Action) (bool, runtime.Object, error) {
		if a.GetResource().Resource == "events" {
			eventCalls++
		}
		return true, &unstructured.UnstructuredList{Object: map[string]any{"metadata": map[string]any{"continue": "more"}}}, nil
	})
	r := &Reader{Dynamic: d, Constraints: func(context.Context) (*metav1.APIResourceList, error) { return gatekeeperKinds(32), nil }}
	p := r.Collect(t.Context(), time.Now())
	if eventCalls == 0 {
		t.Fatal("events were starved")
	}
	named := false
	for _, n := range p.Summary.Notices {
		if strings.Contains(n, "budget") && strings.Contains(n, "constraints.gatekeeper.sh") {
			named = true
		}
	}
	if !named {
		t.Fatalf("budget notice does not name the cut source: %v", p.Summary.Notices)
	}
}

func TestNewestEventsSurviveCap(t *testing.T) {
	now := time.Now().UTC()
	d := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), baseKinds())
	page := 0
	d.PrependReactor("list", "events", func(a ktesting.Action) (bool, runtime.Object, error) {
		l := &unstructured.UnstructuredList{}
		if a.(ktesting.ListAction).GetListRestrictions().Fields.String() != "reason=PolicyViolation" {
			return true, l, nil
		}
		start := page * 100 // pages of 100, API order: oldest first
		page++
		if page < 3 {
			l.SetContinue("more")
		}
		for i := start; i < min(start+100, 250); i++ {
			ts := now.Add(-time.Duration(250-i) * time.Second).Format(time.RFC3339Nano)
			l.Items = append(l.Items, *obj(t, fmt.Sprintf(`{"apiVersion":"v1","kind":"Event","metadata":{"name":"e%03d","namespace":"ns"},"reason":"PolicyViolation","reportingComponent":"kyverno-admission","action":"deny","involvedObject":{"kind":"ClusterPolicy","name":"p"},"related":{"kind":"Pod","name":"x"},"lastTimestamp":%q}`, i, ts)))
		}
		return true, l, nil
	})
	p := (&Reader{Dynamic: d}).Collect(t.Context(), now)
	if len(p.Events) != MaxRows || !p.Summary.Truncated {
		t.Fatalf("events=%d truncated=%v", len(p.Events), p.Summary.Truncated)
	}
	if p.Events[0].Id != "ns/e249" || p.Events[MaxRows-1].Id != "ns/e050" {
		t.Fatalf("kept %s..%s", p.Events[0].Id, p.Events[MaxRows-1].Id)
	}
}

func TestTrailingPassResultsDoNotTruncate(t *testing.T) {
	build := func(fails int) *unstructured.Unstructured {
		rs := []string{}
		for i := 0; i < fails; i++ {
			rs = append(rs, `{"source":"kyverno","policy":"p","result":"fail"}`)
		}
		rs = append(rs, `{"result":"pass"}`, `{"result":"skip"}`)
		return obj(t, `{"apiVersion":"wgpolicyk8s.io/v1alpha2","kind":"PolicyReport","metadata":{"name":"r"},"results":[`+strings.Join(rs, ",")+`]}`)
	}
	p := Empty("")
	report(p, build(MaxRows))
	if len(p.Violations) != MaxRows || p.Summary.Truncated {
		t.Fatalf("rows=%d truncated=%v", len(p.Violations), p.Summary.Truncated)
	}
	p = Empty("")
	report(p, build(MaxRows+1))
	if !p.Summary.Truncated {
		t.Fatal("dropped a real row without truncation")
	}
}

func TestForgedGatekeeperFieldsAreClipped(t *testing.T) {
	huge := strings.Repeat("x", 100000)
	e := event(obj(t, fmt.Sprintf(`{"apiVersion":"v1","kind":"Event","metadata":{"name":"ev","annotations":{"process":"admission","constraint_kind":%q,"constraint_name":%q,"constraint_namespace":%q,"resource_kind":%q,"resource_name":%q,"resource_namespace":%q,"resource_api_version":%q,"resource_group":%q}},"reason":"FailedAdmission","source":{"component":"gatekeeper-webhook"},"lastTimestamp":"2026-11-19T00:00:00Z"}`, huge, huge, huge, huge, huge, huge, huge, huge)))
	if e == nil {
		t.Fatal("event rejected")
	}
	for _, r := range []*cloudv1.PolicyObjectReference{e.Policy, e.Resource} {
		if len(r.Kind) > 128 || len(r.Name) > 253 || len(r.Namespace) > 63 || len(r.ApiVersion) > 128 {
			t.Fatalf("unclipped %d %d %d %d", len(r.Kind), len(r.Name), len(r.Namespace), len(r.ApiVersion))
		}
	}
	p := Empty("")
	constraint(p, obj(t, fmt.Sprintf(`{"apiVersion":"constraints.gatekeeper.sh/v1beta1","kind":%q,"metadata":{"name":%q,"namespace":%q},"status":{"totalViolations":1,"auditTimestamp":"2026-11-19T00:00:00Z","violations":[{"kind":"Pod","name":"bad","group":%q,"version":%q}]}}`, huge, huge, huge, huge, huge)))
	v := p.Violations[0]
	if len(v.Policy.Kind) > 128 || len(v.Policy.Name) > 253 || len(v.Policy.Namespace) > 63 || len(v.Policy.ApiVersion) > 128 || len(v.Resource.ApiVersion) > 128 {
		t.Fatal("constraint fields unclipped")
	}
}
