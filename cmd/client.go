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
	// id adds --id, input -f (the manifest), etag --etag, mask
	// --update-mask, page --page-size and --page-token, ack
	// --acknowledge-not-imported, wait --wait and --timeout, plan --plan and
	// --plan-id, force --force, cluster --cluster.
	id, input, etag, mask, page, ack, wait, plan, force, cluster bool
	// table: the verb prints a table unless --output says otherwise;
	// removed adds --only-removed, oidc --oidc.
	table, removed, oidc bool
}

// crud returns the create/get/list/update/delete verbs of a resource whose
// manifest kind is kind.
func crud(noun, kind string, create, get, list, update, del cli.Command) []verb {
	return []verb{
		{use: "create", short: fmt.Sprintf("Create a %s from a %s manifest (-f); prints its server-assigned id", noun, kind),
			run: create, args: cobra.NoArgs, input: true, wait: true},
		{use: "get <id>", short: "Show a " + noun, run: get, args: cobra.ExactArgs(1)},
		{use: "list", short: "List " + noun + "s", run: list, args: cobra.NoArgs, page: true},
		{use: "update [id]", short: fmt.Sprintf("Update the --update-mask fields of a %s from a %s manifest (-f); the id is the argument or metadata.id", noun, kind),
			run: update, args: cobra.MaximumNArgs(1), input: true, etag: true, mask: true, wait: true},
		{use: "delete <id>", short: fmt.Sprintf("Delete a %s; its resources are deleted in order", noun), run: del,
			args: cobra.ExactArgs(1), etag: true, wait: true},
	}
}

// newResourceCmd returns a command group calling the API. Every verb takes
// --tenant (or metadata.tenant of the manifest), --server and --output.
func newResourceCmd(use, short string, verbs []verb) *cobra.Command {
	var o cli.Options
	var output string
	group := &cobra.Command{Use: use, Short: short, Args: cobra.NoArgs}
	pf := group.PersistentFlags()
	pf.StringVar(&o.Tenant, "tenant", "", "tenant ID (required unless the manifest sets metadata.tenant)")
	pf.StringVarP(&output, "output", "o", cli.OutputYAML, "output format: yaml or json")
	for _, s := range config.ClientSettings() {
		pf.String(s.Flag, s.Default, fmt.Sprintf("%s [$%s]", s.Usage, s.Env))
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
				opts.Progress = cmd.ErrOrStderr()
				if len(args) > 0 {
					opts.ID = args[0]
				}
				output := output
				if v.table && !cmd.Flags().Changed("output") {
					output = cli.OutputTable
				}
				return cli.Run(cmd.Context(), v.run, cli.NewClients(cfg.Server, nil, cli.WithToken(apiToken)), opts, cmd.OutOrStdout(), output)
			},
		}
		f := cmd.Flags()
		if v.id {
			f.StringVar(&o.ID, "id", "", "bootstrap ID: the id returned by bootstrap create / list")
		}
		if v.input {
			f.StringVarP(&o.Input, "file", "f", "", "YAML or JSON manifest: apiVersion, kind, metadata, spec")
			f.StringVar(&o.Input, "config", "", "the manifest (use -f)")
			_ = f.MarkDeprecated("config", "use -f/--file")
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
		if v.plan {
			f.BoolVar(&o.Plan, "plan", false, "only show what the update would do (a plan: creates, in-place and disruptive changes, deletes); nothing changes")
			f.StringVar(&o.PlanID, "plan-id", "", "apply only if this plan (planId of --plan) still holds")
		}
		if v.force {
			f.BoolVar(&o.Force, "force", false, "tear the cluster down even if its API is unreachable: Helm releases are released without uninstalling")
		}
		if v.removed {
			f.BoolVar(&o.OnlyRemoved, "only-removed", false, "only the objects whose API version is removed at the target version")
		}
		if v.oidc {
			f.BoolVar(&o.OIDC, "oidc", false, "the user signs in through the cluster's OIDC identity provider (spec.access.oidc), with kubectl oidc-login")
		}
		if v.cluster {
			f.StringVar(&o.Cluster, "cluster", "", "only this cluster's resources (cluster ID); default: all the tenant's")
		}
		if v.wait {
			f.BoolVar(&o.Wait, "wait", false, "wait until the resource is ready (delete: gone; import: its plan is there), printing its progress to stderr; a resource that turns FAILED ends the wait with its message and a non-zero exit")
			f.DurationVar(&o.Timeout, "timeout", cli.DefaultWaitTimeout, "how long --wait waits")
		}
		group.AddCommand(cmd)
	}
	return group
}

func newBootstrapCmd() *cobra.Command {
	return newResourceCmd("bootstrap", "Onboard a tenant's cloud account (TenantService)", []verb{
		{use: "create", short: "Bootstrap the account in a CloudAccount manifest (-f)",
			run: cli.BootstrapCreate, args: cobra.NoArgs, input: true},
		{use: "get", short: "Show the bootstrapped account --id", run: cli.BootstrapGet, args: cobra.NoArgs, id: true},
		{use: "list", short: "List the tenant's bootstrapped accounts", run: cli.BootstrapList, args: cobra.NoArgs},
	})
}

func newNetworkCmd() *cobra.Command {
	verbs := crud("network", cli.KindNetwork, cli.NetworkCreate, cli.NetworkGet, cli.NetworkList, cli.NetworkUpdate, cli.NetworkDelete)
	verbs = append(verbs,
		verb{use: "import", short: "Import an existing network read-only from a NetworkImport manifest (-f)",
			run: cli.NetworkImport, args: cobra.NoArgs, input: true, wait: true},
		verb{use: "takeover <id>", short: "Take over an imported network: the platform becomes its owner",
			run: cli.NetworkTakeOver, args: cobra.ExactArgs(1), etag: true, ack: true, wait: true},
		verb{use: "status <id>", short: "Show a network with its live status (get shows the status last recorded)",
			run: cli.NetworkStatus, args: cobra.ExactArgs(1)},
		watchVerb("network", cli.NetworkWatch))
	return newResourceCmd("network", "Manage networks (NetworkService)", verbs)
}

func newAdminCmd() *cobra.Command {
	return newResourceCmd("admin", "Operator actions (AdminService)", []verb{
		{use: "pause", short: "Pause Crossplane's reconciliation of a tenant's (or one cluster's) resources, e.g. during a provider upgrade",
			run: cli.AdminPause, args: cobra.NoArgs, cluster: true},
		{use: "resume", short: "Resume the reconciliation paused with pause", run: cli.AdminResume, args: cobra.NoArgs, cluster: true},
	})
}

func newClusterCmd() *cobra.Command {
	verbs := crud("cluster", cli.KindCluster, cli.ClusterCreate, cli.ClusterGet, cli.ClusterList, cli.ClusterUpdate, cli.ClusterDelete)
	verbs[3].plan, verbs[4].force = true, true
	verbs = append(verbs,
		verb{use: "import", short: "Import an existing EKS cluster read-only from a ClusterImport manifest (-f)",
			run: cli.ClusterImport, args: cobra.NoArgs, input: true, wait: true},
		verb{use: "takeover <id>", short: "Take over an imported cluster: the platform becomes its owner",
			run: cli.ClusterTakeOver, args: cobra.ExactArgs(1), etag: true, ack: true, wait: true},
		verb{use: "status <id>", short: "Show a cluster with its live status (get shows the status last recorded)",
			run: cli.ClusterStatus, args: cobra.ExactArgs(1)},
		verb{use: "deprecations <id>", short: "List the cluster's objects written with deprecated or removed API versions (the controller's last scan); -o table, yaml or json",
			run: cli.ClusterDeprecations, args: cobra.ExactArgs(1), page: true, table: true, removed: true},
		verb{use: "kubeconfig <id>", short: "Print a kubeconfig for the cluster's Kubernetes API; --oidc: users sign in through its OIDC identity provider (no credential in the file)",
			run: cli.ClusterKubeconfig, args: cobra.ExactArgs(1), oidc: true},
		watchVerb("cluster", cli.ClusterWatch))
	return newResourceCmd("cluster", "Manage EKS clusters (ClusterService)", verbs)
}

// watchVerb streams a resource's state and its managed resources' events.
func watchVerb(noun string, run cli.Command) verb {
	return verb{use: "watch <id>",
		short: fmt.Sprintf("Stream a %s's state and its resources' events (+ added, ~ changed, - deleted) until it is deleted; -o json for JSON lines", noun),
		run:   run, args: cobra.ExactArgs(1)}
}
