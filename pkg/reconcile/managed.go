package reconcile

import (
	"context"
	"errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/crossplane"
	"github.com/yindia/cluster-vending/pkg/crossplane/managed"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/fault"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// now is the clock; tests replace it.
var now = time.Now

// Owner names what objects are created for.
type Owner struct {
	TenantID       string
	CloudAccountID string
	Kind           model.OwnerKind
	ID             string
}

// DeleteOrder is the order an owner's objects are deleted in
// (managed.DeleteOrder).
type DeleteOrder = managed.DeleteOrder

// Objects reads and deletes objects in the management cluster: the
// database-free half of what Deps does with recorded objects.
func (d Deps) Objects() managed.Client { return managed.Client{Kube: d.Kube, Uncached: d.KubeReader} }

// ref is a recorded object's Ref.
func ref(r model.ManagedResource) managed.Ref {
	return managed.Ref{APIVersion: r.APIVersion, Kind: r.Kind, Namespace: r.Namespace, Name: r.Name,
		ExternalName: r.ExternalName, Since: r.CreatedAt}
}

func refs(records []model.ManagedResource) []managed.Ref {
	out := make([]managed.Ref, 0, len(records))
	for _, r := range records {
		out = append(out, ref(r))
	}
	return out
}

// Record records objects as created for owner, in order. Recording the
// same objects again is a no-op.
func (d Deps) Record(ctx context.Context, owner Owner, objects []client.Object) error {
	rows := make([]model.ManagedResource, 0, len(objects))
	for _, obj := range objects {
		r, err := managed.RefOf(obj, d.Kube.Scheme())
		if err != nil {
			return fmt.Errorf("record %w", err)
		}
		rows = append(rows, model.ManagedResource{
			TenantID: owner.TenantID, CloudAccountID: owner.CloudAccountID, OwnerKind: owner.Kind, OwnerID: owner.ID,
			APIVersion: r.APIVersion, Kind: r.Kind, Namespace: r.Namespace, Name: r.Name,
			CarriedOver: obj.GetAnnotations()[crossplane.CarriedOverAnnotation] == "true",
		})
	}
	return d.Store.ManagedResources().Put(ctx, rows)
}

// Observe returns the live view of recorded objects, in record order, and
// stores each object's cloud ID when it is new.
func (d Deps) Observe(ctx context.Context, records []model.ManagedResource) ([]*cloudv1.ManagedResource, error) {
	views, err := d.Objects().Observe(ctx, refs(records))
	var unreadable *managed.ReadError
	if errors.As(err, &unreadable) {
		return nil, fault.Newf(fault.Unavailable, "cannot read %s %s from the management cluster",
			unreadable.Ref.Kind, unreadable.Ref.Name)
	}
	if err != nil {
		return nil, err
	}
	for i, v := range views {
		if v.GetState() != cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_MISSING {
			d.keepExternalName(ctx, records[i], v.GetExternalName())
		}
	}
	return views, nil
}

// keepExternalName stores an object's external name when it is new or
// changed (once per object, in practice). A failure is logged, not
// returned: the name is stored on a later read.
func (d Deps) keepExternalName(ctx context.Context, r model.ManagedResource, name string) {
	if name == "" || name == r.ExternalName || r.ID == "" {
		return
	}
	if err := d.Store.ManagedResources().SetExternalName(ctx, r.ID, name); err != nil && d.Logger != nil {
		d.Logger.WarnContext(ctx, "store external name", "kind", r.Kind, "name", r.Name, "error", err)
	}
}

// MarkGone stores, for the records of an owner being deleted, when each
// was first seen gone (views in record order, as Observe returns them).
func (d Deps) MarkGone(ctx context.Context, records []model.ManagedResource, views []*cloudv1.ManagedResource) error {
	for i, v := range views {
		if i >= len(records) || records[i].GoneAt != nil || records[i].ID == "" ||
			v.GetState() != cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_MISSING {
			continue
		}
		if err := d.Store.ManagedResources().SetGoneAt(ctx, records[i].ID, now()); err != nil {
			return fmt.Errorf("store when %s %s went away: %w", records[i].Kind, records[i].Name, err)
		}
	}
	return nil
}

// DeleteRecorded asks the management cluster to delete recorded objects,
// last recorded first (managed.Client.Delete).
func (d Deps) DeleteRecorded(ctx context.Context, records []model.ManagedResource) error {
	return d.Objects().Delete(ctx, refs(records))
}

// DeleteNext deletes the recorded objects of the first tier of order that
// still has objects and reports how many remain (managed.Client.DeleteNext).
func (d Deps) DeleteNext(ctx context.Context, records []model.ManagedResource, views []*cloudv1.ManagedResource,
	order DeleteOrder,
) (int, error) {
	return d.Objects().DeleteNext(ctx, refs(records), views, order)
}

// ReleaseOrphanUsages deletes recorded Usages whose "by" object is gone
// (managed.Client.ReleaseOrphanUsages).
func (d Deps) ReleaseOrphanUsages(ctx context.Context, records []model.ManagedResource, views []*cloudv1.ManagedResource) error {
	return d.Objects().ReleaseOrphanUsages(ctx, refs(records), views)
}

// Prune deletes the objects recorded for an owner that keep no longer
// contains (e.g. the access entry of a removed admin), and forgets them.
// Objects carried over from an import are never in keep (no spec field
// renders them) and are left alone.
func (d Deps) Prune(ctx context.Context, owner Owner, keep []client.Object) error {
	records, err := d.Store.ManagedResources().ListByOwner(ctx, owner.Kind, owner.ID)
	if err != nil {
		return err
	}
	kept := map[string]bool{}
	for _, obj := range keep {
		r, err := managed.RefOf(obj, d.Kube.Scheme())
		if err != nil {
			return err
		}
		kept[r.Kind+"/"+r.Namespace+"/"+r.Name] = true
	}
	for _, r := range records {
		if r.CarriedOver || kept[r.Kind+"/"+r.Namespace+"/"+r.Name] {
			continue
		}
		if err := d.Objects().DeleteDropped(ctx, ref(r)); err != nil {
			return err
		}
		if err := d.Store.ManagedResources().Delete(ctx, r.ID); err != nil {
			return err
		}
	}
	return nil
}

// AllGone reports whether every recorded object is gone from the cluster
// (MISSING), i.e. a delete finished.
func AllGone(views []*cloudv1.ManagedResource) bool { return managed.AllGone(views) }

// SettleMissing turns MISSING into CREATING for objects recorded a moment
// ago (managed.SettleMissing; views in record order).
func SettleMissing(records []model.ManagedResource, views []*cloudv1.ManagedResource) {
	managed.SettleMissing(refs(records), views)
}

// Get reads a typed object through the cache (managed.Client.Get).
func (d Deps) Get(ctx context.Context, key client.ObjectKey, obj client.Object) error {
	return d.Objects().Get(ctx, key, obj)
}

// List lists typed objects through the cache (managed.Client.List).
func (d Deps) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	return d.Objects().List(ctx, list, opts...)
}

// Reader is the uncached reader, or Kube without one (see Deps.KubeReader).
func (d Deps) Reader() client.Reader { return d.Objects().Reader() }

// ReadRecorded is a recorded object as Observe reads it (cache first, then
// the API server); nil when it does not exist.
func (d Deps) ReadRecorded(ctx context.Context, r model.ManagedResource) (*unstructured.Unstructured, error) {
	return d.Objects().Read(ctx, ref(r))
}
