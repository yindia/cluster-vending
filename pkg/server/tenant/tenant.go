// Package tenant implements the cloud.v1 TenantService.
package tenant

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/config"
	"github.com/yindia/cluster-vending/pkg/crossplane"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1/cloudv1connect"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

// Condition types of CloudAccountStatus.
const (
	// ConditionBootstrapped: the namespace and the account's ProviderConfig
	// are applied.
	ConditionBootstrapped = "Bootstrapped"
	// ConditionAccessVerified: the tenant role was assumed successfully.
	ConditionAccessVerified = "AccessVerified"
)

// TenantService implements cloud.v1.TenantService.
type TenantService struct{ deps service.Deps }

var _ cloudv1connect.TenantServiceHandler = (*TenantService)(nil)

// NewTenantService returns the TenantService.
func NewTenantService(d service.Deps) *TenantService { return &TenantService{deps: d} }

// BootstrapTenant records the tenant and its account, then applies what the
// management cluster needs to reach the account. Every step is idempotent,
// so a failed call is fixed by calling again.
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
	account, err := s.record(ctx, &model.CloudAccount{
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
	if err := s.applyAWS(ctx, account); err != nil {
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
		"cloud_account_id", account.ID, "account_id", accountID, "namespace", account.Namespace,
		"provider_config", account.ProviderConfigName, "identity", account.Identity)
	out, err := s.view(ctx, account)
	return &cloudv1.BootstrapTenantResponse{Account: out}, err
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
	out, err := s.view(ctx, a)
	return &cloudv1.GetCloudAccountResponse{Account: out}, err
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
		v, err := s.view(ctx, &accounts[i])
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return &cloudv1.ListCloudAccountsResponse{Accounts: out}, nil
}

// record stores the tenant (created on its first bootstrap) and the
// account, in one transaction. A new account gets a generated
// ProviderConfig name; an existing one keeps it and updates the rest. The
// tenant's other accounts are not touched.
func (s *TenantService) record(ctx context.Context, account *model.CloudAccount) (*model.CloudAccount, error) {
	var stored *model.CloudAccount
	err := s.deps.Store.WithinTransaction(ctx, func(tx repository.Store) error {
		if err := ensureTenant(ctx, tx.Tenants(), account.TenantID); err != nil {
			return err
		}
		if err := tx.Tenants().PutCloudAccount(ctx, account); err != nil {
			return err
		}
		// Read back: the stored ID, ProviderConfig name and timestamps, also
		// when the account already existed.
		var err error
		stored, err = tx.Tenants().GetCloudAccount(ctx, account.TenantID, account.Provider, account.AccountID)
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

// applyAWS applies the tenant namespace and the account's ProviderConfig,
// from the stored account. The providers and their runtime config are the
// platform's, installed beforehand: without them the ProviderConfig kind
// does not exist and the call fails with FAILED_PRECONDITION.
func (s *TenantService) applyAWS(ctx context.Context, a *model.CloudAccount) error {
	objects := []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: a.Namespace, Labels: crossplane.Labels(a.TenantID)}},
		crossplaneaws.ProviderConfig(a.Namespace, a.ProviderConfigName, a.TenantID, a.Identity),
	}
	for _, obj := range objects {
		err := kube.Apply(ctx, s.deps.Kube, obj)
		switch {
		case err == nil:
		case meta.IsNoMatchError(err):
			return connect.NewError(connect.CodeFailedPrecondition,
				"the Crossplane AWS provider is not installed in the management cluster (no aws.m.upbound.io ProviderConfig kind)")
		default:
			s.logger().ErrorContext(ctx, "bootstrap apply failed", "tenant_id", a.TenantID, "error", err)
			return connect.NewError(connect.CodeInternal, "could not configure the management cluster")
		}
	}
	owner := service.Owner{TenantID: a.TenantID, CloudAccountID: a.ID, Kind: model.OwnerCloudAccount, ID: a.ID}
	if err := s.deps.Record(ctx, owner, objects); err != nil {
		s.logger().ErrorContext(ctx, "record bootstrap resources", "tenant_id", a.TenantID, "error", err)
		return connect.NewError(connect.CodeInternal, "could not record the bootstrap's resources")
	}
	return nil
}

// view is the API view of a stored account with every object created
// under it (its own, and its networks', clusters' and node pools').
func (s *TenantService) view(ctx context.Context, a *model.CloudAccount) (*cloudv1.CloudAccount, error) {
	records, err := s.deps.Store.ManagedResources().ListByAccount(ctx, a.TenantID, a.ID)
	if err != nil {
		return nil, service.ToConnect(err)
	}
	views, err := s.deps.Observe(ctx, records)
	if err != nil {
		return nil, err
	}
	out := response(a)
	out.Status.ManagedResources = views
	return out, nil
}

// response is the API view of a stored account. Only AWS can be
// bootstrapped so far; the status is what bootstrap applies (it is not
// stored).
func response(a *model.CloudAccount) *cloudv1.CloudAccount {
	return &cloudv1.CloudAccount{
		Id:          a.ID,
		TenantId:    a.TenantID,
		AccountName: a.AccountName,
		Provider:    &cloudv1.CloudAccount_Aws{Aws: &cloudv1.AwsAccount{AccountId: a.AccountID, Role: roleName(a.Identity)}},
		CreateTime:  timestamppb.New(a.CreatedAt),
		UpdateTime:  timestamppb.New(a.UpdatedAt),
		Status: &cloudv1.CloudAccountStatus{
			State:              cloudv1.CloudAccountState_CLOUD_ACCOUNT_STATE_READY,
			Namespace:          a.Namespace,
			ProviderConfigName: a.ProviderConfigName,
			Identity:           a.Identity,
			Conditions: []*cloudv1.Condition{
				{Type: ConditionBootstrapped, Status: cloudv1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "Applied"},
				{Type: ConditionAccessVerified, Status: cloudv1.ConditionStatus_CONDITION_STATUS_UNKNOWN, Reason: "VerificationDisabled",
					Message: "the tenant role is not assumed at bootstrap yet; errors show up on the managed resources"},
			},
		},
	}
}

// roleName is the role of an AWS identity, arn:aws:iam::<account>:role/<name>;
// empty for accounts bootstrapped before identities were stored.
func roleName(identity string) string {
	if i := strings.LastIndex(identity, ":role/"); i >= 0 {
		return identity[i+len(":role/"):]
	}
	return ""
}

func (s *TenantService) logger() *slog.Logger {
	if s.deps.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return s.deps.Logger
}
