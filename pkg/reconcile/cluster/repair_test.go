package cluster

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	awskms "github.com/upbound/provider-aws/v2/apis/namespaced/kms/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/yindia/cluster-vending/pkg/crossplane"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// appliedWorld is a cluster whose generation is applied in full: every
// object of its render exists and is recorded. logs is what the
// reconciler logged since.
func appliedWorld(t *testing.T) (w world, logs *bytes.Buffer) {
	t.Helper()
	logs = &bytes.Buffer{}
	w = newWorld(t, interceptor.Funcs{}, nil, func(d *reconcile.Deps) {
		d.Logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	})
	if err := w.svc.Reconcile(t.Context(), worldTenant, w.row.ID); err != nil {
		t.Fatal(err)
	}
	if got := w.cluster(t); got.AppliedGeneration != got.Generation {
		t.Fatalf("applied generation %d of %d", got.AppliedGeneration, got.Generation)
	}
	logs.Reset()
	return w, logs
}

func (w world) prefix() string { return crossplaneaws.ClusterPrefix(worldTenant, w.row.ID, w.row.Name) }

// recorded is the cluster's record of an object; the test fails without.
func (w world) recorded(t *testing.T, kind, name string) model.ManagedResource {
	t.Helper()
	records, err := w.store.ManagedResources().ListByOwner(t.Context(), model.OwnerCluster, w.row.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range records {
		if r.Kind == kind && r.Name == name {
			return r
		}
	}
	t.Fatalf("no record of %s %s", kind, name)
	return model.ManagedResource{}
}

// object reads a recorded object; nil when it is not in the management
// cluster.
func (w world) object(t *testing.T, r model.ManagedResource) *unstructured.Unstructured {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(r.APIVersion)
	u.SetKind(r.Kind)
	if err := w.kube.Get(t.Context(), client.ObjectKey{Namespace: r.Namespace, Name: r.Name}, u); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return nil
		}
		t.Fatal(err)
	}
	return u
}

func (w world) remove(t *testing.T, r model.ManagedResource) {
	t.Helper()
	u := w.object(t, r)
	if u == nil {
		t.Fatalf("%s %s does not exist", r.Kind, r.Name)
	}
	if err := w.kube.Delete(t.Context(), u); err != nil {
		t.Fatal(err)
	}
}

// A recorded object that was deleted from the management cluster is
// created again by the next pass, although the cluster's generation is
// applied: the cluster does not stay FAILED until an unrelated update.
func TestReconcileRecreatesADeletedObject(t *testing.T) {
	cases := []struct {
		name string
		ref  func(w world) (kind, name string)
	}{
		{name: "the Helm ProviderConfig", ref: func(w world) (string, string) {
			return "ProviderConfig", crossplaneaws.HelmProviderConfigName(w.prefix())
		}},
		{name: "the node group", ref: func(w world) (string, string) { return "NodeGroup", w.prefix() + "-default" }},
		{name: "the ClusterAuth", ref: func(w world) (string, string) { return "ClusterAuth", w.prefix() + "-auth" }},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange
			w, logs := appliedWorld(t)
			kind, name := tt.ref(w)
			r := w.recorded(t, kind, name)
			w.remove(t, r)

			// Act
			err := w.svc.Reconcile(t.Context(), worldTenant, w.row.ID)

			// Assert
			if err != nil {
				t.Fatal(err)
			}
			if w.object(t, r) == nil {
				t.Fatalf("%s %s was not created again", kind, name)
			}
			if want := "cluster repaired: recreated " + kind + " " + name; !strings.Contains(logs.String(), want) {
				t.Errorf("log lacks %q:\n%s", want, logs)
			}
			if got := w.cluster(t); got.AppliedGeneration != got.Generation {
				t.Errorf("applied generation %d of %d after the repair", got.AppliedGeneration, got.Generation)
			}

			// A second pass has nothing to repair and says nothing.
			logs.Reset()
			if err := w.svc.Reconcile(t.Context(), worldTenant, w.row.ID); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(logs.String(), "repaired") {
				t.Errorf("a pass with nothing missing logged a repair:\n%s", logs)
			}
		})
	}
}

// A cluster whose recorded status names a missing object is looked at
// again soon: its repair must not wait for an unrelated event.
func TestMissingObject(t *testing.T) {
	missing := cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_MISSING
	ready := cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY
	status := func(states ...cloudv1.ManagedResourceState) []byte {
		st := &cloudv1.ClusterStatus{}
		for _, s := range states {
			st.ManagedResources = append(st.ManagedResources, &cloudv1.ManagedResource{Kind: "ProviderConfig", Name: "x", State: s})
		}
		raw, err := Encode(st)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	tests := []struct {
		name   string
		state  string
		status []byte
		want   bool
	}{
		{name: "everything there", state: StateRunning, status: status(ready, ready)},
		{name: "an object is missing", state: StateRunning, status: status(ready, missing), want: true},
		{name: "missing while updating", state: StateUpdating, status: status(missing), want: true},
		{name: "missing while it is deleted", state: StateDeleting, status: status(missing)},
		{name: "an imported cluster", state: StateImported, status: status(missing), want: true},
		{name: "a blocked import", state: StateBlocked, status: status(missing)},
		{name: "no status yet", state: StateProvisioning},
		{name: "a status this build cannot read", state: StateRunning, status: []byte("{")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			row := &model.Cluster{Resource: model.Resource{State: tt.state, Status: tt.status}}

			if got := MissingObject(row); got != tt.want {
				t.Errorf("MissingObject = %v, want %v", got, tt.want)
			}
		})
	}
}

// What cannot be created again blindly is left alone and reported as
// before; so is everything else of the cluster while it is missing.
func TestReconcileDoesNotRecreate(t *testing.T) {
	missing := func(t *testing.T, w world, kind string) *cloudv1.ManagedResource {
		t.Helper()
		records, err := w.store.ManagedResources().ListByOwner(t.Context(), model.OwnerCluster, w.row.ID)
		if err != nil {
			t.Fatal(err)
		}
		views, err := w.svc.deps.Observe(t.Context(), records)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range views {
			if v.GetKind() == kind && v.GetState() == cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_MISSING {
				return v
			}
		}
		return nil
	}
	t.Run("a kind whose cloud ID AWS assigns", func(t *testing.T) {
		// Arrange: an encryption key, recorded and gone, next to a deleted
		// object that alone would be created again.
		w, logs := appliedWorld(t)
		key := &awskms.Key{ObjectMeta: metav1.ObjectMeta{Namespace: worldNS, Name: w.prefix() + "-secrets",
			Labels: crossplane.ClusterLabels(worldTenant, w.row.ID)}}
		if err := w.svc.deps.Record(t.Context(), reconcile.Owner{TenantID: worldTenant, Kind: model.OwnerCluster, ID: w.row.ID},
			[]client.Object{key}); err != nil {
			t.Fatal(err)
		}
		helm := w.recorded(t, "ProviderConfig", crossplaneaws.HelmProviderConfigName(w.prefix()))
		w.remove(t, helm)

		// Act
		err := w.svc.Reconcile(t.Context(), worldTenant, w.row.ID)

		// Assert
		if err != nil {
			t.Fatal(err)
		}
		if w.object(t, helm) != nil {
			t.Error("the ProviderConfig was created again although a key is missing")
		}
		if strings.Contains(logs.String(), "repaired") {
			t.Errorf("a repair was logged:\n%s", logs)
		}
		if missing(t, w, "Key") == nil {
			t.Error("the missing key is not reported")
		}
	})
	t.Run("the node group being retired", func(t *testing.T) {
		// Arrange: a second node group recorded after the first (its
		// replacement exists), the retired one already gone.
		w, logs := appliedWorld(t)
		replacement := &awseks.NodeGroup{ObjectMeta: metav1.ObjectMeta{Namespace: worldNS, Name: w.prefix() + "-default-new",
			Labels: crossplane.ClusterLabels(worldTenant, w.row.ID)}}
		if err := w.kube.Create(t.Context(), replacement); err != nil {
			t.Fatal(err)
		}
		if err := w.svc.deps.Record(t.Context(), reconcile.Owner{TenantID: worldTenant, Kind: model.OwnerCluster, ID: w.row.ID},
			[]client.Object{replacement}); err != nil {
			t.Fatal(err)
		}
		retired := w.recorded(t, "NodeGroup", w.prefix()+"-default")
		w.remove(t, retired)

		// Act
		err := w.svc.Reconcile(t.Context(), worldTenant, w.row.ID)

		// Assert
		if err != nil {
			t.Fatal(err)
		}
		if w.object(t, retired) != nil {
			t.Error("the retired node group was created again")
		}
		if strings.Contains(logs.String(), "recreated NodeGroup") {
			t.Errorf("a repair of the node group was logged:\n%s", logs)
		}
	})
	t.Run("an object that is being deleted", func(t *testing.T) {
		// Arrange: the ClusterAuth has a finalizer and a deletion
		// timestamp: DELETING, not missing.
		w, logs := appliedWorld(t)
		auth := w.recorded(t, "ClusterAuth", w.prefix()+"-auth")
		u := w.object(t, auth)
		u.SetFinalizers([]string{"finalizer.managedresource.crossplane.io"})
		if err := w.kube.Update(t.Context(), u); err != nil {
			t.Fatal(err)
		}
		w.remove(t, auth)

		// Act
		err := w.svc.Reconcile(t.Context(), worldTenant, w.row.ID)

		// Assert
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(logs.String(), "repaired") || strings.Contains(logs.String(), "applied again") {
			t.Errorf("the cluster was applied again:\n%s", logs)
		}
		if got := w.object(t, auth); got == nil || got.GetDeletionTimestamp() == nil {
			t.Error("the object being deleted was replaced")
		}
	})
	t.Run("on a cluster that is being deleted", func(t *testing.T) {
		// Arrange
		w, logs := appliedWorld(t)
		helm := w.recorded(t, "ProviderConfig", crossplaneaws.HelmProviderConfigName(w.prefix()))
		w.remove(t, helm)
		row := w.cluster(t)
		row.State = StateDeleting
		if err := w.store.Clusters().Update(t.Context(), row); err != nil {
			t.Fatal(err)
		}

		// Act
		_ = w.svc.Reconcile(t.Context(), worldTenant, w.row.ID)

		// Assert
		if w.object(t, helm) != nil || strings.Contains(logs.String(), "repaired") {
			t.Errorf("a deleting cluster was repaired:\n%s", logs)
		}
	})
}
