package server_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yindia/cluster-vending/pkg/auth"
	"github.com/yindia/cluster-vending/pkg/auth/authtest"
	"github.com/yindia/cluster-vending/pkg/reconcile"
	"github.com/yindia/cluster-vending/pkg/server"
)

// With authentication required, every service refuses a call without a
// token, a caller with a role reaches the handlers, and the probes and
// metrics stay open: they are not RPCs.
func TestHandlerAuthenticatesTheServices(t *testing.T) {
	idp := authtest.New(t)
	cfg, err := auth.Config{Mode: auth.ModeRequired,
		Issuers: []auth.Issuer{{Name: "idp", Issuer: idp.URL, Audiences: []string{"api"}}},
		Rules:   []auth.Rule{{Match: auth.Match{Issuer: "idp", Role: "admins"}, Grant: auth.Grant{Role: auth.RolePlatformAdmin}}},
	}.Resolved()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.NewHandler(reconcile.Deps{Logger: discard()}, nil,
		server.WithAuth(cfg.Mode, auth.NewVerifier(cfg))))
	t.Cleanup(srv.Close)
	admin := idp.Sign(t, idp.Claims("api", "u", map[string]any{"groups": []string{"admins"}}))
	post := func(rpc, token string) int {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/"+rpc, strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
		return res.StatusCode
	}

	for _, rpc := range []string{
		"cloud.v1.ClusterService/GetCluster", "cloud.v1.NetworkService/ImportNetwork",
		"cloud.v1.TenantService/BootstrapTenant", "cloud.v1.TenantService/WhoAmI", "cloud.v1.AdminService/PauseReconcile",
	} {
		if got := post(rpc, ""); got != http.StatusUnauthorized {
			t.Errorf("%s without a token: status %d, want 401", rpc, got)
		}
	}
	// Past the authentication the handler validates: an empty request is 400.
	if got := post("cloud.v1.ClusterService/GetCluster", admin); got != http.StatusBadRequest {
		t.Errorf("GetCluster as platform admin: status %d, want the handler's 400", got)
	}
	if got := post("cloud.v1.TenantService/WhoAmI", admin); got != http.StatusOK {
		t.Errorf("WhoAmI as platform admin: status %d, want 200", got)
	}
	// The release configuration is the operators' to read: a caller
	// without the platform admin role is refused; an admin reaches the
	// handler (which cannot read a management cluster here).
	someone := idp.Sign(t, idp.Claims("api", "v", map[string]any{"groups": []string{"devs"}}))
	for _, rpc := range []string{"cloud.v1.AdminService/ListReleases", "cloud.v1.AdminService/ListReleaseChannels"} {
		if got := post(rpc, someone); got != http.StatusForbidden {
			t.Errorf("%s without the admin role: status %d, want 403", rpc, got)
		}
		if got := post(rpc, admin); got != http.StatusServiceUnavailable {
			t.Errorf("%s as platform admin without a management cluster: status %d, want the handler's 503", rpc, got)
		}
	}
	// The account role templates hold no secret: a tenant fetches them
	// before it has an account here.
	for _, path := range []string{"/healthz", "/readyz", "/metrics",
		"/cloudformation/cluster-vending-provisioner-role.template.yaml"} {
		res, err := srv.Client().Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Errorf("%s: status %d, want it open", path, res.StatusCode)
		}
	}
}
