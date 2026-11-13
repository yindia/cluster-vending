package authn_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
)

const audience = "api://cluster-vending"

// handlers count the requests that reached a handler: a refused call must
// never get that far.
type handlers struct {
	cloudv1connect.UnimplementedClusterServiceHandler
	cloudv1connect.UnimplementedAdminServiceHandler
	cloudv1connect.UnimplementedTenantServiceHandler
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

func (h *handlers) WatchCluster(ctx context.Context, _ *cloudv1.WatchClusterRequest, _ cloudv1connect.ClusterServiceWatchClusterServerStream) error {
	h.seen(ctx)
	return nil
}

func (h *handlers) PauseReconcile(ctx context.Context, _ *cloudv1.PauseReconcileRequest) (*cloudv1.PauseReconcileResponse, error) {
	h.seen(ctx)
	return &cloudv1.PauseReconcileResponse{}, nil
}

func (h *handlers) WhoAmI(ctx context.Context, _ *cloudv1.WhoAmIRequest) (*cloudv1.WhoAmIResponse, error) {
	h.seen(ctx)
	return &cloudv1.WhoAmIResponse{Subject: auth.FromContext(ctx).Subject}, nil
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
	cluster cloudv1connect.ClusterServiceClient
	admin   cloudv1connect.AdminServiceClient
	tenant  cloudv1connect.TenantServiceClient
}

func newAPI(t *testing.T, mode auth.Mode) *api {
	t.Helper()
	idp := authtest.New(t)
	cfg, err := auth.Config{Mode: mode,
		Issuers: []auth.Issuer{{Name: "idp", Issuer: idp.URL, Audiences: []string{audience}, RolesClaim: "roles", UsernameClaim: "name"}},
		Rules: []auth.Rule{
			{Match: auth.Match{Issuer: "idp", Role: "platform-admin"}, Grant: auth.Grant{Role: auth.RolePlatformAdmin}},
			{Match: auth.Match{Issuer: "idp", Role: "acme.admin"}, Grant: auth.Grant{Role: auth.RoleTenantAdmin, Tenants: []string{"acme"}}},
			{Match: auth.Match{Issuer: "idp", Role: "acme.viewer"}, Grant: auth.Grant{Role: auth.RoleTenantViewer, Tenants: []string{"acme"}}},
		}}.Resolved()
	if err != nil {
		t.Fatal(err)
	}
	a := &api{handlers: &handlers{}, idp: idp, logs: &bytes.Buffer{}}
	server := connect.NewServer(service.Faults, authn.Interceptor(authn.Options{
		Mode: mode, Verifier: auth.NewVerifier(cfg), Logger: slog.New(slog.NewTextHandler(a.logs, nil))}))
	cloudv1connect.RegisterClusterServiceHandler(server, a.handlers)
	cloudv1connect.RegisterAdminServiceHandler(server, a.handlers)
	cloudv1connect.RegisterTenantServiceHandler(server, a.handlers)
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	a.url = srv.URL
	client := connect.NewClient(connecthttp.NewTransport(srv.Client(), srv.URL))
	a.clients = clients{cluster: cloudv1connect.NewClusterServiceClient(client),
		admin: cloudv1connect.NewAdminServiceClient(client), tenant: cloudv1connect.NewTenantServiceClient(client)}
	return a
}

// token is a token of the API's issuer for a caller with roles.
func (a *api) token(t *testing.T, roles ...string) string {
	t.Helper()
	return a.idp.Sign(t, a.idp.Claims(audience, "user-1", map[string]any{"roles": roles, "name": "Ada"}))
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
			_, err := a.clients.admin.PauseReconcile(ctx, &cloudv1.PauseReconcileRequest{TenantId: tenant})
			return err
		},
		"whoami": func(ctx context.Context, _ string) error {
			_, err := a.clients.tenant.WhoAmI(ctx, &cloudv1.WhoAmIRequest{})
			return err
		},
	}
}

func TestAuthorization(t *testing.T) {
	a := newAPI(t, auth.ModeRequired)
	ok := connect.Code(0)
	denied := connect.CodePermissionDenied
	tests := []struct {
		name   string
		roles  []string
		call   string
		tenant string
		want   connect.Code
	}{
		{"platform admin: platform call", []string{"platform-admin"}, "platform", "acme", ok},
		{"platform admin: any tenant's write", []string{"platform-admin"}, "write", "globex", ok},
		{"tenant admin reads its tenant", []string{"acme.admin"}, "read", "acme", ok},
		{"tenant admin writes its tenant", []string{"acme.admin"}, "write", "acme", ok},
		{"tenant admin: another tenant", []string{"acme.admin"}, "read", "globex", denied},
		{"tenant admin: platform call on its own tenant", []string{"acme.admin"}, "platform", "acme", denied},
		{"viewer reads", []string{"acme.viewer"}, "read", "acme", ok},
		{"viewer attempts a write", []string{"acme.viewer"}, "write", "acme", denied},
		{"viewer: another tenant", []string{"acme.viewer"}, "read", "globex", denied},
		{"no role: tenant call", nil, "read", "acme", denied},
		{"no role: the request names no tenant", nil, "read", "", denied},
		{"no role: who am I", nil, "whoami", "", ok},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := a.handlers.reached.Load()

			err := a.calls()[tt.call](with(t, bearer(a.token(t, tt.roles...))), tt.tenant)

			if got := connect.CodeOf(err); (err == nil) != (tt.want == ok) || (err != nil && got != tt.want) {
				t.Fatalf("call = %v, want code %v", err, tt.want)
			}
			reached := a.handlers.reached.Load() - before
			if (tt.want == ok) != (reached == 1) {
				t.Errorf("the handler ran %d times for a call that ended %v", reached, tt.want)
			}
		})
	}
}

// The handler sees who is calling.
func TestHandlerSeesTheCaller(t *testing.T) {
	a := newAPI(t, auth.ModeRequired)

	res, err := a.clients.tenant.WhoAmI(with(t, bearer(a.token(t, "acme.admin"))), &cloudv1.WhoAmIRequest{})

	if err != nil || res.GetSubject() != "user-1" {
		t.Fatalf("WhoAmI = %v, %v", res, err)
	}
	caller := a.handlers.caller.Load()
	if caller.Anonymous || caller.Username != "Ada" || caller.Issuer != "idp" || len(caller.Grants) != 1 {
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
		{"optional: no token", auth.ModeOptional, nil, accepted, true},
		{"optional: a good token", auth.ModeOptional, func(a *api, t *testing.T) string { return bearer(a.token(t, "acme.admin")) }, accepted, false},
		{"optional: a bad token", auth.ModeOptional, func(*api, *testing.T) string { return "Bearer garbage" }, unauthenticated, false},
		{"required: no token", auth.ModeRequired, nil, unauthenticated, false},
		{"required: a good token", auth.ModeRequired, func(a *api, t *testing.T) string { return bearer(a.token(t, "acme.admin")) }, accepted, false},
		{"required: lower-case scheme", auth.ModeRequired, func(a *api, t *testing.T) string { return "bearer " + a.token(t, "acme.admin") }, accepted, false},
		{"required: basic credentials", auth.ModeRequired, func(*api, *testing.T) string { return "Basic dXNlcjpwYXNz" }, unauthenticated, false},
		{"required: bearer without a token", auth.ModeRequired, func(*api, *testing.T) string { return "Bearer " }, unauthenticated, false},
		{"required: an expired token", auth.ModeRequired, func(a *api, t *testing.T) string {
			return bearer(a.idp.Sign(t, a.idp.Claims(audience, "u", map[string]any{"exp": 1})))
		}, unauthenticated, false},
		{"required: alg none", auth.ModeRequired, func(a *api, t *testing.T) string {
			return bearer(authtest.Unsigned(t, a.idp.Claims(audience, "u", map[string]any{"roles": []string{"platform-admin"}})))
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

// A server-streaming RPC is guarded like a unary one.
func TestStreamingIsGuarded(t *testing.T) {
	a := newAPI(t, auth.ModeRequired)
	watch := func(roles []string, tenant string) error {
		stream, err := a.clients.cluster.WatchCluster(with(t, bearer(a.token(t, roles...))), &cloudv1.WatchClusterRequest{TenantId: tenant})
		if err != nil {
			return err
		}
		defer func() { _ = stream.Close() }()
		for {
			if _, err := stream.Receive(); errors.Is(err, io.EOF) {
				return nil
			} else if err != nil {
				return err
			}
		}
	}

	allowed := watch([]string{"acme.viewer"}, "acme")
	refused := watch([]string{"acme.viewer"}, "globex")

	if allowed != nil {
		t.Errorf("a viewer watching its tenant: %v", allowed)
	}
	if connect.CodeOf(refused) != connect.CodePermissionDenied || a.handlers.reached.Load() != 1 {
		t.Errorf("another tenant's watch = %v (handler ran %d times), want PERMISSION_DENIED before the handler",
			refused, a.handlers.reached.Load())
	}
}

// Writes and refusals are logged with who made them; reads are not, and
// the token never is.
func TestAuditLog(t *testing.T) {
	a := newAPI(t, auth.ModeOptional)
	admin, viewer := a.token(t, "acme.admin"), a.token(t, "acme.viewer")

	_ = a.calls()["read"](with(t, bearer(admin)), "acme")
	if a.logs.Len() != 0 {
		t.Errorf("a read was logged: %s", a.logs)
	}
	_ = a.calls()["write"](with(t, bearer(admin)), "acme")
	_ = a.calls()["write"](with(t, bearer(viewer)), "acme")
	_ = a.calls()["write"](with(t, ""), "acme")
	_ = a.calls()["write"](with(t, "Bearer garbage"), "acme")

	logs := a.logs.String()
	for _, want := range []string{
		`msg="api write" procedure=/cloud.v1.ClusterService/DeleteCluster subject=user-1 username=Ada issuer=idp tenant=acme outcome=ok`,
		`msg="api call denied" procedure=/cloud.v1.ClusterService/DeleteCluster subject=user-1 username=Ada issuer=idp tenant=acme outcome=permission_denied`,
		`subject=anonymous`,
		`msg="api call refused"`,
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("log lacks %q:\n%s", want, logs)
		}
	}
	if strings.Contains(logs, admin) || strings.Contains(logs, viewer) || strings.Contains(logs, "garbage") {
		t.Errorf("a token is in the log:\n%s", logs)
	}
}

// unavailable is a Verifier whose issuer cannot be reached.
type unavailable struct{}

func (unavailable) Verify(context.Context, string) (*auth.Identity, error) {
	return nil, fault.New(fault.Unavailable, "the identity provider idp cannot be reached")
}

// While the issuer is down a token cannot be verified: the call fails
// closed, as UNAVAILABLE, and an unknown procedure is still UNIMPLEMENTED.
func TestFailsClosedWhenTheIssuerIsDown(t *testing.T) {
	interceptor := authn.Interceptor(authn.Options{Mode: auth.ModeOptional, Verifier: unavailable{}})
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
	if got := server.Call(t.Context(), "/cloud.v1.ClusterService/Nope", &connect.CallInfo{}, nil); connect.CodeOf(got) != connect.CodeUnimplemented {
		t.Errorf("an unknown procedure without a token = %v, want UNIMPLEMENTED", got)
	}
}
