package service

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/crossplane/managed"
	"github.com/yindia/cluster-vending/pkg/crossplane/sequence"
	"github.com/yindia/cluster-vending/pkg/database/model"
)

// Sequence applies sets of objects in order (sequence.Engine) in the
// management cluster.
func (d Deps) Sequence() sequence.Engine { return sequence.Engine{Objects: d.Objects()} }

// Inventory is owner's objects as the database records them
// (managed_resources), for sequence.Engine.
func (d Deps) Inventory(owner Owner) sequence.Inventory { return inventory{deps: d, owner: owner} }

type inventory struct {
	deps  Deps
	owner Owner
}

func (i inventory) List(ctx context.Context) ([]sequence.Entry, error) {
	records, err := i.deps.Store.ManagedResources().ListByOwner(ctx, i.owner.Kind, i.owner.ID)
	if err != nil {
		return nil, err
	}
	out := make([]sequence.Entry, 0, len(records))
	for _, r := range records {
		out = append(out, sequence.Entry{Ref: ref(r), Kept: r.CarriedOver})
	}
	return out, nil
}

func (i inventory) Add(ctx context.Context, objects []client.Object) error {
	return i.deps.Record(ctx, i.owner, objects)
}

// Remove forgets the record of entry. Records are looked up again: an
// entry carries no row ID.
func (i inventory) Remove(ctx context.Context, entry sequence.Entry) error {
	records, err := i.deps.Store.ManagedResources().ListByOwner(ctx, i.owner.Kind, i.owner.ID)
	if err != nil {
		return err
	}
	for _, r := range records {
		if sameObject(r, entry.Ref) {
			return i.deps.Store.ManagedResources().Delete(ctx, r.ID)
		}
	}
	return nil
}

func sameObject(r model.ManagedResource, ref managed.Ref) bool {
	return r.APIVersion == ref.APIVersion && r.Kind == ref.Kind && r.Namespace == ref.Namespace && r.Name == ref.Name
}
