package cluster

import (
	"fmt"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"

	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	reconcilecluster "github.com/yindia/cluster-vending/pkg/reconcile/cluster"
)

func requestIDReused(rid string) error {
	return connect.NewError(connect.CodeInvalidArgument, fmt.Sprintf(
		"request_id %s was already used for a different request (another operation or another payload)", rid))
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
