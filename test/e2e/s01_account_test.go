//go:build e2e

package e2e

import (
	"fmt"
	"testing"
	"time"

	"connectrpc.com/connect/v2"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// Checks and reasons of the account access check (pkg/cloud/aws/access_check.go,
// pkg/reconcile/account/access.go).
const (
	actionCreateVpc  = "ec2:CreateVpc"
	actionCreateTags = "ec2:CreateTags"

	conditionAccessVerified = "AccessVerified"
	reasonVerifiedForImport = "VerifiedForImport"
)

var readChecks = []string{"ec2:DescribeAvailabilityZones", "ec2:DescribeVpcs", "eks:ListClusters", "iam:GetRole"}

// scenarioAccount onboards the account, runs the access check and asserts
// the verdict of the configured role.
//
// It documents the assumption behind the updater role: the ec2:CreateTags
// DryRun on a resource that does not exist answers DryRunOperation (or
// not-found) when the permission is there, and UnauthorizedOperation when
// it is not. If AWS answered the same to both, the updater and the reader
// could not be told apart, and this scenario fails on the verdict.
func (e *env) scenarioAccount(t *testing.T) {
	e.ensureTenant(t)
	acc := e.onboardAccount(t)
	e.st.accountID = acc.GetId()
	e.cleanupAccount(e.parent)

	e.assertOnboarding(t)

	prev := acc.GetStatus().GetAccess().GetCheckedTime().AsTime()
	if _, err := rpc(e, e.api.Tenant.VerifyCloudAccount, &cloudv1.VerifyCloudAccountRequest{
		TenantId: e.cfg.Tenant, Id: e.st.accountID, Regions: []string{e.cfg.Region},
	}); err != nil {
		t.Fatalf("verify: %v", err)
	}
	var got *cloudv1.CloudAccount
	err := e.waitFor(t, "access check", 10*time.Minute, func() (bool, string, error) {
		res, err := rpc(e, e.api.Tenant.GetCloudAccount, &cloudv1.GetCloudAccountRequest{TenantId: e.cfg.Tenant, Id: e.st.accountID})
		if err != nil {
			return false, "", err
		}
		got = res.GetAccount()
		a := got.GetStatus().GetAccess()
		if a == nil || !a.GetCheckedTime().AsTime().After(prev) {
			return false, "no new verdict yet", nil
		}
		if regionResult(a, e.cfg.Region) == nil {
			return false, "the verdict has no result for the region yet", nil
		}
		return true, fmt.Sprintf("verified=%v: %s", a.GetVerified(), a.GetMessage()), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	e.assertVerdict(t, got)
	e.assertCapabilities(t)
}

func (e *env) ensureTenant(t *testing.T) {
	t.Helper()
	res, err := rpc(e, e.api.Tenant.ListTenants, &cloudv1.ListTenantsRequest{})
	if err != nil {
		t.Fatalf("list tenants: %v", err)
	}
	for _, tn := range res.GetTenants() {
		if tn.GetId() == e.cfg.Tenant {
			t.Logf("tenant %s exists", e.cfg.Tenant)
			return
		}
	}
	if _, err := rpc(e, e.api.Tenant.CreateTenant, &cloudv1.CreateTenantRequest{TenantId: e.cfg.Tenant}); err != nil && !isCode(err, connect.CodeAlreadyExists) {
		t.Fatalf("create tenant %s: %v", e.cfg.Tenant, err)
	}
	t.Logf("tenant %s created (there is no API to delete a tenant; it stays)", e.cfg.Tenant)
}

// onboardAccount returns the tenant's account for the sandbox AWS account:
// the one already there (it is left alone at the end), or a new one.
func (e *env) onboardAccount(t *testing.T) *cloudv1.CloudAccount {
	t.Helper()
	list, err := rpc(e, e.api.Tenant.ListCloudAccounts, &cloudv1.ListCloudAccountsRequest{TenantId: e.cfg.Tenant})
	if err != nil {
		t.Fatalf("list accounts: %v", err)
	}
	for _, a := range list.GetAccounts() {
		if a.GetAws().GetAccountId() == e.cfg.AccountID && a.GetAws().GetRole() == e.cfg.RoleName {
			t.Logf("account %s already onboarded; it is kept at the end", a.GetId())
			return a
		}
	}
	res, err := rpc(e, e.api.Tenant.BootstrapTenant, &cloudv1.BootstrapTenantRequest{
		TenantId: e.cfg.Tenant,
		Account: &cloudv1.CloudAccount{
			AccountName: e.runID,
			Provider:    &cloudv1.CloudAccount_Aws{Aws: &cloudv1.AwsAccount{AccountId: e.cfg.AccountID, Role: e.cfg.RoleName}},
		},
	})
	if err != nil {
		t.Fatalf("bootstrap account: %v", err)
	}
	e.st.createdAccount = true
	return res.GetAccount()
}

// assertOnboarding checks what the tenant is told to do to create the role.
func (e *env) assertOnboarding(t *testing.T) {
	t.Helper()
	res, err := rpc(e, e.api.Tenant.GetAccountOnboarding, &cloudv1.GetAccountOnboardingRequest{
		TenantId: e.cfg.Tenant, Id: e.st.accountID, AccountId: e.cfg.AccountID, Role: e.cfg.Role, RoleName: e.cfg.RoleName,
	})
	if err != nil {
		t.Fatalf("account onboarding: %v", err)
	}
	ob := res.GetOnboarding()
	if ob.GetExternalId() == "" {
		t.Error("the onboarding has no external ID")
	}
	if ob.GetManagementRoleArn() == "" {
		t.Error("the onboarding has no management role ARN: the server has no aws-management-role-arn")
	}
	if ob.GetTemplatePath() == "" {
		t.Error("the onboarding has no template path")
	}
	if ob.GetRoleName() != e.cfg.RoleName {
		t.Errorf("onboarding role name %q, want %q", ob.GetRoleName(), e.cfg.RoleName)
	}
}

func regionResult(a *cloudv1.AccountAccess, region string) *cloudv1.AccessResult {
	for _, r := range a.GetRegions() {
		if r.GetRegion() == region {
			return r
		}
	}
	return nil
}

func checkResult(a *cloudv1.AccountAccess, action, region string) *cloudv1.AccessResult {
	for _, r := range a.GetChecks() {
		if r.GetAction() == action && r.GetRegion() == region {
			return r
		}
	}
	return nil
}

// assertVerdict asserts the verdict of the configured role, check by check.
func (e *env) assertVerdict(t *testing.T, acc *cloudv1.CloudAccount) {
	t.Helper()
	st := acc.GetStatus()
	a := st.GetAccess()
	for _, c := range a.GetChecks() {
		t.Logf("check %-32s %-14s ok=%v %s %s", c.GetAction(), c.GetRegion(), c.GetOk(), c.GetReason(), c.GetMessage())
	}
	if !regionResult(a, e.cfg.Region).GetOk() {
		t.Fatalf("the tenant role cannot be used in %s: %s", e.cfg.Region, regionResult(a, e.cfg.Region).GetMessage())
	}
	for _, action := range readChecks {
		region := e.cfg.Region
		if action == "iam:GetRole" {
			region = "us-east-1" // IAM is global: checked once
		}
		if r := checkResult(a, action, region); r == nil || !r.GetOk() {
			t.Errorf("read check %s in %s should pass for every role: %v", action, region, r)
		}
	}
	vpc, tags := checkResult(a, actionCreateVpc, e.cfg.Region), checkResult(a, actionCreateTags, e.cfg.Region)
	if vpc == nil || tags == nil {
		t.Fatalf("the verdict lacks the %s or %s check", actionCreateVpc, actionCreateTags)
	}
	cond := condition(st.GetConditions(), conditionAccessVerified)

	switch e.cfg.Role {
	case roleProvisioner:
		if !a.GetVerified() || !vpc.GetOk() || !tags.GetOk() {
			t.Errorf("provisioner: verified=%v CreateVpc=%v CreateTags=%v, want all true: %s", a.GetVerified(), vpc.GetOk(), tags.GetOk(), a.GetMessage())
		}
		if st.GetState() != cloudv1.CloudAccountState_CLOUD_ACCOUNT_STATE_READY {
			t.Errorf("provisioner: account state %s, want READY", st.GetState())
		}
	case roleUpdater:
		// The assumption: the CreateTags DryRun passes for an updater and
		// the CreateVpc DryRun does not.
		if !tags.GetOk() {
			t.Errorf("updater: the ec2:CreateTags DryRun should pass (assumption: DryRun on a resource that does not exist is authorised first): %s %s", tags.GetReason(), tags.GetMessage())
		}
		if vpc.GetOk() || a.GetVerified() {
			t.Errorf("updater: ec2:CreateVpc ok=%v verified=%v, want both false", vpc.GetOk(), a.GetVerified())
		}
		e.assertPartlyVerified(t, st, cond)
	case roleReader:
		if tags.GetOk() || vpc.GetOk() || a.GetVerified() {
			t.Errorf("reader: CreateTags=%v CreateVpc=%v verified=%v, want all false", tags.GetOk(), vpc.GetOk(), a.GetVerified())
		}
		e.assertPartlyVerified(t, st, cond)
	}
}

// assertPartlyVerified is the verdict of a reader or updater: not denied.
func (e *env) assertPartlyVerified(t *testing.T, st *cloudv1.CloudAccountStatus, cond *cloudv1.Condition) {
	t.Helper()
	if st.GetState() != cloudv1.CloudAccountState_CLOUD_ACCOUNT_STATE_READY {
		t.Errorf("%s: account state %s, want READY (a reader or updater is not denied)", e.cfg.Role, st.GetState())
	}
	if cond == nil || cond.GetStatus() != cloudv1.ConditionStatus_CONDITION_STATUS_TRUE || cond.GetReason() != reasonVerifiedForImport {
		t.Errorf("%s: condition %s = %v, want True with reason %s", e.cfg.Role, conditionAccessVerified, cond, reasonVerifiedForImport)
	}
}

// assertCapabilities asserts what the role may be used for: a reader or an
// updater cannot create (FAILED_PRECONDITION naming why); a provisioner can
// (scenario 2).
func (e *env) assertCapabilities(t *testing.T) {
	t.Helper()
	if e.cfg.Role == roleProvisioner {
		return
	}
	n, err := e.networkManifest(e.runID + "-cap")
	if err != nil {
		t.Fatal(err)
	}
	res, err := rpc(e, e.api.Network.CreateNetwork, &cloudv1.CreateNetworkRequest{TenantId: e.cfg.Tenant, Network: n, RequestId: newRequestID()})
	if err == nil {
		// The refusal is the test; a network that was created is cleaned up.
		e.st.networkID = res.GetNetwork().GetId()
		e.st.allNetworkIDs = append(e.st.allNetworkIDs, e.st.networkID)
		e.cleanupNetwork(e.parent, func() string { return e.st.networkID })
		t.Fatalf("%s: CreateNetwork was accepted; it must be refused with FAILED_PRECONDITION", e.cfg.Role)
	}
	if !isCode(err, connect.CodeFailedPrecondition) {
		t.Errorf("%s: CreateNetwork: %v, want FAILED_PRECONDITION", e.cfg.Role, err)
		return
	}
	t.Logf("%s: create refused as expected: %v", e.cfg.Role, err)
}

// cleanupAccount deletes the account the run onboarded, and verifies it is gone.
func (e *env) cleanupAccount(parent *testing.T) {
	parent.Cleanup(func() {
		if !e.st.createdAccount {
			return
		}
		if _, err := rpc(e, e.api.Tenant.DeleteCloudAccount, &cloudv1.DeleteCloudAccountRequest{TenantId: e.cfg.Tenant, Id: e.st.accountID}); err != nil && !isCode(err, connect.CodeNotFound) {
			parent.Errorf("cleanup: delete account %s: %v", e.st.accountID, err)
			return
		}
		err := e.waitFor(parent, "account deletion", 10*time.Minute, func() (bool, string, error) {
			_, err := rpc(e, e.api.Tenant.GetCloudAccount, &cloudv1.GetCloudAccountRequest{TenantId: e.cfg.Tenant, Id: e.st.accountID})
			if isCode(err, connect.CodeNotFound) {
				return true, "gone", nil
			}
			return false, "still there", err
		})
		if err != nil {
			parent.Errorf("cleanup: account %s was not deleted: %v", e.st.accountID, err)
		}
	})
}
