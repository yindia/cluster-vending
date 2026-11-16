package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1/cloudv1connect"
)

type freezeRecorder struct {
	cloudv1connect.UnimplementedAdminServiceHandler
	froze          []*cloudv1.FreezeRequest
	unfroze        []*cloudv1.UnfreezeRequest
	statusReads    int
	status         *cloudv1.GetFreezeResponse
	statusRequests []*cloudv1.GetFreezeRequest
}

func (f *freezeRecorder) Freeze(_ context.Context, req *cloudv1.FreezeRequest) (*cloudv1.FreezeResponse, error) {
	f.froze = append(f.froze, req)
	return &cloudv1.FreezeResponse{Clusters: 12, Phase: "Freezing", Message: "waiting for controller confirmation"}, nil
}

func (f *freezeRecorder) Unfreeze(_ context.Context, req *cloudv1.UnfreezeRequest) (*cloudv1.UnfreezeResponse, error) {
	f.unfroze = append(f.unfroze, req)
	return &cloudv1.UnfreezeResponse{Note: "the platform is not frozen any more", Status: &cloudv1.FreezeStatus{Target: req.Target, Phase: cloudv1.FreezePhase_FREEZE_PHASE_UNFROZEN}}, nil
}

// admin freeze and unfreeze: the reason, and one cluster or network let
// through; their table says what happened.
func TestAdminFreeze(t *testing.T) {
	svc := &freezeRecorder{}
	var out bytes.Buffer

	if err := Run(t.Context(), AdminFreeze, Clients{Admin: svc}, Options{Reason: "provider upgrade"}, &out, OutputTable); err != nil {
		t.Fatal(err)
	}
	if svc.froze[0].GetReason() != "provider upgrade" || (!strings.Contains(out.String(), "Freezing") || !strings.Contains(out.String(), "12")) {
		t.Errorf("freeze %v, output %q", svc.froze[0], out.String())
	}
	for _, o := range []Options{{}, {Cluster: "0190a000-0000-7000-8000-0000000000a0"}, {PauseNetwork: "0190a000-0000-7000-8000-0000000000c0"}} {
		out.Reset()
		if err := Run(t.Context(), AdminUnfreeze, Clients{Admin: svc}, o, &out, OutputTable); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "Unfrozen") {
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

func TestAdminFreezeUntil(t *testing.T) {
	svc := &freezeRecorder{}
	if err := Run(t.Context(), AdminFreeze, Clients{Admin: svc}, Options{Until: "2026-12-31"}, &bytes.Buffer{}, OutputTable); err != nil {
		t.Fatal(err)
	}
	if !svc.froze[0].GetUntil().AsTime().Equal(time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)) {
		t.Fatal(svc.froze[0])
	}
}

// What admin freeze refuses before sending anything.
func TestAdminFreezeRefuses(t *testing.T) {
	tests := []struct {
		name string
		opts Options
		want string
	}{
		{"a channel and a tenant", Options{Tenant: "acme", Channel: "beta"}, "not supported"},
		{"a tier without a channel", Options{Tier: "0"}, "not supported"},
		{"an unknown channel", Options{Channel: "gamma"}, "not supported"},
		{"an invalid cluster ID", Options{Cluster: "c"}, "UUID"},
		{"an until that is no time", Options{Until: "soon"}, "--until"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := &freezeRecorder{}

			err := Run(t.Context(), AdminFreeze, Clients{Admin: svc}, tt.opts, &bytes.Buffer{}, OutputYAML)

			if err == nil || !strings.Contains(err.Error(), tt.want) || len(svc.froze) != 0 {
				t.Fatalf("err = %v (sent %d), want one saying %q", err, len(svc.froze), tt.want)
			}
		})
	}
}
func (f *freezeRecorder) GetFreeze(_ context.Context, req *cloudv1.GetFreezeRequest) (*cloudv1.GetFreezeResponse, error) {
	f.statusReads++
	f.statusRequests = append(f.statusRequests, req)
	if f.status != nil {
		return f.status, nil
	}
	return &cloudv1.GetFreezeResponse{Phase: "Frozen", TotalResources: 3, PausedResources: 3, Exceptions: []string{"canary-id"}}, nil
}
func TestAdminFreezeStatusWaitAndReset(t *testing.T) {
	svc := &freezeRecorder{}
	var out bytes.Buffer
	if err := Run(t.Context(), AdminFreeze, Clients{Admin: svc}, Options{Wait: true, ResetExceptions: true}, &out, OutputTable); err != nil {
		t.Fatal(err)
	}
	if !svc.froze[0].GetResetExceptions() || svc.statusReads != 1 {
		t.Fatalf("reset/wait: %v, %d", svc.froze, svc.statusReads)
	}
	for _, expected := range []string{"Frozen", "3/3", "canary-id"} {
		if !strings.Contains(out.String(), expected) {
			t.Fatalf("missing %s: %s", expected, out.String())
		}
	}
}

func TestAdminUnfreezeWaitUsesSelectedScope(t *testing.T) {
	for _, opts := range []Options{{Cluster: "0190a000-0000-7000-8000-000000000001"}, {PauseNetwork: "0190a000-0000-7000-8000-000000000003"}, {Tenant: "acme"}, {}} {
		svc := &freezeRecorder{status: &cloudv1.GetFreezeResponse{Phase: "Unfrozen"}}
		opts.Wait = true
		var out bytes.Buffer
		if err := Run(t.Context(), AdminUnfreeze, Clients{Admin: svc}, opts, &out, OutputTable); err != nil {
			t.Fatal(err)
		}
		if svc.statusReads != 1 || svc.statusRequests[0].GetClusterId() != opts.Cluster || svc.statusRequests[0].GetNetworkId() != opts.PauseNetwork || svc.statusRequests[0].GetTenantId() != opts.Tenant {
			t.Fatalf("wrong confirmation scope: %+v", svc.statusRequests)
		}
		if !strings.Contains(out.String(), "Unfrozen") {
			t.Fatal(out.String())
		}
	}
}

func TestAdminUnfreezeWaitRejectsSupersededRequest(t *testing.T) {
	svc := &freezeRecorder{status: &cloudv1.GetFreezeResponse{Phase: "Unfrozen", Revision: "newer-request"}}
	err := Run(t.Context(), AdminUnfreeze, Clients{Admin: svc}, Options{Wait: true}, &bytes.Buffer{}, OutputTable)
	if err == nil || !strings.Contains(err.Error(), "changed while waiting") {
		t.Fatalf("superseded unfreeze: %v", err)
	}
}

func TestAdminFreezeWaitAndJSONUseScopedTypedContract(t *testing.T) {
	id := "0190a000-0000-7000-8000-000000000001"
	target := &cloudv1.FreezeTarget{Scope: &cloudv1.FreezeTarget_ClusterId{ClusterId: id}}
	svc := &freezeRecorder{status: &cloudv1.GetFreezeResponse{Status: &cloudv1.FreezeStatus{Target: target, Phase: cloudv1.FreezePhase_FREEZE_PHASE_FROZEN, Clusters: 1, TotalResources: 2, PausedResources: 2}}}
	var out bytes.Buffer
	if err := Run(t.Context(), AdminFreeze, Clients{Admin: svc}, Options{Cluster: id, Wait: true}, &out, OutputJSON); err != nil {
		t.Fatal(err)
	}
	if len(svc.statusRequests) != 1 || svc.statusRequests[0].GetTarget().GetClusterId() != id {
		t.Fatalf("wait read another scope: %v", svc.statusRequests)
	}
	if !strings.Contains(out.String(), "FREEZE_PHASE_FROZEN") || !strings.Contains(out.String(), id) {
		t.Fatalf("typed JSON status missing: %s", out.String())
	}
	if svc.froze[0].GetTarget().GetClusterId() != id || svc.froze[0].GetClusterId() != id {
		t.Fatal("scoped legacy compatibility IDs were not mirrored")
	}
}

func TestAdminExplicitPlatformRejectsAnotherScope(t *testing.T) {
	svc := &freezeRecorder{}
	if err := Run(t.Context(), AdminFreeze, Clients{Admin: svc}, Options{Platform: true, Tenant: "acme"}, &bytes.Buffer{}, OutputTable); err == nil || len(svc.froze) > 0 {
		t.Fatalf("ambiguous platform target: %v", err)
	}
	if err := Run(t.Context(), AdminFreeze, Clients{Admin: svc}, Options{Platform: true}, &bytes.Buffer{}, OutputTable); err != nil {
		t.Fatal(err)
	}
	if !svc.froze[0].GetTarget().GetPlatform() {
		t.Fatal("platform was not explicit in request")
	}
}
