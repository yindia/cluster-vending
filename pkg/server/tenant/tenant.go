// Package tenant implements the cloud.v1 TenantService: the RPC handlers.
// What they apply to and read from the management cluster is
// pkg/reconcile/account.
package tenant

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"connectrpc.com/connect/v2"

	"github.com/yindia/cluster-vending/pkg/config"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1/cloudv1connect"
	"github.com/yindia/cluster-vending/pkg/kube"
	accountreconcile "github.com/yindia/cluster-vending/pkg/reconcile/account"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

// Condition types of CloudAccountStatus.
const (
	ConditionBootstrapped   = accountreconcile.ConditionBootstrapped
	ConditionAccessVerified = accountreconcile.ConditionAccessVerified
)

// TenantService implements cloud.v1.TenantService.
type TenantService struct{ deps service.Deps }

var _ cloudv1connect.TenantServiceHandler = (*TenantService)(nil)

// NewTenantService returns the TenantService.
func NewTenantService(d service.Deps) *TenantService { return &TenantService{deps: d} }

// rec is the account Reconciler on the service's dependencies: what
// applies and reads the account's objects in the management cluster.
func (s *TenantService) rec() *accountreconcile.Reconciler { return accountreconcile.New(s.deps) }

// BootstrapTenant records the tenant and its account. When a controller
// reconciles accounts (Deps.AccountsByController: the CloudAccount
// controller, with the Kubernetes store) that is all: the controller
// applies what the management cluster needs, and the answer shows the
// account as stored. Otherwise (the server's background loop, which has no
// account reconciler) it also applies it here (account.Reconciler.Apply).
// Every step is idempotent, so a failed call is fixed by calling again.
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
	stored, err := s.record(ctx, &model.CloudAccount{
		TenantID:    req.GetTenantId(),
		Provider:    model.ProviderAWS,
		AccountID:   accountID,
		AccountName: req.GetAccount().GetAccountName(),
		Namespace:   namespace,
		Identity:    roleARN,
	})
	if err != nil {
		return nil, err
	}
	if !s.deps.AccountsByController {
		if err := s.rec().Apply(ctx, stored); err != nil {
			return nil, service.ToConnect(err)
		}
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

// record stores the tenant (created on its first bootstrap) and the
// account, in one transaction. A new account gets a generated
// ProviderConfig name; an existing one keeps it and updates the rest. The
// tenant's other accounts are not touched.
func (s *TenantService) record(ctx context.Context, a *model.CloudAccount) (*model.CloudAccount, error) {
	var stored *model.CloudAccount
	err := s.deps.Store.WithinTransaction(ctx, func(tx repository.Store) error {
		if err := ensureTenant(ctx, tx.Tenants(), a.TenantID); err != nil {
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

func ensureTenant(ctx context.Context, tenants repository.Tenants, tenantID string) error {
	_, err := tenants.Get(ctx, tenantID)
	if !errors.Is(err, repository.ErrNotFound) {
		return err
	}
	err = tenants.Create(ctx, &model.Tenant{ID: tenantID})
	if errors.Is(err, repository.ErrAlreadyExists) {
		return nil // created concurrently
	}
	return err
}

func (s *TenantService) logger() *slog.Logger {
	if s.deps.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return s.deps.Logger
}
