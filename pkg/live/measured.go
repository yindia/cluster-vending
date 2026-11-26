package live

import (
	"context"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/telemetry"
)

// Measured is c with every call measured by m (the API server's
// cluster_vending_live_client_* metrics).
func Measured(c Client, m *telemetry.LiveClient) Client {
	if m == nil {
		return c
	}
	return measured{c: c, m: m}
}

type measured struct {
	c Client
	m *telemetry.LiveClient
}

func (m measured) ListNodeGroupNodes(ctx context.Context, req *cloudv1.ListNodeGroupNodesRequest) (*cloudv1.ListNodeGroupNodesResponse, error) {
	done := m.m.Start("ListNodeGroupNodes")
	res, err := m.c.ListNodeGroupNodes(ctx, req)
	done(err)
	return res, err
}

func (m measured) GetNodeResources(ctx context.Context, req *cloudv1.GetNodeResourcesRequest) (*cloudv1.GetNodeResourcesResponse, error) {
	done := m.m.Start("GetNodeResources")
	res, err := m.c.GetNodeResources(ctx, req)
	done(err)
	return res, err
}
