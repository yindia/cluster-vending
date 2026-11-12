package service

import (
	"context"
	"errors"
	"fmt"

	protectionv1beta1 "github.com/crossplane/crossplane/apis/v2/protection/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// ReleaseOrphanUsages deletes recorded Usages whose "by" object is gone but
// that are still there: Kubernetes garbage-collects a Usage only once
// Crossplane owner-referenced it to its user, which never happens when the
// user was deleted (or never created) first. Deleting the Usage releases
// its object at once. views are the records' views, as Observe returns
// them.
func (d Deps) ReleaseOrphanUsages(ctx context.Context, records []model.ManagedResource, views []*cloudv1.ManagedResource) error {
	present := make(map[string]bool, len(views))
	for _, v := range views {
		if v.GetState() != cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_MISSING {
			present[v.GetKind()+"/"+v.GetName()] = true
		}
	}
	var errs []error
	for _, r := range records {
		if r.Kind != usageKind || !present[r.Kind+"/"+r.Name] {
			continue
		}
		u := &protectionv1beta1.Usage{}
		if err := d.Reader().Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: r.Name}, u); err != nil {
			errs = append(errs, client.IgnoreNotFound(err))
			continue
		}
		by := u.Spec.By
		if by == nil || by.ResourceRef == nil || present[by.Kind+"/"+by.ResourceRef.Name] {
			continue
		}
		if err := client.IgnoreNotFound(d.Kube.Delete(ctx, u)); err != nil {
			errs = append(errs, fmt.Errorf("release usage %s: %w", r.Name, err))
		}
	}
	return errors.Join(errs...)
}
