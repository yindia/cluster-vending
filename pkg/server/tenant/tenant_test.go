package tenant

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	pkgv1 "github.com/crossplane/crossplane/apis/v2/pkg/v1"
	pkgv1beta1 "github.com/crossplane/crossplane/apis/v2/pkg/v1beta1"
	awsv1beta1 "github.com/upbound/provider-aws/v2/apis/namespaced/v1beta1"
	"go.uber.org/mock/gomock"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"

	"github.com/yindia/cluster-vending/pkg/crossplane"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/database/repository/mock"
	"github.com/yindia/cluster-vending/pkg/fault"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/reconcile"
	accountreconcile "github.com/yindia/cluster-vending/pkg/reconcile/account"
)

const (
	mgmtRole   = "arn:aws:iam::111122223333:role/crossplane-provider-aws"
	account    = "444455556666"
	newAccount = "777788889999"
)

// fixture is a TenantService on repository mocks and a fake Kubernetes
// client: no database, no cluster.
type fixture struct {
	svc     *TenantService
	kube    client.Client
	tenants *mock.MockTenants
	// recorded holds what the ManagedResources mock was given.
	recorded *[]model.ManagedResource
}

func newFixture(t *testing.T, funcs ...interceptor.Funcs) fixture {
	t.Helper()
	ctrl := gomock.NewController(t)
	store := mock.NewMockStore(ctrl)
	tenants := mock.NewMockTenants(ctrl)
	store.EXPECT().Tenants().Return(tenants).AnyTimes()
	recorded := &[]model.ManagedResource{}
	managed := mock.NewMockManagedResources(ctrl)
	store.EXPECT().ManagedResources().Return(managed).AnyTimes()
	managed.EXPECT().Put(gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, rows []model.ManagedResource) error {
		*recorded = append(*recorded, rows...)
		return nil
	}).AnyTimes()
	managed.EXPECT().ListByAccount(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, tenantID, accountID string) ([]model.ManagedResource, error) {
			var out []model.ManagedResource
			for _, r := range *recorded {
				if r.TenantID == tenantID && r.CloudAccountID == accountID {
					out = append(out, r)
				}
			}
			return out, nil
		}).AnyTimes()
	store.EXPECT().WithinTransaction(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, fn func(repository.Store) error) error { return fn(store) }).AnyTimes()

	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	b := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&cloudv1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "acme"}}, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant-acme", Labels: map[string]string{cloudstore.LabelTenant: "acme", crossplane.LabelTenantID: "acme"}}})
	for _, f := range funcs {
		b = b.WithInterceptorFuncs(f)
	}
	k := b.Build()
	svc := NewTenantService(reconcile.Deps{
		Store: store,
		Kube:  k,
	})
	return fixture{svc: svc, kube: k, tenants: tenants, recorded: recorded}
}

func request(tenantID, accountID string) *cloudv1.BootstrapTenantRequest {
	return &cloudv1.BootstrapTenantRequest{
		TenantId: tenantID,
		Account: &cloudv1.CloudAccount{
			AccountName: "Acme production",
			Provider: &cloudv1.CloudAccount_Aws{Aws: &cloudv1.AwsAccount{
				AccountId: accountID, Role: "cluster-vending-provisioner",
			}},
		},
	}
}

// providerConfigNames are what the database generated per account.
var providerConfigNames = map[string]string{account: "aws-0a1b2c3d", newAccount: "aws-4e5f6a7b"}

func stored(accountID string) *model.CloudAccount {
	now := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	return &model.CloudAccount{
		Base:     model.Base{ID: "row-" + accountID, CreatedAt: now, UpdatedAt: now},
		TenantID: "acme", Provider: model.ProviderAWS, AccountID: accountID, AccountName: "Acme production",
		ProviderConfigName: providerConfigNames[accountID],
		Namespace:          "tenant-acme",
		Identity:           roleARN(accountID),
	}
}

func roleARN(accountID string) string {
	return "arn:aws:iam::" + accountID + ":role/cluster-vending-provisioner"
}

// expectTenant: the tenant exists.
func (f fixture) expectTenant() {
	f.tenants.EXPECT().Get(gomock.Any(), "acme").Return(&model.Tenant{ID: "acme"}, nil)
}

// expectPut: the account is written and read back.
func (f fixture) expectPut(accountID string) {
	f.tenants.EXPECT().PutCloudAccount(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, a *model.CloudAccount) error {
			if a.TenantID != "acme" || a.Provider != model.ProviderAWS || a.AccountID != accountID ||
				a.AccountName != "Acme production" || a.Namespace != "tenant-acme" || a.Identity != roleARN(accountID) {
				return errors.New("unexpected account")
			}
			return nil
		})
	f.tenants.EXPECT().GetCloudAccount(gomock.Any(), "acme", model.ProviderAWS, accountID).Return(stored(accountID), nil)
}

// apply is what the CloudAccount controller does with a stored account:
// BootstrapTenant only stores it.
func (f fixture) apply(t *testing.T, a *model.CloudAccount) error {
	t.Helper()
	return accountreconcile.New(f.svc.deps).Apply(t.Context(), a)
}

func (f fixture) providerConfig(t *testing.T, name string) *awsv1beta1.ProviderConfig {
	t.Helper()
	pc := &awsv1beta1.ProviderConfig{}
	if err := f.kube.Get(t.Context(), client.ObjectKey{Namespace: "tenant-acme", Name: name}, pc); err != nil {
		t.Fatalf("provider config %s: %v", name, err)
	}
	return pc
}

func TestBootstrapTenantCreatesResources(t *testing.T) {
	f := newFixture(t)
	f.expectTenant()
	f.expectPut(account)
	ctx := t.Context()

	res, err := f.svc.BootstrapTenant(ctx, request("acme", account))
	if err != nil {
		t.Fatalf("BootstrapTenant: %v", err)
	}

	st := res.GetAccount().GetStatus()
	if st.GetNamespace() != "tenant-acme" || st.GetProviderConfigName() != "aws-0a1b2c3d" || st.GetIdentity() != roleARN(account) {
		t.Errorf("status = %v", st)
	}
	if res.GetAccount().GetAws().GetAccountId() != account || res.GetAccount().GetCreateTime() == nil {
		t.Errorf("account = %v", res.GetAccount())
	}
	if err := f.apply(t, stored(account)); err != nil {
		t.Fatalf("apply: %v", err)
	}

	ns := &corev1.Namespace{}
	if err := f.kube.Get(ctx, client.ObjectKey{Name: "tenant-acme"}, ns); err != nil {
		t.Fatalf("namespace: %v", err)
	}
	if ns.Labels[crossplane.LabelTenantID] != "acme" {
		t.Errorf("namespace labels = %v", ns.Labels)
	}
	// The providers and their runtime config are the platform's: bootstrap
	// does not touch them.
	runtimeConfigs := &pkgv1beta1.DeploymentRuntimeConfigList{}
	providers := &pkgv1.ProviderList{}
	if err := f.kube.List(ctx, runtimeConfigs); err != nil || len(runtimeConfigs.Items) != 0 {
		t.Errorf("runtime configs = %d, %v; want none", len(runtimeConfigs.Items), err)
	}
	if err := f.kube.List(ctx, providers); err != nil || len(providers.Items) != 0 {
		t.Errorf("providers = %d, %v; want none", len(providers.Items), err)
	}
	pc := f.providerConfig(t, "aws-0a1b2c3d")
	if pc.Spec.Credentials.Source != "IRSA" || *pc.Spec.AssumeRoleChain[0].RoleARN != "arn:aws:iam::444455556666:role/cluster-vending-provisioner" {
		t.Errorf("provider config spec = %+v", pc.Spec)
	}
}

func TestBootstrapTenantAgainWithSameAccount(t *testing.T) {
	f := newFixture(t)
	f.expectTenant()
	f.expectPut(account)

	if _, err := f.svc.BootstrapTenant(t.Context(), request("acme", account)); err != nil {
		t.Fatalf("BootstrapTenant: %v", err)
	}
}

func TestEveryAccountGetsItsOwnProviderConfig(t *testing.T) {
	f := newFixture(t)
	f.expectTenant()
	f.expectPut(account)
	f.expectTenant()
	f.expectPut(newAccount)

	for _, id := range []string{account, newAccount} {
		if _, err := f.svc.BootstrapTenant(t.Context(), request("acme", id)); err != nil {
			t.Fatalf("BootstrapTenant(%s): %v", id, err)
		}
		if err := f.apply(t, stored(id)); err != nil {
			t.Fatalf("apply(%s): %v", id, err)
		}
	}
	for id, name := range providerConfigNames {
		want := "arn:aws:iam::" + id + ":role/cluster-vending-provisioner"
		if got := *f.providerConfig(t, name).Spec.AssumeRoleChain[0].RoleARN; got != want {
			t.Errorf("%s assumes %s, want %s", name, got, want)
		}
	}
}

func TestBootstrapTenantDatabaseErrorIsInternal(t *testing.T) {
	f := newFixture(t)
	f.tenants.EXPECT().Get(gomock.Any(), "acme").Return(nil, errors.New("connection refused"))

	_, err := f.svc.BootstrapTenant(t.Context(), request("acme", account))
	if connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("err = %v, want INTERNAL", err)
	}
}

func TestBootstrapTenantWithoutAWSProvider(t *testing.T) {
	// The ProviderConfig kind exists once the platform installed the AWS
	// provider.
	f := newFixture(t, interceptor.Funcs{
		Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
			if u, ok := obj.(interface{ GetKind() string }); ok && u.GetKind() == "ProviderConfig" {
				return &meta.NoKindMatchError{GroupKind: awsv1beta1.SchemeGroupVersion.WithKind("ProviderConfig").GroupKind()}
			}
			return c.Apply(ctx, obj, opts...)
		},
	})
	f.expectTenant()
	f.expectPut(account)

	// The call stores the account; applying it is what fails.
	if _, err := f.svc.BootstrapTenant(t.Context(), request("acme", account)); err != nil {
		t.Fatalf("BootstrapTenant: %v", err)
	}
	if err := f.apply(t, stored(account)); !fault.Is(err, fault.FailedPrecondition) {
		t.Fatalf("apply = %v, want a FailedPrecondition fault", err)
	}
}

func TestBootstrapTenantValidates(t *testing.T) {
	f := newFixture(t) // no store calls expected
	for _, req := range []*cloudv1.BootstrapTenantRequest{
		request("Acme_Corp", account),
		request("acme", "1234"),
		{TenantId: "acme"},
		func() *cloudv1.BootstrapTenantRequest {
			r := request("acme", account)
			r.Account.GetAws().Role = ""
			return r
		}(),
	} {
		if _, err := f.svc.BootstrapTenant(t.Context(), req); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("BootstrapTenant(%v) = %v, want INVALID_ARGUMENT", req, err)
		}
	}
}

func TestGetAndListCloudAccounts(t *testing.T) {
	f := newFixture(t)
	const id, unknown = "6b1f2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d", "0b1f6b2c-3d4e-4f5a-8b6c-7d8e9f0a1b2c"
	row := stored(account)
	row.ID = id
	f.tenants.EXPECT().GetCloudAccountByID(gomock.Any(), "acme", id).Return(row, nil)
	f.tenants.EXPECT().GetCloudAccountByID(gomock.Any(), "acme", unknown).Return(nil, repository.ErrNotFound)
	f.tenants.EXPECT().ListCloudAccounts(gomock.Any(), "acme").Return([]model.CloudAccount{*row}, nil)
	ctx := t.Context()

	got, err := f.svc.GetCloudAccount(ctx, &cloudv1.GetCloudAccountRequest{TenantId: "acme", Id: id})
	if err != nil || got.GetAccount().GetId() != id || got.GetAccount().GetAws().GetAccountId() != account ||
		got.GetAccount().GetStatus().GetProviderConfigName() != "aws-0a1b2c3d" {
		t.Fatalf("GetCloudAccount = %v, %v", got, err)
	}
	_, err = f.svc.GetCloudAccount(ctx, &cloudv1.GetCloudAccountRequest{TenantId: "acme", Id: unknown})
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("unknown account = %v, want NOT_FOUND", err)
	}
	if _, err := f.svc.GetCloudAccount(ctx, &cloudv1.GetCloudAccountRequest{TenantId: "acme", Id: account}); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("an AWS account ID is not a bootstrap ID: %v, want INVALID_ARGUMENT", err)
	}
	list, err := f.svc.ListCloudAccounts(ctx, &cloudv1.ListCloudAccountsRequest{TenantId: "acme"})
	if err != nil || len(list.GetAccounts()) != 1 || list.GetAccounts()[0].GetId() != id {
		t.Fatalf("ListCloudAccounts = %v, %v", list, err)
	}

	if _, err := f.svc.ListCloudAccounts(ctx, &cloudv1.ListCloudAccountsRequest{}); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("missing tenant = %v, want INVALID_ARGUMENT", err)
	}
}

func TestBootstrapTenantUsesRequestedRole(t *testing.T) {
	f := newFixture(t)
	f.expectTenant()
	f.tenants.EXPECT().PutCloudAccount(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, a *model.CloudAccount) error {
			if a.Identity != "arn:aws:iam::444455556666:role/acme-admin" {
				return errors.New("identity " + a.Identity)
			}
			return nil
		})
	row := stored(account)
	row.Identity = "arn:aws:iam::444455556666:role/acme-admin"
	f.tenants.EXPECT().GetCloudAccount(gomock.Any(), "acme", model.ProviderAWS, account).Return(row, nil)

	req := request("acme", account)
	req.Account.GetAws().Role = "acme-admin"
	res, err := f.svc.BootstrapTenant(t.Context(), req)
	if err != nil {
		t.Fatalf("BootstrapTenant: %v", err)
	}
	if got := res.GetAccount().GetAws().GetRole(); got != "acme-admin" {
		t.Errorf("response role name = %q", got)
	}
	if err := f.apply(t, row); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got := *f.providerConfig(t, "aws-0a1b2c3d").Spec.AssumeRoleChain[0].RoleARN; got != row.Identity {
		t.Errorf("ProviderConfig assumes %s", got)
	}
}

func TestBootstrapRecordsWhatItCreates(t *testing.T) {
	f := newFixture(t)
	f.expectTenant()
	f.expectPut(account)

	if _, err := f.svc.BootstrapTenant(t.Context(), request("acme", account)); err != nil {
		t.Fatal(err)
	}
	if len(*f.recorded) != 0 {
		t.Fatalf("recorded = %+v before the controller applied it, want nothing", *f.recorded)
	}
	if err := f.apply(t, stored(account)); err != nil {
		t.Fatal(err)
	}
	if len(*f.recorded) != 1 {
		t.Fatalf("recorded = %+v, want only the ProviderConfig", *f.recorded)
	}
	pc := (*f.recorded)[0]
	if pc.Kind != "ProviderConfig" || pc.APIVersion != "aws.m.upbound.io/v1beta1" || pc.Name != "aws-0a1b2c3d" ||
		pc.OwnerKind != model.OwnerCloudAccount || pc.OwnerID != "row-"+account || pc.CloudAccountID != "row-"+account {
		t.Errorf("recorded = %+v", pc)
	}
	view, err := accountreconcile.New(f.svc.deps).View(t.Context(), stored(account))
	if err != nil {
		t.Fatal(err)
	}
	views := view.GetStatus().GetManagedResources()
	if len(views) != 1 || views[0].GetState() != cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY ||
		views[0].GetKind() != "ProviderConfig" {
		t.Errorf("account resources = %v", views)
	}
}

func TestBootstrapRequiresTenantCRAndNamespace(t *testing.T) {
	for _, test := range []struct {
		name                      string
		removeCR, removeNamespace bool
		want                      connect.Code
	}{
		{"legacy namespace alone", true, false, connect.CodeNotFound},
		{"registered tenant without namespace", false, true, connect.CodeFailedPrecondition},
		{"registered tenant and namespace", false, false, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t)
			if test.removeCR {
				if err := f.kube.Delete(t.Context(), &cloudv1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "acme"}}); err != nil {
					t.Fatal(err)
				}
			}
			if test.removeNamespace {
				if err := f.kube.Delete(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant-acme"}}); err != nil {
					t.Fatal(err)
				}
			}
			if test.want == 0 {
				f.expectTenant()
				f.expectPut(account)
			}
			// No repository writes are expected on either missing prerequisite.
			_, err := f.svc.BootstrapTenant(t.Context(), request("acme", account))
			if test.want == 0 {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || connect.CodeOf(err) != test.want {
				t.Fatalf("bootstrap: %v, want %v", err, test.want)
			}
			ns := &corev1.Namespace{}
			err = f.kube.Get(t.Context(), client.ObjectKey{Name: "tenant-acme"}, ns)
			if test.removeNamespace && err == nil {
				t.Fatal("bootstrap created a namespace")
			}
			if !test.removeNamespace && err != nil {
				t.Fatal(err)
			}
			if len(*f.recorded) != 0 {
				t.Fatal("bootstrap mutated managed resources")
			}
		})
	}
}
func TestBootstrapRejectsUnownedNamespace(t *testing.T) {
	for _, owner := range []string{"", "foreign"} {
		t.Run(owner, func(t *testing.T) {
			f := newFixture(t)
			ns := &corev1.Namespace{}
			if err := f.kube.Get(t.Context(), client.ObjectKey{Name: "tenant-acme"}, ns); err != nil {
				t.Fatal(err)
			}
			ns.Labels[cloudstore.LabelTenant] = owner
			if err := f.kube.Update(t.Context(), ns); err != nil {
				t.Fatal(err)
			}
			_, err := f.svc.BootstrapTenant(t.Context(), request("acme", account))
			if connect.CodeOf(err) != connect.CodeFailedPrecondition {
				t.Fatal(err)
			}
		})
	}
}

func TestBootstrapRejectsUnavailableTenantOwnership(t *testing.T) {
	for _, state := range []string{"namespace owner reference", "namespace deleting", "namespace terminating", "tenant deleting"} {
		t.Run(state, func(t *testing.T) {
			funcs := noWrites()
			funcs.Get = func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if err := c.Get(ctx, key, obj, opts...); err != nil {
					return err
				}
				switch value := obj.(type) {
				case *corev1.Namespace:
					switch state {
					case "namespace owner reference":
						value.OwnerReferences = []metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: "foreign-owner", UID: "foreign-owner"}}
					case "namespace deleting":
						now := metav1.Now()
						value.DeletionTimestamp = &now
					case "namespace terminating":
						value.Status.Phase = corev1.NamespaceTerminating
					}
				case *cloudv1alpha1.Tenant:
					if state == "tenant deleting" {
						now := metav1.Now()
						value.DeletionTimestamp = &now
					}
				}
				return nil
			}
			f := newFixture(t, funcs)
			// No Create/Put expectations: a failed prerequisite cannot write an account.
			_, err := f.svc.BootstrapTenant(t.Context(), request("acme", account))
			if connect.CodeOf(err) != connect.CodeFailedPrecondition {
				t.Fatalf("bootstrap: %v", err)
			}
			if len(*f.recorded) != 0 {
				t.Fatal("failed prerequisite recorded resources")
			}
		})
	}
}
