package reconcile

import (
	"testing"

	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/fault"
)

const tenantRole = "arn:aws:iam::444455556666:role/provisioner"

// The tenant role stored at bootstrap is always the one assumed, whatever
// the controller's own credentials are: there is no way around it.
func TestAWSAccessAlwaysAssumesTheTenantRole(t *testing.T) {
	a, err := Deps{}.AWSAccess(t.Context(), &model.CloudAccount{AccountID: "444455556666", Identity: tenantRole})
	if err != nil || a.AccountID != "444455556666" || a.RoleARN != tenantRole {
		t.Fatalf("access = %+v, %v; want the account through its tenant role", a, err)
	}
}

func TestAWSAccessRefusesWhatIsNotTheAccountsRole(t *testing.T) {
	for name, account := range map[string]*model.CloudAccount{
		"no role yet":             {AccountID: "444455556666"},
		"role of another account": {AccountID: "777788889999", Identity: tenantRole},
		"not a role":              {AccountID: "444455556666", Identity: "arn:aws:iam::444455556666:user/alice"},
	} {
		if _, err := (Deps{}).AWSAccess(t.Context(), account); !fault.Is(err, fault.FailedPrecondition) {
			t.Errorf("%s: %v, want FAILED_PRECONDITION", name, err)
		}
	}
}
