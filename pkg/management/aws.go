package management

import (
	"context"

	awscloud "github.com/yindia/cluster-vending/pkg/cloud/aws"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// WithAWS is d with what the reconcilers look the cloud up with: the
// discovery of a VPC or EKS cluster that is imported, the check of an EKS
// cluster name, the versions EKS offers, and the way into a cluster's
// Kubernetes API (to scan it for deprecated API versions). Only what reconciles needs
// it (the controller, or the server while it still runs the background
// loop); the API handlers make no AWS call.
//
// The lookups start from the process's own credentials, as the AWS SDK's
// default chain finds them (keys in the environment for local testing,
// IRSA in production), then always assume the tenant role of the account
// they read (reconcile.Deps.AWSAccess).
func WithAWS(ctx context.Context, d reconcile.Deps) (reconcile.Deps, error) {
	lookups, err := awscloud.New(ctx, awscloud.Config{})
	if err != nil {
		return reconcile.Deps{}, err
	}
	d.AWSDiscovery, d.AWSClusterDiscovery, d.AWSClusters, d.AWSClusterAPI = lookups, lookups, lookups, lookups
	return d, nil
}
