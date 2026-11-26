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
	// MetricsAddr, when set, is a second listener serving only /metrics
	// and the probes (NewMetricsHandler).
	MetricsAddr string
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
	servers := []*http.Server{{Handler: NewHandler(r), ReadHeaderTimeout: readHeaderTimeout,
		ErrorLog: slog.NewLogLogger(o.Logger.Handler(), slog.LevelError)}}
	addrs := []string{o.Addr}
	if o.MetricsAddr != "" {
		servers = append(servers, &http.Server{Handler: NewMetricsHandler(), ReadHeaderTimeout: readHeaderTimeout,
			ErrorLog: slog.NewLogLogger(o.Logger.Handler(), slog.LevelError)})
		addrs = append(addrs, o.MetricsAddr)
	}
	listeners := make([]net.Listener, 0, len(addrs))
	for _, addr := range addrs {
		ln, err := new(net.ListenConfig).Listen(ctx, "tcp", addr)
		if err != nil {
			for _, open := range listeners {
				_ = open.Close()
			}
			return fmt.Errorf("listen on %s: %w", addr, err)
		}
		listeners = append(listeners, ln)
	}
	served := make(chan error, len(servers))
	for i, srv := range servers {
		go func() { served <- srv.Serve(listeners[i]) }()
	}
	o.Logger.Info("live reader started", "addr", listeners[0].Addr().String(), "metrics_addr", o.MetricsAddr, "replicas", "one (PROP-02 5.5)")
	select {
	case err := <-served:
		for _, srv := range servers {
			_ = srv.Close()
		}
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), o.ShutdownTimeout)
	defer cancel()
	var errs []error
	for _, srv := range servers {
		if err := srv.Shutdown(shutdown); err != nil {
			errs = append(errs, fmt.Errorf("shut down: %w", err))
		}
	}
	for range servers {
		if err := <-served; err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
