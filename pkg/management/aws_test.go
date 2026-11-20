package management_test

import (
	"errors"
	"testing"

	awscloud "github.com/yindia/cluster-vending/pkg/cloud/aws"
	"github.com/yindia/cluster-vending/pkg/config"
	"github.com/yindia/cluster-vending/pkg/management"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// The AWS client is made from the settings and the SDK's default chain: keys
// in the environment (local testing) are enough, nothing is called yet.
func TestNewAWSClientFromTheEnvironment(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDLOCAL")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_CONFIG_FILE", "/nonexistent")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "/nonexistent")

	c, err := management.NewAWSClient(t.Context(), config.AWSAccess{Region: "eu-west-1",
		SourceRoleARN: "arn:aws:iam::111122223333:role/src", ExternalID: "ext-1234"}, "")
	if err != nil || c == nil {
		t.Fatalf("NewAWSClient = %v, %v", c, err)
	}
	// The same client serves both uses: tenant S3 clients and the
	// reconcilers' lookups.
	if _, err := c.S3(awscloud.Access{AccountID: "444455556666", RoleARN: "arn:aws:iam::444455556666:role/r"}, "us-east-1"); err != nil {
		t.Errorf("S3: %v", err)
	}
	d := management.WithAWSClient(reconcile.Deps{}, c)
	if d.AWSDiscovery == nil || d.AWSClusterDiscovery == nil || d.AWSClusters == nil || d.AWSAccessCheck == nil {
		t.Errorf("deps = %+v, want every lookup set", d)
	}
}

func TestNewAWSClientRefusesABadSourceRole(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDLOCAL")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	t.Setenv("AWS_REGION", "us-east-1")
	_, err := management.NewAWSClient(t.Context(), config.AWSAccess{SourceRoleARN: "nope"}, "")
	if !errors.Is(err, awscloud.ErrInvalid) {
		t.Errorf("err = %v, want ErrInvalid", err)
	}
}
