package service

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect/v2"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

func mr(kind, name string, st cloudv1.ManagedResourceState) *cloudv1.ManagedResource {
	return &cloudv1.ManagedResource{Kind: kind, Name: name, State: st}
}

const (
	creating = cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_CREATING
	ready    = cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY
)

func TestWatchSendsSnapshotThenChanges(t *testing.T) {
	notFound := connect.NewError(connect.CodeNotFound, "gone")
	reads := []struct {
		snap Snapshot[*cloudv1.Cluster]
		err  error
	}{
		{snap: Snapshot[*cloudv1.Cluster]{Resource: &cloudv1.Cluster{Id: "c"}, Summary: "PROVISIONING",
			Resources: []*cloudv1.ManagedResource{mr("Role", "r", ready), mr("Cluster", "eks", creating)}}},
		// Nothing changed: nothing sent.
		{snap: Snapshot[*cloudv1.Cluster]{Resource: &cloudv1.Cluster{Id: "c"}, Summary: "PROVISIONING",
			Resources: []*cloudv1.ManagedResource{mr("Role", "r", ready), mr("Cluster", "eks", creating)}}},
		// EKS ready, add-on added; the cluster summary is unchanged.
		{snap: Snapshot[*cloudv1.Cluster]{Resource: &cloudv1.Cluster{Id: "c"}, Summary: "PROVISIONING",
			Resources: []*cloudv1.ManagedResource{mr("Role", "r", ready), mr("Cluster", "eks", ready), mr("Addon", "a", creating)}}},
		// A transient read error is retried.
		{err: connect.NewError(connect.CodeUnavailable, "blip")},
		// Cluster running: the cluster is sent again.
		{snap: Snapshot[*cloudv1.Cluster]{Resource: &cloudv1.Cluster{Id: "c"}, Summary: "RUNNING",
			Resources: []*cloudv1.ManagedResource{mr("Role", "r", ready), mr("Cluster", "eks", ready), mr("Addon", "a", ready)}}},
		{err: notFound},
	}
	i := 0
	read := func(context.Context) (Snapshot[*cloudv1.Cluster], error) {
		r := reads[i]
		i++
		return r.snap, r.err
	}
	var got []Message[*cloudv1.Cluster]
	send := func(m Message[*cloudv1.Cluster]) error { got = append(got, m); return nil }
	if err := Watch(context.Background(), time.Millisecond, read, send); err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("sent %d messages, want 4 (snapshot, changes, running, deleted)", len(got))
	}
	types := func(m Message[*cloudv1.Cluster]) map[string]cloudv1.ManagedResourceEvent_Type {
		out := map[string]cloudv1.ManagedResourceEvent_Type{}
		for _, e := range m.Events {
			out[e.GetResource().GetName()] = e.GetType()
		}
		return out
	}
	if !got[0].Changed || len(got[0].Events) != 2 || types(got[0])["eks"] != cloudv1.ManagedResourceEvent_TYPE_ADDED {
		t.Errorf("first = %+v, want the cluster and 2 ADDED", got[0])
	}
	if got[1].Changed || types(got[1])["eks"] != cloudv1.ManagedResourceEvent_TYPE_MODIFIED ||
		types(got[1])["a"] != cloudv1.ManagedResourceEvent_TYPE_ADDED || len(got[1].Events) != 2 {
		t.Errorf("second = %+v, want eks MODIFIED, a ADDED, no cluster", got[1])
	}
	if !got[2].Changed || len(got[2].Events) != 1 {
		t.Errorf("third = %+v, want the cluster and the add-on MODIFIED", got[2])
	}
	if !got[3].Deleted || len(got[3].Events) != 3 || types(got[3])["r"] != cloudv1.ManagedResourceEvent_TYPE_DELETED {
		t.Errorf("last = %+v, want deleted with 3 DELETED", got[3])
	}
}

func TestWatchMissingResourceFails(t *testing.T) {
	read := func(context.Context) (Snapshot[*cloudv1.Cluster], error) {
		return Snapshot[*cloudv1.Cluster]{}, connect.NewError(connect.CodeNotFound, "no such cluster")
	}
	err := Watch(context.Background(), time.Millisecond, read, func(Message[*cloudv1.Cluster]) error { return nil })
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("err = %v, want NOT_FOUND", err)
	}
}

func TestWatchGivesUpAfterRepeatedFailures(t *testing.T) {
	read := func(context.Context) (Snapshot[*cloudv1.Cluster], error) {
		return Snapshot[*cloudv1.Cluster]{}, connect.NewError(connect.CodeUnavailable, "down")
	}
	err := Watch(context.Background(), time.Millisecond, read, func(Message[*cloudv1.Cluster]) error { return nil })
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("err = %v, want UNAVAILABLE", err)
	}
}
