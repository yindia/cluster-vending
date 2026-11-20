package account

import (
	"fmt"
	"slices"
	"strings"

	awscloud "github.com/yindia/cluster-vending/pkg/cloud/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
)

// Capability is what an account is used for. An account's role decides
// which it has: a reader reads (Import), an updater also changes what exists
// (TakeOver), a provisioner also creates (Create).
type Capability int

const (
	// Create needs every check to pass, the DryRun create included.
	Create Capability = iota
	// Import needs the reads an import makes.
	Import
	// TakeOver needs the reads and the DryRun write of the updater role.
	TakeOver
)

// String is the capability as a verb in a message.
func (c Capability) String() string {
	switch c {
	case Import:
		return "import"
	case TakeOver:
		return "take over"
	default:
		return "create"
	}
}

// required are the checks, in each region, a capability needs besides the
// region's identity call. Create has no list: it needs all of them.
var required = map[Capability][]string{
	Import: {awscloud.ActionDescribeAvailabilityZones, awscloud.ActionDescribeVpcs, awscloud.ActionListClusters},
	TakeOver: {awscloud.ActionDescribeAvailabilityZones, awscloud.ActionDescribeVpcs, awscloud.ActionListClusters,
		awscloud.ActionCreateTagsDryRun},
}

// Allows says whether the verdict a lets the account serve c in region
// (empty: in every region checked). A verified verdict allows everything;
// otherwise the per-check results decide, and every required check must be
// there and passed.
func Allows(a *model.AccountAccess, c Capability, region string) bool {
	if a == nil || a.CheckedAt.IsZero() {
		return false
	}
	if a.Verified {
		return true
	}
	if c == Create {
		return false
	}
	regions := 0
	for _, r := range a.Regions {
		if (region == "" || r.Region == region) && !r.OK {
			return false
		}
		if region == "" || r.Region == region {
			regions++
		}
	}
	if regions == 0 {
		return false
	}
	for _, action := range required[c] {
		seen := false
		for _, r := range a.Checks {
			if r.Action != action || (region != "" && r.Region != region) {
				continue
			}
			if !r.OK {
				return false
			}
			seen = true
		}
		if !seen {
			return false
		}
	}
	return true
}

// Capabilities are the capabilities of the verdict a in every region
// checked, widest last; none for a denial.
func Capabilities(a *model.AccountAccess) []Capability {
	var out []Capability
	for _, c := range []Capability{Import, TakeOver, Create} {
		if Allows(a, c, "") {
			out = append(out, c)
		}
	}
	return out
}

// partial: the verdict allows import but not create: a reader or an updater.
func partial(a *model.AccountAccess) bool {
	return a != nil && !a.Verified && Allows(a, Import, "")
}

// partialMessage says what a partial verdict allows and what failed.
func partialMessage(a *model.AccountAccess) string {
	caps := Capabilities(a)
	verbs := make([]string, 0, len(caps))
	for _, c := range caps {
		verbs = append(verbs, c.String())
	}
	slices.Reverse(verbs)
	return fmt.Sprintf("the tenant role works in %s and allows %s, not create (%s: %s)",
		strings.Join(accessRegions(a), ", "), strings.Join(verbs, " and "), a.Reason, a.Message)
}
