package account

import (
	"testing"
	"time"

	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/fault"
)

func TestUsable(t *testing.T) {
	checked := time.Date(2026, 11, 14, 9, 30, 0, 0, time.UTC)
	ok := func(region string) model.AccessResult { return model.AccessResult{Region: region, OK: true} }
	account := func(access *model.AccountAccess) *model.CloudAccount {
		return &model.CloudAccount{Base: model.Base{ID: "0190a1b2-0000-7000-8000-000000000001"},
			AccountID: "123456789012", AccountName: "prod", Access: access}
	}
	verified := &model.AccountAccess{Verified: true, CheckedAt: checked, AttemptedAt: checked,
		Regions: []model.AccessResult{ok("us-east-1"), ok("eu-west-1")}}

	tests := []struct {
		name        string
		account     *model.CloudAccount
		region      string
		wantErr     string
		wantWarning string
	}{
		{name: "verified in the region", account: account(verified), region: "eu-west-1"},
		{name: "verified, no region asked", account: account(verified)},
		{
			name: "never checked", account: account(nil), region: "eu-west-1",
			wantErr: "account prod (123456789012) has not been verified yet; the controller checks it shortly after it is added, " +
				"or run: cluster-vending account verify --id 0190a1b2-0000-7000-8000-000000000001",
		},
		{
			name: "never checked, no account name",
			account: &model.CloudAccount{Base: model.Base{ID: "0190a1b2-0000-7000-8000-000000000001"},
				AccountID: "123456789012"},
			wantErr: "account 123456789012 has not been verified yet; the controller checks it shortly after it is added, " +
				"or run: cluster-vending account verify --id 0190a1b2-0000-7000-8000-000000000001",
		},
		{
			name: "no verdict yet: the first attempt failed transiently",
			account: account(&model.AccountAccess{Stale: true, AttemptedAt: checked,
				Reason: "Unavailable", Message: "sts: connection reset"}),
			wantErr: "account prod (123456789012) has not been verified yet; the controller checks it shortly after it is added, " +
				"or run: cluster-vending account verify --id 0190a1b2-0000-7000-8000-000000000001" +
				" (the last attempt failed: Unavailable: sts: connection reset)",
		},
		{
			name: "denied",
			account: account(&model.AccountAccess{CheckedAt: checked, AttemptedAt: checked,
				Reason: "AccessDenied", Message: "ec2:DescribeVpcs in us-east-1: not authorized",
				Regions: []model.AccessResult{ok("us-east-1")}}),
			region: "us-east-1",
			wantErr: "account prod (123456789012) cannot be used: AccessDenied: ec2:DescribeVpcs in us-east-1: not authorized; " +
				"fix the account and run: cluster-vending account verify --id 0190a1b2-0000-7000-8000-000000000001",
		},
		{
			name: "no tenant role",
			account: account(&model.AccountAccess{CheckedAt: checked, AttemptedAt: checked,
				Reason: ReasonNoTenantRole, Message: "the account has no tenant role"}),
			wantErr: "account prod (123456789012) cannot be used: NoTenantRole: the account has no tenant role; " +
				"fix the account and run: cluster-vending account verify --id 0190a1b2-0000-7000-8000-000000000001",
		},
		{
			name: "the region failed",
			account: account(&model.AccountAccess{CheckedAt: checked, AttemptedAt: checked,
				Reason: "RegionDisabled", Message: "ap-east-1 is not enabled",
				Regions: []model.AccessResult{ok("us-east-1"),
					{Region: "ap-east-1", Reason: "RegionDisabled", Message: "ap-east-1 is not enabled"}}}),
			region: "ap-east-1",
			wantErr: "region ap-east-1 of account prod (123456789012) cannot be used: RegionDisabled: ap-east-1 is not enabled; " +
				"fix the account and run: cluster-vending account verify --id 0190a1b2-0000-7000-8000-000000000001",
		},
		{
			name: "another region failed: the account is not verified",
			account: account(&model.AccountAccess{CheckedAt: checked, AttemptedAt: checked,
				Reason: "RegionDisabled", Message: "ap-east-1 is not enabled",
				Regions: []model.AccessResult{ok("us-east-1"),
					{Region: "ap-east-1", Reason: "RegionDisabled", Message: "ap-east-1 is not enabled"}}}),
			region: "us-east-1",
			wantErr: "account prod (123456789012) cannot be used: RegionDisabled: ap-east-1 is not enabled; " +
				"fix the account and run: cluster-vending account verify --id 0190a1b2-0000-7000-8000-000000000001",
		},
		{
			name: "the region was never checked", account: account(verified), region: "ap-south-1",
			wantErr: "region ap-south-1 of account prod (123456789012) has not been verified yet; " +
				"run: cluster-vending account verify --id 0190a1b2-0000-7000-8000-000000000001 --region ap-south-1",
		},
		{
			name: "stale, the last verdict verified",
			account: account(&model.AccountAccess{Verified: true, Stale: true, CheckedAt: checked,
				AttemptedAt: checked.Add(time.Hour), Regions: verified.Regions}),
			region: "eu-west-1",
			wantWarning: "account prod (123456789012) was last verified at 2026-11-14T09:30:00Z; " +
				"the checks since failed for a transient reason (throttling, the network)",
		},
		{
			name: "stale, the last verdict denied",
			account: account(&model.AccountAccess{Stale: true, CheckedAt: checked, AttemptedAt: checked.Add(time.Hour),
				Reason: "AccessDenied", Message: "not authorized"}),
			wantErr: "account prod (123456789012) cannot be used: AccessDenied: not authorized; " +
				"fix the account and run: cluster-vending account verify --id 0190a1b2-0000-7000-8000-000000000001",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			warning, err := Usable(tt.account, tt.region)
			if warning != tt.wantWarning {
				t.Errorf("warning = %q, want %q", warning, tt.wantWarning)
			}
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("err = %v, want none", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("err = nil, want %q", tt.wantErr)
			}
			if err.Error() != tt.wantErr {
				t.Errorf("err =\n  %q\nwant\n  %q", err.Error(), tt.wantErr)
			}
			if !fault.Is(err, fault.FailedPrecondition) {
				t.Errorf("err kind is not FailedPrecondition: %v", err)
			}
		})
	}
}
