package cluster

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"

	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	reconcilecluster "github.com/yindia/cluster-vending/pkg/reconcile/cluster"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

func requestIDReused(rid string) error {
	return connect.NewError(connect.CodeInvalidArgument, fmt.Sprintf(
		"request_id %s was already used for a different request (another operation or another payload)", rid))
}

// createdBefore is the cluster a create with req's request_id already
// stored, when it is the same request; requestIDReused when it is another.
// (nil, nil): no request_id, or none stored.
func (s *ClusterService) createdBefore(ctx context.Context, req *cloudv1.CreateClusterRequest) (*model.Cluster, error) {
	rid := req.GetRequestId()
	if rid == "" {
		return nil, nil
	}
	row, err := s.deps.Store.Clusters().GetByCreateRequest(ctx, req.GetTenantId(), rid)
	if errors.Is(err, repository.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, service.ToConnect(err)
	}
	if !createMatches(row, req) {
		return nil, requestIDReused(rid)
	}
	return row, nil
}

// requestWinner resolves an insert that failed with AlreadyExists on a
// request_id: the claim of the request ID is held by a create that ran
// concurrently. Its cluster is the idempotent result when it is the same
// request (matches), else the ID was reused for another one
// (requestIDReused). Neither (nil, nil): the failure is not about the request
// ID (or its winner is gone), the caller maps err itself.
func (s *ClusterService) requestWinner(ctx context.Context, row *model.Cluster, err error,
	matches func(*model.Cluster) bool) (*model.Cluster, error) {
	if !errors.Is(err, repository.ErrAlreadyExists) || row.CreateRequestID == nil {
		return nil, nil
	}
	won, gerr := s.deps.Store.Clusters().GetByCreateRequest(ctx, row.TenantID, *row.CreateRequestID)
	if errors.Is(gerr, repository.ErrNotFound) {
		return nil, nil // the winner was rolled back
	}
	if gerr != nil {
		return nil, service.ToConnect(gerr)
	}
	if won.ID == row.ID {
		return nil, nil
	}
	if !matches(won) {
		return nil, requestIDReused(*row.CreateRequestID)
	}
	return won, nil
}

// createMatches reports whether row, found by the request ID of req, is the
// cluster that very create stored: not an import, same name and network and,
// while the stored input is still as sent (no update since), the same payload.
func createMatches(row *model.Cluster, req *cloudv1.CreateClusterRequest) bool {
	in := req.GetCluster()
	if row.Imported || row.Name != in.GetName() || row.NetworkID != in.GetSpec().GetNetwork().GetNetworkId() {
		return false
	}
	if row.Generation > 1 || len(row.Input) == 0 {
		return true // edited since: only the immutable identity is comparable
	}
	stored := &cloudv1.Cluster{}
	if err := reconcilecluster.Lenient.Unmarshal(row.Input, stored); err != nil {
		return false
	}
	return proto.Equal(stored, in)
}

// importMatches is createMatches for ImportCluster: the stored input of an
// import is its request.
func importMatches(row *model.Cluster, req *cloudv1.ImportClusterRequest) bool {
	if !row.Imported || row.Name != req.GetName() || row.NetworkID != req.GetNetworkId() {
		return false
	}
	stored := &cloudv1.ImportClusterRequest{}
	if err := reconcilecluster.Lenient.Unmarshal(row.Input, stored); err != nil {
		return false
	}
	return proto.Equal(stored, req)
}
