package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1/cloudv1connect"
)

type freezeRecorder struct {
	cloudv1connect.UnimplementedAdminServiceHandler
	froze   []*cloudv1.FreezeRequest
	unfroze []*cloudv1.UnfreezeRequest
}

func (f *freezeRecorder) Freeze(_ context.Context, req *cloudv1.FreezeRequest) (*cloudv1.FreezeResponse, error) {
	f.froze = append(f.froze, req)
	return &cloudv1.FreezeResponse{Clusters: 12}, nil
}

func (f *freezeRecorder) Unfreeze(_ context.Context, req *cloudv1.UnfreezeRequest) (*cloudv1.UnfreezeResponse, error) {
	f.unfroze = append(f.unfroze, req)
	return &cloudv1.UnfreezeResponse{Note: "the platform is not frozen any more"}, nil
}

// admin freeze and unfreeze: the reason, and one cluster or network let
// through; their table says what happened.
func TestAdminFreeze(t *testing.T) {
	svc := &freezeRecorder{}
	var out bytes.Buffer

	if err := Run(t.Context(), AdminFreeze, Clients{Admin: svc}, Options{Reason: "provider upgrade"}, &out, OutputTable); err != nil {
		t.Fatal(err)
	}
	if svc.froze[0].GetReason() != "provider upgrade" || !strings.Contains(out.String(), "the platform is frozen: 12 clusters") {
		t.Errorf("freeze %v, output %q", svc.froze[0], out.String())
	}
	for _, o := range []Options{{}, {Cluster: "0190a000-0000-7000-8000-0000000000a0"}, {PauseNetwork: "0190a000-0000-7000-8000-0000000000c0"}} {
		out.Reset()
		if err := Run(t.Context(), AdminUnfreeze, Clients{Admin: svc}, o, &out, OutputTable); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "not frozen any more") {
			t.Errorf("output %q", out.String())
		}
	}
	if svc.unfroze[1].GetClusterId() == "" || svc.unfroze[2].GetNetworkId() == "" || svc.unfroze[0].GetClusterId() != "" {
		t.Errorf("unfreeze requests %v", svc.unfroze)
	}
	if err := Run(t.Context(), AdminUnfreeze, Clients{Admin: svc}, Options{Cluster: "a", PauseNetwork: "b"}, &out, OutputTable); err == nil ||
		!strings.Contains(err.Error(), "not both") {
		t.Errorf("--cluster and --network: %v", err)
	}
}
