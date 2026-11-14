package reconcile

import (
	"context"

	"k8s.io/apimachinery/pkg/runtime"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/crossplane/managed"
	"github.com/yindia/cluster-vending/pkg/crossplane/sequence"
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
// A missing object may be created again when it cannot become a second
// cloud resource. Either its kind is one the renderer names
// (crossplaneaws.Recreatable): the provider finds a survivor by that name.
// Or its cloud identity is recorded (ManagedResource.ExternalName, kept
// current by Observe): the object is then created with it in its first
// write (Named, sequence.Item.ExternalName), so its provider takes the
// existing resource over; that covers the kinds whose ID AWS assigns (a
// VPC, a subnet, a key) and everything an import adopted.
//
// It is all or nothing: when one missing object has neither (its ID is
// AWS's and was never recorded: the object was deleted before its
// provider created the resource), none is returned and the owner stays as
// it is, reporting what is missing. An apply creates everything of the
// render that is not there, and must not create that one.
//
// Nothing is repaired while any object of the owner is paused
// (crossplane.io/paused, AdminService.PauseReconcile): an object created
// now would not carry the pause, and its provider would act on it in the
// middle of what the operator paused for (a provider upgrade).
func (d Deps) Repairable(ctx context.Context, records []model.ManagedResource) ([]model.ManagedResource, error) {
	views, err := d.Observe(ctx, records)
	if err != nil {
		return nil, err
	}
	return repairable(records, views), nil
}

// repairable is Repairable on the views of records (same order).
func repairable(records []model.ManagedResource, views []*cloudv1.ManagedResource) []model.ManagedResource {
	if PausedCount(views) > 0 {
		return nil
	}
	var missing []model.ManagedResource
	for i, v := range views {
		if !isMissing(v) {
			continue
		}
		if !crossplaneaws.Recreatable(records[i].APIVersion, records[i].Kind) &&
			(records[i].ExternalName == "" || !crossplaneaws.RecreatableByName(records[i].APIVersion, records[i].Kind)) {
			return nil
		}
		missing = append(missing, records[i])
	}
	return missing
}

// Named is set with the recorded cloud identity (ExternalName) on every
// item that stands for one of the missing objects: applying it creates
// those objects with their identity (sequence.Item.ExternalName). Items of
// objects that are not missing, or whose identity was never recorded, are
// as they were; set itself is not changed.
func Named(set sequence.Set, missing []model.ManagedResource, scheme *runtime.Scheme) (sequence.Set, error) {
	names := map[string]string{}
	for _, r := range missing {
		if r.ExternalName != "" {
			names[r.APIVersion+"/"+r.Kind+"/"+r.Namespace+"/"+r.Name] = r.ExternalName
		}
	}
	if len(names) == 0 {
		return set, nil
	}
	out := make(sequence.Set, len(set))
	copy(out, set)
	for i, item := range out {
		ref, err := managed.RefOf(item.Object, scheme)
		if err != nil {
			return nil, err
		}
		out[i].ExternalName = names[ref.APIVersion+"/"+ref.Kind+"/"+ref.Namespace+"/"+ref.Name]
	}
	return out, nil
}

// missingKey carries, through an apply, the missing objects it repairs.
type missingKey struct{}

// Repairing is ctx carrying the missing objects an apply repairs: the
// owner's apply (WithRecordedNames) then creates them with their recorded
// cloud identity.
func Repairing(ctx context.Context, missing []model.ManagedResource) context.Context {
	if len(missing) == 0 {
		return ctx
	}
	return context.WithValue(ctx, missingKey{}, missing)
}

// WithRecordedNames is set as an apply under ctx sends it: with the
// recorded cloud identity on the items of the objects it repairs
// (Repairing, Named); set as it is otherwise.
func WithRecordedNames(ctx context.Context, set sequence.Set, scheme *runtime.Scheme) (sequence.Set, error) {
	missing, _ := ctx.Value(missingKey{}).([]model.ManagedResource)
	if len(missing) == 0 {
		return set, nil
	}
	return Named(set, missing, scheme)
}

// LogRepaired logs which of the objects that were missing exist again
// after the apply ("<owner> repaired: recreated <kind> <name>"): those of
// the owner's current render. One the render no longer has (a node group
// being retired) was not created and is not logged.
func (d Deps) LogRepaired(ctx context.Context, owner Owner, missing []model.ManagedResource) {
	if len(missing) == 0 {
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
		// The pass reports it as an event of the owner (AddNote).
		AddNote(ctx, Normal(ReasonRepaired, "recreated "+missing[i].Kind+" "+missing[i].Name+
			", which was deleted from the management cluster"))
		if d.Logger == nil {
			continue
		}
		d.Logger.InfoContext(ctx, string(owner.Kind)+" repaired: recreated "+missing[i].Kind+" "+missing[i].Name,
			"tenant_id", owner.TenantID, string(owner.Kind)+"_id", owner.ID, "kind", missing[i].Kind, "name", missing[i].Name)
	}
}
