package cli

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/proto"

	"github.com/yindia/cluster-vending/pkg/convert"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/kube"
)

func TenantCreate(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if o.ID != "" {
		if o.Tenant != "" && !o.tenantDefaulted && o.Tenant != o.ID {
			return nil, fmt.Errorf("tenant ID and --tenant differ")
		}
		o.Tenant = o.ID
	}
	if err := o.requireTenant(); err != nil {
		return nil, err
	}
	return call(ctx, &cloudv1.CreateTenantRequest{TenantId: o.Tenant}, c.Tenant.CreateTenant)
}

// TenantDelete deletes the tenant (TenantService.DeleteTenant): refused,
// naming them, while it has accounts, networks, clusters, components or
// sub-tenants. Its namespace is unregistered and kept. --wait waits until
// the tenant is no longer listed.
func TenantDelete(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if o.ID != "" {
		if o.Tenant != "" && !o.tenantDefaulted && o.Tenant != o.ID {
			return nil, fmt.Errorf("tenant ID and --tenant differ")
		}
		o.Tenant = o.ID
	}
	if err := o.requireTenant(); err != nil {
		return nil, err
	}
	deleted, err := call(ctx, &cloudv1.DeleteTenantRequest{TenantId: o.Tenant}, c.Tenant.DeleteTenant)
	if err != nil {
		return nil, err
	}
	o.progressf("Deleting the tenant %s: its namespace %s is unregistered and kept\n", o.Tenant, kube.TenantNamespace(o.Tenant))
	if !o.Wait {
		return deleted, nil
	}
	err = poll(ctx, o, "the tenant "+o.Tenant, func() (bool, error) {
		res, err := call(ctx, &cloudv1.ListTenantsRequest{}, c.Tenant.ListTenants)
		if err != nil {
			return false, err
		}
		for _, t := range res.(*cloudv1.ListTenantsResponse).GetTenants() {
			if t.GetId() == o.Tenant {
				return false, nil
			}
		}
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	o.progressf("Tenant %s: deleted\n", o.Tenant)
	return deleted, nil
}

func TenantList(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	response, err := call(ctx, &cloudv1.ListTenantsRequest{}, c.Tenant.ListTenants)
	if err != nil || !isTable(o.Output) {
		return response, err
	}
	rows := [][]string{}
	for _, tenant := range response.(*cloudv1.ListTenantsResponse).Tenants {
		rows = append(rows, []string{tenant.Id})
	}
	return nil, table(o.Out, []string{"TENANT"}, rows)
}

func tenantFreezeTarget(o Options) (*cloudv1.FreezeTarget, error) {
	if err := o.requireTenant(); err != nil {
		return nil, err
	}
	if o.Platform || o.ResetExceptions || o.Channel != "" || o.Tier != "" {
		return nil, fmt.Errorf("tenant freezes cannot target the platform or reset exceptions")
	}
	if o.Cluster != "" && o.PauseNetwork != "" {
		return nil, fmt.Errorf("choose --network or --cluster, not both")
	}
	return convert.FreezeTarget(nil, o.Tenant, o.PauseNetwork, o.Cluster), nil
}

func readTenantFreeze(ctx context.Context, c Clients, o Options, target *cloudv1.FreezeTarget) (*cloudv1.FreezeStatus, error) {
	response, err := call(ctx, &cloudv1.TenantGetFreezeRequest{TenantId: o.Tenant, Target: target}, c.Tenant.GetFreeze)
	if err != nil {
		return nil, err
	}
	return convert.FreezeStatus(response.(*cloudv1.GetFreezeResponse), target), nil
}

func TenantFreezeStatus(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	target, err := tenantFreezeTarget(o)
	if err != nil {
		return nil, err
	}
	status, err := readTenantFreeze(ctx, c, o, target)
	if err != nil {
		return nil, err
	}
	return printFreezeStatus(o, status)
}

func TenantFreeze(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	target, err := tenantFreezeTarget(o)
	if err != nil {
		return nil, err
	}
	until, err := parseUntil(o.Until)
	if err != nil {
		return nil, err
	}
	response, err := call(ctx, &cloudv1.TenantFreezeRequest{TenantId: o.Tenant, Target: target, Reason: o.Reason, Until: until}, c.Tenant.Freeze)
	if err != nil {
		return nil, err
	}
	result := response.(*cloudv1.FreezeResponse)
	status := result.GetStatus()
	if status == nil {
		status = convert.FreezeStatus(&cloudv1.GetFreezeResponse{Phase: result.Phase, Revision: result.Revision, Message: result.Message, Clusters: result.Clusters, Networks: result.Networks, TotalResources: result.TotalResources, PausedResources: result.PausedResources}, target)
	}
	if o.Wait {
		status, err = waitFreezeStatus(ctx, o, status.Revision, true, "tenant freeze status", func(ctx context.Context) (*cloudv1.FreezeStatus, error) { return readTenantFreeze(ctx, c, o, target) })
	}
	if err != nil {
		return nil, err
	}
	return printFreezeStatus(o, status)
}

func TenantUnfreeze(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	target, err := tenantFreezeTarget(o)
	if err != nil {
		return nil, err
	}
	response, err := call(ctx, &cloudv1.TenantUnfreezeRequest{TenantId: o.Tenant, Target: target}, c.Tenant.Unfreeze)
	if err != nil {
		return nil, err
	}
	result := response.(*cloudv1.UnfreezeResponse)
	status := result.GetStatus()
	if o.Wait {
		status, err = waitFreezeStatus(ctx, o, result.Revision, false, "tenant freeze status", func(ctx context.Context) (*cloudv1.FreezeStatus, error) { return readTenantFreeze(ctx, c, o, target) })
	} else if status == nil {
		status, err = readTenantFreeze(ctx, c, o, target)
	}
	if err != nil {
		return nil, err
	}
	return printFreezeStatus(o, status)
}
