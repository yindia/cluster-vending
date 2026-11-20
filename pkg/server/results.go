package server

import (
	"context"

	"github.com/yindia/cluster-vending/pkg/config"
	"github.com/yindia/cluster-vending/pkg/management"
	"github.com/yindia/cluster-vending/pkg/results"
	"github.com/yindia/cluster-vending/pkg/server/cluster"
)

// NewResultsStore is what the API server reads the clusters' scan results
// with (WithResults): S3 clients of the clusters' accounts, called through
// the tenant role of each (docs/architecture.md#results-in-s3).
//
// Its credentials are the AWS SDK's default chain (IRSA in production, keys
// in the environment for local testing, never a setting), then the source
// role of the settings when there is one, then the account's tenant role,
// whose session is limited to reading result objects
// (results.ReadOnlySessionPolicy): the server cannot create or change
// anything in a tenant's account, whatever the role allows. It makes no AWS
// call; a server without credentials answers with coverage notices.
func NewResultsStore(ctx context.Context, access config.AWSAccess) (cluster.ResultStore, error) {
	client, err := management.NewAWSClient(ctx, access, results.ReadOnlySessionPolicy)
	if err != nil {
		return nil, err
	}
	return cluster.NewAWSResults(client), nil
}
