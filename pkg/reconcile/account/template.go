package account

import (
	"fmt"
	"strconv"
	"strings"

	awscloud "github.com/yindia/cluster-vending/pkg/cloud/aws"
	"github.com/yindia/cluster-vending/pkg/cloudformation"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// templateVersionPrefix starts the message of the check result that keeps
// the role template version the access check read. The result is always OK:
// an old template is a warning, not a denial. It is stored among the
// checks, which every store keeps, and an account whose version could not
// be read has none.
const templateVersionPrefix = "template version "

// templateVersionResult is the check result that records version.
func templateVersionResult(version int) model.AccessResult {
	return model.AccessResult{Action: awscloud.ActionListRoleTags, Region: awscloud.GlobalRegion, OK: true,
		Message: templateVersionPrefix + strconv.Itoa(version)}
}

// TemplateVersionOf is the version of the role template the account's
// stack was created from, as the last access check read it (0: a stack from
// before versions); known is false when no check read it.
func TemplateVersionOf(a *model.AccountAccess) (version int, known bool) {
	if a == nil {
		return 0, false
	}
	for _, c := range a.Checks {
		if c.Action != awscloud.ActionListRoleTags || !strings.HasPrefix(c.Message, templateVersionPrefix) {
			continue
		}
		v, err := strconv.Atoi(strings.TrimPrefix(c.Message, templateVersionPrefix))
		return v, err == nil
	}
	return 0, false
}

// TemplateOutdated: the account's stack is older than this build's
// templates.
func TemplateOutdated(a *model.AccountAccess) bool {
	v, known := TemplateVersionOf(a)
	return known && v < cloudformation.TemplateVersion
}

// updateStackSteps says how the owner of an account updates its role's
// stack: account create prints the update-stack command, with the
// parameters of the account.
func updateStackSteps(a *model.CloudAccount) string {
	return fmt.Sprintf("run `cluster-vending account create -f <the account's manifest> --tenant %s` and run the update-stack command it prints (stack %s)",
		a.TenantID, RoleName(a.Identity))
}

// TemplateCondition is the account's RoleTemplateCurrent condition: True
// when the stack's template is this build's or newer, False (a warning:
// the account is not failed) when older, with the command to update it;
// nil when no check read the version.
func TemplateCondition(a *model.CloudAccount) *cloudv1.Condition {
	version, known := TemplateVersionOf(a.Access)
	if !known {
		return nil
	}
	c := &cloudv1.Condition{Type: cloudformation.ConditionTemplateCurrent, Status: cloudv1.ConditionStatus_CONDITION_STATUS_TRUE,
		Reason:  cloudformation.ReasonTemplateCurrent,
		Message: fmt.Sprintf("the account's role template is version %d", version)}
	if version < cloudformation.TemplateVersion {
		c.Status, c.Reason = cloudv1.ConditionStatus_CONDITION_STATUS_FALSE, cloudformation.ReasonTemplateOutdated
		c.Message = fmt.Sprintf("the account's role template is version %d, this platform needs %d; update the stack: %s",
			version, cloudformation.TemplateVersion, updateStackSteps(a))
	}
	return c
}

// TemplateHint is what to add to the refusal of something that needs the
// role template of version since or newer (what: "removing Karpenter's
// instance profiles"), when the account's stack is older; empty when it is
// not, or the version is unknown.
func TemplateHint(a *model.CloudAccount, what string, since int) string {
	version, known := TemplateVersionOf(a.Access)
	if !known || version >= since || since <= 0 {
		return ""
	}
	return fmt.Sprintf("%s needs role template version %d but the account's role template is version %d; update the stack: %s",
		what, since, version, updateStackSteps(a))
}

// Versions of the role template that added what a capability needs.
const (
	// SinceTakeOverCheck: the CreateTags DryRun check (updater permissions
	// as the platform now exercises them).
	SinceTakeOverCheck = 1
	// SinceResultsBuckets: the S3 results permissions and ServerPrincipalArn.
	SinceResultsBuckets = 1
	// SinceInstanceProfileCleanup: Karpenter's instance profile cleanup.
	SinceInstanceProfileCleanup = 1
)

// capabilityNeeds is what a refused capability needs a newer template for.
var capabilityNeeds = map[Capability]struct {
	what  string
	since int
}{
	TakeOver: {"taking over", SinceTakeOverCheck},
	Create:   {"creating", SinceResultsBuckets},
}
