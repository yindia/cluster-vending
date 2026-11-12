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

	awscloud "github.com/yindia/cluster-vending/pkg/cloud/aws"
	"github.com/yindia/cluster-vending/pkg/config"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database"
	"github.com/yindia/cluster-vending/pkg/database/implementation"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/loop"
	"github.com/yindia/cluster-vending/pkg/server"
	"github.com/yindia/cluster-vending/pkg/server/cluster"
	"github.com/yindia/cluster-vending/pkg/server/network"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

func newServeCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the Connect API server",
		Long: "Run the Connect API server until SIGINT or SIGTERM, then drain in-flight\n" +
			"requests; a second signal exits at once. It speaks Connect, gRPC and\n" +
			"gRPC-Web, over HTTP/1.1 and cleartext HTTP/2.\n\n" +
			"--aws-management-account-id and --aws-management-role-arn are required.",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{annotationSettingsFile: "true"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.LoadServe(changedFlags(cmd.Flags()), os.LookupEnv, g.file)
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

			management := server.ManagementOptions{
				Kubeconfig: g.cfg.Kubeconfig, KubeContext: g.cfg.KubeContext,
				InstallProviders: cfg.InstallProviders, Logger: g.logger,
				ProviderRuntime: crossplaneaws.ProviderRuntime{
					ManagementRoleARN: cfg.AWS.ManagementRoleARN, Poll: cfg.Provider.Poll, EC2Poll: cfg.Provider.EC2Poll,
					MaxReconcileRate: cfg.Provider.MaxReconcileRate, CPURequest: cfg.Provider.CPURequest,
					MemoryRequest: cfg.Provider.MemoryRequest, CPULimit: cfg.Provider.CPULimit, MemoryLimit: cfg.Provider.MemoryLimit,
				},
				LeaderElection: kube.LeaderElection{Enabled: cfg.LeaderElect, Namespace: cfg.LeaseNamespace},
			}
			store, closeStore, err := openStore(ctx, cfg, management)
			if err != nil {
				return err
			}
			defer closeStore()

			mgr, err := server.ConnectManagement(ctx, management)
			if err != nil {
				return fmt.Errorf("%w (--%s=false skips installing the providers)", err, config.FlagInstallProviders)
			}

			ln, err := new(net.ListenConfig).Listen(ctx, "tcp", cfg.HTTPAddr)
			if err != nil {
				return fmt.Errorf("listen on %s: %w", cfg.HTTPAddr, err)
			}
			g.logger.Info("configuration loaded",
				"kubeconfig", g.cfg.Kubeconfig,
				"context", g.cfg.KubeContext,
				"aws_management_account_id", cfg.AWS.ManagementAccountID,
				"aws_management_role_arn", cfg.AWS.ManagementRoleARN,
				"store", cfg.Store,
				// Never log the DSN: it may hold a password.
				"database_driver", cfg.Database.Driver)
			discovery, err := awscloud.New(ctx, awscloud.Config{})
			if err != nil {
				return err
			}
			if cfg.AWS.StaticKeys != nil {
				g.logger.Warn("AWS keys found in the environment: the server's own AWS lookups use them for every account (local testing only)")
			}
			renderTarget := cfg.RenderTarget
			if renderTarget == 0 {
				renderTarget = crossplaneaws.RenderVersion
			}
			deps := service.Deps{
				Store:        store,
				Kube:         mgr.GetClient(), // reads from the informer cache
				KubeReader:   mgr.GetAPIReader(),
				AWS:          cfg.AWS,
				RenderTarget: renderTarget, RenderRollout: cfg.RenderRollout,
				AWSDiscovery: discovery, AWSClusterDiscovery: discovery,
				AWSClusters: discovery,
				Logger:      g.logger,
			}
			err = mgr.Add(&loop.Loop{
				Store: deps.Store, Networks: network.NewNetworkService(deps), Clusters: cluster.NewClusterService(deps),
				NetworkStates: network.PendingStates(), ClusterStates: cluster.PendingStates(),
				RenderTarget: deps.RenderTarget, Interval: cfg.LoopInterval, Logger: g.logger,
			})
			if err != nil {
				return err
			}
			var ready atomic.Bool
			done, err := server.StartManagement(ctx, mgr, &ready)
			if err != nil {
				return err
			}
			// The API stops with the informers and controller.
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			go func() {
				if err := <-done; err != nil {
					g.logger.Error("management cluster manager stopped", "error", err)
					cancel()
				}
			}()
			return server.Run(ctx, ln, server.NewHandler(deps, ready.Load), cfg.ShutdownTimeout, g.logger)
		},
	}
	addFlags(cmd.Flags(), config.ServeSettings())
	return cmd
}

// openStore opens where the server keeps its state (config.Serve.Store):
// the database, migrated, or the custom resources of the management
// cluster, for which no database is opened. The returned func closes it.
func openStore(ctx context.Context, cfg config.Serve, management server.ManagementOptions) (repository.Store, func(), error) {
	if cfg.Store == config.StoreKubernetes {
		store, err := server.ConnectStore(ctx, management)
		return store, func() {}, err
	}
	db, err := database.Open(ctx, cfg.Database)
	if err != nil {
		return nil, nil, err
	}
	closeDB := func() {
		if err := database.Close(db); err != nil {
			management.Logger.Error("close database", "error", err)
		}
	}
	if err := database.Migrate(ctx, db); err != nil {
		closeDB()
		return nil, nil, err
	}
	return implementation.New(db), closeDB, nil
}
