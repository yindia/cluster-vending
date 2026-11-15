package cloudstore_test

import (
	"errors"
	"testing"

	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
)

// The etag is the user's: the controller recording its progress (state,
// generations) keeps it, so an update the user sends with the etag they
// got still succeeds; a change of what the user asked for rotates it.
func TestRecordKeepsTheUsersETag(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	c := w.cluster(t, "prod")
	users, err := w.s.Clusters().Get(ctx, "acme", c.ID)
	if err != nil {
		t.Fatal(err)
	}

	progress, err := w.s.Clusters().Get(ctx, "acme", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	progress.State, progress.ObservedGeneration = "CLUSTER_STATE_RUNNING", progress.Generation
	if err := w.s.Clusters().Record(ctx, progress); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if progress.ETag != users.ETag {
		t.Fatalf("Record changed the etag: %s, was %s", progress.ETag, users.ETag)
	}

	// The API's update reads the row again and checks the user's etag.
	fresh, err := w.s.Clusters().Get(ctx, "acme", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.ETag != users.ETag || fresh.State != "CLUSTER_STATE_RUNNING" {
		t.Fatalf("after Record: etag %s state %s", fresh.ETag, fresh.State)
	}
	fresh.DisplayName, fresh.Generation = "Prod", fresh.Generation+1
	if err := w.s.Clusters().Update(ctx, fresh); err != nil {
		t.Fatalf("the user's update: %v", err)
	}
	if fresh.ETag == users.ETag {
		t.Error("a change of what the user asked for kept the etag")
	}
	got, err := w.s.Clusters().Get(ctx, "acme", c.ID)
	if err != nil || got.DisplayName != "Prod" || got.State != "CLUSTER_STATE_RUNNING" {
		t.Fatalf("after the update: %+v, %v; want the user's change and the recorded state", got, err)
	}
}

// A row read before another write is not written over it, whichever kind
// of write each is: neither the user's change nor the controller's
// progress is lost.
func TestWritesOfAStaleRowAreRefused(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	c := w.cluster(t, "prod")
	read := func() *model.Cluster {
		t.Helper()
		row, err := w.s.Clusters().Get(ctx, "acme", c.ID)
		if err != nil {
			t.Fatal(err)
		}
		return row
	}

	// The API read the row, the controller recorded progress, then the API
	// writes: refused (it would put the old state back).
	api, controller := read(), read()
	controller.State = "CLUSTER_STATE_RUNNING"
	if err := w.s.Clusters().Record(ctx, controller); err != nil {
		t.Fatal(err)
	}
	api.DisplayName = "Prod"
	if err := w.s.Clusters().Update(ctx, api); !errors.Is(err, repository.ErrConflict) {
		t.Fatalf("Update over recorded progress: %v, want ErrConflict", err)
	}

	// The controller read the row, the user changed it, then the
	// controller records: refused (it would put the old spec back).
	controller, api = read(), read()
	api.DisplayName, api.Generation = "Prod", api.Generation+1
	if err := w.s.Clusters().Update(ctx, api); err != nil {
		t.Fatal(err)
	}
	controller.ObservedGeneration = controller.Generation
	if err := w.s.Clusters().Record(ctx, controller); !errors.Is(err, repository.ErrConflict) {
		t.Fatalf("Record over the user's change: %v, want ErrConflict", err)
	}

	// A status write in between (the reconciler's own) refuses neither.
	row := read()
	if err := w.s.Clusters().SetStatus(ctx, "acme", c.ID, row.ETag, model.JSON(`{"state":"CLUSTER_STATE_RUNNING"}`)); err != nil {
		t.Fatal(err)
	}
	row.AppliedGeneration = row.Generation
	if err := w.s.Clusters().Record(ctx, row); err != nil {
		t.Errorf("Record after a status write: %v", err)
	}
	// And one row object can be written twice in a row.
	row.ObservedGeneration = row.Generation
	if err := w.s.Clusters().Record(ctx, row); err != nil {
		t.Errorf("the second Record of one row: %v", err)
	}
}

// Networks keep the same rule.
func TestNetworkRecordKeepsTheETag(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	n, err := w.s.Networks().Get(ctx, "acme", w.network.ID)
	if err != nil {
		t.Fatal(err)
	}
	etag := n.ETag
	n.State = "NETWORK_STATE_READY"

	if err := w.s.Networks().Record(ctx, n); err != nil {
		t.Fatal(err)
	}

	got, err := w.s.Networks().Get(ctx, "acme", w.network.ID)
	if err != nil || got.ETag != etag || got.State != "NETWORK_STATE_READY" {
		t.Fatalf("network = etag %s state %s (%v), want etag %s READY", got.ETag, got.State, err, etag)
	}
	stale := *n
	stale.Seen = "" // a row written without a read keeps working as before
	if err := w.s.Networks().Update(ctx, &stale); err != nil {
		t.Errorf("Update of a row without Seen: %v", err)
	}
}
