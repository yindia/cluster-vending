package cli

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// AdminFreeze calls AdminService.Freeze: no automatic change starts
// anywhere until admin unfreeze.
func AdminFreeze(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	res, err := call(ctx, &cloudv1.FreezeRequest{Reason: o.Reason}, c.Admin.Freeze)
	if err != nil || !isTable(o.Output) {
		return res, err
	}
	_, err = fmt.Fprintf(o.Out, "the platform is frozen: %d clusters and their networks start no automatic change "+
		"(Kubernetes upgrades, component and add-on upgrades, code changes) until admin unfreeze\n",
		res.(*cloudv1.FreezeResponse).GetClusters())
	return nil, err
}

// AdminUnfreeze calls AdminService.Unfreeze: lifts the freeze, or with
// --cluster or --network lets that one through.
func AdminUnfreeze(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if o.Cluster != "" && o.PauseNetwork != "" {
		return nil, errors.New("--cluster or --network, not both")
	}
	res, err := call(ctx, &cloudv1.UnfreezeRequest{ClusterId: o.Cluster, NetworkId: o.PauseNetwork}, c.Admin.Unfreeze)
	if err != nil || !isTable(o.Output) {
		return res, err
	}
	_, err = fmt.Fprintln(o.Out, res.(*cloudv1.UnfreezeResponse).GetNote())
	return nil, err
}
