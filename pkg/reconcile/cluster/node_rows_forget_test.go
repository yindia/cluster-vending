package cluster

import (
	"errors"
	"testing"
	"time"

	"github.com/yindia/cluster-vending/pkg/database/repository"

	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestNodeRowWritesForget(t *testing.T) {
	var w nodeRowWrites
	at := time.Unix(1000, 0)
	if !w.claim("c-1", at) || w.claim("c-1", at.Add(time.Second)) {
		t.Fatal("claim does not hold the interval")
	}
	w.forget("c-1")
	if len(w.last) != 0 {
		t.Fatalf("entries after forget: %v", w.last)
	}
	if !w.claim("c-1", at.Add(2*time.Second)) {
		t.Error("a forgotten cluster is not due again")
	}
	(*nodeRowWrites)(nil).forget("c-1") // nil-safe like claim
}

// The entry of a cluster is dropped when its deletion finishes.
func TestDeletionForgetsTheNodeRowClock(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{}, nil)
	ctx := t.Context()
	if err := w.svc.Reconcile(ctx, worldTenant, w.row.ID); err != nil {
		t.Fatal(err)
	}
	w.svc.nodeRows.claim(w.row.ID, time.Now())
	row := w.cluster(t)
	row.State = StateDeleting
	if err := w.store.Clusters().Update(ctx, row); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		if err := w.svc.Reconcile(ctx, worldTenant, w.row.ID); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}
	if _, err := w.store.Clusters().Get(ctx, worldTenant, w.row.ID); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("cluster row after the deletion: %v", err)
	}
	if n := len(w.svc.nodeRows.last); n != 0 {
		t.Fatalf("%d node row clocks left after the deletion", n)
	}
}
