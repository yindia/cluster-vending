package cloudstore_test

import (
	"testing"
	"time"

	"github.com/yindia/cluster-vending/pkg/database/model"
)

// A retry is recorded on the cluster and read back on its row; a second
// request adds to the first; the etag the user holds stays; clearing it
// removes only the request that was acted on.
func TestRequestAndClearRetry(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	c := w.cluster(t, "prod")
	before, err := w.s.Clusters().Get(ctx, "acme", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	first := model.Retry{At: time.Unix(100, 0).UTC(), By: "alice", Components: []string{"karpenter"}}
	second := model.Retry{At: time.Unix(200, 0).UTC(), By: "bob", Upgrade: true}

	if err := w.s.Clusters().RequestRetry(ctx, "acme", c.ID, first); err != nil {
		t.Fatal(err)
	}
	if err := w.s.Clusters().RequestRetry(ctx, "acme", c.ID, second); err != nil {
		t.Fatal(err)
	}
	row, err := w.s.Clusters().Get(ctx, "acme", c.ID)

	if err != nil || row.Retry == nil || !row.Retry.Has("karpenter") || !row.Retry.Upgrade || !row.Retry.At.Equal(second.At) {
		t.Fatalf("retry = %+v, %v", row.Retry, err)
	}
	if row.ETag != before.ETag {
		t.Errorf("etag changed: %s, was %s", row.ETag, before.ETag)
	}
	// Acted on an older request: a newer one stays.
	if err := w.s.Clusters().ClearRetry(ctx, "acme", c.ID, first.At); err != nil {
		t.Fatal(err)
	}
	if row, _ := w.s.Clusters().Get(ctx, "acme", c.ID); row.Retry == nil {
		t.Fatal("a newer request was cleared")
	}
	if err := w.s.Clusters().ClearRetry(ctx, "acme", c.ID, second.At); err != nil {
		t.Fatal(err)
	}
	if row, _ := w.s.Clusters().Get(ctx, "acme", c.ID); row.Retry != nil {
		t.Fatalf("retry = %+v, want it cleared", row.Retry)
	}
}
