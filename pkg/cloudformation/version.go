package cloudformation

import "strconv"

// TemplateVersion is the version of the role templates, bumped whenever a
// template's permissions or trust change (a test fails when the templates
// change without it: version_test.go). Every template carries it as the
// role's tag TagTemplateVersion and as the stack output TemplateVersion,
// and the controller's access check reads the tag of an account's role and
// says when the account's stack is older than this build's: its owner
// updates the stack with the template the API server serves. A role
// without the tag (a stack from before versions) is version 0.
//
// scripts/generate.py reads this line: keep it "const TemplateVersion = N".
//
// History:
//
//	1: S3 results permissions and ServerPrincipalArn, Karpenter instance
//	   profile cleanup, the DryRun CreateTags check, the version tag.
const TemplateVersion = 1

// ManagementTemplateVersion is the version of the management account's
// role templates (management and server), bumped whenever one of them
// changes (version_test.go). They carry it as TagTemplateVersion and the
// output TemplateVersion like the tenant templates; the platform does not
// read it back (it never reads its own roles), so the two versions move
// apart: a change of a management template is not a tenant stack update.
//
// scripts/generate.py reads this line: keep it
// "const ManagementTemplateVersion = N".
//
// History:
//
//	1: the management and server roles: IRSA of the controller, the
//	   Crossplane AWS providers and the API server, or a local principal;
//	   sts:AssumeRole on the tenant roles only.
const ManagementTemplateVersion = 1

// TagTemplateVersion is the role tag holding the template's version.
// iam:ListRoleTags, which every role grants itself (the reader's
// ReadIAM statement), reads it.
const TagTemplateVersion = Platform + ".io/template-version"

// Condition type and reason of an account's status that says its role's
// template is older than the platform needs.
const (
	ConditionTemplateCurrent = "RoleTemplateCurrent"
	ReasonTemplateOutdated   = "TemplateOutdated"
	ReasonTemplateCurrent    = "TemplateCurrent"
)

// VersionOfTags is the template version a role's tags say: 0 when there is
// no tag or it is not a version, a stack from before versions.
func VersionOfTags(tags map[string]string) int {
	v, err := strconv.Atoi(tags[TagTemplateVersion])
	if err != nil || v < 0 {
		return 0
	}
	return v
}
