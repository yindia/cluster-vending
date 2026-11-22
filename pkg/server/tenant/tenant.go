// Package tenant implements the cloud.v1 TenantService: the RPC handlers.
// What they apply to and read from the management cluster is
// pkg/reconcile/account.
package tenant

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"connectrpc.com/connect/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/config"
	awscatalog "github.com/yindia/cluster-vending/pkg/constant/aws"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1/cloudv1connect"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/reconcile"
	accountreconcile "github.com/yindia/cluster-vending/pkg/reconcile/account"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

// Condition types of CloudAccountStatus.
const (
	ConditionBootstrapped   = accountreconcile.ConditionBootstrapped
	ConditionAccessVerified = accountreconcile.ConditionAccessVerified
)

// TenantService implements cloud.v1.TenantService.
type TenantService struct {
	deps reconcile.Deps
	// onboarding is what GetAccountOnboarding answers with.
	onboarding Onboarding
}

var _ cloudv1connect.TenantServiceHandler = (*TenantService)(nil)

// NewTenantService returns the TenantService.
func NewTenantService(d reconcile.Deps, opts ...Option) *TenantService {
	s := &TenantService{deps: d}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// rec is the account Reconciler on the service's dependencies: what
// applies and reads the account's objects in the management cluster.
func (s *TenantService) rec() *accountreconcile.Reconciler { return accountreconcile.New(s.deps) }

// BootstrapTenant registers an account for an existing Tenant CR and owned
// namespace. The account controller applies credentials and ProviderConfig;
// tenant lifecycle exclusively owns namespace creation.
func (s *TenantService) BootstrapTenant(ctx context.Context, req *cloudv1.BootstrapTenantRequest) (*cloudv1.BootstrapTenantResponse, error) {
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	accountID := req.GetAccount().GetAws().GetAccountId()
	if req.GetAccount().GetAws() == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, "only aws accounts are supported")
	}
	roleARN, err := config.TenantRoleARN(accountID, req.GetAccount().GetAws().GetRole())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}

	namespace := kube.TenantNamespace(req.GetTenantId())
	externalID, err := newExternalID()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, "generate the account's external ID: "+err.Error())
	}
	stored, err := s.record(ctx, &model.CloudAccount{
		TenantID:    req.GetTenantId(),
		Provider:    model.ProviderAWS,
		AccountID:   accountID,
		AccountName: req.GetAccount().GetAccountName(),
		Namespace:   namespace,
		Identity:    roleARN,
		// Kept only on the account's first bootstrap that has one
		// (the store keeps the first): the role's trust requires it.
		ExternalID: externalID,
	})
	if err != nil {
		return nil, err
	}

	// Account access check, disabled for now: assume the tenant role once,
	// so a wrong account ID or a missing trust policy fails here rather than
	// in every managed resource. Needs a *cloud/aws.Client in Deps.
	//
	//	tenant, err := s.deps.AWSClient.Tenant(accountID, roleARN, awscatalog.USEast1)
	//	if err != nil {
	//		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
	//	}
	//	if err := tenant.VerifyAccess(ctx); err != nil {
	//		return nil, connect.NewError(connect.CodeFailedPrecondition,
	//			fmt.Sprintf("cannot assume %s: %v", roleARN, err))
	//	}

	s.logger().InfoContext(ctx, "tenant bootstrapped",
		"tenant_id", req.GetTenantId(), "provider", model.ProviderAWS,
		"cloud_account_id", stored.ID, "account_id", accountID, "namespace", stored.Namespace,
		"provider_config", stored.ProviderConfigName, "identity", stored.Identity)
	out, err := s.rec().View(ctx, stored)
	return &cloudv1.BootstrapTenantResponse{Account: out}, service.ToConnect(err)
}

// GetCloudAccount returns the tenant's account with the given bootstrap ID.
func (s *TenantService) GetCloudAccount(ctx context.Context, req *cloudv1.GetCloudAccountRequest) (*cloudv1.GetCloudAccountResponse, error) {
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	a, err := s.deps.Store.Tenants().GetCloudAccountByID(ctx, req.GetTenantId(), req.GetId())
	if errors.Is(err, repository.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound,
			fmt.Sprintf("tenant %s has no bootstrapped account %s", req.GetTenantId(), req.GetId()))
	}
	if err != nil {
		return nil, service.ToConnect(err)
	}
	out, err := s.rec().View(ctx, a)
	return &cloudv1.GetCloudAccountResponse{Account: out}, service.ToConnect(err)
}

// VerifyCloudAccount records a request for an access check of the
// tenant's account; the controller runs it (account.Reconciler.
// VerifyAccess) and writes the result to the account's status. It returns
// the account as it is now.
func (s *TenantService) VerifyCloudAccount(ctx context.Context, req *cloudv1.VerifyCloudAccountRequest) (*cloudv1.VerifyCloudAccountResponse, error) {
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	for _, region := range req.GetRegions() {
		if !awscatalog.IsRegion(region) {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Sprintf("region %q is not a known AWS region", region))
		}
	}
	err := s.deps.Store.Tenants().RequestAccessCheck(ctx, req.GetTenantId(), req.GetId(), time.Now(), req.GetRegions())
	if errors.Is(err, repository.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound,
			fmt.Sprintf("tenant %s has no bootstrapped account %s", req.GetTenantId(), req.GetId()))
	}
	if err != nil {
		return nil, service.ToConnect(err)
	}
	a, err := s.deps.Store.Tenants().GetCloudAccountByID(ctx, req.GetTenantId(), req.GetId())
	if err != nil {
		return nil, service.ToConnect(err)
	}
	return &cloudv1.VerifyCloudAccountResponse{Account: accountreconcile.Proto(a)}, nil
}

// ListCloudAccounts returns the tenant's accounts, ordered by provider. An
// unknown tenant has none.
func (s *TenantService) ListCloudAccounts(ctx context.Context, req *cloudv1.ListCloudAccountsRequest) (*cloudv1.ListCloudAccountsResponse, error) {
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	accounts, err := s.deps.Store.Tenants().ListCloudAccounts(ctx, req.GetTenantId())
	if err != nil {
		return nil, service.ToConnect(err)
	}
	out := make([]*cloudv1.CloudAccount, 0, len(accounts))
	for i := range accounts {
		v, err := s.rec().View(ctx, &accounts[i])
		if err != nil {
			return nil, service.ToConnect(err)
		}
		out = append(out, v)
	}
	return &cloudv1.ListCloudAccountsResponse{Accounts: out}, nil
}

// record stores an account only after checking its registered Tenant CR
// and owned namespace. A new account gets a generated
// ProviderConfig name; an existing one keeps it and updates the rest. The
// tenant's other accounts are not touched.
func (s *TenantService) record(ctx context.Context, a *model.CloudAccount) (*model.CloudAccount, error) {
	if s.deps.Store == nil || s.directoryReader() == nil {
		return nil, connect.NewError(connect.CodeUnavailable, "tenant registration is unavailable")
	}
	if err := s.requireRegisteredTenant(ctx, a.TenantID); err != nil {
		return nil, err
	}
	var stored *model.CloudAccount
	err := s.deps.Store.WithinTransaction(ctx, func(tx repository.Store) error {
		if _, err := tx.Tenants().Get(ctx, a.TenantID); err != nil {
			return err
		}
		if err := tx.Tenants().PutCloudAccount(ctx, a); err != nil {
			return err
		}
		// Read back: the stored ID, ProviderConfig name and timestamps, also
		// when the account already existed.
		var err error
		stored, err = tx.Tenants().GetCloudAccount(ctx, a.TenantID, a.Provider, a.AccountID)
		return err
	})
	if err != nil {
		return nil, service.ToConnect(err)
	}
	return stored, nil
}

// Read the CR explicitly: Store.Get permits legacy namespace migration reads,
// which must never authorize a new account registration.
func (s *TenantService) requireRegisteredTenant(ctx context.Context, id string) error {
	reader := s.deps.Kube
	if reader == nil {
		return connect.NewError(connect.CodeUnavailable, "tenant registration is unavailable")
	}
	tenant := &cloudv1alpha1.Tenant{}
	if err := reader.Get(ctx, client.ObjectKey{Name: id}, tenant); apierrors.IsNotFound(err) {
		return connect.NewError(connect.CodeNotFound, "tenant is not registered; create the tenant before registering a cloud account")
	} else if err != nil {
		return connect.NewError(connect.CodeUnavailable, "cannot read tenant registration")
	}
	if tenant.DeletionTimestamp != nil {
		return connect.NewError(connect.CodeFailedPrecondition, "tenant is being deleted")
	}
	namespace := &corev1.Namespace{}
	if err := reader.Get(ctx, client.ObjectKey{Name: kube.TenantNamespace(id)}, namespace); apierrors.IsNotFound(err) {
		return connect.NewError(connect.CodeFailedPrecondition, "tenant namespace is not ready; wait for tenant registration before registering a cloud account")
	} else if err != nil {
		return connect.NewError(connect.CodeUnavailable, "cannot read tenant namespace")
	}
	if namespace.Labels[cloudstore.LabelTenant] != id || len(namespace.OwnerReferences) > 0 || namespace.DeletionTimestamp != nil || namespace.Status.Phase == corev1.NamespaceTerminating {
		return connect.NewError(connect.CodeFailedPrecondition, "tenant namespace is unavailable or is not owned by this tenant")
	}
	return nil
}

func (s *TenantService) logger() *slog.Logger {
	if s.deps.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return s.deps.Logger
}

// DeleteCloudAccount asks for the account's deletion; the controller
// removes its ProviderConfig, then the account.
func (s *TenantService) DeleteCloudAccount(ctx context.Context, req *cloudv1.DeleteCloudAccountRequest) (*cloudv1.DeleteCloudAccountResponse, error) {
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	a, err := s.deps.Store.Tenants().GetCloudAccountByID(ctx, req.GetTenantId(), req.GetId())
	if err == nil {
		err = s.deps.Store.Tenants().DeleteCloudAccount(ctx, req.GetTenantId(), req.GetId())
	}
	if errors.Is(err, repository.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound,
			fmt.Sprintf("tenant %s has no bootstrapped account %s", req.GetTenantId(), req.GetId()))
	}
	if errors.Is(err, repository.ErrInUse) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err.Error())
	}
	if err != nil {
		return nil, service.ToConnect(err)
	}
	return &cloudv1.DeleteCloudAccountResponse{Account: accountreconcile.Proto(a)}, nil
}
