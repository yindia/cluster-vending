package cmd

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/yindia/cluster-vending/pkg/auth"
	"github.com/yindia/cluster-vending/pkg/config"
	"github.com/yindia/cluster-vending/pkg/management"
	"github.com/yindia/cluster-vending/pkg/reconcile"
	"github.com/yindia/cluster-vending/pkg/server"
	"github.com/yindia/cluster-vending/pkg/server/tenant"
)

func newServeCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the Connect API server",
		Long: "Run the Connect API server until SIGINT or SIGTERM, then drain in-flight\n" +
			"requests; a second signal exits at once. It speaks Connect, gRPC and\n" +
			"gRPC-Web, over HTTP/1.1 and cleartext HTTP/2.\n\n" +
			"The server validates requests and stores the desired state as\n" +
			"CloudAccount, Network and Cluster objects in the tenants' namespaces of\n" +
			"the management cluster. It reconciles nothing, installs nothing and\n" +
			"calls AWS only to read the scan results in the clusters' results\n" +
			"buckets (read-only, through each account's tenant role): `cluster-vending\n" +
			"controller` does the rest, and installs the custom resource definitions\n" +
			"the server waits for at startup.\n\n" +
			"Who may call the API is the \"auth\" section of the settings file\n" +
			"(--config): OIDC issuers whose bearer tokens are accepted and the rules\n" +
			"that give their callers roles. Without it nothing is authenticated\n" +
			"(docs/architecture.md#authentication).",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{annotationSettingsFile: "true"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.LoadServe(changedFlags(cmd.Flags()), os.LookupEnv, g.file)
			if err != nil {
				return err
			}
			warnNodeGroups(g.logger, cfg.NodeGroups)
			// Who may call the API: the "auth" section of the settings file.
			authCfg, err := config.AuthFrom(changedFlags(cmd.Flags()), os.LookupEnv)
			if err != nil {
				return err
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			// Unregister on the first signal, so a second one during the
			// drain gets the default action and kills the process.
			go func() {
				<-ctx.Done()
				stop()
			}()

			// The store's kinds and the provider kinds are the controller's
			// to install: the server waits for them and installs nothing.
			options := management.Options{Kubeconfig: g.cfg.Kubeconfig, KubeContext: g.cfg.KubeContext, Logger: g.logger}
			store, err := management.ConnectStore(ctx, options)
			if err != nil {
				return fmt.Errorf("%w (they are installed by `cluster-vending controller`)", err)
			}
			mgr, err := management.Connect(ctx, options)
			if err != nil {
				return fmt.Errorf("%w (the Crossplane AWS providers are installed by `cluster-vending controller`)", err)
			}

			ln, err := new(net.ListenConfig).Listen(ctx, "tcp", cfg.HTTPAddr)
			if err != nil {
				return fmt.Errorf("listen on %s: %w", cfg.HTTPAddr, err)
			}
			g.logger.Info("configuration loaded", "kubeconfig", g.cfg.Kubeconfig, "context", g.cfg.KubeContext)
			// The catalog and the releases are read at the API server, not
			// from the cache: it holds the kinds the reconcilers watch.
			catalog := management.PlatformSource(mgr.GetAPIReader())
			deps := reconcile.Deps{
				Store:      store,
				Kube:       mgr.GetClient(), // reads from the informer cache
				KubeReader: mgr.GetAPIReader(),
				// A cluster's Kubernetes version is checked against the
				// minor its release channel offers.
				Releases: catalog,
				Logger:   g.logger,
				// The node group layout: the allowlist of user groups, the system group.
				NodeGroups: cfg.NodeGroups,
			}
			var ready atomic.Bool
			done, err := management.Start(ctx, mgr, &ready)
			if err != nil {
				return err
			}
			// The API stops with the informers.
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			go func() {
				if err := <-done; err != nil {
					g.logger.Error("management cluster manager stopped", "error", err)
					cancel()
				}
			}()
			g.logger.Info("authentication", "mode", authCfg.Mode, "issuers", len(authCfg.Issuers))
			if authCfg.Mode == auth.ModeNone {
				g.logger.Warn("authentication is off (auth.mode none): every caller has full access to every tenant; for local development only")
			}
			for _, ignored := range authCfg.Ignored {
				g.logger.Warn(ignored)
			}
			// The scan results are read from the clusters' buckets, through
			// each account's tenant role, read-only (docs/architecture.md#results-in-s3);
			// nothing else of AWS is called.
			results, err := server.NewResultsStore(ctx, cfg.AWSAccess)
			if err != nil {
				return err
			}
			handler := server.NewHandler(deps, ready.Load, server.WithAuth(authCfg.Mode, auth.NewVerifier(authCfg)),
				server.WithCatalog(catalog), server.WithResults(results),
				// What account onboarding prints.
				server.WithOnboarding(tenant.Onboarding{ManagementRoleARN: cfg.ManagementRoleARN, ServerPrincipalARN: cfg.ServerPrincipalARN()}))
			return server.Run(ctx, ln, handler, cfg.ShutdownTimeout, g.logger)
		},
	}
	addFlags(cmd.Flags(), config.GlobalSettings())
	addFlags(cmd.Flags(), config.ServeSettings())
	return cmd
}
