package network

import (
	"testing"

	"google.golang.org/protobuf/proto"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// as_requested returns the network's spec as the client sent it, not the
// resolved one.
func TestGetNetworkAsRequested(t *testing.T) {
	f := newFixture(t)
	req := f.request("asked", nil)
	n := f.create(t, "asked", nil)

	for _, live := range []bool{false, true} {
		res, err := f.svc.GetNetwork(t.Context(), &cloudv1.GetNetworkRequest{TenantId: tenant, NetworkId: n.GetId(), AsRequested: true, Live: live})

		if err != nil {
			t.Fatal(err)
		}
		if !res.GetAsRequested() || !proto.Equal(res.GetNetwork().GetSpec(), req.GetNetwork().GetSpec()) {
			t.Errorf("live %v: as_requested %v, spec %v\nwant the request's %v", live, res.GetAsRequested(),
				res.GetNetwork().GetSpec(), req.GetNetwork().GetSpec())
		}
	}
}
