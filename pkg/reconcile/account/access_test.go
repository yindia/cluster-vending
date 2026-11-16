package account

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	awscloud "github.com/yindia/cluster-vending/pkg/cloud/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/storetest"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// checker is a fake access check: it answers report and records what it
// was asked.
type checker struct {
	report  awscloud.AccessReport
	access  awscloud.Access
	regions []string
	calls   int
}

func (c *checker) CheckAccess(_ context.Context, a awscloud.Access, regions []string) (awscloud.AccessReport, error) {
	c.calls++
	c.access, c.regions = a, regions
	return c.report, nil
}

var now = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

func allowed() awscloud.AccessReport {
	return awscloud.AccessReport{PrincipalARN: "arn:aws:sts::444455556666:assumed-role/cluster-vending-provisioner/s",
		Regions: []awscloud.RegionAccess{{Region: "us-east-1", OK: true}, {Region: "us-west-2", OK: true}},
		Checks:  []awscloud.CheckResult{{Action: awscloud.ActionDescribeVpcs, Region: "us-east-1", OK: true}}}
}

func denied() awscloud.AccessReport {
	r := allowed()
	r.Regions[1] = awscloud.RegionAccess{Region: "us-west-2", Reason: awscloud.ReasonDeniedBySCP, Message: "explicit deny in a service control policy"}
	return r
}

func throttled() awscloud.AccessReport {
	r := allowed()
	r.Regions[1] = awscloud.RegionAccess{Region: "us-west-2", Reason: awscloud.ReasonUnavailable, Message: "Rate exceeded"}
	r.Transient = true
	return r
}

func TestAccessDue(t *testing.T) {
	checked := &model.AccountAccess{Verified: true, CheckedAt: now.Add(-time.Hour), AttemptedAt: now.Add(-time.Hour),
		Regions: []model.AccessResult{{Region: "us-east-1", OK: true}, {Region: "us-west-2", OK: true}}}
	stale := *checked
	stale.Stale, stale.AttemptedAt = true, now.Add(-20*time.Minute)
	tests := []struct {
		name    string
		access  *model.AccountAccess
		updated time.Time
		asked   time.Time
		regions []string
		due     bool
		wait    time.Duration
	}{
		{name: "never checked", due: true},
		{name: "checked an hour ago", access: checked, regions: []string{"us-west-2"}, wait: 5 * time.Hour},
		{name: "checked 6 hours ago", access: &model.AccountAccess{Verified: true, CheckedAt: now.Add(-6 * time.Hour),
			AttemptedAt: now.Add(-6 * time.Hour), Regions: checked.Regions}, regions: []string{"us-west-2"}, due: true},
		{name: "asked for since", access: checked, asked: now.Add(-time.Minute), regions: []string{"us-west-2"}, due: true},
		{name: "asked for before", access: checked, asked: now.Add(-2 * time.Hour), regions: []string{"us-west-2"}, wait: 5 * time.Hour},
		{name: "the account changed since", access: checked, updated: now.Add(-time.Minute), regions: []string{"us-west-2"}, due: true},
		{name: "a region not checked yet", access: checked, regions: []string{"us-west-2", "eu-west-1"}, due: true},
		{name: "a region no longer used", access: checked, wait: 5 * time.Hour},
		{name: "stale for 20 minutes", access: &stale, regions: []string{"us-west-2"}, due: true},
		{name: "stale for 5 minutes", access: &model.AccountAccess{Stale: true, CheckedAt: checked.CheckedAt,
			AttemptedAt: now.Add(-5 * time.Minute), Regions: checked.Regions}, regions: []string{"us-west-2"}, wait: 10 * time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := &model.CloudAccount{Access: tt.access, AccessRequestedAt: tt.asked}
			a.UpdatedAt = tt.updated

			due, wait := AccessDue(a, tt.regions, now)

			if due != tt.due || (!due && wait != tt.wait) {
				t.Errorf("due %v, wait %v; want %v, %v", due, wait, tt.due, tt.wait)
			}
		})
	}
}

func TestMergeAccess(t *testing.T) {
	verified := MergeAccess(nil, allowed(), now.Add(-time.Hour))
	tests := []struct {
		name     string
		prev     *model.AccountAccess
		report   awscloud.AccessReport
		verified bool
		stale    bool
		checked  time.Time
		reason   string
	}{
		{name: "allowed", report: allowed(), verified: true, checked: now},
		{name: "denied", prev: verified, report: denied(), checked: now, reason: awscloud.ReasonDeniedBySCP},
		{name: "transient keeps the last verdict", prev: verified, report: throttled(), verified: true, stale: true,
			checked: now.Add(-time.Hour)},
		{name: "transient without a verdict is no verdict", report: throttled(), stale: true, reason: awscloud.ReasonUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MergeAccess(tt.prev, tt.report, now)

			if got.Verified != tt.verified || got.Stale != tt.stale || !got.CheckedAt.Equal(tt.checked) ||
				!got.AttemptedAt.Equal(now) || got.Reason != tt.reason {
				t.Errorf("access = %+v", got)
			}
			if tt.prev != nil && tt.prev.AttemptedAt.Equal(now) {
				t.Error("the previous access was changed")
			}
		})
	}
}

func TestAccessCondition(t *testing.T) {
	tests := []struct {
		name   string
		access *model.AccountAccess
		status cloudv1.ConditionStatus
		reason string
		state  cloudv1.CloudAccountState
	}{
		{name: "not checked", status: cloudv1.ConditionStatus_CONDITION_STATUS_UNKNOWN, reason: "NotChecked",
			state: cloudv1.CloudAccountState_CLOUD_ACCOUNT_STATE_READY},
		{name: "verified", access: MergeAccess(nil, allowed(), now), status: cloudv1.ConditionStatus_CONDITION_STATUS_TRUE,
			reason: "Verified", state: cloudv1.CloudAccountState_CLOUD_ACCOUNT_STATE_READY},
		{name: "denied", access: MergeAccess(nil, denied(), now), status: cloudv1.ConditionStatus_CONDITION_STATUS_FALSE,
			reason: awscloud.ReasonDeniedBySCP, state: cloudv1.CloudAccountState_CLOUD_ACCOUNT_STATE_FAILED},
		{name: "unavailable", access: MergeAccess(nil, throttled(), now), status: cloudv1.ConditionStatus_CONDITION_STATUS_UNKNOWN,
			reason: awscloud.ReasonUnavailable, state: cloudv1.CloudAccountState_CLOUD_ACCOUNT_STATE_READY},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := AccessCondition(tt.access)
			if c.GetType() != ConditionAccessVerified || c.GetStatus() != tt.status || c.GetReason() != tt.reason || c.GetMessage() == "" {
				t.Errorf("condition = %v", c)
			}
			out := Proto(&model.CloudAccount{Access: tt.access})
			if out.GetStatus().GetState() != tt.state || (tt.access != nil) != (out.GetStatus().GetAccess() != nil) {
				t.Errorf("status = %v", out.GetStatus())
			}
		})
	}
}

func TestAccessNote(t *testing.T) {
	ok, no, unknown := MergeAccess(nil, allowed(), now), MergeAccess(nil, denied(), now), MergeAccess(nil, throttled(), now)
	stale := MergeAccess(ok, throttled(), now)
	tests := []struct {
		name    string
		was, is *model.AccountAccess
		want    string
	}{
		{name: "first verified", is: ok, want: "Normal AccessVerified"},
		{name: "still verified", was: ok, is: ok},
		{name: "denied", was: ok, is: no, want: "Warning AccessDenied"},
		{name: "still denied", was: no, is: no},
		{name: "recovered", was: no, is: ok, want: "Normal AccessVerified"},
		{name: "unavailable is no event", is: unknown},
		{name: "stale is no event", was: ok, is: stale},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ""
			if n := AccessNote(tt.was, tt.is); n != nil {
				got = n.Type + " " + n.Reason
			}
			if got != tt.want {
				t.Errorf("note %q, want %q", got, tt.want)
			}
		})
	}
}

func TestVerifyAccess(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	for i, region := range []string{"us-west-2", "eu-west-1", "us-west-2"} {
		n := &model.Network{TenantID: tenant, Name: fmt.Sprintf("net-%d", i), Provider: model.ProviderAWS,
			Region: region, CloudAccountID: w.account.ID, Resource: model.Resource{Spec: storetest.Spec("network")}}
		if err := w.store.Networks().Create(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	c := &checker{report: allowed()}
	c.report.Regions = append(c.report.Regions, awscloud.RegionAccess{Region: "eu-west-1", OK: true})
	rec := New(reconcile.Deps{Store: w.store, Kube: w.kube, AWSAccessCheck: c})

	// The account's own times are the store's clock.
	at := time.Now()
	got, wait, err := rec.VerifyAccess(ctx, w.account, at)

	if err != nil || got == nil || !got.Verified || wait != AccessInterval {
		t.Fatalf("access %+v, wait %v (%v)", got, wait, err)
	}
	if c.access.RoleARN != identity || c.access.Direct {
		t.Errorf("checked as %+v, want the tenant role", c.access)
	}
	if !slices.Equal(c.regions, []string{"eu-west-1", "us-west-2"}) {
		t.Errorf("regions %v, want those of the account's networks, once each", c.regions)
	}

	// Checked: not again until it is due.
	w.account.Access = got
	if again, _, err := rec.VerifyAccess(ctx, w.account, at.Add(time.Minute)); err != nil || again != nil || c.calls != 1 {
		t.Errorf("checked again before due: %+v, %d calls (%v)", again, c.calls, err)
	}

	// A region asked for (VerifyCloudAccount.regions) that no network uses
	// yet is checked too, at once: it was not checked before.
	w.account.AccessRequestedRegions = []string{"ap-south-1", "eu-west-1"}
	if _, _, err := rec.VerifyAccess(ctx, w.account, at.Add(2*time.Minute)); err != nil || c.calls != 2 {
		t.Fatalf("a requested region: %d calls (%v), want a second check", c.calls, err)
	}
	if !slices.Equal(c.regions, []string{"ap-south-1", "eu-west-1", "us-west-2"}) {
		t.Errorf("regions %v, want the requested ones and the networks', once each", c.regions)
	}
}

func TestVerifyAccessSkips(t *testing.T) {
	tests := []struct {
		name   string
		paused bool
		check  bool
	}{
		{name: "paused", paused: true, check: true},
		{name: "no checker"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			c := &checker{report: allowed()}
			d := reconcile.Deps{Store: w.store, Kube: w.kube}
			if tt.check {
				d.AWSAccessCheck = c
			}
			if tt.paused {
				if err := w.kube.Create(t.Context(), &cloudv1alpha1.ReleaseHold{ObjectMeta: metav1.ObjectMeta{Name: reconcile.FreezeName}, Spec: cloudv1alpha1.ReleaseHoldSpec{Platform: true, By: "ops", Reason: "test"}}); err != nil {
					t.Fatal(err)
				}
			}

			got, _, err := New(d).VerifyAccess(t.Context(), w.account, now)

			if err != nil || got != nil || c.calls != 0 {
				t.Errorf("access %+v, %d calls (%v)", got, c.calls, err)
			}
		})
	}
}

// An account without a usable tenant role fails the check without a call.
func TestVerifyAccessWithoutARole(t *testing.T) {
	w := newWorld(t)
	c := &checker{report: allowed()}
	a := *w.account
	a.Identity = ""

	got, _, err := New(reconcile.Deps{Store: w.store, AWSAccessCheck: c}).VerifyAccess(t.Context(), &a, now)

	if err != nil || got == nil || got.Verified || got.Reason != ReasonNoTenantRole || c.calls != 0 {
		t.Errorf("access %+v, %d calls (%v)", got, c.calls, err)
	}
}
