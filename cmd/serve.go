package cmd

import (
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	awscloud "github.com/yindia/cluster-vending/pkg/cloud/aws"
	"github.com/yindia/cluster-vending/pkg/config"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database"
	"github.com/yindia/cluster-vending/pkg/database/implementation"
	"github.com/yindia/cluster-vending/pkg/kube"
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

			db, err := database.Open(ctx, cfg.Database)
			if err != nil {
				return err
			}
			defer func() {
				if err := database.Close(db); err != nil {
					g.logger.Error("close database", "error", err)
				}
			}()
			if err := database.Migrate(ctx, db); err != nil {
				return err
			}

			kubeClient, err := kube.Connect(g.cfg.Kubeconfig, g.cfg.KubeContext)
			if err != nil {
				return err
			}
			if cfg.InstallProviders {
				res, err := crossplaneaws.InstallProviders(ctx, kubeClient, cfg.AWS.ManagementRoleARN)
				if err != nil {
					return fmt.Errorf("install the Crossplane AWS providers (is Crossplane installed? --%s=false skips this): %w",
						config.FlagInstallProviders, err)
				}
				g.logger.Info("crossplane providers", "created", res.Created, "existing", res.Existing)
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
				// Never log the DSN: it may hold a password.
				"database_driver", cfg.Database.Driver)
			discovery, err := awscloud.New(ctx, awscloud.Config{})
			if err != nil {
				return err
			}
			if cfg.AWS.StaticKeys != nil {
				g.logger.Warn("AWS keys found in the environment: the server's own AWS lookups use them for every account (local testing only)")
			}
			handler := server.NewHandler(service.Deps{
				Store:        implementation.New(db),
				Kube:         kubeClient,
				AWS:          cfg.AWS,
				AWSDiscovery: discovery,
				AWSClusters:  discovery,
				Logger:       g.logger,
			})
			return server.Run(ctx, ln, handler, cfg.ShutdownTimeout, g.logger)
		},
	}
	addFlags(cmd.Flags(), config.ServeSettings())
	return cmd
}
