package cli

import (
	"strings"
	"testing"
	"time"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

func deniedAccess() *cloudv1.AccountAccess {
	return &cloudv1.AccountAccess{CheckedTime: ago(2 * time.Hour), AttemptedTime: ago(2 * time.Hour),
		PrincipalArn: "arn:aws:sts::111122223333:assumed-role/cluster-vending-provisioner/cluster-vending",
		Reason:       "DeniedBySCP", Message: "us-west-2: the tenant role cannot be used: explicit deny in a service control policy",
		Regions: []*cloudv1.AccessResult{{Region: "us-east-1", Ok: true},
			{Region: "us-west-2", Reason: "DeniedBySCP", Message: "explicit deny in a service control policy"}},
		Checks: []*cloudv1.AccessResult{{Action: "iam:GetRole", Region: "us-east-1", Ok: true},
			{Action: "ec2:CreateVpc", Region: "us-east-1", Ok: true}}}
}

func TestAccessColumn(t *testing.T) {
	stale := &cloudv1.AccountAccess{Verified: true, Stale: true, CheckedTime: ago(time.Hour)}
	tests := []struct {
		name   string
		access *cloudv1.AccountAccess
		want   string
	}{
		{name: "not checked", want: "Unchecked"},
		{name: "verified", access: &cloudv1.AccountAccess{Verified: true, CheckedTime: ago(time.Hour)}, want: "Verified"},
		{name: "stale", access: stale, want: "Verified (stale)"},
		{name: "denied", access: deniedAccess(), want: "Denied: DeniedBySCP"},
		{name: "no verdict yet", access: &cloudv1.AccountAccess{Stale: true, Reason: "Unavailable"}, want: "Unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := accessWord(tt.access); got != tt.want {
				t.Errorf("access %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPrintAccountAccess(t *testing.T) {
	withClock(t)
	account := &cloudv1.CloudAccount{Id: "01a1d0a0-4a34-735f-be7d-19d1662f1138", TenantId: "acme", AccountName: "aws-111122223333",
		CreateTime: ago(24 * time.Hour), Provider: &cloudv1.CloudAccount_Aws{Aws: &cloudv1.AwsAccount{AccountId: "111122223333"}},
		Status: &cloudv1.CloudAccountStatus{State: cloudv1.CloudAccountState_CLOUD_ACCOUNT_STATE_FAILED, Access: deniedAccess()}}

	list := printed(t, &cloudv1.ListCloudAccountsResponse{Accounts: []*cloudv1.CloudAccount{account}}, OutputTable)
	if want := "NAME               ID                                     ACCOUNT        STATE    ACCESS                AGE\n" +
		"aws-111122223333   01a1d0a0-4a34-735f-be7d-19d1662f1138   111122223333   Failed   Denied: DeniedBySCP   24h\n"; list != want {
		t.Errorf("list: got\n%s\nwant\n%s", list, want)
	}
	got := printed(t, &cloudv1.GetCloudAccountResponse{Account: account}, OutputTable)
	for _, want := range []string{
		"Access:            Denied: DeniedBySCP (checked 120m ago)\n",
		"Access Message:    us-west-2: the tenant role cannot be used: explicit deny in a service control policy\n",
		"Principal:         arn:aws:sts::111122223333:assumed-role/cluster-vending-provisioner/cluster-vending\n",
		"Checks:\n  REGION      CHECK           RESULT   REASON        MESSAGE\n",
		"  us-west-2   assume role     Failed   DeniedBySCP   explicit deny in a service control policy\n",
		"  us-east-1   iam:GetRole     OK       <none>        <none>\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("get lacks %q:\n%s", want, got)
		}
	}
	if got := printed(t, &cloudv1.VerifyCloudAccountResponse{Account: account}, OutputTable); !strings.HasPrefix(got,
		"cloudaccount/aws-111122223333 (01a1d0a0-4a34-735f-be7d-19d1662f1138) access check requested\n") {
		t.Errorf("verify: got\n%s", got)
	}
}
