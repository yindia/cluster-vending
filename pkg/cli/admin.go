package cli

import (
	"context"

	"google.golang.org/protobuf/proto"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// AdminPause calls AdminService.PauseReconcile for --tenant, or only its
// --cluster.
func AdminPause(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if err := o.requireTenant(); err != nil {
		return nil, err
	}
	return call(ctx, &cloudv1.PauseReconcileRequest{TenantId: o.Tenant, ClusterId: o.Cluster}, c.Admin.PauseReconcile)
}

// AdminResume calls AdminService.ResumeReconcile for --tenant, or only its
// --cluster.
func AdminResume(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if err := o.requireTenant(); err != nil {
		return nil, err
	}
	return call(ctx, &cloudv1.ResumeReconcileRequest{TenantId: o.Tenant, ClusterId: o.Cluster}, c.Admin.ResumeReconcile)
}
