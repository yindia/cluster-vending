package cli

import (
	"bytes"
	"strings"
	"testing"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// account get says whether the account has an external ID, never which.
func TestDescribeAccountSaysWhetherItHasAnExternalID(t *testing.T) {
	for set, want := range map[bool]string{true: "External ID:", false: "External ID:"} {
		var out bytes.Buffer
		a := &cloudv1.CloudAccount{Id: accountID, TenantId: "acme",
			Provider: &cloudv1.CloudAccount_Aws{Aws: &cloudv1.AwsAccount{AccountId: tenantAccount, Role: "cluster-vending-provisioner", ExternalIdSet: set}},
			Status:   &cloudv1.CloudAccountStatus{}}

		if err := describeAccount(&out, a); err != nil {
			t.Fatal(err)
		}

		line := ""
		for _, l := range strings.Split(out.String(), "\n") {
			if strings.HasPrefix(l, want) {
				line = l
			}
		}
		word := map[bool]string{true: "set", false: "none: account create again adds one"}[set]
		if !strings.HasSuffix(line, word) {
			t.Errorf("external_id_set %v: line %q, want it to end in %q\n%s", set, line, word, out.String())
		}
	}
}
