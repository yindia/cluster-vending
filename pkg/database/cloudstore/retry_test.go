package cloudstore_test

import (
	"context"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
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

// A ClearRetry that acts between RequestRetry's read and its write is not
// undone: the request is merged again from what is stored, so the request
// that was acted on does not come back.
func TestRequestRetryDoesNotRestoreARequestClearedMeanwhile(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	c := w.cluster(t, "prod")
	first := model.Retry{At: time.Unix(100, 0).UTC(), By: "alice", Components: []string{"karpenter"}}
	second := model.Retry{At: time.Unix(200, 0).UTC(), By: "bob", Upgrade: true}
	if err := w.s.Clusters().RequestRetry(ctx, "acme", c.ID, first); err != nil {
		t.Fatal(err)
	}
	armed := true
	k := interceptor.NewClient(w.k.(client.WithWatch), interceptor.Funcs{Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
		if _, ok := obj.(*cloud.Cluster); ok && armed {
			armed = false // the controller acted on the first request meanwhile
			if err := w.s.Clusters().ClearRetry(ctx, "acme", c.ID, first.At); err != nil {
				t.Fatal(err)
			}
		}
		return cl.Patch(ctx, obj, patch, opts...)
	}})
	if err := cloudstore.New(k).Clusters().RequestRetry(ctx, "acme", c.ID, second); err != nil {
		t.Fatal(err)
	}
	row, err := w.s.Clusters().Get(ctx, "acme", c.ID)
	if err != nil || row.Retry == nil || !row.Retry.Upgrade || row.Retry.Has("karpenter") {
		t.Fatalf("retry = %+v, %v; want only the second request", row.Retry, err)
	}
}
