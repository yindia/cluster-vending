package account

import (
	"strings"
	"testing"
	"time"

	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// A verified account without an external ID stays verified, with a
// warning that says how to add one; one with an external ID has none.
func TestNoExternalIDIsAWarning(t *testing.T) {
	verified := &model.AccountAccess{Verified: true, CheckedAt: time.Now(), Regions: []model.AccessResult{{Region: "us-east-1", OK: true}}}
	for _, tt := range []struct {
		name, ext string
		warns     bool
	}{
		{"without one: a warning", "", true},
		{"with one: none", strings.Repeat("e", 40), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a := &model.CloudAccount{AccountID: "444455556666", Identity: "arn:aws:iam::444455556666:role/cluster-vending-provisioner",
				ExternalID: tt.ext, Access: verified}

			out := Proto(a)

			var c *cloudv1.Condition
			for _, cond := range out.GetStatus().GetConditions() {
				if cond.GetType() == ConditionAccessVerified {
					c = cond
				}
			}
			if c == nil || c.GetStatus() != cloudv1.ConditionStatus_CONDITION_STATUS_TRUE {
				t.Fatalf("AccessVerified = %v, want True either way", c)
			}
			if got := strings.Contains(c.GetMessage(), "no external ID"); got != tt.warns {
				t.Errorf("message = %q, warning %v, want %v", c.GetMessage(), got, tt.warns)
			}
			if out.GetAws().GetExternalIdSet() != (tt.ext != "") || strings.Contains(out.String(), "eeee") {
				t.Errorf("view = %v: external_id_set must say whether there is one, without the ID", out.GetAws())
			}
		})
	}
}
