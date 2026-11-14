package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/yindia/cluster-vending/pkg/cli"
	"github.com/yindia/cluster-vending/pkg/config"
)

// componentVerb is one subcommand of `component`.
type componentVerb struct {
	use, short string
	run        cli.Command
	args       cobra.PositionalArgs
	// name: the argument is a catalog name, not a component ID.
	name bool
	// flags adds the verb's own flags.
	flags func(*cobra.Command, *cli.Options)
}

// newComponentCmd is the component catalog and a tenant's components
// (ComponentService). A component is enabled by its catalog name on a
// cluster and addressed by its ID from then on.
func newComponentCmd() *cobra.Command {
	var o cli.Options
	var output string
	group := &cobra.Command{Use: "component", Args: cobra.NoArgs,
		Short: "The component catalog, and the components enabled on a cluster (ComponentService)"}
	pf := group.PersistentFlags()
	pf.StringVar(&o.Tenant, "tenant", "", "tenant ID (required, except by values)")
	pf.StringVarP(&output, "output", "o", cli.OutputTable, cli.OutputHelp)
	for _, s := range config.ClientSettings() {
		pf.String(s.Flag, s.Default, fmt.Sprintf("%s [$%s]", s.Usage, s.Env))
	}

	cluster := func(usage string) func(*cobra.Command, *cli.Options) {
		return func(cmd *cobra.Command, o *cli.Options) { cmd.Flags().StringVar(&o.Cluster, "cluster", "", usage) }
	}
	values := func(cmd *cobra.Command, o *cli.Options) {
		cmd.Flags().StringVarP(&o.Input, "file", "f", "", "values file: a YAML or JSON object (component values <name> prints the one to start from)")
		cmd.Flags().BoolVar(&o.ValidateOnly, "validate-only", false, "check the request and say what would be stored; nothing changes")
	}
	wait := func(cmd *cobra.Command, o *cli.Options) {
		cmd.Flags().BoolVar(&o.Wait, "wait", false, "wait until the component is ready (disable: gone), printing its progress to stderr")
		cmd.Flags().DurationVar(&o.Timeout, "timeout", cli.DefaultWaitTimeout, "how long --wait waits")
	}
	etag := func(cmd *cobra.Command, o *cli.Options) {
		cmd.Flags().StringVar(&o.ETag, "etag", "", "fail unless the component still has this etag")
	}
	all := func(fs ...func(*cobra.Command, *cli.Options)) func(*cobra.Command, *cli.Options) {
		return func(cmd *cobra.Command, o *cli.Options) {
			for _, f := range fs {
				f(cmd, o)
			}
		}
	}

	verbs := []componentVerb{
		{use: "catalog", short: "List every component that can be installed: who manages it (the platform's cannot be enabled or disabled), whether it takes values, what it requires",
			run: cli.ComponentCatalog, args: cobra.NoArgs,
			flags: cluster("also show the version this cluster's release gives each component (cluster ID)")},
		{use: "template <name>", short: "Print a component's values template (YAML, with the platform's comments) to edit and send with enable -f",
			run: cli.ComponentTemplate, args: cobra.ExactArgs(1), name: true},
		{use: "enable <name>", short: "Enable a component on --cluster, with the values of -f if any; prints the component with its id",
			run: cli.ComponentEnable, args: cobra.ExactArgs(1), name: true,
			flags: all(cluster("the cluster to enable the component on (cluster ID, required)"), values, wait)},
		{use: "update <id>", short: "Replace a component's values with the values of -f",
			run: cli.ComponentUpdate, args: cobra.ExactArgs(1), flags: all(values, etag, wait)},
		{use: "edit <id>", short: "Open a component's values in $VISUAL or $EDITOR and store them when they changed",
			run: cli.ComponentEdit, args: cobra.ExactArgs(1)},
		{use: "disable <id>", short: "Remove a component from its cluster", run: cli.ComponentDisable, args: cobra.ExactArgs(1),
			flags: all(etag, wait)},
		{use: "get <id>", short: "Show a component; --values prints only the values stored for it", run: cli.ComponentGet, args: cobra.ExactArgs(1),
			flags: func(cmd *cobra.Command, o *cli.Options) {
				cmd.Flags().BoolVar(&o.Values, "values", false, "print only the stored values, as YAML to edit and send back with update -f")
			}},
		{use: "list", short: "List what is on --cluster: the platform's components, the tenant's, and the ones it could enable; without --cluster the tenant's components",
			run: cli.ComponentList, args: cobra.NoArgs,
			flags: all(cluster("everything on this cluster (cluster ID); default: the tenant's components on all clusters"),
				func(cmd *cobra.Command, o *cli.Options) {
					cmd.Flags().BoolVar(&o.Details, "details", false, "also show why each component is not ready: chart, Helm state, last error, failures")
					cmd.Flags().Int32Var(&o.PageSize, "page-size", 0, "maximum results per page; 0 uses the server default")
					cmd.Flags().StringVar(&o.PageToken, "page-token", "", "next_page_token of the previous page")
				})},
	}
	for _, v := range verbs {
		usage, takesID, idRequired := cli.IDVerb(v.use)
		if takesID {
			// The component acted on is --id; a positional ID still works (hidden).
			v.use, v.args = usage, cobra.MaximumNArgs(1)
		}
		cmd := &cobra.Command{Use: v.use, Short: v.short, Args: v.args,
			RunE: func(cmd *cobra.Command, args []string) error {
				cfg, err := config.LoadClient(changedFlags(cmd.Flags()), os.LookupEnv)
				if err != nil {
					return err
				}
				opts := o
				opts.Progress, opts.RequestTimeout, opts.Terminal = cmd.ErrOrStderr(), cfg.RequestTimeout, onTerminal()
				switch {
				case v.name && len(args) > 0:
					opts.Component = args[0]
				case takesID:
					if opts.ID, err = cli.ResolveID(opts.ID, args); err != nil {
						return err
					}
					if idRequired {
						if err := cli.RequireID(opts.ID, "component"); err != nil {
							return err
						}
					}
				}
				return cli.Run(cmd.Context(), v.run, cli.NewClients(cfg.Server, nil, cli.WithToken(apiToken)), opts, cmd.OutOrStdout(), output)
			},
		}
		if takesID {
			cmd.Flags().StringVar(&o.ID, "id", "", "the component's ID, as component enable and component list print it")
		}
		if v.flags != nil {
			v.flags(cmd, &o)
		}
		group.AddCommand(cmd)
	}
	return group
}

// onTerminal reports whether a person is at the terminal: stdin is a
// character device, not a pipe or a file.
func onTerminal() bool {
	info, err := os.Stdin.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
