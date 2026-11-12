package kubestore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	platformv1alpha1 "github.com/yindia/cluster-vending/pkg/apis/platform/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
)

type managedResources struct{ store }

// ownerKinds are the owner kinds a record may have.
var ownerKinds = map[model.OwnerKind]bool{
	model.OwnerCloudAccount: true, model.OwnerNetwork: true, model.OwnerCluster: true, model.OwnerNodePool: true,
}

// managedName is the object name, and row ID, of the record of one object
// for one owner. It is derived from what makes a record unique (owner and
// object), so recording an object twice, also at the same moment, creates
// one record: the second create finds the name taken. A random ID with a
// claim would need two objects per record.
func managedName(o model.ManagedResource) string {
	key := strings.Join([]string{string(o.OwnerKind), o.OwnerID, o.APIVersion, o.Kind, o.Namespace, o.Name}, "\x00")
	sum := sha256.Sum256([]byte(key))
	return "mr-" + hex.EncodeToString(sum[:20])
}

func ownerLabels(kind model.OwnerKind, ownerID string) map[string]string {
	return map[string]string{platformv1alpha1.LabelOwnerKind: string(kind), platformv1alpha1.LabelOwner: ownerID}
}

// Put records objects; one already recorded is left as it is. Records
// keep the order they were made in (Order: the time of the call, then the
// position in objects).
func (r managedResources) Put(ctx context.Context, objects []model.ManagedResource) error {
	for _, o := range objects {
		if !ownerKinds[o.OwnerKind] {
			return fmt.Errorf("%w: owner kind %q", repository.ErrInvalid, o.OwnerKind)
		}
	}
	at := now()
	for i, o := range objects {
		labels := ownerLabels(o.OwnerKind, o.OwnerID)
		labels[platformv1alpha1.LabelTenant] = o.TenantID
		labels[platformv1alpha1.LabelCloudAccount] = o.CloudAccountID
		obj := &platformv1alpha1.ManagedResource{ObjectMeta: r.meta(managedName(o), labels), Spec: platformv1alpha1.ManagedResourceSpec{
			TenantID: o.TenantID, CloudAccountID: o.CloudAccountID, OwnerKind: string(o.OwnerKind), OwnerID: o.OwnerID,
			APIVersion: o.APIVersion, Kind: o.Kind, Namespace: o.Namespace, Name: o.Name,
			ExternalName: o.ExternalName, CarriedOver: o.CarriedOver,
			Order: at.UnixNano() + int64(i), CreatedAt: micro(at), UpdatedAt: micro(at),
		}}
		if err := r.c.Create(ctx, obj); err != nil && !apierrors.IsAlreadyExists(err) {
			return translate(err)
		}
	}
	return nil
}

// records lists the records with labels, in the order they were made.
func (r managedResources) records(ctx context.Context, labels map[string]string) ([]model.ManagedResource, error) {
	list := &platformv1alpha1.ManagedResourceList{}
	if err := r.list(ctx, list, labels); err != nil {
		return nil, err
	}
	items := list.Items
	sort.SliceStable(items, func(i, j int) bool { return items[i].Spec.Order < items[j].Spec.Order })
	out := make([]model.ManagedResource, 0, len(items))
	for i := range items {
		out = append(out, fromManaged(&items[i]))
	}
	return out, nil
}

func (r managedResources) ListByOwner(ctx context.Context, kind model.OwnerKind, ownerID string) ([]model.ManagedResource, error) {
	return r.records(ctx, ownerLabels(kind, ownerID))
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

func (r managedResources) ListByAccount(ctx context.Context, tenantID, cloudAccountID string) ([]model.ManagedResource, error) {
	return r.records(ctx, map[string]string{platformv1alpha1.LabelTenant: tenantID, platformv1alpha1.LabelCloudAccount: cloudAccountID})
}

func (r managedResources) DeleteByOwner(ctx context.Context, kind model.OwnerKind, ownerID string) error {
	err := r.c.DeleteAllOf(ctx, &platformv1alpha1.ManagedResource{}, client.InNamespace(r.ns), client.MatchingLabels(ownerLabels(kind, ownerID)))
	return translate(err)
}

func (r managedResources) Delete(ctx context.Context, id string) error {
	if id == "" {
		return repository.ErrNotFound
	}
	return r.remove(ctx, &platformv1alpha1.ManagedResource{ObjectMeta: r.meta(id, nil)})
}

// change edits the record named id; a record that is gone is left alone,
// as an UPDATE matching no row is.
func (r managedResources) change(ctx context.Context, id string, edit func(*platformv1alpha1.ManagedResourceSpec)) error {
	obj := &platformv1alpha1.ManagedResource{}
	err := r.mutate(ctx, id, obj, func() error {
		edit(&obj.Spec)
		obj.Spec.UpdatedAt = micro(now())
		return nil
	})
	if errors.Is(err, repository.ErrNotFound) {
		return nil
	}
	return err
}

func (r managedResources) SetExternalName(ctx context.Context, id, externalName string) error {
	return r.change(ctx, id, func(s *platformv1alpha1.ManagedResourceSpec) { s.ExternalName = externalName })
}

func (r managedResources) SetGoneAt(ctx context.Context, id string, at time.Time) error {
	return r.change(ctx, id, func(s *platformv1alpha1.ManagedResourceSpec) {
		if s.GoneAt == nil {
			gone := micro(at)
			s.GoneAt = &gone
		}
	})
}
