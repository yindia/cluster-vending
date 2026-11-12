package cmd

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/yindia/cluster-vending/controller"
	"github.com/yindia/cluster-vending/pkg/config"
)

func newControllerCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "controller",
		Short: "Run the controller manager of the platform's custom resources",
		Long: "Run the controller manager for the cloud.cluster-vending.io kinds\n" +
			"(CloudAccount, Network, Cluster) until SIGINT or SIGTERM. It reconciles\n" +
			"the objects the API server writes with --store=kubernetes (and adopts\n" +
			"Networks and Clusters created with kubectl): it creates, updates and\n" +
			"deletes their cloud resources through Crossplane. It installs the\n" +
			"kinds' CRDs and waits for the Crossplane AWS providers, which the API\n" +
			"server (serve --install-providers) or an operator installs.",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{annotationSettingsFile: "true"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.LoadController(changedFlags(cmd.Flags()), os.LookupEnv, g.file)
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return controller.Run(ctx, controller.Options{
				Kubeconfig: g.cfg.Kubeconfig, KubeContext: g.cfg.KubeContext,
				MetricsAddr: cfg.MetricsAddr, ProbeAddr: cfg.ProbeAddr,
				LeaderElect: cfg.LeaderElect, LeaseNamespace: cfg.LeaseNamespace,
				RenderTarget: cfg.RenderTarget, RenderRollout: cfg.RenderRollout, AWS: cfg.AWS, Logger: g.logger,
			})
		},
	}
	addFlags(cmd.Flags(), config.ControllerSettings())
	return cmd
}
