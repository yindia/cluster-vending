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

	"github.com/yindia/cluster-vending/pkg/config"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database"
	"github.com/yindia/cluster-vending/pkg/database/implementation"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/loop"
	"github.com/yindia/cluster-vending/pkg/management"
	reconcilecluster "github.com/yindia/cluster-vending/pkg/reconcile/cluster"
	netreconcile "github.com/yindia/cluster-vending/pkg/reconcile/network"
	"github.com/yindia/cluster-vending/pkg/server"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

func newServeCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the Connect API server",
		Long: "Run the Connect API server until SIGINT or SIGTERM, then drain in-flight\n" +
			"requests; a second signal exits at once. It speaks Connect, gRPC and\n" +
			"gRPC-Web, over HTTP/1.1 and cleartext HTTP/2.\n\n" +
			"The server validates requests and stores the desired state; it makes\n" +
			"no AWS call and needs no AWS setting or credentials. What reconciles is\n" +
			"`cluster-vending controller` (with --store=kubernetes). Only while the\n" +
			"server runs the background loop itself (--background-loop, the default\n" +
			"with --store=database) is it the reconciler too, and then it takes the\n" +
			"controller's settings: --aws-management-account-id and\n" +
			"--aws-management-role-arn are required, and it needs AWS credentials.",
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

			options := management.Options{
				Kubeconfig: g.cfg.Kubeconfig, KubeContext: g.cfg.KubeContext,
				// The providers are the reconciler's to install: the
				// controller, or this server while it runs the loop.
				InstallProviders: cfg.BackgroundLoop && cfg.InstallProviders, InstallKinds: cfg.BackgroundLoop, Logger: g.logger,
				ProviderRuntime: crossplaneaws.ProviderRuntime{
					ManagementRoleARN: cfg.AWS.ManagementRoleARN, Poll: cfg.Provider.Poll, EC2Poll: cfg.Provider.EC2Poll,
					MaxReconcileRate: cfg.Provider.MaxReconcileRate, CPURequest: cfg.Provider.CPURequest,
					MemoryRequest: cfg.Provider.MemoryRequest, CPULimit: cfg.Provider.CPULimit, MemoryLimit: cfg.Provider.MemoryLimit,
				},
				LeaderElection: kube.LeaderElection{Enabled: cfg.LeaderElect, Namespace: cfg.LeaseNamespace},
			}
			store, closeStore, err := openStore(ctx, cfg, options)
			if err != nil {
				return err
			}
			defer closeStore()

			mgr, err := management.Connect(ctx, options)
			if err != nil {
				return fmt.Errorf("%w (the Crossplane AWS providers are installed by `cluster-vending controller`)", err)
			}

			ln, err := new(net.ListenConfig).Listen(ctx, "tcp", cfg.HTTPAddr)
			if err != nil {
				return fmt.Errorf("listen on %s: %w", cfg.HTTPAddr, err)
			}
			g.logger.Info("configuration loaded",
				"kubeconfig", g.cfg.Kubeconfig,
				"context", g.cfg.KubeContext,
				"store", cfg.Store,
				"background_loop", cfg.BackgroundLoop,
				// Never log the DSN: it may hold a password.
				"database_driver", cfg.Database.Driver)
			renderTarget := cfg.RenderTarget
			if renderTarget == 0 {
				renderTarget = crossplaneaws.RenderVersion
			}
			deps := service.Deps{
				Store:        store,
				Kube:         mgr.GetClient(), // reads from the informer cache
				KubeReader:   mgr.GetAPIReader(),
				RenderTarget: renderTarget, RenderRollout: cfg.RenderRollout,
				Logger: g.logger,
			}
			if cfg.BackgroundLoop {
				// The server is the reconciler too: only the loop's
				// reconcilers get the AWS lookups, the handlers never do.
				reconciling, err := management.WithAWS(ctx, deps, "")
				if err != nil {
					return err
				}
				g.logger.Info("the server reconciles (background loop): it needs AWS credentials and installs the providers",
					"aws_management_account_id", cfg.AWS.ManagementAccountID, "aws_management_role_arn", cfg.AWS.ManagementRoleARN,
					"aws_keys_in_environment", cfg.AWS.StaticKeys != nil)
				err = mgr.Add(&loop.Loop{
					Store: deps.Store, Networks: netreconcile.New(reconciling), Clusters: reconcilecluster.New(reconciling),
					NetworkStates: netreconcile.PendingStates(), ClusterStates: reconcilecluster.PendingStates(),
					RenderTarget: deps.RenderTarget, Interval: cfg.LoopInterval, Logger: g.logger,
				})
				if err != nil {
					return err
				}
			} else {
				// One reconciler per resource: with the Kubernetes store that
				// is the controller.
				g.logger.Info("the server does not reconcile: run `cluster-vending controller` (or --background-loop=true)",
					"store", cfg.Store)
			}
			var ready atomic.Bool
			done, err := management.Start(ctx, mgr, &ready)
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
func openStore(ctx context.Context, cfg config.Serve, options management.Options) (repository.Store, func(), error) {
	if cfg.Store == config.StoreKubernetes {
		store, err := management.ConnectStore(ctx, options)
		return store, func() {}, err
	}
	db, err := database.Open(ctx, cfg.Database)
	if err != nil {
		return nil, nil, err
	}
	closeDB := func() {
		if err := database.Close(db); err != nil {
			options.Logger.Error("close database", "error", err)
		}
	}
	if err := database.Migrate(ctx, db); err != nil {
		closeDB()
		return nil, nil, err
	}
	return implementation.New(db), closeDB, nil
}
