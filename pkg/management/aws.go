package management

import (
	"context"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"

	awscloud "github.com/yindia/cluster-vending/pkg/cloud/aws"
	"github.com/yindia/cluster-vending/pkg/config"
	"github.com/yindia/cluster-vending/pkg/reconcile"
	"github.com/yindia/cluster-vending/pkg/results"
)

// WithAWS is d with what the reconcilers look the cloud up with: the
// discovery of a VPC or EKS cluster that is imported, the check of an EKS
// cluster name, the versions EKS offers, and the access check of an
// account. Only what reconciles needs
// it (the controller, or the server while it still runs the background
// loop); the API handlers make no AWS call.
//
// The lookups start from the process's own credentials, as the AWS SDK's
// default chain finds them (keys in the environment for local testing,
// IRSA in production), then always assume the tenant role of the account
// they read (reconcile.Deps.AWSAccess).
func WithAWS(ctx context.Context, d reconcile.Deps) (reconcile.Deps, error) {
	lookups, err := NewAWSClient(ctx, config.AWSAccess{}, "")
	if err != nil {
		return reconcile.Deps{}, err
	}
	return WithAWSClient(d, lookups), nil
}

// WithAWSClient is d with the reconcilers' lookups made with c.
func WithAWSClient(d reconcile.Deps, c *awscloud.Client) reconcile.Deps {
	d.AWSDiscovery, d.AWSClusterDiscovery, d.AWSClusters, d.AWSAccessCheck = c, c, c, c
	d.ResultsS3 = func(a awscloud.Access, region string) (results.S3API, error) {
		client, err := c.S3(a, region)
		if err != nil {
			return nil, err
		}
		return client, nil
	}
	return d
}

// NewAWSClient is the client that reaches tenant accounts, for the
// controller and for the API server alike (pkg/cloud/aws): the process's own
// credentials from the AWS SDK's default chain (IRSA in production; keys in
// the environment for local testing, never a setting), then the source role
// of the settings when there is one, then always the tenant role of the
// account. sessionPolicy, when not empty, limits every tenant-role session to
// what it allows (the server passes results.ReadOnlySessionPolicy; the
// controller none). It makes no AWS call.
func NewAWSClient(ctx context.Context, a config.AWSAccess, sessionPolicy string) (*awscloud.Client, error) {
	var load []func(*awsconfig.LoadOptions) error
	if a.Region != "" {
		load = append(load, awsconfig.WithRegion(a.Region))
	}
	return awscloud.New(ctx, awscloud.Config{SourceRoleARN: a.SourceRoleARN, ExternalID: a.ExternalID, SessionPolicy: sessionPolicy}, load...)
}
