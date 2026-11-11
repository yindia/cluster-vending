// Package service holds what every cloud.v1 service implementation shares:
// its dependencies and the mapping of errors to Connect codes. The services
// themselves live in pkg/server/tenant, network and cluster.
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"buf.build/go/protovalidate"
	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"
	"sigs.k8s.io/controller-runtime/pkg/client"

	awscloud "github.com/yindia/cluster-vending/pkg/cloud/aws"
	"github.com/yindia/cluster-vending/pkg/config"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
)

// Deps are what the services work with: the database (the source of
// truth), the management cluster where Crossplane managed resources are
// created, and how tenant cloud accounts are reached.
type Deps struct {
	Store repository.Store
	// Kube reads from the informer cache (the server's manager) and writes
	// to the API server; tests use a plain client.
	Kube client.Client
	// KubeReader reads from the API server, uncached: for objects the cache
	// does not have yet (just applied) or does not hold (no managed-by
	// label, managedFields). Nil: reads use Kube only.
	KubeReader client.Reader
	AWS        config.AWS
	// AWSDiscovery makes the server's own read-only AWS lookups
	// (ImportNetwork).
	AWSDiscovery awscloud.NetworkDiscoverer
	// AWSClusters checks EKS cluster names before CreateCluster; nil skips
	// the check (tests).
	AWSClusters awscloud.ClusterLookup
	Logger      *slog.Logger
}

// Validate checks a request against its protovalidate rules (the
// buf.validate options in idl/), as INVALID_ARGUMENT.
func Validate(req proto.Message) error {
	if err := protovalidate.Validate(req); err != nil {
		return connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	return nil
}

// Unimplemented is the answer of every RPC until it is written.
func Unimplemented(method string) error {
	return connect.NewError(connect.CodeUnimplemented, method+" is not implemented yet")
}

// AWSTenantRoleARN returns the IAM role the platform assumes in one of a
// tenant's bootstrapped AWS accounts, by bootstrap ID (CloudAccount.id, as
// in NetworkSpec.bootstrap_id): the role stored at bootstrap.
func (d Deps) AWSTenantRoleARN(ctx context.Context, tenantID, cloudAccountID string) (string, error) {
	account, err := d.Store.Tenants().GetCloudAccountByID(ctx, tenantID, cloudAccountID)
	if err != nil {
		return "", ToConnect(fmt.Errorf("account %s of tenant %s: %w", cloudAccountID, tenantID, err))
	}
	if account.Provider != model.ProviderAWS {
		return "", connect.NewError(connect.CodeInvalidArgument,
			fmt.Sprintf("account %s is a %s account, not aws", cloudAccountID, account.Provider))
	}
	if account.Identity == "" {
		return "", connect.NewError(connect.CodeFailedPrecondition,
			fmt.Sprintf("account %s has no tenant role yet; bootstrap it again", cloudAccountID))
	}
	return account.Identity, nil
}

// ToConnect maps repository errors to Connect codes. Unknown errors become
// INTERNAL without their text, which may hold database details.
func ToConnect(err error) error {
	var codeErr *connect.Error
	switch {
	case err == nil:
		return nil
	case errors.As(err, &codeErr):
		return err
	case errors.Is(err, repository.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, err.Error())
	case errors.Is(err, repository.ErrAlreadyExists):
		return connect.NewError(connect.CodeAlreadyExists, err.Error())
	case errors.Is(err, repository.ErrConflict):
		return connect.NewError(connect.CodeAborted, err.Error())
	case errors.Is(err, repository.ErrInUse):
		return connect.NewError(connect.CodeFailedPrecondition, err.Error())
	case errors.Is(err, repository.ErrInvalid):
		return connect.NewError(connect.CodeInvalidArgument, err.Error())
	case errors.Is(err, context.Canceled):
		return connect.NewError(connect.CodeCanceled, err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		return connect.NewError(connect.CodeDeadlineExceeded, err.Error())
	default:
		return connect.NewError(connect.CodeInternal, "internal error")
	}
}
