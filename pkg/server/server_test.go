package server_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yindia/cluster-vending/pkg/server"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

const testTimeout = 5 * time.Second

func listen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return ln
}

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

func waitFor(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(testTimeout):
		t.Fatal("Run did not return")
		return nil
	}
}

func TestRunAnswersRPCsAndStopsOnCancel(t *testing.T) {
	ln := listen(t)
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx, ln, server.NewHandler(service.Deps{}), time.Second, logger) }()

	// Every service is mounted: each rejects an empty request
	// (INVALID_ARGUMENT, 400).
	client := &http.Client{Timeout: testTimeout}
	for rpc, want := range map[string]int{
		"cloud.v1.ClusterService/GetCluster":       http.StatusBadRequest,
		"cloud.v1.NetworkService/ImportNetwork":    http.StatusBadRequest,
		"cloud.v1.TenantService/BootstrapTenant":   http.StatusBadRequest,
		"cloud.v1.TenantService/ListCloudAccounts": http.StatusBadRequest,
	} {
		url := "http://" + ln.Addr().String() + "/" + rpc
		res, err := client.Post(url, "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatalf("POST %s: %v", url, err)
		}
		// Drain the body: an unread one keeps the connection busy, and
		// shutdown would wait for it.
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
		if res.StatusCode != want {
			t.Errorf("%s: status = %d, want %d", rpc, res.StatusCode, want)
		}
	}

	cancel()
	if err := waitFor(t, done); err != nil {
		t.Fatalf("Run returned %v after cancel, want nil", err)
	}
	for _, want := range []string{"server started", "server stopped"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("logs %q do not contain %q", logs.String(), want)
		}
	}
}

func TestRunReturnsErrorWhenListenerFails(t *testing.T) {
	ln := listen(t)
	_ = ln.Close() // Serve fails at once on a closed listener

	if err := server.Run(t.Context(), ln, server.NewHandler(service.Deps{}), time.Second, discard()); err == nil {
		t.Fatal("want error from a closed listener")
	}
}

func TestRunForceClosesAfterShutdownTimeout(t *testing.T) {
	ln := listen(t)
	entered := make(chan struct{})
	// Stands in for a long-lived stream: it only returns once its
	// connection is closed.
	blocking := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx, ln, blocking, 50*time.Millisecond, discard()) }()
	go func() {
		client := &http.Client{Timeout: testTimeout}
		if res, err := client.Get("http://" + ln.Addr().String()); err == nil {
			_ = res.Body.Close()
		}
	}()

	<-entered
	cancel()

	err := waitFor(t, done)
	if err == nil || !strings.Contains(err.Error(), "shutdown") {
		t.Fatalf("Run error = %v, want a shutdown timeout error", err)
	}
}
