package account

import (
	"strings"
	"testing"
	"time"

	awscloud "github.com/yindia/cluster-vending/pkg/cloud/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/fault"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// shaped builds the access of a role: denied are the actions whose check
// failed in every region.
func shaped(denied ...string) *model.CloudAccount {
	now := time.Date(2026, 11, 14, 9, 30, 0, 0, time.UTC)
	report := awscloud.AccessReport{}
	for _, region := range []string{"us-east-1", "eu-west-1"} {
		report.Regions = append(report.Regions, awscloud.RegionAccess{Region: region, OK: true})
		for _, action := range []string{awscloud.ActionDescribeAvailabilityZones, awscloud.ActionDescribeVpcs,
			awscloud.ActionCreateVpcDryRun, awscloud.ActionCreateTagsDryRun, awscloud.ActionListClusters} {
			c := awscloud.CheckResult{Action: action, Region: region, OK: true}
			for _, d := range denied {
				if d == action {
					c.OK, c.Reason, c.Message = false, awscloud.ReasonNotAllowed, "UnauthorizedOperation: not authorized"
				}
			}
			report.Checks = append(report.Checks, c)
		}
	}
	return &model.CloudAccount{Base: model.Base{ID: "id"}, AccountID: "123456789012", AccountName: "prod",
		Access: MergeAccess(nil, report, now)}
}

func TestUsableByCapability(t *testing.T) {
	reader := shaped(awscloud.ActionCreateVpcDryRun, awscloud.ActionCreateTagsDryRun)
	updater := shaped(awscloud.ActionCreateVpcDryRun)
	provisioner := shaped()
	noReads := shaped(awscloud.ActionDescribeVpcs)

	tests := []struct {
		name    string
		account *model.CloudAccount
		region  string
		c       Capability
		wantErr string // a substring of the error; none when allowed
	}{
		{"reader imports", reader, "eu-west-1", Import, ""},
		{"reader imports, no region", reader, "", Import, ""},
		{"reader creates: refused clearly", reader, "eu-west-1", Create, "cannot create networks or clusters"},
		{"reader takes over: refused", reader, "eu-west-1", TakeOver, "cannot be used"},
		{"updater imports", updater, "us-east-1", Import, ""},
		{"updater takes over", updater, "us-east-1", TakeOver, ""},
		{"updater creates: refused", updater, "us-east-1", Create, "allows take over and import, not create"},
		{"provisioner imports", provisioner, "us-east-1", Import, ""},
		{"provisioner takes over", provisioner, "us-east-1", TakeOver, ""},
		{"provisioner creates", provisioner, "us-east-1", Create, ""},
		{"no reads: import refused", noReads, "us-east-1", Import, "cannot be used"},
		{"unchecked region of a reader", reader, "ap-south-1", Import, "has not been checked yet"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Usable(tt.account, tt.region, tt.c)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("err = %v, want none", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.wantErr)
			}
			if !fault.Is(err, fault.FailedPrecondition) {
				t.Errorf("err = %v, want FailedPrecondition", err)
			}
		})
	}
}

func TestUsableRegionChecksStillApply(t *testing.T) {
	a := shaped(awscloud.ActionCreateVpcDryRun, awscloud.ActionCreateTagsDryRun)
	a.Access.Regions[1] = model.AccessResult{Region: "eu-west-1", Reason: awscloud.ReasonRegionNotEnabled, Message: "not enabled"}
	if _, err := Usable(a, "eu-west-1", Import); err == nil || !strings.Contains(err.Error(), "region eu-west-1") {
		t.Fatalf("err = %v, want the region refused", err)
	}
	if _, err := Usable(a, "us-east-1", Import); err != nil {
		t.Fatalf("the healthy region: err = %v, want none", err)
	}
}

func TestReaderIsNotDenied(t *testing.T) {
	reader := shaped(awscloud.ActionCreateVpcDryRun, awscloud.ActionCreateTagsDryRun).Access
	if reader.Verified {
		t.Fatal("a reader must not be verified for create")
	}
	if AccessDenied(reader) {
		t.Error("a reader is no denial")
	}
	c := AccessCondition(reader)
	if c.GetStatus() != cloudv1.ConditionStatus_CONDITION_STATUS_TRUE || c.GetReason() != ReasonAccessVerifiedPartly ||
		!strings.Contains(c.GetMessage(), "allows import, not create") {
		t.Errorf("condition = %v", c)
	}
	if n := AccessNote(nil, reader); n == nil {
		t.Error("no verified note for a reader")
	}
	broken := shaped(awscloud.ActionDescribeVpcs).Access
	if !AccessDenied(broken) || AccessCondition(broken).GetStatus() != cloudv1.ConditionStatus_CONDITION_STATUS_FALSE {
		t.Error("a role that cannot read is denied")
	}
}

func TestRegionToCheckForReader(t *testing.T) {
	a := shaped(awscloud.ActionCreateVpcDryRun, awscloud.ActionCreateTagsDryRun)
	if regions, ok := RegionToCheck(a, "ap-south-1"); !ok || len(regions) != 3 {
		t.Errorf("regions = %v, ok = %v", regions, ok)
	}
}
