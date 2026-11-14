package cli

import (
	"context"
	"fmt"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"

	"github.com/yindia/cluster-vending/pkg/cloudformation"
	awscatalog "github.com/yindia/cluster-vending/pkg/constant/aws"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// defaultStackRegion is where account create's stack is when --region was
// not given: the server's default (GetAccountOnboarding).
var defaultStackRegion = string(awscatalog.USEast1)

// AccountDelete deletes the account --id (TenantService.DeleteCloudAccount);
// --wait waits until it is gone. With --delete-role it also deletes the
// CloudFormation stack of the account's role, with the caller's AWS
// credentials of the account, and only a stack account create made (its
// tag): that stack is checked before anything is deleted, and deleted
// once the account is gone. Any other role is left alone.
func AccountDelete(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if err := o.requireTenant(); err != nil {
		return nil, err
	}
	if err := RequireID(o.ID, "account"); err != nil {
		return nil, err
	}
	res, err := call(ctx, &cloudv1.GetCloudAccountRequest{TenantId: o.Tenant, Id: o.ID}, c.Tenant.GetCloudAccount)
	if err != nil {
		return nil, err
	}
	account := res.(*cloudv1.GetCloudAccountResponse).GetAccount()
	var stacks RoleStacks
	role := account.GetAws().GetRole()
	if o.DeleteRole {
		if stacks, err = ownStack(ctx, o, account); err != nil {
			return nil, err
		}
	}
	deleted, err := call(ctx, &cloudv1.DeleteCloudAccountRequest{TenantId: o.Tenant, Id: o.ID}, c.Tenant.DeleteCloudAccount)
	if err != nil {
		return nil, err
	}
	o.progressf("Deleting the account %s (%s): the controller removes its ProviderConfig\n", account.GetAccountName(), o.ID)
	if o.Wait || stacks != nil {
		err := poll(ctx, o, "the account "+o.ID, func() (bool, error) {
			_, err := call(ctx, &cloudv1.GetCloudAccountRequest{TenantId: o.Tenant, Id: o.ID}, c.Tenant.GetCloudAccount)
			if connect.CodeOf(err) == connect.CodeNotFound {
				return true, nil
			}
			return false, err
		})
		if err != nil {
			return nil, err
		}
		o.progressf("Account %s: deleted\n", o.ID)
	}
	if stacks != nil {
		if err := deleteStack(ctx, o, stacks, role); err != nil {
			return nil, err
		}
	}
	return deleted, nil
}

// ownStack opens the account's stacks with the caller's credentials and
// checks that its role is in a stack account create made: the only stack
// --delete-role deletes.
func ownStack(ctx context.Context, o Options, account *cloudv1.CloudAccount) (RoleStacks, error) {
	role, id := account.GetAws().GetRole(), account.GetAws().GetAccountId()
	if cloudformation.RoleOf(role) == "" {
		return nil, fmt.Errorf("--delete-role: %s is not a role the platform's templates create; it is yours to delete in account %s", role, id)
	}
	region := o.StackRegion
	if region == "" {
		region = defaultStackRegion
	}
	stacks, err := accountStacks(ctx, o, region, id)
	if err != nil {
		return nil, fmt.Errorf("--delete-role: %w", err)
	}
	info, err := stacks.Stack(ctx, role)
	if err != nil {
		return nil, fmt.Errorf("--delete-role: read the stack %s: %w", role, err)
	}
	switch {
	case info == nil:
		return nil, fmt.Errorf("--delete-role: there is no stack %s in account %s (%s); the role %s is yours to delete, or pass the stack's --region",
			role, id, region, role)
	case !info.createdByCLI():
		return nil, fmt.Errorf("--delete-role: the stack %s was not created by cluster-vending (no %s tag); delete it yourself",
			role, cloudformation.ManagedByTag)
	}
	return stacks, nil
}

// deleteStack deletes the stack and follows it until it is gone, printing
// its events.
func deleteStack(ctx context.Context, o Options, s RoleStacks, name string) error {
	info, err := s.Stack(ctx, name)
	if err != nil {
		return fmt.Errorf("read the stack %s: %w", name, err)
	}
	seen, err := eventIDs(ctx, s, name, info)
	if err != nil {
		return err
	}
	o.progressf("Deleting the stack %s: the role %s and its policies\n", name, name)
	if err := s.DeleteStack(ctx, name); err != nil {
		return fmt.Errorf("delete the stack %s: %w", name, err)
	}
	return poll(ctx, o, "the stack "+name, func() (bool, error) {
		info, err := s.Stack(ctx, name)
		if err != nil {
			return false, fmt.Errorf("read the stack %s: %w", name, err)
		}
		if info == nil || info.Status == "DELETE_COMPLETE" {
			o.progressf("Stack %s: deleted\n", name)
			return true, nil
		}
		if events, err := s.StackEvents(ctx, name); err == nil {
			for i := len(events) - 1; i >= 0; i-- {
				if e := events[i]; !seen[e.ID] {
					seen[e.ID] = true
					o.progressf("  %s   %s   %s   %s\n", e.Resource, e.Type, e.Status, e.Reason)
				}
			}
		}
		if info.Status == "DELETE_FAILED" {
			return false, fmt.Errorf("the stack %s could not be deleted: %s", name, info.Reason)
		}
		return false, nil
	})
}
