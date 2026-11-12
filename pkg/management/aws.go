package management

import (
	"context"

	awscloud "github.com/yindia/cluster-vending/pkg/cloud/aws"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// WithAWS is d with what the reconcilers look the cloud up with: the
// discovery of a VPC or EKS cluster that is imported, the check of an EKS
// cluster name, and the versions EKS offers. Only what reconciles needs
// it (the controller, or the server while it still runs the background
// loop); the API handlers make no AWS call.
//
// The lookups start from the process's own credentials, as the AWS SDK's
// default chain finds them (keys in the environment for local testing,
// IRSA in production), assume sourceRoleARN when it is not empty, and
// then always the tenant role of the account they read
// (reconcile.Deps.AWSAccess).
func WithAWS(ctx context.Context, d reconcile.Deps, sourceRoleARN string) (reconcile.Deps, error) {
	lookups, err := awscloud.New(ctx, awscloud.Config{SourceRoleARN: sourceRoleARN})
	if err != nil {
		return reconcile.Deps{}, err
	}
	d.AWSDiscovery, d.AWSClusterDiscovery, d.AWSClusters = lookups, lookups, lookups
	return d, nil
}
