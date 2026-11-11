// Package cmd is the cluster-vending command line: the root command and its
// subcommands. It only wires flags to pkg/config and calls into pkg; the
// logic and its tests live there.
package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/yindia/cluster-vending/pkg/config"
	"github.com/yindia/cluster-vending/pkg/logging"
	"github.com/yindia/cluster-vending/pkg/version"
)

// annotationSettingsFile marks a command whose --config is a settings file.
const annotationSettingsFile = "cluster-vending/settings-file"

// globals is what the root command resolves before any subcommand runs.
type globals struct {
	cfg    config.Global
	logger *slog.Logger
	// file is the --config file, or nil; subcommands resolve their own
	// settings from it too.
	file config.LookupFunc
}

// NewRootCmd returns the cluster-vending root command with its global flags
// and every subcommand attached.
func NewRootCmd() *cobra.Command {
	g := &globals{}
	root := &cobra.Command{
		Use:   "cluster-vending",
		Short: "Vend Kubernetes clusters and networks through Crossplane",
		Long: "cluster-vending serves the cloud.v1 Connect API (TenantService,\n" +
			"NetworkService and ClusterService) and turns its requests into Crossplane\n" +
			"resources (serve), and calls that API (bootstrap, network, cluster).\n\n" +
			"Every flag can also be set through the environment variable shown in its\n" +
			"help. serve also reads a YAML settings file given with --config (keys are\n" +
			"flag names); a flag wins over the variable, which wins over the file.",
		Version: fmt.Sprintf("%s (commit %s)", version.Version, version.Commit),
		// Errors are printed once by main; a usage dump on every runtime
		// error hides the message.
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			flags := changedFlags(cmd.Flags())
			// Only commands with a settings file read one; for the API client
			// commands --config is the request input.
			var file config.LookupFunc
			if cmd.Annotations[annotationSettingsFile] == "true" {
				f, err := config.FileFrom(flags, os.LookupEnv)
				if err != nil {
					return err
				}
				file = f
			}
			cfg, err := config.LoadGlobal(flags, os.LookupEnv, file)
			if err != nil {
				return err
			}
			g.file = file
			logger, err := logging.New(cmd.ErrOrStderr(), cfg.LogLevel, cfg.LogFormat)
			if err != nil {
				return err
			}
			g.cfg, g.logger = cfg, logger
			return nil
		},
	}
	root.SetVersionTemplate("{{.Name}} {{.Version}}\n")
	addFlags(root.PersistentFlags(), config.GlobalSettings())
	root.AddCommand(
		newServeCmd(g),
		newBootstrapCmd(),
		newNetworkCmd(),
		newClusterCmd(),
		newAdminCmd(),
	)
	return root
}

// Execute runs the root command with the process arguments. ctx is passed to
// every subcommand, which stops when it is cancelled.
func Execute(ctx context.Context) error {
	return NewRootCmd().ExecuteContext(ctx)
}

// addFlags registers one string flag per setting. Values are parsed and
// validated by pkg/config, so every flag is a string here.
func addFlags(fs *pflag.FlagSet, settings []config.Setting) {
	for _, s := range settings {
		fs.String(s.Flag, s.Default, fmt.Sprintf("%s [$%s]", s.Usage, s.Env))
	}
}

// changedFlags looks up the flags given on the command line; a flag left at
// its default counts as unset, so the environment variable can apply.
func changedFlags(fs *pflag.FlagSet) config.LookupFunc {
	return func(name string) (string, bool) {
		f := fs.Lookup(name)
		if f == nil || !f.Changed {
			return "", false
		}
		return f.Value.String(), true
	}
}
