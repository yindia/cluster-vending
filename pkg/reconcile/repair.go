package reconcile

import (
	"context"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// isMissing reports whether a recorded object is gone from the management
// cluster. An object that is being deleted still exists: it is not
// missing.
func isMissing(v *cloudv1.ManagedResource) bool {
	return v.GetState() == cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_MISSING
}

// Repairable are the recorded objects that are gone from the management
// cluster and may be created again by applying their owner once more
// (crossplaneaws.Recreatable): somebody deleted them, and the owner's
// generation is applied, so nothing else would bring them back.
//
// It is all or nothing: when one missing object may not be created again
// (its cloud ID is AWS's, a new object would be a second resource), none
// is returned and the owner stays as it is, reporting what is missing. An
// apply creates everything of the render that is not there, and must not
// create that one.
func (d Deps) Repairable(ctx context.Context, records []model.ManagedResource) ([]model.ManagedResource, error) {
	views, err := d.Observe(ctx, records)
	if err != nil {
		return nil, err
	}
	var missing []model.ManagedResource
	for i, v := range views {
		if !isMissing(v) {
			continue
		}
		if !crossplaneaws.Recreatable(records[i].APIVersion, records[i].Kind) {
			return nil, nil
		}
		missing = append(missing, records[i])
	}
	return missing, nil
}

// LogRepaired logs which of the objects that were missing exist again
// after the apply ("<owner> repaired: recreated <kind> <name>"): those of
// the owner's current render. One the render no longer has (a node group
// being retired) was not created and is not logged.
func (d Deps) LogRepaired(ctx context.Context, owner Owner, missing []model.ManagedResource) {
	if d.Logger == nil || len(missing) == 0 {
		return
	}
	views, err := d.Observe(ctx, missing)
	if err != nil {
		return
	}
	for i, v := range views {
		if isMissing(v) {
			continue
		}
		d.Logger.InfoContext(ctx, string(owner.Kind)+" repaired: recreated "+missing[i].Kind+" "+missing[i].Name,
			"tenant_id", owner.TenantID, string(owner.Kind)+"_id", owner.ID, "kind", missing[i].Kind, "name", missing[i].Name)
	}
}
