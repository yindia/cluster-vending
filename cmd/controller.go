package cmd

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/yindia/cluster-vending/pkg/catalog"
	"github.com/yindia/cluster-vending/pkg/config"
	"github.com/yindia/cluster-vending/pkg/controller/manager"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/kube"
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
			"--aws-management-role-arn is required.\n\n" +
			"Two read-only modes run no controller. --export reads the management\n" +
			"cluster (--kubeconfig) and writes its ComponentDefinitions, Releases and\n" +
			"ReleaseChannels as clean manifests: one stream on stdout, or the\n" +
			"config/catalog layout under --export-dir. --check -f <path|-> reads\n" +
			"rendered manifests (kustomize build ... | cluster-vending controller\n" +
			"--check -f -), makes no cluster call, prints every problem and exits 1 if\n" +
			"there is any; with --config it also judges the system node group's size.",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{annotationSettingsFile: "true"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			export, _ := cmd.Flags().GetBool(flagExport)
			check, _ := cmd.Flags().GetBool(flagCheck)
			switch {
			case export && check:
				return fmt.Errorf("--%s and --%s are separate modes: use one", flagExport, flagCheck)
			case export:
				return runCatalogExport(cmd, g)
			case check:
				return runCatalogCheck(cmd, g)
			}
			cfg, err := config.LoadController(changedFlags(cmd.Flags()), os.LookupEnv, g.file)
			if err != nil {
				return err
			}
			warnNodeGroups(g.logger, cfg.NodeGroups)
			// Which groups sub-tenants may name: the "naas" section of the file.
			naasCfg, err := config.NaaSFrom(changedFlags(cmd.Flags()), os.LookupEnv)
			if err != nil {
				return err
			}
			// How the Crossplane providers run: the "providers" section.
			providers, err := config.ProvidersFrom(changedFlags(cmd.Flags()), os.LookupEnv)
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return manager.Run(ctx, manager.Options{
				Kubeconfig: g.cfg.Kubeconfig, KubeContext: g.cfg.KubeContext,
				MetricsAddr: cfg.MetricsAddr, ProbeAddr: cfg.ProbeAddr,
				LeaderElect: cfg.LeaderElect, LeaseNamespace: cfg.LeaseNamespace, ServiceAccount: cfg.ServiceAccount,
				AWS: cfg.AWS, AWSAccess: cfg.AWSAccess, Logger: g.logger, InstallProviders: cfg.InstallProviders, InstallCatalog: cfg.InstallCatalog,
				RecoverStuckReleases: cfg.RecoverStuckReleases, HelmBackoffMax: cfg.HelmBackoffMax, HelmMaxFailures: cfg.HelmMaxFailures, Recommendations: cfg.Recommendations,
				NodeGroups:  cfg.NodeGroups,
				NaaS:        naasCfg,
				Providers:   providers,
				ScanWorkers: cfg.ScanWorkers, KubeQPS: cfg.KubeQPS, KubeBurst: cfg.KubeBurst,
				ReconcileWorkers: cfg.ReconcileWorkers, AutomaticUpgrades: cfg.AutomaticUpgrades, UpgradeTimeout: cfg.UpgradeTimeout, TeardownTimeout: cfg.TeardownTimeout,
			})
		},
	}
	addFlags(cmd.Flags(), config.GlobalSettings())
	addFlags(cmd.Flags(), config.ControllerSettings())
	cmd.Flags().Bool(flagExport, false, "write the management cluster's component definitions, releases and channels as manifests, and exit (read-only)")
	cmd.Flags().String(flagExportDir, "", "with --export: write the config/catalog layout under this directory instead of one stream on stdout")
	cmd.Flags().Bool(flagCheck, false, "check rendered catalog manifests (-f) and the node groups of --config, print every problem, exit 1 if any (no cluster call)")
	cmd.Flags().StringP(flagFilename, "f", "", "with --check: the manifests, a file or - for stdin")
	return cmd
}

// Flags of the controller's two catalog modes.
const (
	flagExport    = "export"
	flagExportDir = "export-dir"
	flagCheck     = "check"
	flagFilename  = "filename"
)

// runCatalogExport is controller --export: it reads the management cluster
// and writes the catalog (pkg/catalog RunExport).
func runCatalogExport(cmd *cobra.Command, g *globals) error {
	restConfig, err := kube.RESTConfig(g.cfg.Kubeconfig, g.cfg.KubeContext)
	if err != nil {
		return err
	}
	c, err := kube.New(restConfig)
	if err != nil {
		return err
	}
	dir, _ := cmd.Flags().GetString(flagExportDir)
	return catalog.RunExport(cmd.Context(), c, dir, cmd.OutOrStdout(), cmd.ErrOrStderr())
}

// runCatalogCheck is controller --check -f: the offline rules over rendered
// manifests (pkg/catalog RunCheck), with the node groups of the config file.
func runCatalogCheck(cmd *cobra.Command, g *globals) error {
	name, _ := cmd.Flags().GetString(flagFilename)
	if name == "" {
		return fmt.Errorf("--%s needs -f <path|->: the rendered manifests to check", flagCheck)
	}
	serve, err := config.LoadServe(changedFlags(cmd.Flags()), os.LookupEnv, g.file)
	if err != nil {
		return err
	}
	in := cmd.InOrStdin()
	if name != "-" {
		f, err := os.Open(name)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		in = f
	}
	return catalog.RunCheck(in, cmd.OutOrStdout(), catalog.Options{
		RenderVersion: crossplaneaws.RenderVersion, SystemGroup: &serve.NodeGroups.System, PlatformPool: &serve.NodeGroups.Platform})
}
