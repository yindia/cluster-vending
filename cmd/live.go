package cmd

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/yindia/cluster-vending/pkg/config"
	"github.com/yindia/cluster-vending/pkg/live"
)

func newLiveCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "live",
		Short: "Run the live reader (internal: called by the API server only)",
		Long: "Run the live reader (PROP-02 5.5) until SIGINT or SIGTERM. It reads the\n" +
			"workload clusters on request with the platform's read-only identity (each\n" +
			"cluster's \"<prefix>-live-kubeconfig\" Secret) and serves sorted, paginated\n" +
			"node lists and node totals to the API server over an internal Connect\n" +
			"endpoint. It writes nothing anywhere and holds no AWS credentials. Run one\n" +
			"replica: its views, cooldowns and limits are this process's memory.",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{annotationSettingsFile: "true"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.LoadLive(changedFlags(cmd.Flags()), os.LookupEnv, g.file)
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return live.Run(ctx, live.Options{Addr: cfg.Addr, ShutdownTimeout: cfg.ShutdownTimeout,
				Kubeconfig: g.cfg.Kubeconfig, KubeContext: g.cfg.KubeContext, Limits: live.DefaultLimits(), Logger: g.logger})
		},
	}
	addFlags(cmd.Flags(), config.GlobalSettings())
	addFlags(cmd.Flags(), config.LiveSettings())
	return cmd
}
