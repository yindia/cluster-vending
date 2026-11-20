package account

import (
	"strconv"
	"strings"
	"testing"
	"time"

	awscloud "github.com/yindia/cluster-vending/pkg/cloud/aws"
	"github.com/yindia/cluster-vending/pkg/cloudformation"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

var templateNow = time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)

// accountWithVersion is a verified account whose check read the role
// template version (known false: the tags could not be read).
func accountWithVersion(version int, known bool) *model.CloudAccount {
	report := awscloud.AccessReport{Regions: []awscloud.RegionAccess{{Region: "us-east-1", OK: true}},
		TemplateVersion: version, TemplateVersionKnown: known}
	return &model.CloudAccount{Base: model.Base{ID: "acct-1"}, TenantID: "acme", AccountID: "123456789012", AccountName: "prod",
		Identity: "arn:aws:iam::123456789012:role/cluster-vending-provisioner", ExternalID: strings.Repeat("e", 32),
		Access: MergeAccess(nil, report, templateNow)}
}

func templateCondition(a *model.CloudAccount) *cloudv1.Condition {
	for _, c := range Proto(a).GetStatus().GetConditions() {
		if c.GetType() == cloudformation.ConditionTemplateCurrent {
			return c
		}
	}
	return nil
}

func TestMergeAccessKeepsTheTemplateVersion(t *testing.T) {
	for _, v := range []int{0, 1, 7} {
		got, known := TemplateVersionOf(accountWithVersion(v, true).Access)
		if !known || got != v {
			t.Errorf("version %d: got %d known %t", v, got, known)
		}
	}
	if _, known := TemplateVersionOf(accountWithVersion(0, false).Access); known {
		t.Error("an unread version is reported as known")
	}
	if _, known := TemplateVersionOf(nil); known {
		t.Error("no access is reported as known")
	}
	// A stale verdict keeps what the last one found.
	prev := accountWithVersion(1, true).Access
	next := MergeAccess(prev, awscloud.AccessReport{Transient: true}, templateNow.Add(time.Hour))
	if v, known := TemplateVersionOf(next); !known || v != 1 {
		t.Errorf("stale merge lost the version: %d %t", v, known)
	}
}

func TestCurrentTemplateHasNoWarning(t *testing.T) {
	a := accountWithVersion(cloudformation.TemplateVersion, true)

	c := templateCondition(a)

	if c == nil || c.GetStatus() != cloudv1.ConditionStatus_CONDITION_STATUS_TRUE || c.GetReason() != cloudformation.ReasonTemplateCurrent {
		t.Fatalf("condition = %v, want True/%s", c, cloudformation.ReasonTemplateCurrent)
	}
	if strings.Contains(c.GetMessage(), "update the stack") {
		t.Errorf("a current template says to update: %q", c.GetMessage())
	}
	if got := Proto(a).GetStatus().GetState(); got != cloudv1.CloudAccountState_CLOUD_ACCOUNT_STATE_READY {
		t.Errorf("state = %v", got)
	}
}

func TestOldOrMissingTemplateWarnsWithTheUpdateCommand(t *testing.T) {
	for name, version := range map[string]int{"no tag": 0, "older": cloudformation.TemplateVersion - 1} {
		t.Run(name, func(t *testing.T) {
			if version < 0 {
				t.Skip("the first version has no older one")
			}
			a := accountWithVersion(version, true)

			c := templateCondition(a)

			if c == nil || c.GetStatus() != cloudv1.ConditionStatus_CONDITION_STATUS_FALSE || c.GetReason() != cloudformation.ReasonTemplateOutdated {
				t.Fatalf("condition = %v, want False/%s", c, cloudformation.ReasonTemplateOutdated)
			}
			for _, want := range []string{
				"the account's role template is version " + strconv.Itoa(version),
				"this platform needs " + strconv.Itoa(cloudformation.TemplateVersion),
				"update the stack: ",
				"cluster-vending account create",
				"cluster-vending-provisioner",
				"--tenant acme",
			} {
				if !strings.Contains(c.GetMessage(), want) {
					t.Errorf("message %q lacks %q", c.GetMessage(), want)
				}
			}
			// Not failed: the account works, it only lacks newer permissions.
			if got := Proto(a).GetStatus().GetState(); got != cloudv1.CloudAccountState_CLOUD_ACCOUNT_STATE_READY {
				t.Errorf("state = %v, want READY", got)
			}
			if AccessDenied(a.Access) {
				t.Error("an old template is not a denial")
			}
		})
	}
}

func TestUnreadTemplateVersionSaysNothing(t *testing.T) {
	if c := templateCondition(accountWithVersion(0, false)); c != nil {
		t.Errorf("condition = %v, want none: the version is unknown", c)
	}
	if c := templateCondition(&model.CloudAccount{}); c != nil {
		t.Errorf("condition = %v for an account never checked", c)
	}
}

func TestRefusalNamesTheOldTemplate(t *testing.T) {
	// An updater that lacks the take-over check: refused, and the message
	// says the stack is old.
	a := accountWithVersion(0, true)
	a.Access.Verified = false
	a.Access.Reason, a.Access.Message = awscloud.ReasonNotAllowed, "UnauthorizedOperation: ec2:CreateTags"
	a.Access.Checks = append(a.Access.Checks, model.AccessResult{Action: awscloud.ActionCreateTagsDryRun, Region: "us-east-1",
		Reason: awscloud.ReasonNotAllowed, Message: "UnauthorizedOperation"})

	for _, c := range []Capability{TakeOver, Create} {
		_, err := Usable(a, "us-east-1", c)
		if err == nil || !strings.Contains(err.Error(), "role template is version 0") || !strings.Contains(err.Error(), "update the stack") {
			t.Errorf("%s: err = %v, want the old template named", c, err)
		}
	}
	// Current: the refusal is unchanged.
	a = accountWithVersion(cloudformation.TemplateVersion, true)
	a.Access.Verified = false
	a.Access.Reason, a.Access.Message = awscloud.ReasonNotAllowed, "x"
	if _, err := Usable(a, "us-east-1", Create); err == nil || strings.Contains(err.Error(), "template") {
		t.Errorf("err = %v, want a refusal without template talk", err)
	}
}

func TestTemplateHint(t *testing.T) {
	old, current := accountWithVersion(0, true), accountWithVersion(cloudformation.TemplateVersion, true)
	if got := TemplateHint(old, "removing Karpenter's instance profiles", 1); !strings.Contains(got, "removing Karpenter's instance profiles needs role template version 1") ||
		!strings.Contains(got, "update the stack") {
		t.Errorf("hint = %q", got)
	}
	if got := TemplateHint(current, "x", 1); got != "" {
		t.Errorf("hint for a current template = %q", got)
	}
	if got := TemplateHint(accountWithVersion(0, false), "x", 1); got != "" {
		t.Errorf("hint for an unknown version = %q", got)
	}
	if got := TemplateHint(old, "x", 0); got != "" {
		t.Errorf("hint for a feature every version has = %q", got)
	}
}
