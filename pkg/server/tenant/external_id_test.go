package tenant

import (
	"context"
	"strings"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// A bootstrap gives the account a fresh external ID, long and random;
// the store keeps the first one an account got (cloudstore).
func TestBootstrapGivesAnExternalID(t *testing.T) {
	var given []string
	for range 2 {
		f := newFixture(t)
		f.expectTenant()
		f.tenants.EXPECT().PutCloudAccount(gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, a *model.CloudAccount) error {
			given = append(given, a.ExternalID)
			return nil
		})
		row := stored(account)
		row.ExternalID = strings.Repeat("x", 40)
		f.tenants.EXPECT().GetCloudAccount(gomock.Any(), "acme", model.ProviderAWS, account).Return(row, nil)

		res, err := f.svc.BootstrapTenant(t.Context(), request("acme", account))

		if err != nil {
			t.Fatal(err)
		}
		if !res.GetAccount().GetAws().GetExternalIdSet() {
			t.Errorf("external_id_set = false for an account that has one")
		}
		if strings.Contains(res.GetAccount().String(), row.ExternalID) {
			t.Errorf("the account's view carries its external ID: %v", res.GetAccount())
		}
	}
	if len(given[0]) < 32 || given[0] == given[1] {
		t.Errorf("external IDs = %q, want two different ones of at least 32 characters", given)
	}
}

// GetAccountOnboarding gives the external ID of the account it names;
// without one named, only the template.
func TestOnboardingCarriesTheAccountsExternalID(t *testing.T) {
	const id = "6b1f2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d"
	f := newFixture(t)
	f.svc.onboarding = Onboarding{ManagementRoleARN: "arn:aws:iam::111122223333:role/platform"}
	row := stored(account)
	row.ID, row.ExternalID = id, strings.Repeat("e", 40)
	f.tenants.EXPECT().GetCloudAccountByID(gomock.Any(), "acme", id).Return(row, nil)

	res, err := f.svc.GetAccountOnboarding(t.Context(), &cloudv1.GetAccountOnboardingRequest{TenantId: "acme", Id: id})

	if err != nil {
		t.Fatal(err)
	}
	ob := res.GetOnboarding()
	if ob.GetExternalId() != row.ExternalID || ob.GetAccountId() != account || ob.GetRoleName() != "cluster-vending-provisioner" {
		t.Errorf("onboarding = %v, want the account's external ID, account and role", ob)
	}

	plain, err := f.svc.GetAccountOnboarding(t.Context(), &cloudv1.GetAccountOnboardingRequest{TenantId: "acme"})
	if err != nil || plain.GetOnboarding().GetExternalId() != "" {
		t.Errorf("without an account: %v, %v; want no external ID", plain.GetOnboarding().GetExternalId(), err)
	}
}
