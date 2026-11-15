package cluster

import (
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect/v2"

	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/storetest"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// A cluster is created, imported or taken over only when the account of
// its network was verified, in the network's region.
func TestClusterNeedsVerifiedAccount(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	denied := &model.AccountAccess{CheckedAt: now, AttemptedAt: now, Reason: "DeniedBySCP", Message: "eks:CreateCluster: denied",
		Regions: []model.AccessResult{{Region: "us-east-1", OK: true}}}
	regionFailed := &model.AccountAccess{CheckedAt: now, AttemptedAt: now, Reason: "NotAllowed", Message: "us-east-1: denied",
		Regions: []model.AccessResult{{Region: "us-east-1", Reason: "NotAllowed", Message: "us-east-1: denied"}}}
	stale := storetest.Verified("us-east-1")
	stale.Stale, stale.AttemptedAt = true, now.Add(time.Minute)
	states := []struct {
		name   string
		access *model.AccountAccess // nil: never checked
		want   string               // the refusal; empty: allowed
	}{
		{name: "never checked", want: "account 444455556666 has not been verified yet; the controller checks it shortly after it is added, " +
			"or run: cluster-vending account verify --id "},
		{name: "denied", access: denied, want: "account 444455556666 cannot be used: DeniedBySCP: eks:CreateCluster: denied; " +
			"fix the account and run: cluster-vending account verify --id "},
		{name: "region failed", access: regionFailed, want: "region us-east-1 of account 444455556666 cannot be used: NotAllowed: us-east-1: denied; " +
			"fix the account and run: cluster-vending account verify --id "},
		{name: "region never checked", access: storetest.Verified("eu-west-1"),
			want: "region us-east-1 of account 444455556666 has not been checked yet; its check has started: try again in a minute (cluster-vending account get --id "},
		{name: "stale but verified", access: stale},
		{name: "verified", access: storetest.Verified("us-east-1")},
	}
	calls := []struct {
		name string
		// call returns the error of the call; before runs while the
		// account is still verified (an import to take over).
		before func(t *testing.T, w world) string
		call   func(t *testing.T, w world, id string) error
	}{
		{name: "create", call: func(t *testing.T, w world, _ string) error {
			_, err := w.svc.CreateCluster(t.Context(), createRequest(w, "second"))
			return err
		}},
		{name: "import", call: func(t *testing.T, w world, _ string) error {
			_, err := w.svc.ImportCluster(t.Context(), &cloudv1.ImportClusterRequest{TenantId: worldTenant, Name: "tf",
				NetworkId: w.row.NetworkID, Provider: &cloudv1.ImportClusterRequest_Aws{Aws: &cloudv1.AwsImportCluster{DefaultNodeGroupName: "default-ng"}}})
			return err
		}},
		{name: "takeover",
			before: func(t *testing.T, w world) string { return importTF(t, w).GetId() },
			call: func(t *testing.T, w world, id string) error {
				_, err := w.svc.TakeOverCluster(t.Context(), &cloudv1.TakeOverClusterRequest{TenantId: worldTenant, ClusterId: id,
					AcknowledgeNotImported: true})
				return err
			}},
	}
	for _, c := range calls {
		for _, s := range states {
			t.Run(c.name+"/"+s.name, func(t *testing.T) {
				w := importWorld(t, terraformCluster())
				var id string
				if c.before != nil {
					id = c.before(t, w)
				}
				storetest.SetAccess(t, w.storeKube, worldTenant, w.accountID, s.access)

				err := c.call(t, w, id)

				if s.want == "" {
					if err != nil {
						t.Fatalf("refused: %v", err)
					}
					return
				}
				if connect.CodeOf(err) != connect.CodeFailedPrecondition {
					t.Fatalf("err = %v, want FAILED_PRECONDITION", err)
				}
				if msg := err.Error(); !strings.Contains(msg, s.want+w.accountID) {
					t.Errorf("message %q, want it to say %q and name the account %s", msg, s.want, w.accountID)
				}
			})
		}
	}
}
