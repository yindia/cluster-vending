package cloudstore_test

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	"github.com/yindia/cluster-vending/pkg/kube"
)

// ListFor returns the events about one object of the tenant, newest
// first, whichever events API wrote them, and nothing of another object
// or another tenant.
func TestEventsListFor(t *testing.T) {
	k := newClient(t)
	s := cloudstore.New(k)
	ns := kube.TenantNamespace("acme")
	at := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	event := func(namespace, name, kind, object, reason, eventType string, edit func(*eventsv1.Event)) {
		t.Helper()
		e := &eventsv1.Event{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
			Regarding: corev1.ObjectReference{Kind: kind, Namespace: namespace, Name: object},
			Reason:    reason, Type: eventType, Note: "about " + object, Action: "Reconcile",
			ReportingController: "cluster-vending-controller", ReportingInstance: "test"}
		edit(e)
		if err := k.Create(t.Context(), e); err != nil {
			t.Fatalf("create event %s: %v", name, err)
		}
	}
	// Recorded once, through the events.k8s.io recorder.
	event(ns, "e1", "Cluster", "prod", "Provisioning", "Normal", func(e *eventsv1.Event) {
		e.EventTime = metav1.NewMicroTime(at)
	})
	// A series: counted, and as new as it was seen last.
	event(ns, "e2", "Cluster", "prod", "Failed", "Warning", func(e *eventsv1.Event) {
		e.EventTime = metav1.NewMicroTime(at.Add(time.Minute))
		e.Series = &eventsv1.EventSeries{Count: 4, LastObservedTime: metav1.NewMicroTime(at.Add(10 * time.Minute))}
	})
	// Written through the old events API.
	event(ns, "e3", "Cluster", "prod", "Running", "Normal", func(e *eventsv1.Event) {
		e.DeprecatedLastTimestamp, e.DeprecatedCount = metav1.NewTime(at.Add(5*time.Minute)), 2
	})
	// Not this object's.
	event(ns, "e4", "Cluster", "staging", "Running", "Normal", func(e *eventsv1.Event) { e.EventTime = metav1.NewMicroTime(at) })
	event(ns, "e5", "Network", "prod", "Ready", "Normal", func(e *eventsv1.Event) { e.EventTime = metav1.NewMicroTime(at) })
	event(kube.TenantNamespace("globex"), "e6", "Cluster", "prod", "Running", "Normal", func(e *eventsv1.Event) {
		e.EventTime = metav1.NewMicroTime(at)
	})

	got, err := s.Events().ListFor(t.Context(), "acme", "Cluster", "prod")

	if err != nil {
		t.Fatalf("ListFor: %v", err)
	}
	var have []string
	for _, e := range got {
		have = append(have, e.Reason)
	}
	if strings.Join(have, ",") != "Failed,Running,Provisioning" {
		t.Fatalf("events = %v, want Failed, Running, Provisioning (newest first)", have)
	}
	failed, running, first := got[0], got[1], got[2]
	if failed.Count != 4 || failed.Type != "Warning" || !failed.Time.Equal(at.Add(10*time.Minute)) || failed.Message != "about prod" {
		t.Errorf("the series = %+v", failed)
	}
	if running.Count != 2 || !running.Time.Equal(at.Add(5*time.Minute)) {
		t.Errorf("the old-API event = %+v", running)
	}
	if first.Count != 1 || !first.Time.Equal(at) || first.Kind != "Cluster" || first.Name != "prod" {
		t.Errorf("the single event = %+v", first)
	}

	if none, err := s.Events().ListFor(t.Context(), "acme", "Network", "other"); err != nil || len(none) != 0 {
		t.Errorf("an object without events: %v, %v", none, err)
	}
	if none, err := s.Events().ListFor(t.Context(), "Not A Tenant", "Cluster", "prod"); err != nil || len(none) != 0 {
		t.Errorf("an impossible tenant: %v, %v", none, err)
	}
}
