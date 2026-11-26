package authn_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

	"github.com/yindia/cluster-vending/pkg/auth"
	"github.com/yindia/cluster-vending/pkg/auth/authtest"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1/cloudv1connect"
	"github.com/yindia/cluster-vending/pkg/server/authn"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

// auditCluster answers the cluster RPCs the audit tests call.
type auditCluster struct {
	cloudv1connect.UnimplementedClusterServiceHandler
}

func (auditCluster) GetCluster(context.Context, *cloudv1.GetClusterRequest) (*cloudv1.GetClusterResponse, error) {
	return &cloudv1.GetClusterResponse{}, nil
}

func (auditCluster) DeleteCluster(context.Context, *cloudv1.DeleteClusterRequest) (*cloudv1.DeleteClusterResponse, error) {
	return &cloudv1.DeleteClusterResponse{}, nil
}

func (auditCluster) ListNodeGroupNodes(context.Context, *cloudv1.ListNodeGroupNodesRequest) (*cloudv1.ListNodeGroupNodesResponse, error) {
	return &cloudv1.ListNodeGroupNodesResponse{}, nil
}

func (auditCluster) GetNodeResources(context.Context, *cloudv1.GetNodeResourcesRequest) (*cloudv1.GetNodeResourcesResponse, error) {
	return &cloudv1.GetNodeResourcesResponse{}, nil
}

// auditTenant answers GetAccountOnboarding.
type auditTenant struct {
	cloudv1connect.UnimplementedTenantServiceHandler
}

func (auditTenant) GetAccountOnboarding(context.Context, *cloudv1.GetAccountOnboardingRequest) (*cloudv1.GetAccountOnboardingResponse, error) {
	return &cloudv1.GetAccountOnboardingResponse{}, nil
}

// auditAPI is the API behind the interceptor in mode, with its log.
type auditAPI struct {
	cluster cloudv1connect.ClusterServiceClient
	tenant  cloudv1connect.TenantServiceClient
	idp     *authtest.Issuer
	logs    *bytes.Buffer
}

func newAuditAPI(t *testing.T, mode auth.Mode) *auditAPI {
	t.Helper()
	idp := authtest.New(t)
	cfg, err := auth.Config{Mode: mode,
		Issuers: []auth.Issuer{{Name: "idp", Issuer: idp.URL, Audiences: []string{audience}, UsernameClaim: "name"}}}.Resolved()
	if err != nil {
		t.Fatal(err)
	}
	a := &auditAPI{idp: idp, logs: &bytes.Buffer{}}
	server := connect.NewServer(service.Faults, authn.Interceptor(authn.Options{
		Mode: mode, Verifier: auth.NewVerifier(cfg), Logger: slog.New(slog.NewTextHandler(a.logs, nil))}))
	cloudv1connect.RegisterClusterServiceHandler(server, auditCluster{})
	cloudv1connect.RegisterTenantServiceHandler(server, auditTenant{})
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client := connect.NewClient(connecthttp.NewTransport(srv.Client(), srv.URL))
	a.cluster = cloudv1connect.NewClusterServiceClient(client)
	a.tenant = cloudv1connect.NewTenantServiceClient(client)
	return a
}

// header is the Authorization the mode needs: a token in mode required.
func (a *auditAPI) header(t *testing.T, mode auth.Mode) string {
	t.Helper()
	if mode != auth.ModeRequired {
		return ""
	}
	return bearer(a.idp.Sign(t, a.idp.Claims(audience, "user-1", map[string]any{"name": "Ada"})))
}

// In both modes, writes and the sensitive reads (account onboarding, the
// live reads) are audited with the caller and its address; other reads are
// not. Mode none audits the caller as subject "anonymous".
func TestAuditByMode(t *testing.T) {
	calls := map[string]func(a *auditAPI, ctx context.Context) error{
		"write": func(a *auditAPI, ctx context.Context) error {
			_, err := a.cluster.DeleteCluster(ctx, &cloudv1.DeleteClusterRequest{TenantId: "acme"})
			return err
		},
		"onboarding": func(a *auditAPI, ctx context.Context) error {
			_, err := a.tenant.GetAccountOnboarding(ctx, &cloudv1.GetAccountOnboardingRequest{TenantId: "acme"})
			return err
		},
		"live nodes": func(a *auditAPI, ctx context.Context) error {
			_, err := a.cluster.ListNodeGroupNodes(ctx, &cloudv1.ListNodeGroupNodesRequest{TenantId: "acme"})
			return err
		},
		"live resources": func(a *auditAPI, ctx context.Context) error {
			_, err := a.cluster.GetNodeResources(ctx, &cloudv1.GetNodeResourcesRequest{TenantId: "acme"})
			return err
		},
		"read": func(a *auditAPI, ctx context.Context) error {
			_, err := a.cluster.GetCluster(ctx, &cloudv1.GetClusterRequest{TenantId: "acme"})
			return err
		},
	}
	tests := []struct {
		call string
		// want is the start of the audit line; "" for none.
		want string
	}{
		{"write", `msg="api write" procedure=/cloud.v1.ClusterService/DeleteCluster`},
		{"onboarding", `msg="api sensitive read" procedure=/cloud.v1.TenantService/GetAccountOnboarding`},
		{"live nodes", `msg="api sensitive read" procedure=/cloud.v1.ClusterService/ListNodeGroupNodes`},
		{"live resources", `msg="api sensitive read" procedure=/cloud.v1.ClusterService/GetNodeResources`},
		{"read", ""},
	}
	modes := map[auth.Mode]string{
		auth.ModeNone:     `subject=anonymous username="" issuer="" tenant=acme outcome=ok peer=127.0.0.1:`,
		auth.ModeRequired: `subject=user-1 username=Ada issuer=idp tenant=acme outcome=ok peer=127.0.0.1:`,
	}
	for mode, caller := range modes {
		for _, tt := range tests {
			t.Run(string(mode)+": "+tt.call, func(t *testing.T) {
				a := newAuditAPI(t, mode)
				ctx := with(t, a.header(t, mode))

				if err := calls[tt.call](a, ctx); err != nil {
					t.Fatalf("call = %v", err)
				}

				logs := a.logs.String()
				if tt.want == "" {
					if logs != "" {
						t.Errorf("a read was logged: %s", logs)
					}
					return
				}
				if want := tt.want + " " + caller; !strings.Contains(logs, want) {
					t.Errorf("log lacks %q:\n%s", want, logs)
				}
			})
		}
	}
}

// A refused call is logged with the caller's address.
func TestRefusalLogsThePeer(t *testing.T) {
	a := newAuditAPI(t, auth.ModeRequired)

	_, err := a.tenant.GetAccountOnboarding(with(t, ""), &cloudv1.GetAccountOnboardingRequest{TenantId: "acme"})

	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("call = %v, want UNAUTHENTICATED", err)
	}
	if want := `msg="api call refused" procedure=/cloud.v1.TenantService/GetAccountOnboarding peer=127.0.0.1:`; !strings.Contains(a.logs.String(), want) {
		t.Errorf("log lacks %q:\n%s", want, a.logs)
	}
}
