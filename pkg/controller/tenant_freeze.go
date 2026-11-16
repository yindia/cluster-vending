package controller

import (
	"context"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
	platform "github.com/yindia/cluster-vending/pkg/reconcile"
)

// syncTenantFreeze observes policy even when the Tenant has a manual pause.
// Namespace lifecycle and the freeze projection must remain able to clear only
// the pauses they own; they do not perform cloud reconciliation.
func (r *TenantReconciler) syncTenantFreeze(ctx context.Context, tenantID string) (time.Duration, error) {
	// Use the start of the pass when scheduling: expiry may occur during the
	// projection writes, and must still trigger another observation.
	now := time.Now()
	d := platform.Deps{Kube: r.deps.Direct, KubeReader: r.deps.Direct}
	if err := d.SyncTenantFreeze(ctx, tenantID); err != nil {
		return 0, err
	}
	hold := &cloud.ReleaseHold{}
	if err := r.deps.Direct.Get(ctx, client.ObjectKey{Name: platform.FreezeName}, hold); err != nil {
		return resyncDelay, client.IgnoreNotFound(err)
	}
	return tenantFreezeExpiry(hold, tenantID, now), nil
}

// Include expiring let-through rules: their expiry can restore a broader hold.
func tenantFreezeExpiry(hold *cloud.ReleaseHold, tenantID string, now time.Time) time.Duration {
	next := resyncDelay
	consider := func(until *time.Time) {
		if until != nil && until.After(now) {
			next = min(next, max(time.Second, until.Sub(now)))
		}
	}
	if hold.Spec.Until != nil {
		consider(&hold.Spec.Until.Time)
	}
	for _, rule := range hold.Spec.Rules {
		if rule.TenantID == tenantID && rule.Until != nil {
			consider(&rule.Until.Time)
		}
	}
	return next
}

// A canonical hold create/update/delete can change any tenant's effective
// policy. Queue registrations, never names inferred from untrusted rule text.
func (r *TenantReconciler) freezeTenants(ctx context.Context, obj client.Object) []reconcile.Request {
	if obj.GetName() != platform.FreezeName {
		return nil
	}
	tenants := &cloud.TenantList{}
	if err := r.deps.Direct.List(ctx, tenants); err != nil {
		if !apierrors.IsNotFound(err) {
			r.deps.logger().ErrorContext(ctx, "queue tenants after freeze change", "error", err)
		}
		return nil
	}
	requests := make([]reconcile.Request, 0, len(tenants.Items))
	for _, tenant := range tenants.Items {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKey{Name: tenant.Name}})
	}
	return requests
}
