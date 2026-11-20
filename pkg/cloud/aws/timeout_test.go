package aws

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// hangingHTTP is an HTTP client that never answers: it returns when the
// request's context is done.
type hangingHTTP struct{}

func (hangingHTTP) Do(req *http.Request) (*http.Response, error) {
	<-req.Context().Done()
	return nil, req.Context().Err()
}

func hangingClient(t *testing.T, timeout time.Duration) *Client {
	t.Helper()
	base := awssdk.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("AKIA", "secret", ""),
		HTTPClient:  hangingHTTP{},
		// One attempt: the bound under test is the call's, not the retries'.
		RetryMaxAttempts: 1,
	}
	c, err := NewFromConfig(base, Config{CallTimeout: timeout})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// A call that hangs returns a timeout within the bound, and the access
// check's classification treats it as transient, not as a verdict.
func TestSDKCallsAreBoundedByTheCallTimeout(t *testing.T) {
	const bound = 100 * time.Millisecond
	c := hangingClient(t, bound)
	tenant, err := c.TenantFor(Access{AccountID: tenantAccount, Direct: true}, "us-east-1")
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	err = tenant.VerifyAccess(t.Context()) // the caller's context has no deadline
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want a deadline error", err)
	}
	if elapsed > 20*bound {
		t.Errorf("returned after %s, want about %s", elapsed, bound)
	}
	if got := callReason(err); got != ReasonUnavailable {
		t.Errorf("reason = %s, want %s: a hung call is retryable, not a denial", got, ReasonUnavailable)
	}
}

func TestEveryServiceClientIsBounded(t *testing.T) {
	c := hangingClient(t, 100*time.Millisecond)
	a := Access{AccountID: tenantAccount, Direct: true}
	start := time.Now()
	if _, err := c.EKSClusterExists(t.Context(), a, "us-east-1", "prod"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("EKSClusterExists: %v", err)
	}
	if _, err := c.ResolveEKSVersions(t.Context(), a, "us-east-1", VersionRequest{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("ResolveEKSVersions: %v", err)
	}
	if _, err := c.DiscoverNetwork(t.Context(), a, "us-east-1", "vpc-1", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("DiscoverNetwork: %v", err)
	}
	if _, err := c.DiscoverCluster(t.Context(), a, "us-east-1", "prod", ""); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("DiscoverCluster: %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("took %s", time.Since(start))
	}
}

func TestCallTimeoutDefault(t *testing.T) {
	c, err := NewFromConfig(awssdk.Config{}, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if c.cfg.CallTimeout != DefaultCallTimeout || DefaultCallTimeout != 20*time.Second {
		t.Errorf("CallTimeout = %s, want %s", c.cfg.CallTimeout, DefaultCallTimeout)
	}
	if _, err := NewFromConfig(awssdk.Config{}, Config{CallTimeout: -time.Second}); !errors.Is(err, ErrInvalid) {
		t.Errorf("a negative timeout: %v, want ErrInvalid", err)
	}
}

// The SDK's own configuration has bounded retries and transport timeouts.
func TestDefaultLoadOptionsBoundRetriesAndTransport(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIA")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	t.Setenv("AWS_CONFIG_FILE", "/dev/null")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "/dev/null")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")

	cfg, err := awsconfig.LoadDefaultConfig(t.Context(), defaultLoadOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Retryer().MaxAttempts(); got != MaxAttempts {
		t.Errorf("retryer MaxAttempts = %d", got)
	}
	client, ok := cfg.HTTPClient.(*awshttp.BuildableClient)
	if !ok {
		t.Fatalf("HTTPClient is %T, want a buildable client with timeouts", cfg.HTTPClient)
	}
	if client.GetTimeout() != HTTPTimeout {
		t.Errorf("http timeout = %s, want %s", client.GetTimeout(), HTTPTimeout)
	}
}

// S3 calls are not cut by the call timeout (a body is read after the call
// returns): they end with the caller's context, and the HTTP timeouts
// bound them.
func TestS3IsBoundedByTheCallersContext(t *testing.T) {
	c := hangingClient(t, 50*time.Millisecond)
	client, err := c.S3(Access{AccountID: tenantAccount, Direct: true}, "us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err = client.ListBuckets(ctx, &s3.ListBucketsInput{})

	if err == nil || time.Since(start) < 250*time.Millisecond {
		t.Errorf("err = %v after %s: the call timeout must not apply to S3", err, time.Since(start))
	}
}
