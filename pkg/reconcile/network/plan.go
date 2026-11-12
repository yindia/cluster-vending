package network

import (
	"context"
	"errors"
	"fmt"

	awscatalog "github.com/yindia/cluster-vending/pkg/constant/aws"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/fault"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	corenetwork "github.com/yindia/cluster-vending/pkg/network"
)

// Plan checks the account and region and resolves the layout. The cloud
// is the account's: a network has no provider of its own.
func (s *Reconciler) Plan(ctx context.Context, tenantID string, spec *cloudv1.NetworkSpec) (*model.CloudAccount, corenetwork.Layout, error) {
	account, err := s.Account(ctx, tenantID, spec.GetBootstrapId())
	if err != nil {
		return nil, corenetwork.Layout{}, err
	}
	if account.Provider != model.ProviderAWS {
		return nil, corenetwork.Layout{}, fault.New(fault.Unimplemented,
			fmt.Sprintf("account %s is %s; only aws networks are supported", account.ID, account.Provider))
	}
	if !awscatalog.IsRegion(spec.GetRegion()) {
		return nil, corenetwork.Layout{}, fault.New(fault.Invalid,
			fmt.Sprintf("spec.region %q is not a known AWS region", spec.GetRegion()))
	}
	layout, err := corenetwork.PlanLayout(spec.GetRegion(), spec.GetLayout())
	switch {
	case errors.Is(err, corenetwork.ErrUnsupported):
		return nil, corenetwork.Layout{}, fault.New(fault.Unimplemented, "spec.layout: "+err.Error())
	case err != nil:
		return nil, corenetwork.Layout{}, fault.New(fault.Invalid, "spec.layout: "+err.Error())
	}
	return account, layout, nil
}

// RenderPlan is the plan a stored network's managed resources are rendered
// from (corenetwork.RenderPlan).
func RenderPlan(row *model.Network, spec *cloudv1.NetworkSpec, a *model.CloudAccount, l corenetwork.Layout) crossplaneaws.NetworkPlan {
	return corenetwork.RenderPlan(
		corenetwork.Identity{TenantID: row.TenantID, ID: row.ID, Name: row.Name, Labels: row.Labels}, spec, l,
		corenetwork.Account{Namespace: a.Namespace, ProviderConfigName: a.ProviderConfigName, AccountID: a.AccountID})
}

// Account returns the bootstrapped AWS account the network lives in.
func (s *Reconciler) Account(ctx context.Context, tenantID, cloudAccountID string) (*model.CloudAccount, error) {
	a, err := s.deps.Store.Tenants().GetCloudAccountByID(ctx, tenantID, cloudAccountID)
	if errors.Is(err, repository.ErrNotFound) {
		return nil, fault.New(fault.FailedPrecondition, fmt.Sprintf(
			"spec.bootstrap_id %s is not a bootstrapped account of tenant %s; run BootstrapTenant first",
			cloudAccountID, tenantID))
	}
	if err != nil {
		return nil, err
	}
	if a.Provider != model.ProviderAWS {
		return nil, fault.New(fault.Invalid,
			fmt.Sprintf("account %s is a %s account; this network is aws", cloudAccountID, a.Provider))
	}
	return a, nil
}

// Reapply renders the stored network again and applies it.
func (s *Reconciler) Reapply(ctx context.Context, row *model.Network, spec *cloudv1.NetworkSpec) error {
	account, layout, err := s.Plan(ctx, row.TenantID, spec)
	if err != nil {
		return err
	}
	return s.Apply(ctx, row, account, RenderPlan(row, spec, account, layout))
}
