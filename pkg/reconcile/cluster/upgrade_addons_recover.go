package cluster

import (
	"context"
	"errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// recoverAddons applies the cluster's add-ons that are not settled again:
// each one's object is annotated (AnnotationRetriedAt), an update the
// provider reconciles at once, which updates the add-on in AWS to the
// version it asks for, overwriting conflicting changes (resolveConflicts
// OVERWRITE). It is the add-ons' step's recovery (watchAddons).
func (s *Reconciler) recoverAddons(ctx context.Context, row *model.Cluster, at time.Time) error {
	records, err := s.deps.Store.ManagedResources().ListByOwner(ctx, model.OwnerCluster, row.ID)
	if err != nil {
		return err
	}
	var addons []model.ManagedResource
	for _, r := range records {
		if r.Kind == "Addon" {
			addons = append(addons, r)
		}
	}
	views, err := s.deps.Observe(ctx, addons)
	if err != nil {
		return err
	}
	var errs []error
	for i, r := range addons {
		if i < len(views) && views[i].GetState() == cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY && !reconcile.Behind(views[i]) {
			continue
		}
		if err := s.nudge(ctx, r, at); err != nil {
			errs = append(errs, fmt.Errorf("add-on %s: %w", r.Name, err))
		}
	}
	return errors.Join(errs...)
}

// nudge annotates a managed resource's object, so its provider acts on it
// now rather than at its next poll.
func (s *Reconciler) nudge(ctx context.Context, r model.ManagedResource, at time.Time) error {
	gv, err := schema.ParseGroupVersion(r.APIVersion)
	if err != nil {
		return err
	}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gv.WithKind(r.Kind))
	u.SetNamespace(r.Namespace)
	u.SetName(r.Name)
	patch := client.RawPatch(client.Merge.Type(), []byte(fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`,
		AnnotationRetriedAt, at.UTC().Format(time.RFC3339))))
	return client.IgnoreNotFound(s.deps.Kube.Patch(ctx, u, patch))
}
