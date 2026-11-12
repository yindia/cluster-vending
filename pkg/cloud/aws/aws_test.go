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
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
)

const (
	tenantAccount = "444455556666"
	tenantRole    = "arn:aws:iam::444455556666:role/cluster-vending-provisioner"
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

// envClient is New as the controller calls it, with the SDK's default
// chain reading keys from the environment (local testing), every AWS call
// sent to the fake.
func envClient(t *testing.T, sts http.Handler, cfg Config) *Client {
	t.Helper()
	srv := httptest.NewServer(sts)
	t.Cleanup(srv.Close)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDENV")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	t.Setenv("AWS_SESSION_TOKEN", "")
	// Nothing of the machine the test runs on.
	t.Setenv("AWS_CONFIG_FILE", "/dev/null")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "/dev/null")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	c, err := New(t.Context(), cfg, awsconfig.WithRegion("us-east-1"), awsconfig.WithBaseEndpoint(srv.URL),
		awsconfig.WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// Keys in the controller's environment are its own credentials, not a way
// around the tenant role: the role is assumed with them, and the calls
// are the role's.
func TestEnvironmentKeysAssumeTheTenantRole(t *testing.T) {
	sts := &fakeSTS{callerAccount: tenantAccount}
	tenant, err := envClient(t, sts, Config{}).TenantFor(Access{AccountID: tenantAccount, RoleARN: tenantRole}, "us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := tenant.VerifyAccess(t.Context()); err != nil {
		t.Fatalf("VerifyAccess: %v", err)
	}
	want := []stsCall{{role: tenantRole, session: DefaultSessionName, signedBy: "AKIDENV"}}
	if calls := sts.recorded(); fmt.Sprint(calls) != fmt.Sprint(want) {
		t.Errorf("STS calls = %+v, want the tenant role assumed with the environment's keys: %+v", calls, want)
	}
}

// There is no way to a tenant's account around its role: an access
// without one is refused before any call, whatever the client's own
// credentials are.
func TestTenantAccountIsOnlyReachedThroughItsRole(t *testing.T) {
	sts := &fakeSTS{callerAccount: tenantAccount}
	for name, c := range map[string]*Client{"own credentials": newTestClient(t, sts, Config{}), "environment keys": envClient(t, sts, Config{})} {
		if _, err := c.TenantFor(Access{AccountID: tenantAccount}, "us-east-1"); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s, no role: %v, want ErrInvalid", name, err)
		}
		if _, err := c.DiscoverNetwork(t.Context(), Access{AccountID: tenantAccount}, "us-east-1", "vpc-0123456789abcdef0", nil); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s, DiscoverNetwork without a role: %v, want ErrInvalid", name, err)
		}
	}
	if n := len(sts.recorded()); n != 0 {
		t.Errorf("%d STS calls for refused accesses, want none", n)
	}
}

// Credentials that land in another account are refused before any EC2
// call: the role that was assumed is not the tenant's.
func TestDiscoverNetworkRefusesOtherAccounts(t *testing.T) {
	sts := &fakeSTS{callerAccount: "999999999999"}
	_, err := newTestClient(t, sts, Config{}).DiscoverNetwork(t.Context(),
		Access{AccountID: tenantAccount, RoleARN: tenantRole}, "us-east-1", "vpc-0123456789abcdef0", []string{"subnet-0123456789abcdef0"})
	if !errors.Is(err, ErrAccessDenied) || !strings.Contains(err.Error(), "999999999999") {
		t.Fatalf("DiscoverNetwork = %v, want ErrAccessDenied naming the other account", err)
	}
}
