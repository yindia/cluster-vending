package live

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/kube"
)

// readHeaderTimeout bounds how long a caller may take to send its headers.
const readHeaderTimeout = 10 * time.Second

// Options are how `cluster-vending live` runs.
type Options struct {
	// Addr is the listen address of the internal endpoint.
	Addr string
	// ShutdownTimeout is what in-flight reads get on shutdown.
	ShutdownTimeout time.Duration
	// Kubeconfig and KubeContext reach the management cluster (empty:
	// in-cluster).
	Kubeconfig, KubeContext string
	Limits                  Limits
	Logger                  *slog.Logger
}

// Run serves the live reader until ctx is cancelled. It reads the management
// cluster without a cache (Cluster objects, ClusterAuths, the live
// kubeconfig Secrets) and the workload clusters on request; it writes
// nothing anywhere.
func Run(ctx context.Context, o Options) error {
	cfg, err := kube.RESTConfig(o.Kubeconfig, o.KubeContext)
	if err != nil {
		return err
	}
	scheme, err := kube.Scheme()
	if err != nil {
		return err
	}
	management, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("management cluster client: %w", err)
	}
	r := New(&KubeOpener{Management: management, Limits: o.Limits}, o.Limits, o.Logger)
	ln, err := new(net.ListenConfig).Listen(ctx, "tcp", o.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", o.Addr, err)
	}
	srv := &http.Server{Handler: NewHandler(r), ReadHeaderTimeout: readHeaderTimeout,
		ErrorLog: slog.NewLogLogger(o.Logger.Handler(), slog.LevelError)}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	o.Logger.Info("live reader started", "addr", ln.Addr().String(), "replicas", "one (PROP-02 5.5)")
	select {
	case err := <-served:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), o.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil {
		return fmt.Errorf("shut down: %w", err)
	}
	if err := <-served; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
