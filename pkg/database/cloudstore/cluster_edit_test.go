package cloudstore_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// metadataGenerations makes the fake API server count metadata.generation as
// the real one does: 1 on create, one more with every change of a Cluster's
// spec.
func metadataGenerations(k client.Client) client.Client {
	return interceptor.NewClient(k.(client.WithWatch), interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*cloud.Cluster); ok {
				obj.SetGeneration(1)
			}
			return c.Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if next, ok := obj.(*cloud.Cluster); ok {
				have := &cloud.Cluster{}
				if err := c.Get(ctx, client.ObjectKeyFromObject(obj), have); err != nil {
					return err
				}
				next.Generation = have.Generation
				if !reflect.DeepEqual(have.Spec, next.Spec) {
					next.Generation++
				}
			}
			return c.Update(ctx, obj, opts...)
		},
	})
}

type editWorld struct {
	world
	k client.Client
	s repository.Store
}

func newEditWorld(t *testing.T) editWorld {
	t.Helper()
	w := newWorld(t)
	k := metadataGenerations(w.k)
	return editWorld{world: w, k: k, s: cloudstore.New(k)}
}

// kubectlEdit is a spec edit made on the object, as kubectl does.
func (w editWorld) kubectlEdit(t *testing.T, name string, retention int32) {
	t.Helper()
	obj := &cloud.Cluster{}
	if err := w.k.Get(t.Context(), client.ObjectKey{Namespace: namespace, Name: name}, obj); err != nil {
		t.Fatal(err)
	}
	obj.Spec.AWS.ControlPlaneLogGroup.RetentionDays = retention
	if err := w.k.Update(t.Context(), obj); err != nil {
		t.Fatal(err)
	}
}

func (w editWorld) object(t *testing.T, name string) *cloud.Cluster {
	t.Helper()
	obj := &cloud.Cluster{}
	if err := w.k.Get(t.Context(), client.ObjectKey{Namespace: namespace, Name: name}, obj); err != nil {
		t.Fatal(err)
	}
	return obj
}

func (w editWorld) create(t *testing.T, name string) *model.Cluster {
	t.Helper()
	c := &model.Cluster{TenantID: "acme", Name: name, NetworkID: w.network.ID, Provider: "aws", Region: "us-east-1",
		Resource: model.Resource{State: "CLUSTER_STATE_PROVISIONING"}}
	c.Spec = document(t, clusterSpec(c))
	if err := w.s.Clusters().Create(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	return c
}

func retentionOf(t *testing.T, row *model.Cluster) uint32 {
	t.Helper()
	spec := &cloudv1.ClusterSpec{}
	if err := protojson.Unmarshal(row.Spec, spec); err != nil {
		t.Fatal(err)
	}
	return spec.GetAws().GetControlPlaneLogGroup().GetRetentionDays()
}

func TestRecordRefusesToOverwriteAnEditMadeSinceTheRead(t *testing.T) {
	w := newEditWorld(t)
	c := w.create(t, "prod")
	row, err := w.s.Clusters().Get(t.Context(), "acme", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	w.kubectlEdit(t, "prod", 9) // lands during the pass, after the row was read
	row.State = "CLUSTER_STATE_RUNNING"
	if err := w.s.Clusters().Record(t.Context(), row); !errors.Is(err, repository.ErrConflict) {
		t.Fatalf("Record after an edit = %v, want ErrConflict", err)
	}
	got, err := w.s.Clusters().Get(t.Context(), "acme", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if retentionOf(t, got) != 9 {
		t.Fatalf("the edit was reverted: retention %d", retentionOf(t, got))
	}
}

func TestStatusWritesNeverObserveAnUnseenEdit(t *testing.T) {
	w := newEditWorld(t)
	c := w.create(t, "prod")
	before := w.object(t, "prod").Status.ObservedGeneration
	if before != w.object(t, "prod").Generation {
		t.Fatalf("a new cluster observed generation %d, has %d", before, w.object(t, "prod").Generation)
	}
	w.kubectlEdit(t, "prod", 9)
	row, err := w.s.Clusters().Get(t.Context(), "acme", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	status := model.JSON(`{"message":"working"}`)
	if err := w.s.Clusters().SetStatus(t.Context(), "acme", c.ID, row.ETag, status); err != nil {
		t.Fatal(err)
	}
	if got := w.object(t, "prod").Status.ObservedGeneration; got != before {
		t.Fatalf("SetStatus observed the edit: %d, want %d", got, before)
	}
	// A pass that read the row after the edit, but did not take it up (no new
	// row generation, as in a frozen pass), leaves it unobserved too.
	row.Status = model.JSON(`{"message":"frozen"}`)
	if err := w.s.Clusters().Record(t.Context(), row); err != nil {
		t.Fatal(err)
	}
	if got := w.object(t, "prod").Status.ObservedGeneration; got != before {
		t.Fatalf("Record without a new generation observed the edit: %d, want %d", got, before)
	}
	if retentionOf(t, row) != 9 {
		t.Fatal("the edit was lost")
	}
}

func TestTakingUpAnEditObservesIt(t *testing.T) {
	w := newEditWorld(t)
	c := w.create(t, "prod")
	w.kubectlEdit(t, "prod", 9)
	row, err := w.s.Clusters().Get(t.Context(), "acme", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	row.Generation++
	if err := w.s.Clusters().Update(t.Context(), row); err != nil {
		t.Fatal(err)
	}
	obj := w.object(t, "prod")
	if obj.Status.ObservedGeneration != obj.Generation {
		t.Fatalf("observed %d, generation %d", obj.Status.ObservedGeneration, obj.Generation)
	}
}

func TestAPIUpdatesStayObservedAndKeepTheirSpec(t *testing.T) {
	w := newEditWorld(t)
	c := w.create(t, "prod")
	row, err := w.s.Clusters().Get(t.Context(), "acme", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	spec := &cloudv1.ClusterSpec{}
	if err := protojson.Unmarshal(row.Spec, spec); err != nil {
		t.Fatal(err)
	}
	spec.GetAws().ControlPlaneLogGroup.RetentionDays = 7
	if row.Spec, err = protojson.Marshal(spec); err != nil {
		t.Fatal(err)
	}
	row.Generation++
	if err := w.s.Clusters().Update(t.Context(), row); err != nil {
		t.Fatal(err)
	}
	obj := w.object(t, "prod")
	if obj.Spec.AWS.ControlPlaneLogGroup.RetentionDays != 7 || obj.Status.ObservedGeneration != obj.Generation {
		t.Fatalf("API update: retention %d, observed %d, generation %d",
			obj.Spec.AWS.ControlPlaneLogGroup.RetentionDays, obj.Status.ObservedGeneration, obj.Generation)
	}
	// A platform write of the spec (resolved values) is observed as well.
	row, _ = w.s.Clusters().Get(t.Context(), "acme", c.ID)
	spec.GetAws().ControlPlaneLogGroup.RetentionDays = 8
	row.Spec, _ = protojson.Marshal(spec)
	if err := w.s.Clusters().Record(t.Context(), row); err != nil {
		t.Fatal(err)
	}
	obj = w.object(t, "prod")
	if obj.Status.ObservedGeneration != obj.Generation {
		t.Fatalf("a platform spec write left an edit pending: observed %d, generation %d", obj.Status.ObservedGeneration, obj.Generation)
	}
}

// The generation a store write produces is recorded on the object, so the
// controller can tell it from a kubectl edit while the status is behind.
func TestStoreWriteRecordsTheGenerationItProduced(t *testing.T) {
	w := newEditWorld(t)
	c := w.create(t, "prod")
	row, err := w.s.Clusters().Get(t.Context(), "acme", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	spec := &cloudv1.ClusterSpec{}
	if err := protojson.Unmarshal(row.Spec, spec); err != nil {
		t.Fatal(err)
	}
	spec.GetAws().ControlPlaneLogGroup.RetentionDays = 7
	row.Spec, _ = protojson.Marshal(spec)
	row.Generation++
	if err := w.s.Clusters().Update(t.Context(), row); err != nil {
		t.Fatal(err)
	}
	obj := w.object(t, "prod")
	if obj.Generation != 2 || !cloudstore.StoreWrote(obj) {
		t.Fatalf("API update: generation %d, recorded %q", obj.Generation, obj.Annotations[cloudstore.AnnotationRowGeneration])
	}
	w.kubectlEdit(t, "prod", 9)
	if obj := w.object(t, "prod"); obj.Generation != 3 || cloudstore.StoreWrote(obj) {
		t.Fatalf("kubectl edit taken for the store's: generation %d, recorded %q", obj.Generation, obj.Annotations[cloudstore.AnnotationRowGeneration])
	}
}

// A write that does not change the spec records the generation as it is,
// even when the write looks like a change (the server counted none): a later
// kubectl edit must not be taken for the store's.
func TestStoreWriteNeverRecordsAGenerationAheadOfTheObject(t *testing.T) {
	w := newWorld(t) // the fake API server counts no generations
	c := &model.Cluster{TenantID: "acme", Name: "prod", NetworkID: w.network.ID, Provider: "aws", Region: "us-east-1",
		Resource: model.Resource{State: "CLUSTER_STATE_PROVISIONING"}}
	c.Spec = document(t, clusterSpec(c))
	if err := w.s.Clusters().Create(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	row, err := w.s.Clusters().Get(t.Context(), "acme", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	spec := &cloudv1.ClusterSpec{}
	if err := protojson.Unmarshal(row.Spec, spec); err != nil {
		t.Fatal(err)
	}
	spec.GetAws().ControlPlaneLogGroup.RetentionDays = 7
	row.Spec, _ = protojson.Marshal(spec)
	row.Generation++
	if err := w.s.Clusters().Update(t.Context(), row); err != nil {
		t.Fatal(err)
	}
	obj := &cloud.Cluster{}
	if err := w.k.Get(t.Context(), client.ObjectKey{Namespace: namespace, Name: "prod"}, obj); err != nil {
		t.Fatal(err)
	}
	if !cloudstore.StoreWrote(obj) {
		t.Fatalf("generation %d, recorded %q: the record is ahead of the object", obj.Generation, obj.Annotations[cloudstore.AnnotationRowGeneration])
	}
}

// A store write on a generation the store produced itself acknowledges it,
// although the status is behind (the controller's catch-up of the window
// between an API update and its status write).
func TestRecordObservesAGenerationTheStoreProduced(t *testing.T) {
	w := newEditWorld(t)
	c := w.create(t, "prod")
	row, _ := w.s.Clusters().Get(t.Context(), "acme", c.ID)
	spec := &cloudv1.ClusterSpec{}
	if err := protojson.Unmarshal(row.Spec, spec); err != nil {
		t.Fatal(err)
	}
	spec.GetAws().ControlPlaneLogGroup.RetentionDays = 7
	row.Spec, _ = protojson.Marshal(spec)
	row.Generation++
	if err := w.s.Clusters().Update(t.Context(), row); err != nil {
		t.Fatal(err)
	}
	obj := w.object(t, "prod")
	obj.Status.ObservedGeneration = obj.Generation - 1 // the status write has not happened yet
	if err := w.k.Status().Update(t.Context(), obj); err != nil {
		t.Fatal(err)
	}
	row, _ = w.s.Clusters().Get(t.Context(), "acme", c.ID)
	if err := w.s.Clusters().Record(t.Context(), row); err != nil {
		t.Fatal(err)
	}
	if obj := w.object(t, "prod"); obj.Status.ObservedGeneration != obj.Generation {
		t.Fatalf("observed %d, generation %d", obj.Status.ObservedGeneration, obj.Generation)
	}
}
