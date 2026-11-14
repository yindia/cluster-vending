package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// listRecorder is a pauseRecorder that lists pauses and answers a channel's
// pause with how many clusters it covers.
type listRecorder struct {
	pauseRecorder
	pauses []*cloudv1.PauseRecord
}

func (l *listRecorder) PauseReconcile(ctx context.Context, req *cloudv1.PauseReconcileRequest) (*cloudv1.PauseReconcileResponse, error) {
	_, _ = l.pauseRecorder.PauseReconcile(ctx, req)
	return &cloudv1.PauseReconcileResponse{Resources: 40, Clusters: 2, Note: "a cluster that joins it later is not paused"}, nil
}

func (l *listRecorder) ListPauses(context.Context, *cloudv1.ListPausesRequest) (*cloudv1.ListPausesResponse, error) {
	return &cloudv1.ListPausesResponse{Pauses: l.pauses}, nil
}

// admin pause by channel and tier, of upgrades only and until a time, sends
// exactly that; admin resume the same target.
func TestAdminPauseOfAChannel(t *testing.T) {
	svc := &listRecorder{}
	opts := Options{Channel: "beta", Tier: "0", Only: "upgrades", Until: "2026-12-31", Reason: "quarter-end freeze"}

	if err := Run(t.Context(), AdminPause, Clients{Admin: svc}, opts, &bytes.Buffer{}, OutputYAML); err != nil {
		t.Fatal(err)
	}
	if err := Run(t.Context(), AdminResume, Clients{Admin: svc}, opts, &bytes.Buffer{}, OutputYAML); err != nil {
		t.Fatal(err)
	}

	p, r := svc.paused[0], svc.resumed[0]
	if p.GetChannel() != cloudv1.ReleaseChannel_RELEASE_CHANNEL_BETA || p.GetTier() != cloudv1.ClusterTier_CLUSTER_TIER_0 ||
		!p.GetOnlyUpgrades() || p.GetTenantId() != "" || p.GetReason() != "quarter-end freeze" ||
		!p.GetUntil().AsTime().Equal(time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("pause request %v", p)
	}
	if r.GetChannel() != cloudv1.ReleaseChannel_RELEASE_CHANNEL_BETA || r.GetTier() != cloudv1.ClusterTier_CLUSTER_TIER_0 || !r.GetOnlyUpgrades() {
		t.Errorf("resume request %v", r)
	}
}

// What the CLI refuses before sending anything.
func TestAdminPauseRefuses(t *testing.T) {
	tests := []struct {
		name string
		opts Options
		want string
	}{
		{"no target", Options{}, "--tenant or --channel"},
		{"a channel and a tenant", Options{Tenant: "acme", Channel: "beta"}, "not both"},
		{"a tier without a channel", Options{Tenant: "acme", Tier: "0"}, "--tier needs --channel"},
		{"an unknown channel", Options{Channel: "gamma"}, "alpha, beta or stable"},
		{"an unknown tier", Options{Channel: "beta", Tier: "3"}, "0, 1 or 2"},
		{"only what", Options{Tenant: "acme", Only: "nodes"}, "--only upgrades"},
		{"a network's upgrades", Options{Tenant: "acme", PauseNetwork: "n", Only: "upgrades"}, "has no upgrades"},
		{"an until that is no time", Options{Tenant: "acme", Until: "soon"}, "--until"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := &listRecorder{}

			err := Run(t.Context(), AdminPause, Clients{Admin: svc}, tt.opts, &bytes.Buffer{}, OutputYAML)

			if err == nil || !strings.Contains(err.Error(), tt.want) || len(svc.paused) != 0 {
				t.Fatalf("err = %v (sent %d), want one saying %q", err, len(svc.paused), tt.want)
			}
		})
	}
}

// As a table, a channel's pause says what it covers, what it stops and how
// many clusters, then what else to know.
func TestAdminPauseOfAChannelTable(t *testing.T) {
	var out bytes.Buffer

	err := Run(t.Context(), AdminPause, Clients{Admin: &listRecorder{}}, Options{Channel: "beta", Tier: "0"}, &out, OutputTable)

	want := "TARGET                WHAT         CLUSTERS   OBJECTS PAUSED\n" +
		"channel beta tier 0   everything   2          40\n" +
		"Note: a cluster that joins it later is not paused\n"
	if err != nil || out.String() != want {
		t.Fatalf("got %v\n%s\nwant\n%s", err, out.String(), want)
	}
}

// admin pause list prints one row per pause: what it covers, whether it
// stops everything or only upgrades, who, since and until when, why, and
// how many clusters.
func TestAdminPauseList(t *testing.T) {
	withClock(t)
	until := timestamppb.New(clock().Add(48 * time.Hour))
	svc := &listRecorder{pauses: []*cloudv1.PauseRecord{
		{Target: "tenant acme", Kind: cloudv1.PauseKind_PAUSE_KIND_EVERYTHING, By: "alice", Since: timestamppb.New(clock().Add(-2 * time.Hour)),
			Reason: "provider upgrade", Clusters: 3},
		{Target: "channel beta tier 0", Kind: cloudv1.PauseKind_PAUSE_KIND_UPGRADES, By: "bob", Since: timestamppb.New(clock().Add(-time.Hour)),
			Until: until, Reason: "quarter-end freeze", Clusters: 4},
	}}
	var out bytes.Buffer

	err := Run(t.Context(), AdminPauseList, Clients{Admin: svc}, Options{}, &out, OutputTable)

	want := "TARGET                WHAT         BY      SINCE   UNTIL    CLUSTERS   REASON\n" +
		"tenant acme           everything   alice   120m    <none>   3          provider upgrade\n" +
		"channel beta tier 0   upgrades     bob     60m     in 2d    4          quarter-end freeze\n"
	if err != nil || out.String() != want {
		t.Fatalf("got %v\n%s\nwant\n%s", err, out.String(), want)
	}
}
