package cloudstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/kube"
)

// managedResources keeps the records of the objects created for an owner
// in the owner's status.inventory, in the order they were recorded. A
// record's ID names its owner's object ("<owner kind>/<namespace>/<object
// name>/<key of the recorded object>"), so one record is found without
// looking for its owner.
type managedResources struct{ store }

// owned is an owner object with its inventory.
type owned struct {
	obj       client.Object
	kind      model.OwnerKind
	inventory *[]cloudv1alpha1.InventoryEntry
}

// ownedBy wraps an owner object: its own inventory.
func ownedBy(obj client.Object) *owned { return ownedAs(obj, "") }

// ownedAs wraps an owner object for the owner kind: a Cluster holds two
// inventories, its own (OwnerCluster) and that of its platform components
// (OwnerClusterComponents). An empty kind is the object's own.
func ownedAs(obj client.Object, kind model.OwnerKind) *owned {
	if c, ok := obj.(*cloudv1alpha1.Cluster); ok && kind == model.OwnerClusterComponents {
		return &owned{obj: c, kind: kind, inventory: &c.Status.ComponentInventory}
	}
	switch o := obj.(type) {
	case *cloudv1alpha1.CloudAccount:
		return &owned{obj: o, kind: model.OwnerCloudAccount, inventory: &o.Status.Inventory}
	case *cloudv1alpha1.Network:
		return &owned{obj: o, kind: model.OwnerNetwork, inventory: &o.Status.Inventory}
	case *cloudv1alpha1.Cluster:
		return &owned{obj: o, kind: model.OwnerCluster, inventory: &o.Status.Inventory}
	}
	return nil
}

// ownerList is an empty list of the owner kind's objects; an error for a
// kind that owns nothing here.
func ownerList(kind model.OwnerKind) (client.ObjectList, error) {
	switch kind {
	case model.OwnerCloudAccount:
		return &cloudv1alpha1.CloudAccountList{}, nil
	case model.OwnerNetwork:
		return &cloudv1alpha1.NetworkList{}, nil
	case model.OwnerCluster, model.OwnerClusterComponents:
		return &cloudv1alpha1.ClusterList{}, nil
	}
	return nil, fmt.Errorf("%w: owner kind %q", repository.ErrInvalid, kind)
}

// owner is the object of the owner row; ErrNotFound when there is none.
// namespace narrows the search ("" for every tenant's).
func (r managedResources) owner(ctx context.Context, kind model.OwnerKind, namespace, id string) (*owned, error) {
	list, err := ownerList(kind)
	if err != nil {
		return nil, err
	}
	obj, err := byID[client.Object](ctx, r.store, list, namespace, id)
	if err != nil {
		return nil, err
	}
	return ownedAs(obj, kind), nil
}

// accountID is the row ID of the account the owner belongs to: its own
// for an account.
func (o *owned) accountID() string {
	if o.kind == model.OwnerCloudAccount {
		return o.obj.GetLabels()[LabelID]
	}
	return o.obj.GetLabels()[LabelAccountID]
}

// entryKey identifies a recorded object within its owner.
func entryKey(apiVersion, kind, namespace, name string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{apiVersion, kind, namespace, name}, "\x00")))
	return hex.EncodeToString(sum[:8])
}

func keyOf(e cloudv1alpha1.InventoryEntry) string {
	return entryKey(e.APIVersion, e.Kind, e.Namespace, e.Name)
}

// recordID is the ID of the record of e for o.
func (o *owned) recordID(e cloudv1alpha1.InventoryEntry) string {
	return strings.Join([]string{string(o.kind), o.obj.GetNamespace(), o.obj.GetName(), keyOf(e)}, "/")
}

// records are the owner's inventory as rows, in recorded order.
func (o *owned) records() []model.ManagedResource {
	out := make([]model.ManagedResource, 0, len(*o.inventory))
	for _, e := range *o.inventory {
		row := model.ManagedResource{
			Base:     model.Base{ID: o.recordID(e)},
			TenantID: o.obj.GetLabels()[LabelTenant], CloudAccountID: o.accountID(),
			OwnerKind: o.kind, OwnerID: o.obj.GetLabels()[LabelID],
			APIVersion: e.APIVersion, Kind: e.Kind, Namespace: e.Namespace, Name: e.Name,
			ExternalName: e.ExternalName, CarriedOver: e.CarriedOver,
		}
		if e.RecordedAt != nil {
			row.CreatedAt, row.UpdatedAt = e.RecordedAt.Time, e.RecordedAt.Time
		}
		if e.GoneAt != nil {
			row.GoneAt = ptrTime(e.GoneAt.Time)
		}
		out = append(out, row)
	}
	return out
}

func ptrTime(t time.Time) *time.Time { return &t }

// edit reads the owner (read returns it; nil for an owner that is gone),
// lets change edit its inventory and, when change reports a change,
// writes the status back; again when another writer was faster.
func (r managedResources) edit(ctx context.Context, read func() (*owned, error), change func(*owned) bool) error {
	return translate(retry(func() error {
		o, err := read()
		if err != nil || o == nil {
			return err
		}
		if !change(o) {
			return nil
		}
		return r.c.Status().Update(ctx, o.obj)
	}))
}

// Put records objects for their owners; one already recorded is left as
// it is. An owner that does not exist is ErrNotFound: there is nothing to
// record on.
func (r managedResources) Put(ctx context.Context, objects []model.ManagedResource) error {
	type ownerKey struct {
		kind model.OwnerKind
		id   string
	}
	var order []ownerKey
	byOwner := map[ownerKey][]model.ManagedResource{}
	for _, o := range objects {
		if _, err := ownerList(o.OwnerKind); err != nil {
			return err
		}
		k := ownerKey{o.OwnerKind, o.OwnerID}
		if _, seen := byOwner[k]; !seen {
			order = append(order, k)
		}
		byOwner[k] = append(byOwner[k], o)
	}
	for _, k := range order {
		at := metav1.NewTime(now())
		err := r.edit(ctx,
			func() (*owned, error) { return r.owner(ctx, k.kind, "", k.id) },
			func(o *owned) bool {
				changed := false
				for _, row := range byOwner[k] {
					e := cloudv1alpha1.InventoryEntry{APIVersion: row.APIVersion, Kind: row.Kind, Namespace: row.Namespace,
						Name: row.Name, ExternalName: row.ExternalName, CarriedOver: row.CarriedOver, RecordedAt: &at}
					if !slices.ContainsFunc(*o.inventory, func(have cloudv1alpha1.InventoryEntry) bool { return keyOf(have) == keyOf(e) }) {
						*o.inventory = append(*o.inventory, e)
						changed = true
					}
				}
				return changed
			})
		if err != nil {
			return fmt.Errorf("record for %s %s: %w", k.kind, k.id, err)
		}
	}
	return nil
}

// ListByOwner returns the owner's records; none for an owner that does
// not exist.
func (r managedResources) ListByOwner(ctx context.Context, kind model.OwnerKind, ownerID string) ([]model.ManagedResource, error) {
	o, err := r.owner(ctx, kind, "", ownerID)
	if errors.Is(err, repository.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return o.records(), nil
}

func (r managedResources) ListByOwners(ctx context.Context, kind model.OwnerKind, ownerIDs []string) (map[string][]model.ManagedResource, error) {
	out := make(map[string][]model.ManagedResource, len(ownerIDs))
	for _, id := range ownerIDs {
		rows, err := r.ListByOwner(ctx, kind, id)
		if err != nil {
			return nil, err
		}
		if len(rows) > 0 {
			out[id] = rows
		}
	}
	return out, nil
}

// ListByAccount returns the records of the account itself, then those of
// its networks and of its clusters (each oldest first).
func (r managedResources) ListByAccount(ctx context.Context, tenantID, cloudAccountID string) ([]model.ManagedResource, error) {
	if !labelValue(tenantID) {
		return nil, nil
	}
	namespace := kube.TenantNamespace(tenantID)
	var out []model.ManagedResource
	account, err := r.owner(ctx, model.OwnerCloudAccount, namespace, cloudAccountID)
	switch {
	case err == nil:
		out = append(out, account.records()...)
	case !errors.Is(err, repository.ErrNotFound):
		return nil, err
	}
	for _, list := range []client.ObjectList{&cloudv1alpha1.NetworkList{}, &cloudv1alpha1.ClusterList{}} {
		objects, err := items[client.Object](ctx, r.store, list, namespace, map[string]string{LabelAccountID: cloudAccountID})
		if err != nil {
			return nil, err
		}
		slices.SortFunc(objects, func(a, b client.Object) int { return strings.Compare(a.GetLabels()[LabelID], b.GetLabels()[LabelID]) })
		for _, obj := range objects {
			out = append(out, ownedBy(obj).records()...)
			if _, cluster := obj.(*cloudv1alpha1.Cluster); cluster {
				out = append(out, ownedAs(obj, model.OwnerClusterComponents).records()...)
			}
		}
	}
	return out, nil
}

// DeleteByOwner forgets the owner's records. The times its objects went
// away (GoneAt) go with them: there is no history of deleted records.
func (r managedResources) DeleteByOwner(ctx context.Context, kind model.OwnerKind, ownerID string) error {
	return r.edit(ctx,
		func() (*owned, error) {
			o, err := r.owner(ctx, kind, "", ownerID)
			if errors.Is(err, repository.ErrNotFound) {
				return nil, nil
			}
			return o, err
		},
		func(o *owned) bool {
			had := len(*o.inventory) > 0
			*o.inventory = nil
			return had
		})
}

// byRecord reads the owner a record ID names; nil when the ID is not one
// of this store's or its owner is gone.
func (r managedResources) byRecord(ctx context.Context, id string) (*owned, string, error) {
	parts := strings.Split(id, "/")
	if len(parts) != 4 {
		return nil, "", nil
	}
	var obj client.Object
	switch model.OwnerKind(parts[0]) {
	case model.OwnerCloudAccount:
		obj = &cloudv1alpha1.CloudAccount{}
	case model.OwnerNetwork:
		obj = &cloudv1alpha1.Network{}
	case model.OwnerCluster, model.OwnerClusterComponents:
		obj = &cloudv1alpha1.Cluster{}
	default:
		return nil, "", nil
	}
	if err := r.c.Get(ctx, client.ObjectKey{Namespace: parts[1], Name: parts[2]}, obj); err != nil {
		return nil, "", client.IgnoreNotFound(err)
	}
	return ownedAs(obj, model.OwnerKind(parts[0])), parts[3], nil
}

// change edits the record with the ID and reports whether there was one.
func (r managedResources) change(ctx context.Context, id string, edit func(inventory *[]cloudv1alpha1.InventoryEntry, i int) bool) (bool, error) {
	found := false
	err := r.edit(ctx,
		func() (*owned, error) {
			o, _, err := r.byRecord(ctx, id)
			return o, err
		},
		func(o *owned) bool {
			key := id[strings.LastIndex(id, "/")+1:]
			i := slices.IndexFunc(*o.inventory, func(e cloudv1alpha1.InventoryEntry) bool { return keyOf(e) == key })
			if found = i >= 0; !found {
				return false
			}
			return edit(o.inventory, i)
		})
	return found, err
}

// Delete forgets one record; ErrNotFound when there is none.
func (r managedResources) Delete(ctx context.Context, id string) error {
	found, err := r.change(ctx, id, func(inventory *[]cloudv1alpha1.InventoryEntry, i int) bool {
		*inventory = slices.Delete(*inventory, i, i+1)
		return true
	})
	if err == nil && !found {
		return repository.ErrNotFound
	}
	return err
}

// SetExternalName stores a record's external name; a record that is gone
// is left alone, as an UPDATE matching no row is.
func (r managedResources) SetExternalName(ctx context.Context, id, externalName string) error {
	_, err := r.change(ctx, id, func(inventory *[]cloudv1alpha1.InventoryEntry, i int) bool {
		if (*inventory)[i].ExternalName == externalName {
			return false
		}
		(*inventory)[i].ExternalName = externalName
		return true
	})
	return err
}

// SetGoneAt stores when a record's object was first seen gone; a time
// already stored is kept.
func (r managedResources) SetGoneAt(ctx context.Context, id string, at time.Time) error {
	_, err := r.change(ctx, id, func(inventory *[]cloudv1alpha1.InventoryEntry, i int) bool {
		if (*inventory)[i].GoneAt != nil {
			return false
		}
		gone := metav1.NewTime(at)
		(*inventory)[i].GoneAt = &gone
		return true
	})
	return err
}
