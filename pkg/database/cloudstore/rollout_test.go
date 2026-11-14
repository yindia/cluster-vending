package cloudstore_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
)

// A cluster's rollout record is stored on its status and read back on the
// row; the reconciler's own status writes keep it, and the etag the user
// holds does not change.
func TestSetRollout(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	c := w.cluster(t, "prod")
	before, err := w.s.Clusters().Get(ctx, "acme", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	record := model.JSON(`{"channel":"alpha","fromVersion":"1.36","toVersion":"1.37",` +
		`"startTime":"2026-11-15T02:00:00Z","result":"CLUSTER_ROLLOUT_RESULT_IN_PROGRESS","tier":2}`)

	if err := w.s.Clusters().SetRollout(ctx, "acme", c.ID, record); err != nil {
		t.Fatal(err)
	}
	if err := w.s.Clusters().SetStatus(ctx, "acme", c.ID, before.ETag, model.JSON(`{"state":"CLUSTER_STATE_RUNNING"}`)); err != nil {
		t.Fatal(err)
	}
	row, err := w.s.Clusters().Get(ctx, "acme", c.ID)

	if err != nil || !bytes.Contains(row.Rollout, []byte(`"toVersion":"1.37"`)) || !bytes.Contains(row.Rollout, []byte("IN_PROGRESS")) {
		t.Fatalf("rollout = %s, %v", row.Rollout, err)
	}
	if row.ETag != before.ETag {
		t.Errorf("etag changed: %s, was %s", row.ETag, before.ETag)
	}
	if err := w.s.Clusters().SetRollout(ctx, "acme", c.ID, model.JSON("not json")); !errors.Is(err, repository.ErrInvalid) {
		t.Errorf("garbage stored: %v", err)
	}
}
