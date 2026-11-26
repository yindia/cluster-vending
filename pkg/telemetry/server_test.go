package telemetry

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect/v2"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
)

func TestAuthCountsRefusalsByBoundedReason(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	a := NewAuth(reg)
	a.Failed(AuthRejected)
	a.Failed("token abc.def expired")
	if value(t, reg, "auth_failures_total", map[string]string{"reason": "rejected"}) != 1 ||
		value(t, reg, "auth_failures_total", map[string]string{"reason": "other"}) != 1 ||
		value(t, reg, "auth_failures_total", map[string]string{"reason": "missing_token"}) != 0 {
		t.Fatal("auth failures miscounted")
	}
}

func TestStoreMeasuresCallsByOperationKindAndOutcome(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := cloud.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	reg := prometheus.NewPedanticRegistry()
	s := NewStore(reg)
	c := s.Wrap(fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&cloud.Cluster{}).Build())
	ctx := t.Context()
	cluster := &cloud.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "secret-name", Namespace: "tenant-a"}}
	if err := c.Create(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKey{Namespace: "tenant-a", Name: "missing"}, &cloud.Cluster{}); !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	if err := c.List(ctx, &cloud.ClusterList{}); err != nil {
		t.Fatal(err)
	}
	if err := c.Status().Update(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "y"}}); err != nil {
		t.Fatal(err)
	}
	checks := []map[string]string{
		{"operation": "create", "kind": "Cluster", "outcome": "ok"},
		{"operation": "get", "kind": "Cluster", "outcome": "not_found"},
		{"operation": "list", "kind": "Cluster", "outcome": "ok"},
		{"operation": "update_status", "kind": "Cluster", "outcome": "ok"},
		{"operation": "create", "kind": "other", "outcome": "ok"},
	}
	for _, labels := range checks {
		if got := value(t, reg, "store_requests_total", labels); got != 1 {
			t.Errorf("%v = %v", labels, got)
		}
	}
	if got := value(t, reg, "store_request_duration_seconds", map[string]string{"operation": "get", "kind": "Cluster"}); got != 1 {
		t.Fatalf("durations %v", got)
	}
}

func TestOutcomeIsBounded(t *testing.T) {
	gr := schema.GroupResource{Group: "cloud.cluster-vending.io", Resource: "clusters"}
	for err, want := range map[error]string{
		nil:                                          "ok",
		apierrors.NewConflict(gr, "x", nil):          "conflict",
		apierrors.NewAlreadyExists(gr, "x"):          "already_exists",
		apierrors.NewForbidden(gr, "x", nil):         "forbidden",
		apierrors.NewTimeoutError("slow", 1):         "timeout",
		errors.New("connection refused to 10.0.0.1"): "error",
	} {
		if got := Outcome(err); got != want {
			t.Errorf("Outcome(%v) = %q, want %q", err, got, want)
		}
	}
}

func TestLiveClientOutcomes(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	l := NewLiveClient(reg)
	remote := connect.NewError(connect.CodeUnavailable, "busy")
	for _, err := range []error{nil, context.DeadlineExceeded, errors.New("dial tcp: connection refused")} {
		l.Start("GetNodeResources")(err)
	}
	if liveOutcome(remote) != "unreachable" { // not marked remote: the call did not reach the reader
		t.Fatal(liveOutcome(remote))
	}
	for _, outcome := range []string{"ok", "timeout", "unreachable"} {
		if got := value(t, reg, "live_client_requests_total", map[string]string{"method": "GetNodeResources", "outcome": outcome}); got != 1 {
			t.Errorf("%s = %v", outcome, got)
		}
	}
	if value(t, reg, "live_client_in_flight", nil) != 0 {
		t.Fatal("in flight not released")
	}
}
