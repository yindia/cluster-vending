package cmd

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/yindia/cluster-vending/controller"
	"github.com/yindia/cluster-vending/pkg/config"
	"github.com/yindia/cluster-vending/pkg/kube"
)

func newControllerCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "controller",
		Short: "Run the controller manager of the platform's custom resources",
		Long: "Run the controller manager for the cloud.cluster-vending.io kinds\n" +
			"(CloudAccount, Network, Cluster) until SIGINT or SIGTERM. The\n" +
			"reconcilers are not implemented yet: the manager watches the kinds and\n" +
			"changes nothing. Their CRDs (controller/config/crd/bases) must be\n" +
			"installed in the management cluster.",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{annotationSettingsFile: "true"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.LoadController(changedFlags(cmd.Flags()), os.LookupEnv, g.file)
			if err != nil {
				return err
			}
			restConfig, err := kube.RESTConfig(g.cfg.Kubeconfig, g.cfg.KubeContext)
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return controller.Run(ctx, controller.Options{
				Config: restConfig, MetricsAddr: cfg.MetricsAddr, ProbeAddr: cfg.ProbeAddr,
				LeaderElect: cfg.LeaderElect, LeaseNamespace: cfg.LeaseNamespace, Logger: g.logger,
			})
		},
	}
	addFlags(cmd.Flags(), config.ControllerSettings())
	return cmd
}
