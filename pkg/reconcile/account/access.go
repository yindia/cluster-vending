package account

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	awscloud "github.com/yindia/cluster-vending/pkg/cloud/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

const (
	// AccessInterval is how often an account's access is checked when
	// nothing asks for it sooner.
	AccessInterval = 6 * time.Hour
	// AccessRetry is how soon a check that failed transiently is tried
	// again.
	AccessRetry = 15 * time.Minute
	// maxAccessRegions and maxAccessChecks are the kind's bounds of
	// status.access.regions and .checks.
	maxAccessRegions, maxAccessChecks = 40, 200
	networkPage                       = 100
)

// Reasons of the AccessVerified condition besides the check's own
// (awscloud.Reason*).
const (
	ReasonAccessNotChecked = "NotChecked"
	ReasonAccessVerified   = "Verified"
	// ReasonNoTenantRole: the account has no usable tenant role stored.
	ReasonNoTenantRole = "NoTenantRole"
)

// AccessDue says whether the account's access is to be checked now, and
// when not, how long until it is: never checked, asked for since the last
// attempt (VerifyCloudAccount), the account changed since, a region its
// networks use was not checked, the last verdict is AccessInterval old,
// or the last attempt failed transiently AccessRetry ago.
func AccessDue(a *model.CloudAccount, regions []string, now time.Time) (bool, time.Duration) {
	last := a.Access
	if last == nil {
		return true, 0
	}
	// The status keeps seconds (metav1.Time): a request or a change in the
	// second of the last attempt is taken as seen by it.
	attempted := last.AttemptedAt.Truncate(time.Second)
	if a.AccessRequestedAt.Truncate(time.Second).After(attempted) || a.UpdatedAt.Truncate(time.Second).After(attempted) {
		return true, 0
	}
	for _, region := range regions {
		if !slices.ContainsFunc(last.Regions, func(r model.AccessResult) bool { return r.Region == region }) {
			return true, 0
		}
	}
	next := last.CheckedAt.Add(AccessInterval)
	if last.Stale {
		next = last.AttemptedAt.Add(AccessRetry)
	}
	if wait := next.Sub(now); wait > 0 {
		return false, wait
	}
	return true, 0
}

// MergeAccess is the account's access after a check that reported report
// at now. A transient failure is no verdict: the last verdict stays, with
// Stale set; without one there is none (Verified false, CheckedAt zero).
// prev is not changed.
func MergeAccess(prev *model.AccountAccess, report awscloud.AccessReport, now time.Time) *model.AccountAccess {
	if report.Transient && prev != nil && !prev.CheckedAt.IsZero() {
		next := *prev
		next.Stale, next.AttemptedAt = true, now
		return &next
	}
	next := &model.AccountAccess{Verified: report.OK(), AttemptedAt: now, Stale: report.Transient, PrincipalARN: report.PrincipalARN}
	if !report.Transient {
		next.CheckedAt = now
	}
	if failed := report.Failed(); failed != nil {
		next.Reason, next.Message = failed.Reason, report.Message()
	}
	for _, r := range report.Regions[:min(len(report.Regions), maxAccessRegions)] {
		next.Regions = append(next.Regions, model.AccessResult{Region: r.Region, OK: r.OK, Reason: r.Reason, Message: r.Message})
	}
	for _, c := range report.Checks[:min(len(report.Checks), maxAccessChecks)] {
		next.Checks = append(next.Checks, model.AccessResult{Action: c.Action, Region: c.Region, OK: c.OK, Reason: c.Reason, Message: c.Message})
	}
	return next
}

// VerifyAccess checks, when it is due (AccessDue), that the controller can
// reach and act in the account: the tenant role in every region the
// account's networks use, and the calls the platform needs
// (awscloud.AccessChecks). It returns the account's access after the
// check, nil when none ran (not due, paused, or no checker), and how long
// until the next is due. Only the store is written to by the caller: it
// changes nothing itself.
func (r *Reconciler) VerifyAccess(ctx context.Context, a *model.CloudAccount, now time.Time) (*model.AccountAccess, time.Duration, error) {
	if a.Paused != nil || r.deps.AWSAccessCheck == nil {
		return nil, 0, nil
	}
	regions, err := r.accountRegions(ctx, a)
	if err != nil {
		return nil, 0, err
	}
	if due, wait := AccessDue(a, regions, now); !due {
		return nil, wait, nil
	}
	access, roleErr := r.deps.AWSAccess(ctx, a)
	if roleErr != nil {
		// A verdict, not a failure of the pass: the account cannot be used.
		return &model.AccountAccess{CheckedAt: now, AttemptedAt: now, Reason: ReasonNoTenantRole, Message: roleErr.Error()}, AccessInterval, nil //nolint:nilerr // reported in the status
	}
	report, err := r.deps.AWSAccessCheck.CheckAccess(ctx, access, regions)
	if err != nil {
		return nil, 0, err
	}
	next := MergeAccess(a.Access, report, now)
	if next.Stale {
		return next, AccessRetry, nil
	}
	return next, AccessInterval, nil
}

// accountRegions are the regions of the account's networks and the ones
// VerifyCloudAccount asked for (AccessRequestedRegions), sorted, each
// once. A cluster is in its network's region.
func (r *Reconciler) accountRegions(ctx context.Context, a *model.CloudAccount) ([]string, error) {
	var regions []string
	for _, region := range a.AccessRequestedRegions {
		if region != "" && !slices.Contains(regions, region) {
			regions = append(regions, region)
		}
	}
	page := repository.Page{Limit: networkPage}
	for {
		networks, err := r.deps.Store.Networks().List(ctx, a.TenantID, page)
		if err != nil {
			return nil, err
		}
		for _, n := range networks {
			if n.CloudAccountID == a.ID && n.Region != "" && !slices.Contains(regions, n.Region) {
				regions = append(regions, n.Region)
			}
		}
		if len(networks) < page.Limit {
			break
		}
		page.After = networks[len(networks)-1].Name
	}
	slices.Sort(regions)
	return regions, nil
}

// AccessCondition is the AccessVerified condition of an account's access:
// True when verified, False when a check was denied, Unknown before the
// first verdict.
func AccessCondition(a *model.AccountAccess) *cloudv1.Condition {
	c := &cloudv1.Condition{Type: ConditionAccessVerified, Status: cloudv1.ConditionStatus_CONDITION_STATUS_UNKNOWN}
	switch {
	case a == nil:
		c.Reason, c.Message = ReasonAccessNotChecked, "the controller has not checked the account's access yet"
	case a.CheckedAt.IsZero():
		c.Reason, c.Message = a.Reason, "the access check could not finish: "+a.Message
	case a.Verified:
		c.Status, c.Reason = cloudv1.ConditionStatus_CONDITION_STATUS_TRUE, ReasonAccessVerified
		c.Message = fmt.Sprintf("the tenant role works in %s and allows the platform's calls (checked %s)",
			strings.Join(accessRegions(a), ", "), a.CheckedAt.UTC().Format(time.RFC3339))
	default:
		c.Status, c.Reason, c.Message = cloudv1.ConditionStatus_CONDITION_STATUS_FALSE, a.Reason, a.Message
	}
	if a != nil && a.Stale && !a.CheckedAt.IsZero() {
		c.Message += fmt.Sprintf("; the check at %s could not finish (it is tried again)", a.AttemptedAt.UTC().Format(time.RFC3339))
	}
	return c
}

// AccessDenied: the last verdict of the account's access is a denial.
func AccessDenied(a *model.AccountAccess) bool {
	return a != nil && !a.Verified && !a.CheckedAt.IsZero()
}

func accessRegions(a *model.AccountAccess) []string {
	out := make([]string, 0, len(a.Regions))
	for _, r := range a.Regions {
		out = append(out, r.Region)
	}
	return out
}

// AccessNote says what a change of the account's access means: verified
// (the first time, or again after a denial), or denied (newly, or for
// another reason). A check that could not finish is no event. Nil when
// nothing changed.
func AccessNote(was, is *model.AccountAccess) *reconcile.Note {
	switch {
	case is == nil || is.CheckedAt.IsZero():
		return nil
	case is.Verified && (was == nil || !was.Verified):
		n := reconcile.Normal(reconcile.ReasonAccessVerified, AccessCondition(is).GetMessage())
		return &n
	case AccessDenied(is) && (!AccessDenied(was) || was.Reason != is.Reason || was.Message != is.Message):
		n := reconcile.Warning(reconcile.ReasonAccessDenied, is.Reason+": "+is.Message)
		return &n
	default:
		return nil
	}
}

// accessProto is the API view of an account's access; nil before the
// first check.
func accessProto(a *model.AccountAccess) *cloudv1.AccountAccess {
	if a == nil {
		return nil
	}
	out := &cloudv1.AccountAccess{Verified: a.Verified, Stale: a.Stale, PrincipalArn: a.PrincipalARN, Reason: a.Reason,
		Message: a.Message, CheckedTime: timestamp(a.CheckedAt), AttemptedTime: timestamp(a.AttemptedAt)}
	for _, r := range a.Regions {
		out.Regions = append(out.Regions, resultProto(r))
	}
	for _, c := range a.Checks {
		out.Checks = append(out.Checks, resultProto(c))
	}
	return out
}

func resultProto(r model.AccessResult) *cloudv1.AccessResult {
	return &cloudv1.AccessResult{Action: r.Action, Region: r.Region, Ok: r.OK, Reason: r.Reason, Message: r.Message}
}

func timestamp(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}

// NoExternalIDWarning is added to a verified account's AccessVerified
// message when the account has no external ID (onboarded before them): it
// works, but its role trusts the management role without one.
const NoExternalIDWarning = "warning: no external ID: run account create again, so the role's stack requires one"

// withExternalIDWarning is c with NoExternalIDWarning when a is verified
// and has no external ID; the condition stays True.
func withExternalIDWarning(c *cloudv1.Condition, a *model.CloudAccount) *cloudv1.Condition {
	if c.GetStatus() == cloudv1.ConditionStatus_CONDITION_STATUS_TRUE && a.ExternalID == "" {
		c.Message += "; " + NoExternalIDWarning
	}
	return c
}
