package cloudstore_test

import (
	"strings"
	"testing"

	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/storetest"
)

// An account keeps the external ID of its first bootstrap that had one:
// a later bootstrap (with another, or none) does not change it, since the
// role's trust in the AWS account requires that one.
func TestAccountKeepsItsExternalID(t *testing.T) {
	s := cloudstore.New(newClient(t))
	storetest.MustTenant(t, s, "acme")
	ctx := t.Context()
	first, second := strings.Repeat("a", 40), strings.Repeat("b", 40)
	put := func(ext string) *model.CloudAccount {
		t.Helper()
		a := &model.CloudAccount{TenantID: "acme", Provider: model.ProviderAWS, AccountID: "444455556666", ExternalID: ext}
		if err := s.Tenants().PutCloudAccount(ctx, a); err != nil {
			t.Fatal(err)
		}
		got, err := s.Tenants().GetCloudAccount(ctx, "acme", model.ProviderAWS, "444455556666")
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	if got := put(""); got.ExternalID != "" {
		t.Fatalf("an account bootstrapped without one has %q", got.ExternalID)
	}
	if got := put(first); got.ExternalID != first {
		t.Fatalf("external ID = %q, want the first given", got.ExternalID)
	}
	if got := put(second); got.ExternalID != first {
		t.Errorf("a later bootstrap changed the external ID to %q", got.ExternalID)
	}
	if got := put(""); got.ExternalID != first {
		t.Errorf("a bootstrap without one dropped it: %q", got.ExternalID)
	}
}
