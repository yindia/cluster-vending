package aws

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
)

const (
	tenantAccount = "444455556666"
	tenantRole    = "arn:aws:iam::444455556666:role/cluster-vending-provisioner"
	sourceRole    = "arn:aws:iam::111122223333:role/cluster-vending"
)

func tenantRoleARN(accountID string) string {
	return "arn:aws:iam::" + accountID + ":role/cluster-vending-provisioner"
}

// fakeSTS answers AssumeRole. Each call returns credentials with access key
// ASIA<n>; it records the role, session name and the key that signed the
// call, so tests can check the chain.
type fakeSTS struct {
	mu            sync.Mutex
	calls         []stsCall
	denyAll       bool
	callerAccount string // GetCallerIdentity's answer
}

type stsCall struct{ role, session, signedBy string }

func (f *fakeSTS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "unexpected request", http.StatusBadRequest)
		return
	}
	if r.Form.Get("Action") == "GetCallerIdentity" {
		w.Header().Set("Content-Type", "text/xml")
		_, _ = fmt.Fprintf(w, `<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><GetCallerIdentityResult>
<Arn>arn:aws:sts::%[1]s:assumed-role/x/s</Arn><UserId>AROA:s</UserId><Account>%[1]s</Account>
</GetCallerIdentityResult></GetCallerIdentityResponse>`, f.callerAccount)
		return
	}
	if r.Form.Get("Action") != "AssumeRole" {
		http.Error(w, "unexpected request", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.denyAll {
		w.Header().Set("Content-Type", "text/xml")
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprint(w, `<ErrorResponse><Error><Type>Sender</Type><Code>AccessDenied</Code>`+
			`<Message>not authorized to perform sts:AssumeRole</Message></Error><RequestId>1</RequestId></ErrorResponse>`)
		return
	}
	// Authorization: AWS4-HMAC-SHA256 Credential=<key>/<date>/...
	auth := r.Header.Get("Authorization")
	signedBy := ""
	if _, after, ok := strings.Cut(auth, "Credential="); ok {
		signedBy, _, _ = strings.Cut(after, "/")
	}
	f.calls = append(f.calls, stsCall{role: r.Form.Get("RoleArn"), session: r.Form.Get("RoleSessionName"), signedBy: signedBy})
	w.Header().Set("Content-Type", "text/xml")
	_, _ = fmt.Fprintf(w, `<AssumeRoleResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><AssumeRoleResult>
<Credentials><AccessKeyId>ASIA%d</AccessKeyId><SecretAccessKey>secret</SecretAccessKey>
<SessionToken>token</SessionToken><Expiration>2099-01-01T00:00:00Z</Expiration></Credentials>
<AssumedRoleUser><Arn>%s/s</Arn><AssumedRoleId>AROA:s</AssumedRoleId></AssumedRoleUser>
</AssumeRoleResult></AssumeRoleResponse>`, len(f.calls), r.Form.Get("RoleArn"))
}

func (f *fakeSTS) recorded() []stsCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]stsCall(nil), f.calls...)
}

// newTestClient starts from static "ambient" credentials AKIDBASE and
// sends every AWS call to the fake.
func newTestClient(t *testing.T, sts http.Handler, cfg Config) *Client {
	t.Helper()
	srv := httptest.NewServer(sts)
	t.Cleanup(srv.Close)
	base := awssdk.Config{
		Region:       "us-east-1",
		Credentials:  credentials.NewStaticCredentialsProvider("AKIDBASE", "secret", ""),
		BaseEndpoint: awssdk.String(srv.URL),
		HTTPClient:   srv.Client(),
	}
	c, err := NewFromConfig(base, cfg)
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	return c
}

func TestAssumesTenantRoleFromAmbientCredentials(t *testing.T) {
	sts := &fakeSTS{}
	c := newTestClient(t, sts, Config{})

	got, err := c.tenantCredentials(tenantRole, "eu-west-1").Retrieve(t.Context())
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if got.AccessKeyID != "ASIA1" {
		t.Errorf("credentials = %s, want the assumed role's", got.AccessKeyID)
	}
	want := []stsCall{{role: tenantRole, session: DefaultSessionName, signedBy: "AKIDBASE"}}
	if calls := sts.recorded(); fmt.Sprint(calls) != fmt.Sprint(want) {
		t.Errorf("STS calls = %+v, want %+v", calls, want)
	}
}

func TestChainsThroughSourceRole(t *testing.T) {
	sts := &fakeSTS{}
	c := newTestClient(t, sts, Config{SourceRoleARN: sourceRole, SessionName: "dev-alice"})

	if _, err := c.tenantCredentials(tenantRole, "us-east-1").Retrieve(t.Context()); err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	// The tenant role is assumed with the source role's credentials.
	want := []stsCall{
		{role: sourceRole, session: "dev-alice", signedBy: "AKIDBASE"},
		{role: tenantRole, session: "dev-alice", signedBy: "ASIA1"},
	}
	if calls := sts.recorded(); fmt.Sprint(calls) != fmt.Sprint(want) {
		t.Errorf("STS calls = %+v, want %+v", calls, want)
	}
}

func TestCredentialsAreCachedPerAccountAndRegion(t *testing.T) {
	sts := &fakeSTS{}
	c := newTestClient(t, sts, Config{})
	retrieve := func(account, region string) {
		t.Helper()
		if _, err := c.tenantCredentials(tenantRoleARN(account), region).Retrieve(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	retrieve(tenantAccount, "us-east-1")
	retrieve(tenantAccount, "us-east-1") // cached
	retrieve("777788889999", "us-east-1")
	if n := len(sts.recorded()); n != 2 {
		t.Errorf("STS calls = %d, want 2 (one per account)", n)
	}
}

func TestDeniedAssumeRoleIsAccessDenied(t *testing.T) {
	c := newTestClient(t, &fakeSTS{denyAll: true}, Config{})
	tenant, err := c.Tenant(tenantAccount, tenantRole, "us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	_, err = tenant.DiscoverNetwork(t.Context(), "vpc-0123456789abcdef0", []string{"subnet-0123456789abcdef0"})
	if !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("DiscoverNetwork = %v, want ErrAccessDenied", err)
	}
}

func TestTenantRejectsBadInput(t *testing.T) {
	c := newTestClient(t, &fakeSTS{}, Config{})
	for _, tt := range []struct{ account, role, region string }{
		{"4444", tenantRole, "us-east-1"},
		{tenantAccount, tenantRole, "mars-1"},
		{tenantAccount, tenantRoleARN("777788889999"), "us-east-1"}, // role of another account
	} {
		if _, err := c.Tenant(tt.account, tt.role, tt.region); !errors.Is(err, ErrInvalid) {
			t.Errorf("Tenant(%q, %q, %q) = %v, want ErrInvalid", tt.account, tt.role, tt.region, err)
		}
	}
}

func TestConfigValidation(t *testing.T) {
	if _, err := NewFromConfig(awssdk.Config{}, Config{SessionName: "has space"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad session name: %v", err)
	}
}

func TestVerifyAccess(t *testing.T) {
	tests := []struct {
		name string
		sts  *fakeSTS
		want error
	}{
		{"role in the tenant account", &fakeSTS{callerAccount: tenantAccount}, nil},
		{"role in another account", &fakeSTS{callerAccount: "999999999999"}, ErrAccessDenied},
		{"role cannot be assumed", &fakeSTS{denyAll: true}, ErrAccessDenied},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tenant, err := newTestClient(t, tt.sts, Config{}).Tenant(tenantAccount, tenantRole, "us-east-1")
			if err != nil {
				t.Fatal(err)
			}
			if err := tenant.VerifyAccess(t.Context()); !errors.Is(err, tt.want) {
				t.Fatalf("VerifyAccess = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestTenantWithAmbientCredentials(t *testing.T) {
	sts := &fakeSTS{callerAccount: tenantAccount}
	tenant, err := newTestClient(t, sts, Config{}).Tenant(tenantAccount, "", "us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := tenant.VerifyAccess(t.Context()); err != nil {
		t.Fatalf("VerifyAccess: %v", err)
	}
	if n := len(sts.recorded()); n != 0 {
		t.Errorf("AssumeRole calls = %d, want none without a role", n)
	}
}

func TestTenantWithStaticKeys(t *testing.T) {
	keys := &StaticCredentials{AccessKeyID: "AKIDSTATIC", SecretAccessKey: "secret"}

	// Keys alone: no AssumeRole, calls signed with the keys.
	sts := &fakeSTS{callerAccount: tenantAccount}
	tenant, err := newTestClient(t, sts, Config{}).TenantFor(Access{AccountID: tenantAccount, Static: keys}, "us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := tenant.VerifyAccess(t.Context()); err != nil || len(sts.recorded()) != 0 {
		t.Fatalf("VerifyAccess = %v, AssumeRole calls = %d; want ok and none", err, len(sts.recorded()))
	}

	// Keys plus a role: the role is assumed with the keys.
	sts = &fakeSTS{callerAccount: tenantAccount}
	c := newTestClient(t, sts, Config{})
	tenant, err = c.TenantFor(Access{AccountID: tenantAccount, Static: keys, RoleARN: tenantRole}, "us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := tenant.VerifyAccess(t.Context()); err != nil {
		t.Fatal(err)
	}
	if calls := sts.recorded(); len(calls) != 1 || calls[0].signedBy != "AKIDSTATIC" || calls[0].role != tenantRole {
		t.Errorf("AssumeRole calls = %+v; want one, signed with the static key", calls)
	}

	if _, err := c.TenantFor(Access{AccountID: tenantAccount, Static: &StaticCredentials{AccessKeyID: "x"}}, "us-east-1"); !errors.Is(err, ErrInvalid) {
		t.Errorf("keys without a secret = %v, want ErrInvalid", err)
	}
}

func TestDiscoverNetworkRefusesOtherAccounts(t *testing.T) {
	// Environment keys of another account: refused before any EC2 call.
	sts := &fakeSTS{callerAccount: "999999999999"}
	keys := &StaticCredentials{AccessKeyID: "AKIDOTHER", SecretAccessKey: "s"}
	_, err := newTestClient(t, sts, Config{}).DiscoverNetwork(t.Context(),
		Access{AccountID: tenantAccount, Static: keys}, "us-east-1", "vpc-0123456789abcdef0", []string{"subnet-0123456789abcdef0"})
	if !errors.Is(err, ErrAccessDenied) || !strings.Contains(err.Error(), "999999999999") {
		t.Fatalf("DiscoverNetwork = %v, want ErrAccessDenied naming the other account", err)
	}
}
