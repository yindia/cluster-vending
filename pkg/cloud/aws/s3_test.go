package aws

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

const sourceRole = "arn:aws:iam::111122223333:role/cluster-vending-server"

// With a source role, the client's own credentials assume it first and the
// source role's credentials assume the tenant role: own -> source -> tenant.
func TestSourceRoleIsAssumedBeforeTheTenantRole(t *testing.T) {
	sts := &fakeSTS{}
	c := newTestClient(t, sts, Config{SourceRoleARN: sourceRole})

	got, err := c.tenantCredentials(tenantRole, "", "us-east-1").Retrieve(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessKeyID != "ASIA2" {
		t.Errorf("credentials = %s, want the tenant role's (the second assume)", got.AccessKeyID)
	}
	want := []stsCall{
		{role: sourceRole, session: DefaultSessionName, signedBy: "AKIDBASE"},
		{role: tenantRole, session: DefaultSessionName, signedBy: "ASIA1"}, // signed by the source role's keys
	}
	if calls := sts.recorded(); fmt.Sprint(calls) != fmt.Sprint(want) {
		t.Errorf("STS calls = %+v, want %+v", calls, want)
	}
}

func TestSourceRoleCredentialsAreSharedByTenants(t *testing.T) {
	sts := &fakeSTS{}
	c := newTestClient(t, sts, Config{SourceRoleARN: sourceRole})
	for _, account := range []string{tenantAccount, "777788889999"} {
		if _, err := c.tenantCredentials(tenantRoleARN(account), "", "us-east-1").Retrieve(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	sources := 0
	for _, call := range sts.recorded() {
		if call.role == sourceRole {
			sources++
		}
	}
	if sources != 1 {
		t.Errorf("source role assumed %d times for two tenants, want once (cached)", sources)
	}
}

func TestNoSourceRoleMeansOwnCredentialsAssumeTheTenantRole(t *testing.T) {
	sts := &fakeSTS{}
	c := newTestClient(t, sts, Config{})
	if _, err := c.tenantCredentials(tenantRole, "", "us-east-1").Retrieve(t.Context()); err != nil {
		t.Fatal(err)
	}
	if calls := sts.recorded(); len(calls) != 1 || calls[0].signedBy != "AKIDBASE" {
		t.Errorf("STS calls = %+v, want one signed by the own credentials", calls)
	}
}

func TestSourceRoleMustBeARoleARN(t *testing.T) {
	for _, bad := range []string{"not-an-arn", "arn:aws:iam::123:role/x", "arn:aws:iam::111122223333:user/x"} {
		if _, err := NewFromConfig(awssdk.Config{}, Config{SourceRoleARN: bad}); !errors.Is(err, ErrInvalid) {
			t.Errorf("source role %q: %v, want ErrInvalid", bad, err)
		}
	}
}

// The setting's external ID is the default for a tenant role; an account's
// own wins.
func TestConfigExternalIDIsTheDefaultOfAnAccountWithoutOne(t *testing.T) {
	sts := &fakeSTS{}
	c := newTestClient(t, sts, Config{ExternalID: "from-settings"})
	for _, a := range []Access{
		{AccountID: tenantAccount, RoleARN: tenantRole},
		{AccountID: tenantAccount, RoleARN: tenantRole, ExternalID: "of-the-account"},
	} {
		cfg, err := c.regionConfig(a, "us-east-1")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := cfg.Credentials.Retrieve(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	calls := sts.recorded()
	if len(calls) != 2 || calls[0].externalID != "from-settings" || calls[1].externalID != "of-the-account" {
		t.Errorf("external IDs = %+v, want the setting's, then the account's", calls)
	}
}

func TestS3ClientCallsAsTheTenantRole(t *testing.T) {
	sts := &fakeSTS{}
	c := newTestClient(t, sts, Config{})
	client, err := c.S3(Access{AccountID: tenantAccount, RoleARN: tenantRole}, "eu-west-1")
	if err != nil {
		t.Fatal(err)
	}
	// The fake answers S3 calls with an error; the tenant role is assumed
	// to sign the request first.
	_, _ = client.ListObjectsV2(t.Context(), &s3.ListObjectsV2Input{Bucket: awssdk.String("cv-x-0123456789ab")})
	if calls := sts.recorded(); len(calls) != 1 || calls[0].role != tenantRole {
		t.Errorf("STS calls = %+v, want the tenant role assumed", calls)
	}
}

func TestS3ClientRefusesWhatTenantForRefuses(t *testing.T) {
	c := newTestClient(t, &fakeSTS{}, Config{})
	for name, a := range map[string]Access{
		"no role":       {AccountID: tenantAccount},
		"other account": {AccountID: "777788889999", RoleARN: tenantRole},
		"bad account":   {AccountID: "1", RoleARN: tenantRole},
	} {
		if _, err := c.S3(a, "us-east-1"); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
	if _, err := c.S3(Access{AccountID: tenantAccount, RoleARN: tenantRole}, "mars-1"); !errors.Is(err, ErrInvalid) {
		t.Errorf("unknown region: %v, want ErrInvalid", err)
	}
}

func TestAccessFor(t *testing.T) {
	got, err := AccessFor(tenantAccount, tenantRole, "ext")
	if err != nil || got != (Access{AccountID: tenantAccount, RoleARN: tenantRole, ExternalID: "ext"}) {
		t.Errorf("AccessFor = %+v, %v", got, err)
	}
	for name, args := range map[string][3]string{
		"no role yet":      {tenantAccount, "", ""},
		"role of an other": {tenantAccount, tenantRoleARN("777788889999"), ""},
		"not a role":       {tenantAccount, "arn:aws:iam::" + tenantAccount + ":user/x", ""},
		"bad account":      {"12", tenantRole, ""},
		"other partition":  {tenantAccount, strings.Replace(tenantRole, "arn:aws:", "arn:aws-cn:", 1), ""},
	} {
		if _, err := AccessFor(args[0], args[1], args[2]); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
}

// A session policy narrows what the tenant role can do in the session: the
// API server reads results only, whatever its role allows. It applies to the
// tenant role, not to the source role before it.
func TestSessionPolicyLimitsTheTenantRoleSession(t *testing.T) {
	const policy = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::cv-*/*"}]}`
	sts := &fakeSTS{}
	c := newTestClient(t, sts, Config{SourceRoleARN: sourceRole, SessionPolicy: policy})
	if _, err := c.tenantCredentials(tenantRole, "", "us-east-1").Retrieve(t.Context()); err != nil {
		t.Fatal(err)
	}
	calls := sts.recorded()
	if len(calls) != 2 || calls[0].policy != "" || calls[1].policy != policy {
		t.Errorf("STS calls = %+v, want the policy on the tenant role's assume only", calls)
	}
	// None configured: none sent (the controller).
	sts = &fakeSTS{}
	c = newTestClient(t, sts, Config{})
	if _, err := c.tenantCredentials(tenantRole, "", "us-east-1").Retrieve(t.Context()); err != nil {
		t.Fatal(err)
	}
	if calls := sts.recorded(); len(calls) != 1 || calls[0].policy != "" {
		t.Errorf("STS calls = %+v, want no policy", calls)
	}
}

func TestSessionPolicyMustBeJSONAndSmall(t *testing.T) {
	for name, policy := range map[string]string{"not json": "allow all", "too large": `{"a":"` + strings.Repeat("x", 2048) + `"}`} {
		if _, err := NewFromConfig(awssdk.Config{}, Config{SessionPolicy: policy}); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
}
