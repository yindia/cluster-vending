package network

import (
	"errors"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect/v2"

	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/database/storetest"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// A network is created, imported or taken over only in an account whose
// access the controller verified, in the network's region.
func TestNetworkNeedsVerifiedAccount(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	denied := &model.AccountAccess{CheckedAt: now, AttemptedAt: now, Reason: "DeniedBySCP", Message: "ec2:CreateVpc: denied",
		Regions: []model.AccessResult{{Region: "us-east-1", OK: true}}}
	stale := storetest.Verified("us-east-1")
	stale.Stale, stale.AttemptedAt = true, now.Add(time.Minute)
	states := []struct {
		name   string
		access *model.AccountAccess // nil: never checked
		want   string               // the refusal; empty: allowed
	}{
		{name: "never checked", want: "has not been verified yet; the controller checks it shortly after it is added, " +
			"or run: cluster-vending account verify --id "},
		{name: "denied", access: denied, want: "account 444455556666 cannot be used: DeniedBySCP: ec2:CreateVpc: denied; " +
			"fix the account and run: cluster-vending account verify --id "},
		{name: "region never checked", access: storetest.Verified("eu-west-1"),
			want: "region us-east-1 of account 444455556666 has not been verified yet; run: cluster-vending account verify --id "},
		{name: "stale but verified", access: stale},
		{name: "verified", access: storetest.Verified("us-east-1")},
	}
	calls := []struct {
		name string
		call func(t *testing.T, f fixture, imported string) error
	}{
		{name: "create", call: func(t *testing.T, f fixture, imported string) error {
			_, err := f.svc.CreateNetwork(t.Context(), f.request("prod", nil))
			return err
		}},
		{name: "import", call: func(t *testing.T, f fixture, imported string) error {
			_, err := f.svc.ImportNetwork(t.Context(), importRequest(f.accountID))
			return err
		}},
		{name: "takeover", call: func(t *testing.T, f fixture, imported string) error {
			_, err := f.svc.TakeOverNetwork(t.Context(), &cloudv1.TakeOverNetworkRequest{TenantId: tenant,
				NetworkId: imported, AcknowledgeNotImported: true})
			return err
		}},
	}
	for _, c := range calls {
		for _, s := range states {
			t.Run(c.name+"/"+s.name, func(t *testing.T) {
				f := newFixture(t)
				var imported string
				if c.name == "takeover" {
					f.svc.deps.AWSDiscovery = fakeDiscoverer{network: existingVPC()}
					imported = f.imported(t).GetId()
				}
				storetest.SetAccess(t, f.storeKube, tenant, f.accountID, s.access)

				err := c.call(t, f, imported)

				if s.want == "" {
					if err != nil {
						t.Fatalf("refused: %v", err)
					}
					return
				}
				var ce *connect.Error
				if !errors.As(err, &ce) || ce.Code() != connect.CodeFailedPrecondition {
					t.Fatalf("err = %v, want FAILED_PRECONDITION", err)
				}
				if got := ce.Message(); !strings.Contains(got, s.want+f.accountID) {
					t.Errorf("message %q, want it to say %q and name the account %s", got, s.want, f.accountID)
				}
				if c.name != "takeover" {
					rows, err := f.store.Networks().List(t.Context(), tenant, repository.Page{Limit: 10})
					if err != nil || len(rows) != 0 {
						t.Errorf("%d networks stored after a refusal (%v)", len(rows), err)
					}
				}
			})
		}
	}
}
