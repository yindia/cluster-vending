package cloudstore

import (
	"context"
	"fmt"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/kube"
)

// DeleteCloudAccount deletes the account's object with its finalizer kept
// (unlike remove): the controller then deletes the account's
// ProviderConfig and removes the finalizer. Networks are read at the API
// server: one created a moment ago keeps its account.
func (r tenants) DeleteCloudAccount(ctx context.Context, tenantID, id string) error {
	if !labelValue(tenantID) {
		return repository.ErrNotFound
	}
	ns := kube.TenantNamespace(tenantID)
	obj, err := byID[*cloudv1alpha1.CloudAccount](ctx, r.store, &cloudv1alpha1.CloudAccountList{}, ns, id)
	if err != nil {
		return err
	}
	networks, err := items[*cloudv1alpha1.Network](ctx, r.store, &cloudv1alpha1.NetworkList{}, ns, nil)
	if err != nil {
		return err
	}
	var names []string
	for _, n := range networks {
		if n.Labels[LabelAccountID] == id || n.Spec.AccountRef.Name == obj.Name {
			names = append(names, n.Name)
		}
	}
	if len(names) > 0 {
		return fmt.Errorf("%w: networks %s belong to the account; delete them first", repository.ErrInUse, strings.Join(names, ", "))
	}
	version := obj.GetResourceVersion()
	return translate(r.c.Delete(ctx, obj, client.Preconditions{ResourceVersion: &version}))
}
