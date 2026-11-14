package cmd

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/yindia/cluster-vending/pkg/config"
	"github.com/yindia/cluster-vending/pkg/controller/manager"
)

func newControllerCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "controller",
		Short: "Run the controller manager of the platform's custom resources",
		Long: "Run the controller manager for the cloud.cluster-vending.io kinds\n" +
			"(CloudAccount, Network, Cluster) until SIGINT or SIGTERM. It reconciles\n" +
			"the objects the API server writes (and adopts\n" +
			"Networks and Clusters created with kubectl): it creates, updates and\n" +
			"deletes their cloud resources through Crossplane. It installs the\n" +
			"kinds' CRDs, the Crossplane AWS providers (--install-providers) and the\n" +
			"component catalog (--install-catalog), and\n" +
			"it is the only command that looks at AWS: it discovers imported VPCs and\n" +
			"EKS clusters and checks names and versions against EKS.\n\n" +
			"Its AWS credentials come from the AWS SDK's default chain: IRSA in\n" +
			"production; for local testing export AWS_ACCESS_KEY_ID and\n" +
			"AWS_SECRET_ACCESS_KEY (never a flag or a file). Whatever they are, every\n" +
			"lookup assumes the tenant role of the account it reads.\n\n" +
			"The Crossplane AWS providers authenticate with IRSA as the management\n" +
			"role, or, with --provider-credentials-secret, with the keys in that\n" +
			"Secret (a local kind cluster); either way they assume the tenant role.\n\n" +
			"--aws-management-role-arn is required.",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{annotationSettingsFile: "true"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.LoadController(changedFlags(cmd.Flags()), os.LookupEnv, g.file)
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return manager.Run(ctx, manager.Options{
				Kubeconfig: g.cfg.Kubeconfig, KubeContext: g.cfg.KubeContext,
				MetricsAddr: cfg.MetricsAddr, ProbeAddr: cfg.ProbeAddr,
				LeaderElect: cfg.LeaderElect, LeaseNamespace: cfg.LeaseNamespace, ServiceAccount: cfg.ServiceAccount,
				AWS: cfg.AWS, Logger: g.logger, InstallProviders: cfg.InstallProviders, InstallCatalog: cfg.InstallCatalog,
				RecoverStuckReleases: cfg.RecoverStuckReleases, Recommendations: cfg.Recommendations,
			})
		},
	}
	addFlags(cmd.Flags(), config.ControllerSettings())
	return cmd
}
