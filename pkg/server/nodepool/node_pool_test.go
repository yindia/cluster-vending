package nodepool

import (
	"context"
	"testing"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

func TestEveryRPCReturnsEmpty(t *testing.T) {
	ctx := context.Background()
	p := NewNodePoolService(service.Deps{})
	calls := map[string]func() error{
		"CreateNodePool": func() error { _, err := p.CreateNodePool(ctx, &cloudv1.CreateNodePoolRequest{}); return err },
		"GetNodePool":    func() error { _, err := p.GetNodePool(ctx, &cloudv1.GetNodePoolRequest{}); return err },
		"ListNodePools":  func() error { _, err := p.ListNodePools(ctx, &cloudv1.ListNodePoolsRequest{}); return err },
		"UpdateNodePool": func() error { _, err := p.UpdateNodePool(ctx, &cloudv1.UpdateNodePoolRequest{}); return err },
		"DeleteNodePool": func() error { _, err := p.DeleteNodePool(ctx, &cloudv1.DeleteNodePoolRequest{}); return err },
	}
	for name, call := range calls {
		if err := call(); err != nil {
			t.Errorf("%s: %v, want an empty response", name, err)
		}
	}
}
