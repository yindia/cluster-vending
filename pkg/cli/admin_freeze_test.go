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

// admin freeze of a channel's tier (or a tenant), until a date, sends that
// target; unfreeze names the same target, with one cluster let through.
func TestAdminFreezeOfATarget(t *testing.T) {
	svc := &freezeRecorder{}
	var out bytes.Buffer
	opts := Options{Channel: "beta", Tier: "0", Until: "2026-12-31", Reason: "quarter end"}

	if err := Run(t.Context(), AdminFreeze, Clients{Admin: svc}, opts, &out, OutputTable); err != nil {
		t.Fatal(err)
	}
	let := opts
	let.Cluster = "0190a000-0000-7000-8000-0000000000a0"
	if err := Run(t.Context(), AdminUnfreeze, Clients{Admin: svc}, let, &bytes.Buffer{}, OutputTable); err != nil {
		t.Fatal(err)
	}
	if err := Run(t.Context(), AdminFreeze, Clients{Admin: svc}, Options{Tenant: "acme"}, &bytes.Buffer{}, OutputTable); err != nil {
		t.Fatal(err)
	}

	f, u := svc.froze[0], svc.unfroze[0]
	if f.GetChannel() != cloudv1.ReleaseChannel_RELEASE_CHANNEL_BETA || f.GetTier() != cloudv1.ClusterTier_CLUSTER_TIER_0 ||
		!f.GetUntil().AsTime().Equal(time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)) || f.GetReason() != "quarter end" {
		t.Errorf("freeze request %v", f)
	}
	if !strings.HasPrefix(out.String(), "channel beta tier 0 is frozen: 12 clusters") {
		t.Errorf("output %q", out.String())
	}
	if u.GetChannel() != cloudv1.ReleaseChannel_RELEASE_CHANNEL_BETA || u.GetTier() != cloudv1.ClusterTier_CLUSTER_TIER_0 || u.GetClusterId() != let.Cluster {
		t.Errorf("unfreeze request %v", u)
	}
	if svc.froze[1].GetTenantId() != "acme" {
		t.Errorf("tenant freeze %v", svc.froze[1])
	}
}

// What admin freeze refuses before sending anything.
func TestAdminFreezeRefuses(t *testing.T) {
	tests := []struct {
		name string
		opts Options
		want string
	}{
		{"a channel and a tenant", Options{Tenant: "acme", Channel: "beta"}, "not both"},
		{"a tier without a channel", Options{Tier: "0"}, "--tier needs --channel"},
		{"an unknown channel", Options{Channel: "gamma"}, "alpha, beta or stable"},
		{"a cluster", Options{Cluster: "c"}, "--cluster"},
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
