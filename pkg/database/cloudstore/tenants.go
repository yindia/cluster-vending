package cloudstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/kube"
	tenantlife "github.com/yindia/cluster-vending/pkg/reconcile/tenant"
)

type tenants struct{ store }

// AnnotationAccount holds what an account row has and the kind's spec
// does not (JSON: the ProviderConfig name, namespace, identity, ...).
const AnnotationAccount = group + "account"

// accountMeta is AnnotationAccount.
type accountMeta struct {
	ProviderConfigName string    `json:"providerConfigName,omitempty"`
	Namespace          string    `json:"namespace,omitempty"`
	Identity           string    `json:"identity,omitempty"`
	DirectoryID        string    `json:"directoryID,omitempty"`
	ExternalID         string    `json:"externalID,omitempty"`
	CreatedAt          time.Time `json:"createdAt"`
	UpdatedAt          time.Time `json:"updatedAt"`
}

// Create stores the canonical cluster-scoped registration, then ensures its
// namespace synchronously for callers that immediately register an account.
func (r tenants) Create(ctx context.Context, t *model.Tenant) error {
	if !tenantlife.ValidID(t.ID) {
		return fmt.Errorf("%w: tenant ID %q", repository.ErrInvalid, t.ID)
	}
	if _, err := r.Get(ctx, t.ID); err == nil {
		return fmt.Errorf("%w: tenant %s", repository.ErrAlreadyExists, t.ID)
	} else if !errors.Is(err, repository.ErrNotFound) {
		return err
	}
	t.Defaults()
	t.CreatedAt = now()
	t.UpdatedAt = t.CreatedAt
	obj := &cloudv1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: t.ID, Annotations: map[string]string{
		AnnotationCreatedAt: t.CreatedAt.UTC().Format(time.RFC3339Nano),
	}}, Spec: cloudv1alpha1.TenantSpec{DisplayName: t.DisplayName}}
	if err := r.c.Create(ctx, obj); err != nil {
		return translate(err)
	}
	if _, err := tenantlife.EnsureNamespace(ctx, r.c, obj); err != nil {
		cleanup := r.deleteRegistration(ctx, t.ID, obj.UID)
		if errors.Is(err, tenantlife.ErrNamespaceCollision) {
			err = fmt.Errorf("%w: %w", repository.ErrConflict, err)
		}
		return errors.Join(translate(err), cleanup)
	}
	r.created(func(ctx context.Context) error { return r.Delete(ctx, t.ID) })
	return nil
}

// Get prefers the canonical registration. Legacy namespace reads are strictly
// read-only and never adopt an unmarked or foreign namespace.
func (r tenants) Get(ctx context.Context, id string) (*model.Tenant, error) {
	if !tenantlife.ValidID(id) {
		return nil, repository.ErrNotFound
	}
	obj := &cloudv1alpha1.Tenant{}
	err := r.c.Get(ctx, client.ObjectKey{Name: id}, obj)
	if err == nil {
		if !obj.DeletionTimestamp.IsZero() {
			return nil, repository.ErrNotFound
		}
		created := tenantlife.Timestamp(obj)
		result := &model.Tenant{ID: id, DisplayName: obj.Spec.DisplayName, CreatedAt: created, UpdatedAt: created}
		result.Defaults()
		return result, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, translate(err)
	}
	ns, err := r.tenantNamespace(ctx, id)
	if err != nil {
		return nil, err
	}
	if len(ns.OwnerReferences) != 0 || !ns.DeletionTimestamp.IsZero() || ns.Annotations[tenantlife.AnnotationRetainedTenant] != "" || ns.Annotations[tenantlife.AnnotationRegistration] != "" {
		return nil, repository.ErrNotFound
	}
	created, _ := time.Parse(time.RFC3339Nano, ns.Annotations[AnnotationCreatedAt])
	if created.IsZero() {
		created = ns.CreationTimestamp.Time
	}
	result := &model.Tenant{ID: id, DisplayName: ns.Annotations[AnnotationDisplayName], CreatedAt: created, UpdatedAt: created}
	result.Defaults()
	return result, nil
}

// Delete removes the registration and accounts unless networks or clusters use
// it. The known-owned namespace and its contents are retained, never cascaded.
func (r tenants) Delete(ctx context.Context, id string) error {
	obj := &cloudv1alpha1.Tenant{}
	err := r.c.Get(ctx, client.ObjectKey{Name: id}, obj)
	if err != nil && !apierrors.IsNotFound(err) {
		return translate(err)
	}
	canonical := err == nil
	ns, err := r.tenantNamespace(ctx, id)
	if err != nil && !errors.Is(err, repository.ErrNotFound) {
		return err
	}
	if ns == nil {
		if !canonical {
			return repository.ErrNotFound
		}
		// A registration whose namespace has not yet been created can still be removed.
		return r.deleteRegistration(ctx, id)
	}
	if len(ns.OwnerReferences) != 0 {
		return fmt.Errorf("%w: namespace %s has a foreign owner", repository.ErrConflict, ns.Name)
	}
	networks, err := items[*cloudv1alpha1.Network](ctx, r.store, &cloudv1alpha1.NetworkList{}, ns.Name, nil)
	if err != nil {
		return err
	}
	clusters, err := items[*cloudv1alpha1.Cluster](ctx, r.store, &cloudv1alpha1.ClusterList{}, ns.Name, nil)
	if err != nil {
		return err
	}
	if n := len(networks) + len(clusters); n > 0 {
		return fmt.Errorf("%w: the tenant has %d networks and clusters", repository.ErrInUse, n)
	}
	accounts, err := items[*cloudv1alpha1.CloudAccount](ctx, r.store, &cloudv1alpha1.CloudAccountList{}, ns.Name, nil)
	if err != nil {
		return err
	}
	for _, a := range accounts {
		if err := r.remove(ctx, a); err != nil && !errors.Is(err, repository.ErrNotFound) {
			return err
		}
	}
	if err := tenantlife.RetainNamespace(ctx, r.c, id); err != nil {
		return translate(err)
	}
	if canonical {
		return r.deleteRegistration(ctx, id)
	}
	return nil
}

func (r tenants) deleteRegistration(ctx context.Context, id string, expectedUID ...types.UID) error {
	return translate(retry(func() error {
		obj := &cloudv1alpha1.Tenant{}
		if err := r.c.Get(ctx, client.ObjectKey{Name: id}, obj); err != nil {
			return client.IgnoreNotFound(err)
		}
		if len(expectedUID) > 0 && obj.UID != expectedUID[0] {
			return apierrors.NewConflict(cloudv1alpha1.GroupVersion.WithResource("tenants").GroupResource(), id, fmt.Errorf("tenant registration was replaced during namespace creation"))
		}
		if controllerutil.RemoveFinalizer(obj, cloudv1alpha1.TenantFinalizer) {
			if err := r.c.Update(ctx, obj); err != nil {
				return err
			}
			if !obj.DeletionTimestamp.IsZero() {
				return nil
			}
		}
		return client.IgnoreNotFound(r.c.Delete(ctx, obj, client.PropagationPolicy(metav1.DeletePropagationOrphan), client.Preconditions{UID: &obj.UID, ResourceVersion: &obj.ResourceVersion}))
	}))
}

// accountName is the object name of an account: one per provider and
// account ID in a tenant, so a second create of it is refused by the API
// server.
func accountName(provider model.Provider, accountID string) string {
	return string(provider) + "-" + strings.ToLower(accountID)
}

// roleName is the IAM role name of a role ARN: what spec.aws.role holds.
// "" when identity is not a role ARN (an account not bootstrapped yet).
func roleName(identity string) string {
	_, path, found := strings.Cut(identity, ":role/")
	if !found {
		return ""
	}
	return path[strings.LastIndex(path, "/")+1:]
}

// fillAccount writes what a (re-)bootstrap may change onto obj. The
// ProviderConfig name is written only when obj has none: the account's
// managed resources reference it.
func fillAccount(a *model.CloudAccount, obj *cloudv1alpha1.CloudAccount) error {
	m := accountMetaOf(obj)
	if m.ProviderConfigName == "" {
		m.ProviderConfigName, m.CreatedAt = a.ProviderConfigName, now()
	}
	m.Namespace, m.Identity, m.DirectoryID, m.UpdatedAt = a.Namespace, a.Identity, a.DirectoryID, now()
	if m.ExternalID == "" {
		// The first one given stays: the role's trust requires it.
		m.ExternalID = a.ExternalID
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	annotate(obj, map[string]string{AnnotationAccount: string(raw)})
	reader := a.ReaderRole
	if reader == "" && obj.Spec.AWS != nil {
		reader = obj.Spec.AWS.ReaderRole // set on the object by the platform team; the API does not carry it
	}
	obj.Spec = cloudv1alpha1.CloudAccountSpec{AccountName: a.AccountName,
		AWS: &cloudv1alpha1.AWSAccount{AccountID: a.AccountID, Role: roleName(a.Identity), ReaderRole: reader}}
	return nil
}

func accountMetaOf(obj *cloudv1alpha1.CloudAccount) accountMeta {
	var m accountMeta
	_ = json.Unmarshal([]byte(obj.Annotations[AnnotationAccount]), &m)
	return m
}

// accountRow is the row an object holds.
func accountRow(obj *cloudv1alpha1.CloudAccount) *model.CloudAccount {
	m := accountMetaOf(obj)
	a := &model.CloudAccount{
		Base:     model.Base{ID: obj.Labels[LabelID], CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt},
		TenantID: obj.Labels[LabelTenant], Provider: model.ProviderAWS, ProviderConfigName: m.ProviderConfigName,
		Namespace: m.Namespace, Identity: m.Identity, DirectoryID: m.DirectoryID, AccountName: obj.Spec.AccountName,
		ExternalID: m.ExternalID,
	}
	if obj.Spec.AWS != nil {
		a.AccountID, a.ReaderRole = obj.Spec.AWS.AccountID, obj.Spec.AWS.ReaderRole
	}
	a.Access = AccessModel(obj.Status.Access)
	a.AccessRequestedAt = accessRequestedOf(obj)
	a.AccessRequestedRegions = accessRegionsOf(obj)
	return a
}

// PutCloudAccount creates the account, or updates the one the tenant has
// for the provider and account ID (the object of that name, so two puts
// of a new account cannot both create it). The typed status mirrors the
// ProviderConfig name and identity.
func (r tenants) PutCloudAccount(ctx context.Context, a *model.CloudAccount) error {
	ns, err := r.tenantNamespace(ctx, a.TenantID)
	if err != nil {
		return err
	}
	if err := invalid(model.ValidateAccount(a.Provider, a.AccountID, a.DirectoryID)); err != nil {
		return err
	}
	if err := onlyAWS(a.Provider); err != nil {
		return err
	}
	key := client.ObjectKey{Namespace: ns.Name, Name: accountName(a.Provider, a.AccountID)}
	var obj *cloudv1alpha1.CloudAccount
	err = retry(func() error {
		obj = &cloudv1alpha1.CloudAccount{}
		err := r.c.Get(ctx, key, obj)
		if apierrors.IsNotFound(err) {
			if obj, err = r.createAccount(ctx, key, a); apierrors.IsAlreadyExists(err) {
				return apierrors.NewConflict(cloudv1alpha1.GroupVersion.WithResource("cloudaccounts").GroupResource(), key.Name, err)
			}
			return err
		}
		if err != nil {
			return err
		}
		if err := fillAccount(a, obj); err != nil {
			return err
		}
		return r.c.Update(ctx, obj)
	})
	if err != nil {
		return translate(err)
	}
	stored := accountRow(obj)
	a.ID, a.ProviderConfigName, a.CreatedAt, a.UpdatedAt = stored.ID, stored.ProviderConfigName, stored.CreatedAt, stored.UpdatedAt
	return r.mirrorAccount(ctx, key)
}

// createAccount creates the object of a new account, with a new row ID
// and ProviderConfig name.
func (r tenants) createAccount(ctx context.Context, key client.ObjectKey, a *model.CloudAccount) (*cloudv1alpha1.CloudAccount, error) {
	row := *a
	row.Base = model.Base{}
	if err := row.AssignNew(); err != nil {
		return nil, err
	}
	obj := &cloudv1alpha1.CloudAccount{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name,
		Labels: map[string]string{LabelID: row.ID, LabelTenant: row.TenantID}}}
	if err := fillAccount(&row, obj); err != nil {
		return nil, err
	}
	if err := r.c.Create(ctx, obj); err != nil {
		return nil, err
	}
	r.created(func(ctx context.Context) error {
		return translate(r.c.Delete(ctx, obj, client.PropagationPolicy(metav1.DeletePropagationOrphan)))
	})
	return obj, nil
}

// mirrorAccount writes the account's typed status from its row: the
// ProviderConfig name and the identity. The inventory stays.
func (r tenants) mirrorAccount(ctx context.Context, key client.ObjectKey) error {
	return translate(retry(func() error {
		obj := &cloudv1alpha1.CloudAccount{}
		if err := r.c.Get(ctx, key, obj); err != nil {
			return client.IgnoreNotFound(err)
		}
		before := obj.Status.DeepCopy()
		m := accountMetaOf(obj)
		obj.Status.ProviderConfigName, obj.Status.Identity, obj.Status.ObservedGeneration = m.ProviderConfigName, m.Identity, obj.Generation
		if equality.Semantic.DeepEqual(before, &obj.Status) {
			return nil
		}
		return r.c.Status().Update(ctx, obj)
	}))
}

func (r tenants) GetCloudAccount(ctx context.Context, tenantID string, provider model.Provider, accountID string) (*model.CloudAccount, error) {
	if !labelValue(tenantID) || provider != model.ProviderAWS {
		return nil, repository.ErrNotFound
	}
	obj := &cloudv1alpha1.CloudAccount{}
	key := client.ObjectKey{Namespace: kube.TenantNamespace(tenantID), Name: accountName(provider, accountID)}
	if err := r.c.Get(ctx, key, obj); err != nil {
		return nil, translate(err)
	}
	if obj.Labels[LabelTenant] != tenantID {
		return nil, repository.ErrNotFound
	}
	return accountRow(obj), nil
}

func (r tenants) GetCloudAccountByID(ctx context.Context, tenantID, id string) (*model.CloudAccount, error) {
	if !labelValue(tenantID) {
		return nil, repository.ErrNotFound
	}
	obj, err := byID[*cloudv1alpha1.CloudAccount](ctx, r.store, &cloudv1alpha1.CloudAccountList{}, kube.TenantNamespace(tenantID), id)
	if err != nil {
		return nil, err
	}
	return accountRow(obj), nil
}

func (r tenants) ListCloudAccounts(ctx context.Context, tenantID string) ([]model.CloudAccount, error) {
	objects, err := items[*cloudv1alpha1.CloudAccount](ctx, r.store, &cloudv1alpha1.CloudAccountList{},
		kube.TenantNamespace(tenantID), map[string]string{LabelTenant: tenantID})
	if err != nil {
		return nil, err
	}
	out := make([]model.CloudAccount, 0, len(objects))
	for _, obj := range objects {
		out = append(out, *accountRow(obj))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].AccountID < out[j].AccountID
	})
	return out, nil
}
