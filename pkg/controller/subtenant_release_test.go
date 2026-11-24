package controller

import (
	"errors"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
)

// deletingParent deletes the provisioned SubTenant and marks its parent
// cluster's row DELETING (forced or not).
func (w *stWorld) deletingParent(t *testing.T, state string, force bool) {
	t.Helper()
	if err := w.management.Delete(t.Context(), w.sub(t)); err != nil {
		t.Fatal(err)
	}
	parent := w.parent(t)
	row := `{"etag":"e","state":"` + state + `"`
	if force {
		row += `,"deleteForce":true`
	}
	parent.Annotations = map[string]string{cloudstore.AnnotationRow: row + "}"}
	if err := w.management.Update(t.Context(), parent); err != nil {
		t.Fatal(err)
	}
}

func (w *stWorld) released(t *testing.T) bool {
	t.Helper()
	err := w.management.Get(t.Context(), client.ObjectKey{Namespace: "tenant-acme", Name: stID}, &cloud.SubTenant{})
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	return apierrors.IsNotFound(err)
}

// A SubTenant being deleted whose cluster is being force-deleted is let go
// at once, without a workload call: the cluster takes its namespaces down.
func TestSubTenantReleasedWithForceDeletedParent(t *testing.T) {
	w := newSubTenantWorld(t)
	w.provision(t)
	w.deletingParent(t, "CLUSTER_STATE_DELETING", true)
	calls, writes := w.clients.calls, len(w.writes)

	w.pass(t)

	if !w.released(t) {
		t.Fatal("finalizer kept for a force-deleted parent")
	}
	if w.clients.calls != calls || len(w.writes) != writes {
		t.Fatal("released SubTenant reached the workload API")
	}
}

// A SubTenant being deleted whose deleting cluster's API cannot be reached
// is let go once that lasted the teardown timeout; a healthy cluster's never.
func TestSubTenantReleasedWhenDeletingParentUnreachable(t *testing.T) {
	for _, test := range []struct {
		name, state string
		release     bool
	}{
		{name: "deleting parent", state: "CLUSTER_STATE_DELETING", release: true},
		{name: "running parent", state: "CLUSTER_STATE_RUNNING"},
	} {
		t.Run(test.name, func(t *testing.T) {
			w := newSubTenantWorld(t)
			w.deps.TeardownTimeout = time.Hour
			w.provision(t)
			w.deletingParent(t, test.state, false)
			w.clients.err = errors.New("dial tcp: no route to host")
			uid := w.sub(t).UID
			t.Cleanup(func() { subTenantUnreachable.Delete(uid) })

			w.pass(t)
			if w.released(t) {
				t.Fatal("released before the teardown timeout")
			}
			subTenantUnreachable.Store(uid, time.Now().Add(-2*time.Hour))
			w.pass(t)

			if got := w.released(t); got != test.release {
				t.Fatalf("released = %t, want %t", got, test.release)
			}
		})
	}
}
