// Package server runs the cloud.v1 Connect API over HTTP.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

	"github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1/cloudv1connect"
	"github.com/yindia/cluster-vending/pkg/server/cluster"
	"github.com/yindia/cluster-vending/pkg/server/network"
	"github.com/yindia/cluster-vending/pkg/server/nodepool"
	"github.com/yindia/cluster-vending/pkg/server/service"
	"github.com/yindia/cluster-vending/pkg/server/tenant"
	"github.com/yindia/cluster-vending/pkg/version"
)

// readHeaderTimeout bounds how long a client may take to send request
// headers (slowloris). There is no read or write timeout on the whole
// request because streaming RPCs are long-lived.
const readHeaderTimeout = 10 * time.Second

// NewHandler registers every cloud.v1 service and returns the HTTP handler
// that serves them over Connect, gRPC and gRPC-Web. The services answer
// UNIMPLEMENTED for now.
func NewHandler(d service.Deps) http.Handler {
	server := connect.NewServer()
	cloudv1connect.RegisterClusterServiceHandler(server, cluster.NewClusterService(d))
	cloudv1connect.RegisterNetworkServiceHandler(server, network.NewNetworkService(d))
	cloudv1connect.RegisterNodePoolServiceHandler(server, nodepool.NewNodePoolService(d))
	cloudv1connect.RegisterTenantServiceHandler(server, tenant.NewTenantService(d))

	mux := http.NewServeMux()
	connecthttp.Mount(mux, server)
	return mux
}

// Run serves h on ln over HTTP/1.1 and cleartext HTTP/2 until ctx is
// cancelled, then shuts down, giving in-flight requests up to
// shutdownTimeout to finish before their connections are closed. It returns
// nil after a clean shutdown.
func Run(ctx context.Context, ln net.Listener, h http.Handler, shutdownTimeout time.Duration, logger *slog.Logger) error {
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true) // gRPC clients without TLS
	srv := &http.Server{
		Handler:           h,
		Protocols:         protocols,
		ReadHeaderTimeout: readHeaderTimeout,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	logger.Info("server started",
		"addr", ln.Addr().String(), "version", version.Version, "commit", version.Commit)

	select {
	case err := <-serveErr:
		return fmt.Errorf("serve on %s: %w", ln.Addr(), err)
	case <-ctx.Done():
	}

	logger.Info("shutting down", "timeout", shutdownTimeout)
	// ctx is already cancelled; the drain needs its own deadline.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		// The drain timed out (e.g. a long-lived stream): drop the
		// remaining connections so the process can exit.
		closeErr := srv.Close()
		return fmt.Errorf("shutdown after %s: %w", shutdownTimeout, errors.Join(err, closeErr))
	}
	if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve on %s: %w", ln.Addr(), err)
	}
	logger.Info("server stopped")
	return nil
}
