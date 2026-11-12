package kubestore

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	platformv1alpha1 "github.com/yindia/cluster-vending/pkg/apis/platform/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
)

type tenants struct{ store }

// A tenant's object is named after the tenant ID: a second create of the
// ID is refused by the API server.
func (r tenants) Create(ctx context.Context, t *model.Tenant) error {
	if err := t.BeforeCreate(nil); err != nil {
		return err
	}
	t.CreatedAt = now()
	t.UpdatedAt = t.CreatedAt
	obj := &platformv1alpha1.Tenant{ObjectMeta: r.meta(t.ID, nil), Spec: platformv1alpha1.TenantSpec{
		DisplayName: t.DisplayName, CreatedAt: micro(t.CreatedAt), UpdatedAt: micro(t.UpdatedAt)}}
	return r.insert(ctx, "Tenant", obj)
}

func (r tenants) Get(ctx context.Context, id string) (*model.Tenant, error) {
	obj := &platformv1alpha1.Tenant{}
	if err := r.get(ctx, id, obj); err != nil {
		return nil, err
	}
	return fromTenant(obj), nil
}

func (r tenants) Delete(ctx context.Context, id string) error {
	obj := &platformv1alpha1.Tenant{}
	if err := r.get(ctx, id, obj); err != nil {
		return err
	}
	ofTenant := map[string]string{platformv1alpha1.LabelTenant: id}
	for _, children := range []client.ObjectList{&platformv1alpha1.NetworkList{}, &platformv1alpha1.ClusterList{}} {
		if err := r.list(ctx, children, ofTenant); err != nil {
			return err
		}
		if n := meta.LenList(children); n > 0 {
			return fmt.Errorf("%w: %d live %T rows reference it", repository.ErrInUse, n, children)
		}
	}
	accounts := &platformv1alpha1.CloudAccountList{}
	if err := r.list(ctx, accounts, ofTenant); err != nil {
		return err
	}
	for i := range accounts.Items {
		a := &accounts.Items[i]
		if err := r.remove(ctx, a); err != nil && !errors.Is(err, repository.ErrNotFound) {
			return err
		}
		r.release(ctx, accountKey(a.Spec.TenantID, model.Provider(a.Spec.Provider), a.Spec.AccountID))
	}
	return r.remove(ctx, obj)
}

// accountKey is the claim of a cloud account: one per tenant, provider
// and account ID.
func accountKey(tenantID string, provider model.Provider, accountID string) string {
	return fmt.Sprintf("cloud-account/%s/%s/%s", tenantID, provider, accountID)
}

// PutCloudAccount creates the account, or updates the one the tenant has
// for the provider and account ID (found through its claim, so two puts
// of a new account cannot both create it).
func (r tenants) PutCloudAccount(ctx context.Context, a *model.CloudAccount) error {
	if err := r.get(ctx, a.TenantID, &platformv1alpha1.Tenant{}); err != nil {
		return err
	}
	if err := invalid(model.ValidateAccount(a.Provider, a.AccountID, a.DirectoryID)); err != nil {
		return err
	}
	key := accountKey(a.TenantID, a.Provider, a.AccountID)
	for range writeAttempts {
		id, err := r.holder(ctx, key)
		switch {
		case err == nil:
			err = r.updateAccount(ctx, id, a)
			if !errors.Is(err, repository.ErrNotFound) {
				return err
			}
			// Claimed, its row not stored (yet, or ever): create it under
			// the claim's ID unless the claim is still fresh.
			err = r.createAccount(ctx, key, a)
		case errors.Is(err, repository.ErrNotFound):
			err = r.createAccount(ctx, key, a)
		}
		if !errors.Is(err, repository.ErrAlreadyExists) {
			return err
		}
	}
	return fmt.Errorf("%w: cloud account %s", repository.ErrConflict, key)
}

// updateAccount writes what a re-bootstrap may change: the ProviderConfig
// name stays (the account's managed resources reference it).
func (r tenants) updateAccount(ctx context.Context, id string, a *model.CloudAccount) error {
	obj := &platformv1alpha1.CloudAccount{}
	return r.mutate(ctx, id, obj, func() error {
		s := &obj.Spec
		s.DirectoryID, s.AccountName, s.Namespace, s.Identity = a.DirectoryID, a.AccountName, a.Namespace, a.Identity
		s.UpdatedAt = micro(now())
		return nil
	})
}

func (r tenants) createAccount(ctx context.Context, key string, a *model.CloudAccount) error {
	row := *a
	row.Base = model.Base{}
	if err := row.BeforeCreate(nil); err != nil {
		return err
	}
	row.CreatedAt = now()
	row.UpdatedAt = row.CreatedAt
	labels := map[string]string{platformv1alpha1.LabelTenant: row.TenantID}
	obj := &platformv1alpha1.CloudAccount{ObjectMeta: r.meta(row.ID, labels), Spec: toAccount(&row)}
	if err := r.insert(ctx, kindCloudAccount, obj, key); err != nil {
		return err
	}
	*a = row
	return nil
}

func (r tenants) GetCloudAccount(ctx context.Context, tenantID string, provider model.Provider, accountID string) (*model.CloudAccount, error) {
	accounts, err := r.ListCloudAccounts(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	for i := range accounts {
		if accounts[i].Provider == provider && accounts[i].AccountID == accountID {
			return &accounts[i], nil
		}
	}
	return nil, repository.ErrNotFound
}

func (r tenants) GetCloudAccountByID(ctx context.Context, tenantID, id string) (*model.CloudAccount, error) {
	obj := &platformv1alpha1.CloudAccount{}
	if err := r.get(ctx, id, obj); err != nil {
		return nil, err
	}
	if obj.Spec.TenantID != tenantID {
		return nil, repository.ErrNotFound
	}
	return fromAccount(obj), nil
}

func (r tenants) ListCloudAccounts(ctx context.Context, tenantID string) ([]model.CloudAccount, error) {
	list := &platformv1alpha1.CloudAccountList{}
	if err := r.list(ctx, list, map[string]string{platformv1alpha1.LabelTenant: tenantID}); err != nil {
		return nil, err
	}
	out := make([]model.CloudAccount, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, *fromAccount(&list.Items[i]))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].AccountID < out[j].AccountID
	})
	return out, nil
}
