package authn_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

	"github.com/yindia/cluster-vending/pkg/auth"
	"github.com/yindia/cluster-vending/pkg/auth/authtest"
	"github.com/yindia/cluster-vending/pkg/fault"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1/cloudv1connect"
	"github.com/yindia/cluster-vending/pkg/server/authn"
	"github.com/yindia/cluster-vending/pkg/server/service"
	"github.com/yindia/cluster-vending/pkg/telemetry"
)

const audience = "api://cluster-vending"

// handlers count the requests that reached a handler: a refused call must
// never get that far.
type handlers struct {
	cloudv1connect.UnimplementedClusterServiceHandler
	reached atomic.Int32
	// caller is the identity the last handler saw.
	caller atomic.Pointer[auth.Identity]
}

func (h *handlers) seen(ctx context.Context) {
	h.reached.Add(1)
	h.caller.Store(auth.FromContext(ctx))
}

func (h *handlers) GetCluster(ctx context.Context, _ *cloudv1.GetClusterRequest) (*cloudv1.GetClusterResponse, error) {
	h.seen(ctx)
	return &cloudv1.GetClusterResponse{}, nil
}

func (h *handlers) DeleteCluster(ctx context.Context, _ *cloudv1.DeleteClusterRequest) (*cloudv1.DeleteClusterResponse, error) {
	h.seen(ctx)
	return &cloudv1.DeleteClusterResponse{}, nil
}

func (h *handlers) Freeze(ctx context.Context, _ *cloudv1.FreezeRequest) (*cloudv1.FreezeResponse, error) {
	h.seen(ctx)
	return &cloudv1.FreezeResponse{}, nil
}

func (h *handlers) WhoAmI(ctx context.Context, _ *cloudv1.WhoAmIRequest) (*cloudv1.WhoAmIResponse, error) {
	h.seen(ctx)
	return &cloudv1.WhoAmIResponse{Subject: auth.FromContext(ctx).Subject}, nil
}

// Tenant and operator services now share method names with different request
// types. Keep separate adapters while retaining the same request counters and
// identity observations used by every existing assertion.
type adminHandlers struct {
	cloudv1connect.UnimplementedAdminServiceHandler
	observed *handlers
}

func (h adminHandlers) Freeze(ctx context.Context, req *cloudv1.FreezeRequest) (*cloudv1.FreezeResponse, error) {
	return h.observed.Freeze(ctx, req)
}

type tenantHandlers struct {
	cloudv1connect.UnimplementedTenantServiceHandler
	observed *handlers
}

func (h tenantHandlers) WhoAmI(ctx context.Context, req *cloudv1.WhoAmIRequest) (*cloudv1.WhoAmIResponse, error) {
	return h.observed.WhoAmI(ctx, req)
}

// api is the API behind the interceptor in mode, its handlers, its log and
// the issuer that signs its tokens.
type api struct {
	handlers *handlers
	clients  clients
	idp      *authtest.Issuer
	logs     *bytes.Buffer
	url      string
}

type clients struct {
	cluster   cloudv1connect.ClusterServiceClient
	admin     cloudv1connect.AdminServiceClient
	tenant    cloudv1connect.TenantServiceClient
	subtenant cloudv1connect.SubTenantServiceClient
}

func newAPI(t *testing.T, mode auth.Mode) *api {
	t.Helper()
	idp := authtest.New(t)
	cfg, err := auth.Config{Mode: mode,
		Issuers: []auth.Issuer{{Name: "idp", Issuer: idp.URL, Audiences: []string{audience}, UsernameClaim: "name"}}}.Resolved()
	if err != nil {
		t.Fatal(err)
	}
	a := &api{handlers: &handlers{}, idp: idp, logs: &bytes.Buffer{}}
	server := connect.NewServer(service.Faults, authn.Interceptor(authn.Options{
		Mode: mode, Verifier: auth.NewVerifier(cfg), Logger: slog.New(slog.NewTextHandler(a.logs, nil))}))
	cloudv1connect.RegisterClusterServiceHandler(server, a.handlers)
	cloudv1connect.RegisterAdminServiceHandler(server, adminHandlers{observed: a.handlers})
	cloudv1connect.RegisterTenantServiceHandler(server, tenantHandlers{observed: a.handlers})
	cloudv1connect.RegisterSubTenantServiceHandler(server, subTenantHandlers{observed: a.handlers})
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	a.url = srv.URL
	client := connect.NewClient(connecthttp.NewTransport(srv.Client(), srv.URL))
	a.clients = clients{cluster: cloudv1connect.NewClusterServiceClient(client),
		admin: cloudv1connect.NewAdminServiceClient(client), tenant: cloudv1connect.NewTenantServiceClient(client),
		subtenant: cloudv1connect.NewSubTenantServiceClient(client)}
	return a
}

// token is a token of the API's issuer. Groups and roles in it change
// nothing: the API has no roles.
func (a *api) token(t *testing.T, groups ...string) string {
	t.Helper()
	return a.idp.Sign(t, a.idp.Claims(audience, "user-1", map[string]any{"groups": groups, "name": "Ada"}))
}

// with returns a context that sends header as Authorization; "" sends
// none.
func with(t *testing.T, header string) context.Context {
	t.Helper()
	ctx, info := connect.NewClientContext(t.Context())
	if header != "" {
		info.RequestHeader().Set("Authorization", header)
	}
	return ctx
}

func bearer(token string) string { return "Bearer " + token }

// calls are one call of each kind: a tenant read, a tenant write, a
// platform call and a call open to every accepted caller.
func (a *api) calls() map[string]func(ctx context.Context, tenant string) error {
	return map[string]func(ctx context.Context, tenant string) error{
		"read": func(ctx context.Context, tenant string) error {
			_, err := a.clients.cluster.GetCluster(ctx, &cloudv1.GetClusterRequest{TenantId: tenant})
			return err
		},
		"write": func(ctx context.Context, tenant string) error {
			_, err := a.clients.cluster.DeleteCluster(ctx, &cloudv1.DeleteClusterRequest{TenantId: tenant})
			return err
		},
		"platform": func(ctx context.Context, tenant string) error {
			_, err := a.clients.admin.Freeze(ctx, &cloudv1.FreezeRequest{})
			return err
		},
		"whoami": func(ctx context.Context, _ string) error {
			_, err := a.clients.tenant.WhoAmI(ctx, &cloudv1.WhoAmIRequest{})
			return err
		},
	}
}

// Authentication only: an accepted token reaches every RPC, a platform
// call included, for every tenant, whatever groups the token carries.
func TestAnAcceptedCallerMayCallEverything(t *testing.T) {
	a := newAPI(t, auth.ModeRequired)
	tests := []struct {
		name   string
		groups []string
		call   string
		tenant string
	}{
		{"platform call", nil, "platform", ""},
		{"read of a tenant", nil, "read", "acme"},
		{"write of a tenant", nil, "write", "acme"},
		{"write of another tenant", []string{"tenant-acme-viewer"}, "write", "globex"},
		{"a request that names no tenant", nil, "read", ""},
		{"who am I", nil, "whoami", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := a.handlers.reached.Load()

			err := a.calls()[tt.call](with(t, bearer(a.token(t, tt.groups...))), tt.tenant)

			if err != nil {
				t.Fatalf("call = %v, want it accepted", err)
			}
			if reached := a.handlers.reached.Load() - before; reached != 1 {
				t.Errorf("the handler ran %d times", reached)
			}
		})
	}
}

// The handler sees who is calling.
func TestHandlerSeesTheCaller(t *testing.T) {
	a := newAPI(t, auth.ModeRequired)

	res, err := a.clients.tenant.WhoAmI(with(t, bearer(a.token(t))), &cloudv1.WhoAmIRequest{})

	if err != nil || res.GetSubject() != "user-1" {
		t.Fatalf("WhoAmI = %v, %v", res, err)
	}
	caller := a.handlers.caller.Load()
	if caller.Anonymous || caller.Username != "Ada" || caller.Issuer != "idp" {
		t.Errorf("caller = %+v", caller)
	}
}

func TestAuthenticationByMode(t *testing.T) {
	const (
		accepted        = connect.Code(0)
		unauthenticated = connect.CodeUnauthenticated
	)
	tests := []struct {
		name          string
		mode          auth.Mode
		header        func(a *api, t *testing.T) string
		want          connect.Code
		wantAnonymous bool
	}{
		{"none: no token", auth.ModeNone, nil, accepted, true},
		{"none: a bad token is not looked at", auth.ModeNone, func(*api, *testing.T) string { return "Bearer garbage" }, accepted, true},
		{"required: no token", auth.ModeRequired, nil, unauthenticated, false},
		{"required: a good token", auth.ModeRequired, func(a *api, t *testing.T) string { return bearer(a.token(t)) }, accepted, false},
		{"required: lower-case scheme", auth.ModeRequired, func(a *api, t *testing.T) string { return "bearer " + a.token(t) }, accepted, false},
		{"required: basic credentials", auth.ModeRequired, func(*api, *testing.T) string { return "Basic dXNlcjpwYXNz" }, unauthenticated, false},
		{"required: bearer without a token", auth.ModeRequired, func(*api, *testing.T) string { return "Bearer " }, unauthenticated, false},
		{"required: an expired token", auth.ModeRequired, func(a *api, t *testing.T) string {
			return bearer(a.idp.Sign(t, a.idp.Claims(audience, "u", map[string]any{"exp": 1})))
		}, unauthenticated, false},
		{"required: alg none", auth.ModeRequired, func(a *api, t *testing.T) string {
			return bearer(authtest.Unsigned(t, a.idp.Claims(audience, "u", nil)))
		}, unauthenticated, false},
		{"required: signed by another key", auth.ModeRequired, func(a *api, t *testing.T) string {
			return bearer(a.idp.SignWithOtherKey(t, a.idp.Claims(audience, "u", nil)))
		}, unauthenticated, false},
		{"required: another audience", auth.ModeRequired, func(a *api, t *testing.T) string {
			return bearer(a.idp.Sign(t, a.idp.Claims("api://another", "u", nil)))
		}, unauthenticated, false},
		{"required: another issuer", auth.ModeRequired, func(_ *api, t *testing.T) string {
			other := authtest.New(t)
			return bearer(other.Sign(t, other.Claims(audience, "u", nil)))
		}, unauthenticated, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newAPI(t, tt.mode)
			header := ""
			if tt.header != nil {
				header = tt.header(a, t)
			}

			err := a.calls()["write"](with(t, header), "acme")

			if got := connect.CodeOf(err); (err == nil) != (tt.want == accepted) || (err != nil && got != tt.want) {
				t.Fatalf("call = %v, want code %v", err, tt.want)
			}
			if tt.want != accepted {
				if a.handlers.reached.Load() != 0 {
					t.Error("the handler ran for an unauthenticated call")
				}
				return
			}
			if caller := a.handlers.caller.Load(); caller.Anonymous != tt.wantAnonymous {
				t.Errorf("caller anonymous = %t, want %t", caller.Anonymous, tt.wantAnonymous)
			}
		})
	}
}

// A refused request is answered 401 with a Bearer challenge, and the
// probes stay open: they are not RPCs.
func TestUnauthenticatedAnswer(t *testing.T) {
	a := newAPI(t, auth.ModeRequired)

	res, err := http.Post(a.url+"/cloud.v1.ClusterService/GetCluster", "application/json", strings.NewReader(`{"tenantId":"acme"}`))

	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusUnauthorized || res.Header.Get("WWW-Authenticate") != "Bearer" {
		t.Errorf("status %d, WWW-Authenticate %q; want 401 with a Bearer challenge", res.StatusCode, res.Header.Get("WWW-Authenticate"))
	}
	if !strings.Contains(string(body), "cluster-vending login") {
		t.Errorf("body %s does not say how to log in", body)
	}
}

// Writes and refusals are logged with who made them; reads are not, and
// the token never is.
func TestAuditLog(t *testing.T) {
	a := newAPI(t, auth.ModeRequired)
	token := a.token(t)

	_ = a.calls()["read"](with(t, bearer(token)), "acme")
	if a.logs.Len() != 0 {
		t.Errorf("a read was logged: %s", a.logs)
	}
	_ = a.calls()["write"](with(t, bearer(token)), "acme")
	_ = a.calls()["write"](with(t, ""), "acme")
	_ = a.calls()["write"](with(t, "Bearer garbage"), "acme")

	logs := a.logs.String()
	for _, want := range []string{
		`msg="api write" procedure=/cloud.v1.ClusterService/DeleteCluster subject=user-1 username=Ada issuer=idp tenant=acme outcome=ok`,
		`msg="api call refused" procedure=/cloud.v1.ClusterService/DeleteCluster`,
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("log lacks %q:\n%s", want, logs)
		}
	}
	if strings.Contains(logs, token) || strings.Contains(logs, "garbage") {
		t.Errorf("a token is in the log:\n%s", logs)
	}
}

// unavailable is a Verifier whose issuer cannot be reached.
type unavailable struct{}

func (unavailable) Verify(context.Context, string) (*auth.Identity, error) {
	return nil, fault.New(fault.Unavailable, "the identity provider idp cannot be reached")
}

// While the issuer is down a token cannot be verified: the call fails
// closed, as UNAVAILABLE, and a call without a token is UNAUTHENTICATED.
func TestFailsClosedWhenTheIssuerIsDown(t *testing.T) {
	interceptor := authn.Interceptor(authn.Options{Mode: auth.ModeRequired, Verifier: unavailable{}})
	reached := false
	info := &connect.CallInfo{}
	info.RequestHeader().Set("Authorization", "Bearer some-token")
	server := connect.NewServer(interceptor)
	server.SetUnknownHandler(func(context.Context, connect.Spec, connect.ServerStream) error {
		reached = true
		return connect.NewError(connect.CodeUnimplemented, "unknown")
	})

	err := server.Call(t.Context(), "/cloud.v1.ClusterService/Nope", info, nil)

	if !fault.Is(err, fault.Unavailable) || reached {
		t.Fatalf("issuer down: %v (handler reached: %t), want Unavailable before any handler", err, reached)
	}
	if got := server.Call(t.Context(), "/cloud.v1.ClusterService/Nope", &connect.CallInfo{}, nil); !fault.Is(got, fault.Unauthenticated) || reached {
		t.Errorf("a call without a token = %v, want UNAUTHENTICATED before any handler", got)
	}
}

// Every refusal says why, for cluster_vending_auth_failures_total.
func TestRefusalsAreCountedByReason(t *testing.T) {
	var reasons []string
	failed := func(reason string) { reasons = append(reasons, reason) }
	call := func(verifier authn.Verifier, header string) {
		server := connect.NewServer(authn.Interceptor(authn.Options{Mode: auth.ModeRequired, Verifier: verifier, Failed: failed}))
		server.SetUnknownHandler(func(context.Context, connect.Spec, connect.ServerStream) error { return nil })
		info := &connect.CallInfo{}
		if header != "" {
			info.RequestHeader().Set("Authorization", header)
		}
		_ = server.Call(t.Context(), "/cloud.v1.ClusterService/Nope", info, nil)
	}
	a := newAPI(t, auth.ModeRequired)
	cfg, err := auth.Config{Mode: auth.ModeRequired,
		Issuers: []auth.Issuer{{Name: "idp", Issuer: a.idp.URL, Audiences: []string{audience}, UsernameClaim: "name"}}}.Resolved()
	if err != nil {
		t.Fatal(err)
	}
	verifier := auth.NewVerifier(cfg)
	call(verifier, "")
	call(verifier, "Basic dXNlcjpwYXNz")
	call(verifier, "Bearer garbage")
	call(unavailable{}, "Bearer some-token")
	call(verifier, bearer(a.token(t)))
	want := []string{telemetry.AuthMissingToken, telemetry.AuthMalformedHeader, telemetry.AuthRejected, telemetry.AuthUnavailable}
	if !slices.Equal(reasons, want) {
		t.Fatalf("reasons %v, want %v", reasons, want)
	}
}
