package policy

import (
	"context"
	"fmt"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
)

// reportsCluster serves n PolicyReports (one failing result each) in pages
// of 500 with a continue token, and nothing else.
func reportsCluster(t *testing.T, n int) *fake.FakeDynamicClient {
	t.Helper()
	gvr := schema.GroupVersionResource{Group: "wgpolicyk8s.io", Version: "v1alpha2", Resource: "policyreports"}
	kinds := map[schema.GroupVersionResource]string{gvr: "PolicyReportList",
		{Group: "wgpolicyk8s.io", Version: "v1alpha2", Resource: "clusterpolicyreports"}: "ClusterPolicyReportList",
		{Group: "openreports.io", Version: "v1alpha1", Resource: "reports"}:              "ReportList",
		{Group: "openreports.io", Version: "v1alpha1", Resource: "clusterreports"}:       "ClusterReportList",
		{Version: "v1", Resource: "events"}:                                              "EventList",
	}
	d := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), kinds)
	served := 0
	d.PrependReactor("list", "policyreports", func(ktesting.Action) (bool, runtime.Object, error) {
		list := &unstructured.UnstructuredList{Object: map[string]any{"metadata": map[string]any{}}}
		for i := 0; i < 500 && served < n; i++ {
			list.Items = append(list.Items, *obj(t, fmt.Sprintf(`{"apiVersion":"wgpolicyk8s.io/v1alpha2","kind":"PolicyReport","metadata":{"name":"r%d"},"results":[{"source":"kyverno","policy":"p","rule":"r","result":"fail","resources":[{"kind":"Pod","namespace":"web","name":"pod-%d"}]}]}`, served, served)))
			served++
		}
		if served < n {
			list.Object["metadata"] = map[string]any{"continue": fmt.Sprint(served)}
		}
		return true, list, nil
	})
	return d
}

// A full collection reads every page of 500 and keeps every violation; the
// bounded one keeps what the Cluster object holds.
func TestFullCollectionKeepsMoreThanTheClusterObjectCaps(t *testing.T) {
	r := &Reader{Full: true, Dynamic: reportsCluster(t, 1200)}

	p := r.Collect(t.Context(), time.Now())

	if len(p.Violations) != 1200 || p.Summary.Truncated {
		t.Fatalf("%d violations, truncated %v, notices %v", len(p.Violations), p.Summary.Truncated, p.Summary.Notices)
	}

	capped := (&Reader{Dynamic: reportsCluster(t, 1200)}).Collect(t.Context(), time.Now())
	if len(capped.Violations) != MaxRows || !capped.Summary.Truncated {
		t.Fatalf("bounded: %d violations, truncated %v", len(capped.Violations), capped.Summary.Truncated)
	}
}

// A collection that hits the deadline says so: truncated and incomplete.
func TestFullCollectionMarksTheDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	p := (&Reader{Full: true, Dynamic: reportsCluster(t, 1200)}).Collect(ctx, time.Now())
	if p.Summary.CollectedAt != nil || !p.Summary.Incomplete {
		t.Fatalf("a collection past its deadline looks complete: %v", p.Summary)
	}
}
