package cli

import (
	"context"
	"fmt"
	"io"

	"google.golang.org/protobuf/proto"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

var retryHeader = []string{"NAME", "KIND", "ACTION", "MESSAGE"}

// ClusterRetry calls ClusterService.RetryCluster: try again what failed on
// the cluster --id (or only the --component ones, and --upgrade).
func ClusterRetry(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if err := o.requireTenant(); err != nil {
		return nil, err
	}
	return call(ctx, &cloudv1.RetryClusterRequest{TenantId: o.Tenant, ClusterId: o.ID, Components: o.RetryComponents,
		Upgrade: o.RetryUpgrade}, c.Cluster.RetryCluster)
}

// printRetry is what a retry does, one row per item.
func printRetry(w io.Writer, items []*cloudv1.RetryItem) error {
	if len(items) == 0 {
		_, err := fmt.Fprintln(w, "nothing has failed on the cluster: nothing to retry")
		return err
	}
	rows := make([][]string, 0, len(items))
	for _, it := range items {
		rows = append(rows, []string{it.GetName(), word(it.GetKind().String(), "RETRY_ITEM_KIND_"),
			word(it.GetAction().String(), "RETRY_ACTION_"), orNone(oneLine(it.GetMessage()))})
	}
	return table(w, retryHeader, rows)
}
