package server_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yindia/cluster-vending/pkg/auth"
	"github.com/yindia/cluster-vending/pkg/reconcile"
	"github.com/yindia/cluster-vending/pkg/server"
)

func TestMetricsIncludeAuthenticationAndTranslatedFaults(t *testing.T) {
	for _, test := range []struct {
		mode   auth.Mode
		code   string
		status int
	}{
		{auth.ModeRequired, "unauthenticated", http.StatusUnauthorized},
		{auth.ModeNone, "invalid_argument", http.StatusBadRequest},
	} {
		t.Run(test.code, func(t *testing.T) {
			h := server.NewHandler(reconcile.Deps{}, nil, server.WithAuth(test.mode, nil))
			call := httptest.NewRequest(http.MethodPost, "/cloud.v1.ClusterService/GetCluster", strings.NewReader("{}"))
			call.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			h.ServeHTTP(response, call)
			if response.Code != test.status {
				t.Fatalf("status %d: %s", response.Code, response.Body)
			}
			scrape := httptest.NewRecorder()
			h.ServeHTTP(scrape, httptest.NewRequest(http.MethodGet, "/metrics", nil))
			if scrape.Code != http.StatusOK {
				t.Fatal(scrape.Code)
			}
			raw, err := io.ReadAll(scrape.Result().Body)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, line := range strings.Split(string(raw), "\n") {
				if strings.HasPrefix(line, "cluster_vending_rpc_requests_total{") && strings.Contains(line, `rpc_method="GetCluster"`) && strings.Contains(line, `code="`+test.code+`"`) {
					found = true
				}
			}
			if !found {
				t.Fatalf("scrape missing %s RPC result", test.code)
			}
		})
	}
}
