package account

import (
	"fmt"
	"slices"
	"time"

	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/fault"
)

// Usable says whether a network or cluster may be created, imported or
// taken over in account a and region (empty: the account only): the
// controller's last access check of it (VerifyAccess) gave a verdict, the
// verdict is verified, and the region was among those checked. A refusal
// is FailedPrecondition, saying what to do. A verified verdict whose later
// checks failed for a transient reason is still usable; the warning then
// says so.
func Usable(a *model.CloudAccount, region string) (warning string, err error) {
	name, verify := accountLabel(a), "cluster-vending account verify --id "+a.ID
	last := a.Access
	if last == nil || last.CheckedAt.IsZero() {
		msg := fmt.Sprintf("account %s has not been verified yet; the controller checks it shortly after it is added, or run: %s", name, verify)
		if last != nil && last.Reason != "" {
			msg += fmt.Sprintf(" (the last attempt failed: %s: %s)", last.Reason, last.Message)
		}
		return "", fault.New(fault.FailedPrecondition, msg)
	}
	i := slices.IndexFunc(last.Regions, func(r model.AccessResult) bool { return r.Region == region })
	if region != "" && i >= 0 && !last.Regions[i].OK {
		r := last.Regions[i]
		return "", fault.Newf(fault.FailedPrecondition, "region %s of account %s cannot be used: %s: %s; fix the account and run: %s",
			region, name, r.Reason, r.Message, verify)
	}
	if !last.Verified {
		return "", fault.Newf(fault.FailedPrecondition, "account %s cannot be used: %s: %s; fix the account and run: %s",
			name, last.Reason, last.Message, verify)
	}
	if region != "" && i < 0 {
		return "", fault.Newf(fault.FailedPrecondition, "region %s of account %s has not been verified yet; run: %s --region %s",
			region, name, verify, region)
	}
	if last.Stale {
		warning = fmt.Sprintf("account %s was last verified at %s; the checks since failed for a transient reason (throttling, the network)",
			name, last.CheckedAt.UTC().Format(time.RFC3339))
	}
	return warning, nil
}

// accountLabel names an account in a message: its name and cloud ID, or
// the ID alone when it has no name.
func accountLabel(a *model.CloudAccount) string {
	if a.AccountName == "" {
		return a.AccountID
	}
	return fmt.Sprintf("%s (%s)", a.AccountName, a.AccountID)
}
