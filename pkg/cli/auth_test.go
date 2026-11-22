package cli

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1/cloudv1connect"
)

// whoService answers WhoAmI and remembers the Authorization header it was
// called with.
type whoService struct {
	cloudv1connect.UnimplementedTenantServiceHandler
	authorization string
	calls         int
}

func (s *whoService) WhoAmI(ctx context.Context, _ *cloudv1.WhoAmIRequest) (*cloudv1.WhoAmIResponse, error) {
	s.calls++
	if info, ok := connect.CallInfoForServerContext(ctx); ok {
		s.authorization = info.RequestHeader().Get("Authorization")
	}
	return &cloudv1.WhoAmIResponse{Subject: "user-1", Username: "Ada", Issuer: "entra"}, nil
}

func whoServer(t *testing.T) (*whoService, *httptest.Server) {
	t.Helper()
	svc := &whoService{}
	server := connect.NewServer()
	cloudv1connect.RegisterTenantServiceHandler(server, svc)
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return svc, srv
}

func TestClientsSendTheToken(t *testing.T) {
	tests := []struct {
		name   string
		source TokenSource
		want   string
	}{
		{"no source", nil, ""},
		{"a source without a token", func(context.Context) (string, error) { return "", nil }, ""},
		{"a token", func(context.Context) (string, error) { return "tok-1", nil }, "Bearer tok-1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, srv := whoServer(t)
			var opts []ClientOption
			if tt.source != nil {
				opts = append(opts, WithToken(tt.source))
			}
			var out bytes.Buffer

			err := Run(t.Context(), WhoAmI, NewClients(srv.URL, srv.Client(), opts...), Options{}, &out, OutputYAML)

			if err != nil || svc.authorization != tt.want {
				t.Fatalf("Run = %v; the server saw Authorization %q, want %q", err, svc.authorization, tt.want)
			}
			for _, want := range []string{"username: Ada", "issuer: entra", "subject: user-1"} {
				if !strings.Contains(out.String(), want) {
					t.Errorf("output lacks %q:\n%s", want, out.String())
				}
			}
		})
	}
}

// A token source that fails stops the call: it is not made anonymously.
func TestClientsStopOnATokenError(t *testing.T) {
	svc, srv := whoServer(t)
	source := func(context.Context) (string, error) { return "", errors.New("your login has expired") }

	err := Run(t.Context(), WhoAmI, NewClients(srv.URL, srv.Client(), WithToken(source)), Options{}, &bytes.Buffer{}, OutputYAML)

	if err == nil || !strings.Contains(err.Error(), "your login has expired") || svc.calls != 0 {
		t.Fatalf("Run = %v, %d calls reached the server; want the source's error and none", err, svc.calls)
	}
}

// A token is not sent over plain http to another machine.
func TestClientsRefuseToSendATokenInTheClear(t *testing.T) {
	token := func(context.Context) (string, error) { return "tok-1", nil }
	none := func(context.Context) (string, error) { return "", nil }
	requests := 0
	client := &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
		requests++
		return nil, errors.New("no network in tests")
	})}
	tests := []struct {
		server  string
		source  TokenSource
		refused bool
	}{
		{"http://api.example.com:8080", token, true},
		{"http://10.0.0.5:8080", token, true},
		{"https://api.example.com", token, false},
		{"http://127.0.0.1:8080", token, false},
		{"http://localhost:8080", token, false},
		{"http://[::1]:8080", token, false},
		{"http://api.example.com:8080", none, false},
	}
	for _, tt := range tests {
		t.Run(tt.server, func(t *testing.T) {
			requests = 0

			_, err := WhoAmI(t.Context(), NewClients(tt.server, client, WithToken(tt.source)), Options{})

			refused := err != nil && strings.Contains(err.Error(), "refusing to send your token")
			if refused != tt.refused || (tt.refused && requests != 0) || (!tt.refused && requests != 1) {
				t.Fatalf("call = %v (%d requests), want refused: %t", err, requests, tt.refused)
			}
		})
	}
	if sendsTokensSafely("://not a url") {
		t.Error("an unparsable server counts as safe")
	}
}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
