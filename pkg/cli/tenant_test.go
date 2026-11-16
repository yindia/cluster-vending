package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect/v2"

	cloud "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1/cloudv1connect"
)

type tenantRecorder struct {
	cloudv1connect.UnimplementedTenantServiceHandler
	freezes   []*cloud.TenantFreezeRequest
	unfreezes []*cloud.TenantUnfreezeRequest
	reads     []*cloud.TenantGetFreezeRequest
	creates   []*cloud.CreateTenantRequest
	lists     int
	phase     string
	revision  string
}

func (r *tenantRecorder) Freeze(_ context.Context, req *cloud.TenantFreezeRequest) (*cloud.FreezeResponse, error) {
	r.freezes = append(r.freezes, req)
	return &cloud.FreezeResponse{Revision: "one", Phase: "Freezing"}, nil
}
func (r *tenantRecorder) Unfreeze(_ context.Context, req *cloud.TenantUnfreezeRequest) (*cloud.UnfreezeResponse, error) {
	r.unfreezes = append(r.unfreezes, req)
	return &cloud.UnfreezeResponse{Revision: "one"}, nil
}
func (r *tenantRecorder) GetFreeze(_ context.Context, req *cloud.TenantGetFreezeRequest) (*cloud.GetFreezeResponse, error) {
	r.reads = append(r.reads, req)
	return &cloud.GetFreezeResponse{Revision: r.revision, Phase: r.phase}, nil
}
func (r *tenantRecorder) CreateTenant(_ context.Context, req *cloud.CreateTenantRequest) (*cloud.CreateTenantResponse, error) {
	r.creates = append(r.creates, req)
	return &cloud.CreateTenantResponse{Tenant: &cloud.TenantInfo{Id: req.TenantId}}, nil
}
func (r *tenantRecorder) ListTenants(context.Context, *cloud.ListTenantsRequest) (*cloud.ListTenantsResponse, error) {
	r.lists++
	return &cloud.ListTenantsResponse{Tenants: []*cloud.TenantInfo{{Id: "acme"}}}, nil
}

func TestTenantFreezeScopes(t *testing.T) {
	const id = "0190a000-0000-7000-8000-0000000000a0"
	for _, o := range []Options{{Tenant: "acme"}, {Tenant: "acme", Cluster: id}, {Tenant: "acme", PauseNetwork: id}} {
		r := &tenantRecorder{phase: "Frozen", revision: "one"}
		var out bytes.Buffer
		o.Wait = true
		o.PollInterval = time.Millisecond
		o.Reason = "maintenance"
		o.Until = "2026-12-31"
		if err := Run(t.Context(), TenantFreeze, Clients{Tenant: r}, o, &out, OutputTable); err != nil {
			t.Fatal(err)
		}
		req := r.freezes[0]
		if req.TenantId != "acme" || req.Target.GetPlatform() || req.Target.GetClusterId() != o.Cluster || req.Target.GetNetworkId() != o.PauseNetwork || req.Reason != o.Reason || req.Until == nil {
			t.Fatalf("unsafe request %v", req)
		}
		if len(r.reads) != 1 || !strings.Contains(out.String(), "Frozen") {
			t.Fatalf("missing confirmation: %v %s", r.reads, &out)
		}
		r.phase = "Unfrozen"
		out.Reset()
		if err := Run(t.Context(), TenantUnfreeze, Clients{Tenant: r}, o, &out, OutputTable); err != nil {
			t.Fatal(err)
		}
		if len(r.unfreezes) != 1 || r.unfreezes[0].TenantId != "acme" {
			t.Fatal(r.unfreezes)
		}
	}
}
func TestTenantFreezeRefusesUnsafeOptions(t *testing.T) {
	for _, command := range []Command{TenantFreeze, TenantUnfreeze, TenantFreezeStatus} {
		for _, o := range []Options{{}, {Tenant: "acme", Platform: true}, {Tenant: "acme", ResetExceptions: true}, {Tenant: "acme", Cluster: "a", PauseNetwork: "b"}, {Tenant: "acme", Cluster: "not-uuid"}, {Tenant: "Bad_Name"}} {
			r := &tenantRecorder{}
			if err := Run(t.Context(), command, Clients{Tenant: r}, o, &bytes.Buffer{}, OutputJSON); err == nil {
				t.Fatalf("accepted %+v", o)
			}
			if len(r.freezes)+len(r.unfreezes)+len(r.reads) != 0 {
				t.Fatal("sent invalid request")
			}
		}
	}
}
func TestTenantWaitDetectsConcurrentChange(t *testing.T) {
	r := &tenantRecorder{phase: "Frozen", revision: "two"}
	err := Run(t.Context(), TenantFreeze, Clients{Tenant: r}, Options{Tenant: "acme", Wait: true}, &bytes.Buffer{}, OutputJSON)
	if connect.CodeOf(err) != connect.CodeAborted || !strings.Contains(err.Error(), "tenant freeze status") {
		t.Fatal(err)
	}
}
func TestTenantDirectoryCLI(t *testing.T) {
	r := &tenantRecorder{}
	var out bytes.Buffer
	if err := Run(t.Context(), TenantList, Clients{Tenant: r}, Options{}, &out, OutputTable); err != nil {
		t.Fatal(err)
	}
	if r.lists != 1 || !strings.Contains(out.String(), "acme") {
		t.Fatal(out.String())
	}
	if err := Run(t.Context(), TenantCreate, Clients{Tenant: r}, Options{ID: "acme", DefaultTenant: "other"}, &out, OutputJSON); err != nil {
		t.Fatal(err)
	}
	if r.creates[0].TenantId != "acme" {
		t.Fatal(r.creates)
	}
	for _, o := range []Options{{ID: "Bad_Name"}, {ID: "acme", Tenant: "other"}, {}} {
		if err := Run(t.Context(), TenantCreate, Clients{Tenant: r}, o, &out, OutputJSON); err == nil {
			t.Fatalf("accepted %+v", o)
		}
	}
	if len(r.creates) != 1 {
		t.Fatal("invalid create sent")
	}
}
