package reconcile

import (
	"context"
	"reflect"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/crossplane/managed"
)

// SyncTenantFreeze projects the canonical ReleaseHold onto one Tenant. It never
// changes policy or claims ownership of a pre-existing manual pause.
func (d Deps) SyncTenantFreeze(ctx context.Context, tenantID string) error {
	return d.syncTenantFreezeAt(ctx, tenantID, time.Now())
}

func (d Deps) syncTenantFreezeAt(ctx context.Context, tenantID string, now time.Time) error {
	tenant := &cloud.Tenant{}
	if err := d.Reader().Get(ctx, client.ObjectKey{Name: tenantID}, tenant); err != nil {
		return client.IgnoreNotFound(err)
	}
	hold := &cloud.ReleaseHold{}
	if err := d.Reader().Get(ctx, client.ObjectKey{Name: FreezeName}, hold); err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		hold = nil
	}
	frozen := FreezeForTarget(hold, now, FreezeTarget{TenantID: tenantID})
	before := tenant.DeepCopy()
	var status *cloud.TenantFreezeStatus
	if frozen != nil {
		status = &cloud.TenantFreezeStatus{Frozen: frozen.Holds(), Scope: frozen.Scope, Reason: frozen.Reason, Revision: hold.Spec.Revision}
		if frozen.Until != nil {
			until := metav1.NewTime(*frozen.Until)
			status.Until = &until
		}
	}
	tenant.Status.Freeze = status
	if frozen.Holds() {
		meta.SetStatusCondition(&tenant.Status.Conditions, metav1.Condition{Type: ConditionFrozen, Status: metav1.ConditionTrue, Reason: "ReleaseHold", Message: FrozenBy(frozen), ObservedGeneration: tenant.Generation, LastTransitionTime: metav1.NewTime(now)})
	} else {
		meta.RemoveStatusCondition(&tenant.Status.Conditions, ConditionFrozen)
	}
	if !reflect.DeepEqual(before.Status, tenant.Status) {
		if err := d.Kube.Status().Patch(ctx, tenant, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return client.IgnoreNotFound(err)
		}
	}
	before = tenant.DeepCopy()
	annotations := tenant.Annotations
	if frozen.Holds() {
		if annotations[managed.PausedAnnotation] != "true" {
			if annotations == nil {
				annotations = map[string]string{}
			}
			annotations[managed.PausedAnnotation] = "true"
			annotations[freezePausedAnnotation] = "true"
		}
	} else if annotations[freezePausedAnnotation] == "true" {
		delete(annotations, managed.PausedAnnotation)
		delete(annotations, freezePausedAnnotation)
	}
	tenant.Annotations = annotations
	if reflect.DeepEqual(before.Annotations, annotations) {
		return nil
	}
	return client.IgnoreNotFound(d.Kube.Patch(ctx, tenant, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})))
}

// SyncTenantFreezes refreshes all tenant projections after a policy change or
// expiry. It only patches Tenant resources and never modifies tenant policy.
func (d Deps) SyncTenantFreezes(ctx context.Context) error {
	tenants := &cloud.TenantList{}
	if err := d.Reader().List(ctx, tenants); err != nil {
		return err
	}
	for _, tenant := range tenants.Items {
		if err := d.SyncTenantFreeze(ctx, tenant.Name); err != nil {
			return err
		}
	}
	return nil
}
