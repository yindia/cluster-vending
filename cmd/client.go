package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/yindia/cluster-vending/pkg/cli"
	"github.com/yindia/cluster-vending/pkg/config"
)

// verb is one subcommand of a resource command (create, get, ...).
type verb struct {
	use   string
	short string
	run   cli.Command
	args  cobra.PositionalArgs
	// id adds --id, input --config (the request input), etag --etag, mask
	// --update-mask, page --page-size and --page-token, ack
	// --acknowledge-not-imported.
	id, input, etag, mask, page, ack bool
}

// crud returns the create/get/list/update/delete verbs of a resource whose
// ID is the argument.
func crud(noun string, create, get, list, update, del cli.Command) []verb {
	return []verb{
		{use: "create [id]", short: "Create a " + noun + " from --config", run: create, args: cobra.MaximumNArgs(1), input: true},
		{use: "get <id>", short: "Show a " + noun, run: get, args: cobra.ExactArgs(1)},
		{use: "list", short: "List " + noun + "s", run: list, args: cobra.NoArgs, page: true},
		{use: "update <id>", short: "Update the --update-mask fields of a " + noun + " from --config", run: update,
			args: cobra.ExactArgs(1), input: true, etag: true, mask: true},
		{use: "delete <id>", short: "Delete a " + noun, run: del, args: cobra.ExactArgs(1), etag: true},
	}
}

// newResourceCmd returns a command group calling the API. Every verb takes
// --tenant, --server and --output.
func newResourceCmd(use, short string, verbs []verb, extra func(*cobra.Command, *cli.Options)) *cobra.Command {
	var o cli.Options
	var output string
	group := &cobra.Command{Use: use, Short: short, Args: cobra.NoArgs}
	pf := group.PersistentFlags()
	pf.StringVar(&o.Tenant, "tenant", "", "tenant ID (required)")
	pf.StringVarP(&output, "output", "o", cli.OutputYAML, "output format: yaml or json")
	for _, s := range config.ClientSettings() {
		pf.String(s.Flag, s.Default, fmt.Sprintf("%s [$%s]", s.Usage, s.Env))
	}
	if extra != nil {
		extra(group, &o)
	}

	for _, v := range verbs {
		cmd := &cobra.Command{
			Use:   v.use,
			Short: v.short,
			Args:  v.args,
			RunE: func(cmd *cobra.Command, args []string) error {
				cfg, err := config.LoadClient(changedFlags(cmd.Flags()), os.LookupEnv)
				if err != nil {
					return err
				}
				opts := o
				if len(args) > 0 {
					opts.ID = args[0]
				}
				return cli.Run(cmd.Context(), v.run, cli.NewClients(cfg.Server, nil), opts, cmd.OutOrStdout(), output)
			},
		}
		f := cmd.Flags()
		if v.id {
			f.StringVar(&o.ID, "id", "", "bootstrap ID: the id returned by bootstrap create / list")
		}
		if v.input {
			f.StringVar(&o.Input, "config", "", "YAML or JSON file with the request input (protojson field names)")
		}
		if v.etag {
			f.StringVar(&o.ETag, "etag", "", "fail unless the resource still has this etag")
		}
		if v.mask {
			f.StringSliceVar(&o.UpdateMask, "update-mask", nil, "fields to change, e.g. display_name,spec.access (required)")
		}
		if v.ack {
			f.BoolVar(&o.AcknowledgeNotImported, "acknowledge-not-imported", false,
				"take over even though some resources were not imported (see status.import)")
		}
		if v.page {
			f.Int32Var(&o.PageSize, "page-size", 0, "maximum results per page; 0 uses the server default")
			f.StringVar(&o.PageToken, "page-token", "", "next_page_token of the previous page")
		}
		group.AddCommand(cmd)
	}
	return group
}

func newBootstrapCmd() *cobra.Command {
	return newResourceCmd("bootstrap", "Onboard a tenant's cloud account (TenantService)", []verb{
		{use: "create", short: "Bootstrap the account in --config (accountName, aws.accountId) for --tenant",
			run: cli.BootstrapCreate, args: cobra.NoArgs, input: true},
		{use: "get", short: "Show the bootstrapped account --id", run: cli.BootstrapGet, args: cobra.NoArgs, id: true},
		{use: "list", short: "List the tenant's bootstrapped accounts", run: cli.BootstrapList, args: cobra.NoArgs},
	}, nil)
}

func newNetworkCmd() *cobra.Command {
	verbs := crud("network", cli.NetworkCreate, cli.NetworkGet, cli.NetworkList, cli.NetworkUpdate, cli.NetworkDelete)
	// Networks are named by the client and identified by the server.
	verbs[0].use, verbs[0].short = "create [name]", "Create a network from --config; prints its server-assigned id"
	verbs = append(verbs,
		verb{use: "import [name]", short: "Import an existing network read-only from --config (name, bootstrapId, region, aws.vpcId, aws.subnetIds)",
			run: cli.NetworkImport, args: cobra.MaximumNArgs(1), input: true},
		verb{use: "takeover <id>", short: "Take over an imported network: the platform becomes its owner",
			run: cli.NetworkTakeOver, args: cobra.ExactArgs(1), etag: true, ack: true})
	return newResourceCmd("network", "Manage networks (NetworkService)", verbs, nil)
}

func newClusterCmd() *cobra.Command {
	verbs := crud("cluster", cli.ClusterCreate, cli.ClusterGet, cli.ClusterList, cli.ClusterUpdate, cli.ClusterDelete)
	// Clusters are named by the client and identified by the server.
	verbs[0].use, verbs[0].short = "create [name]", "Create an EKS cluster in a network from --config; prints its server-assigned id"
	return newResourceCmd("cluster", "Manage clusters (ClusterService)", verbs, nil)
}

func newNodePoolCmd() *cobra.Command {
	return newResourceCmd("nodepool", "Manage the node pools of a cluster (NodePoolService)",
		crud("node pool", cli.NodePoolCreate, cli.NodePoolGet, cli.NodePoolList, cli.NodePoolUpdate, cli.NodePoolDelete),
		func(group *cobra.Command, o *cli.Options) {
			group.PersistentFlags().StringVar(&o.Cluster, "cluster", "", "cluster ID (required)")
		})
}
