package cli

import (
	"context"
	"strconv"

	"google.golang.org/protobuf/proto"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// AdminPause calls AdminService.PauseReconcile for --tenant, or only its
// --cluster, with --reason: the platform stops acting on them, and the
// providers on their resources.
func AdminPause(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if err := o.requireTenant(); err != nil {
		return nil, err
	}
	res, err := call(ctx, &cloudv1.PauseReconcileRequest{TenantId: o.Tenant, ClusterId: o.Cluster, NetworkId: o.PauseNetwork,
		Reason: o.Reason}, c.Admin.PauseReconcile)
	if err != nil || !isTable(o.Output) {
		return res, err
	}
	return nil, printPause(o, "OBJECTS PAUSED", res.(*cloudv1.PauseReconcileResponse).GetResources())
}

// AdminResume calls AdminService.ResumeReconcile for --tenant, or only its
// --cluster or --network.
func AdminResume(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if err := o.requireTenant(); err != nil {
		return nil, err
	}
	res, err := call(ctx, &cloudv1.ResumeReconcileRequest{TenantId: o.Tenant, ClusterId: o.Cluster, NetworkId: o.PauseNetwork},
		c.Admin.ResumeReconcile)
	if err != nil || !isTable(o.Output) {
		return res, err
	}
	return nil, printPause(o, "OBJECTS RESUMED", res.(*cloudv1.ResumeReconcileResponse).GetResources())
}

// printPause prints what a pause or resume applied to: the scope it was
// asked at, its target and how many managed resources it covered.
func printPause(o Options, count string, resources int32) error {
	scope, target := "tenant", o.Tenant
	switch {
	case o.Cluster != "":
		scope, target = "cluster", o.Cluster
	case o.PauseNetwork != "":
		scope, target = "network", o.PauseNetwork
	}
	return table(o.Out, []string{"TENANT", "SCOPE", "TARGET", count},
		[][]string{{o.Tenant, scope, target, strconv.Itoa(int(resources))}})
}
