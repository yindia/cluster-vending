package cli

import (
	"bytes"
	"context"
	"testing"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1/cloudv1connect"
)

// pauseRecorder is an AdminService that keeps the requests it got.
type pauseRecorder struct {
	cloudv1connect.UnimplementedAdminServiceHandler
	paused  []*cloudv1.PauseReconcileRequest
	resumed []*cloudv1.ResumeReconcileRequest
}

func (p *pauseRecorder) PauseReconcile(_ context.Context, req *cloudv1.PauseReconcileRequest) (*cloudv1.PauseReconcileResponse, error) {
	p.paused = append(p.paused, req)
	return &cloudv1.PauseReconcileResponse{Resources: 3}, nil
}

func (p *pauseRecorder) ResumeReconcile(_ context.Context, req *cloudv1.ResumeReconcileRequest) (*cloudv1.ResumeReconcileResponse, error) {
	p.resumed = append(p.resumed, req)
	return &cloudv1.ResumeReconcileResponse{Resources: 3}, nil
}

// admin pause sends the tenant, the cluster and the reason; admin resume
// the tenant and the cluster.
func TestAdminPauseAndResume(t *testing.T) {
	const cluster = "0190a000-0000-7000-8000-00000000000a"
	tests := []struct {
		name        string
		opts        Options
		wantCluster string
		wantReason  string
	}{
		{"a tenant, with the reason", Options{Tenant: "acme", Reason: "provider upgrade"}, "", "provider upgrade"},
		{"one cluster", Options{Tenant: "acme", Cluster: cluster}, cluster, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := &pauseRecorder{}
			clients := Clients{Admin: svc}

			err := Run(t.Context(), AdminPause, clients, tt.opts, &bytes.Buffer{}, OutputYAML)
			if err != nil {
				t.Fatal(err)
			}
			err = Run(t.Context(), AdminResume, clients, tt.opts, &bytes.Buffer{}, OutputYAML)

			if err != nil || len(svc.paused) != 1 || len(svc.resumed) != 1 {
				t.Fatalf("%v; %d pauses, %d resumes", err, len(svc.paused), len(svc.resumed))
			}
			p, r := svc.paused[0], svc.resumed[0]
			if p.GetTenantId() != "acme" || p.GetClusterId() != tt.wantCluster || p.GetReason() != tt.wantReason {
				t.Errorf("pause request %v", p)
			}
			if r.GetTenantId() != "acme" || r.GetClusterId() != tt.wantCluster {
				t.Errorf("resume request %v", r)
			}
		})
	}
	if err := Run(t.Context(), AdminPause, Clients{Admin: &pauseRecorder{}}, Options{}, &bytes.Buffer{}, OutputYAML); err == nil {
		t.Error("a pause without --tenant was sent")
	}
}
