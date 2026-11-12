package network

import (
	"context"
	"encoding/json"
	"fmt"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/crossplane/sequence"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/fault"
)

// importedKey is the key of the stored input (a JSON object) under which
// an imported network keeps what was discovered of the VPC: what its
// Observe-only managed resources are rendered from. The rest of the input
// is the request as sent; readers of that skip the key.
const importedKey = "importedResources"

// WithImportedResources is input (the import request as stored JSON) with
// what the import discovered kept next to it. ImportNetwork stores it; the
// reconciler renders the network's Observe-only objects from it, so the
// cloud is looked up once, by the call.
func WithImportedResources(input model.JSON, resources crossplaneaws.ImportedNetwork) (model.JSON, error) {
	fields := map[string]json.RawMessage{}
	if len(input) > 0 {
		if err := json.Unmarshal(input, &fields); err != nil {
			return nil, fmt.Errorf("stored input: %w", err)
		}
	}
	raw, err := json.Marshal(resources)
	if err != nil {
		return nil, fmt.Errorf("encode the imported resources: %w", err)
	}
	fields[importedKey] = raw
	out, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("encode the input: %w", err)
	}
	return model.JSON(out), nil
}

// KeepImportedResources is next (a stored input rewritten by an update)
// with the imported resources of before, if it had any.
func KeepImportedResources(before, next model.JSON) (model.JSON, error) {
	resources, ok, err := importedResources(before)
	if err != nil || !ok {
		return next, err
	}
	return WithImportedResources(next, *resources)
}

// importedResources is what an import kept with the stored input; false
// when there is none (a created network, or one imported before it was
// kept: its objects exist already).
func importedResources(input model.JSON) (*crossplaneaws.ImportedNetwork, bool, error) {
	if len(input) == 0 {
		return nil, false, nil
	}
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(input, &fields); err != nil {
		return nil, false, fmt.Errorf("stored input: %w", err)
	}
	raw, ok := fields[importedKey]
	if !ok {
		return nil, false, nil
	}
	resources := &crossplaneaws.ImportedNetwork{}
	if err := json.Unmarshal(raw, resources); err != nil {
		return nil, false, fmt.Errorf("stored imported resources: %w", err)
	}
	return resources, true, nil
}

// applyImported creates the Observe-only managed resources of an imported
// network from what its import discovered (kept with the row). Nothing is
// looked up in the cloud again. It is safe to repeat.
func (s *Reconciler) applyImported(ctx context.Context, row *model.Network) error {
	resources, ok, err := importedResources(row.Input)
	if err != nil || !ok {
		return err
	}
	account, err := s.Account(ctx, row.TenantID, row.CloudAccountID)
	if err != nil {
		return err
	}
	_, err = s.ApplySet(ctx, row, account, sequence.Of(crossplaneaws.ImportedNetworkResources(*resources)...))
	return err
}

// applyOwned applies the managed resources of a taken-over network: the
// set its import discovered (kept with the row), rendered as owned
// (crossplaneaws.OwnedNetworkResources) with the network's labels as
// tags. A takeover and every later change of the labels are this one
// operation: nothing is read and edited in place, and nothing is looked
// up in the cloud. It is safe to repeat.
//
// A network imported before its discovered resources were kept with it
// has nothing to render from: it must be imported again.
func (s *Reconciler) applyOwned(ctx context.Context, row *model.Network) error {
	resources, ok, err := importedResources(row.Input)
	if err != nil {
		return err
	}
	if !ok {
		return fault.Newf(fault.FailedPrecondition,
			"network %s was imported before what its import discovered was kept with it; delete it and import the VPC again", row.ID)
	}
	account, err := s.Account(ctx, row.TenantID, row.CloudAccountID)
	if err != nil {
		return err
	}
	_, err = s.ApplySet(ctx, row, account, sequence.Of(crossplaneaws.OwnedNetworkResources(*resources, row.Labels)...))
	return err
}
