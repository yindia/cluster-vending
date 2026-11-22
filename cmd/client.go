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
	// id adds --id, input -f (the manifest), etag --etag, page
	// --page-size and --page-token, ack
	// --acknowledge-not-imported, wait --wait and --timeout, plan --plan and
	// --plan-id, force --force, cluster --cluster.
	id, input, etag, page, ack, wait, plan, force, cluster bool
	// reason adds --reason; regions adds --region for account verification.
	reason, regions bool
	resetExceptions bool
	// removed adds --only-removed, oidc --oidc, warnings --only-warnings,
	// noPlatform --skip-platform-components.
	removed, oidc, warnings, noPlatform bool
	// live adds --live: read the status now instead of the recorded one.
	live bool
	// retry adds --component and --upgrade (cluster retry).
	retry bool
	// recommend adds --namespace and --only-changes.
	recommend bool
	// sort adds --sort (its values, for the help) and --order.
	sort string
	// nodes adds --group, --ready, --not-ready, --name, --manifests and
	// --options; violations --namespace, --engine and --events.
	nodes, violations bool
	// until adds --until; letThrough adds --cluster and --network.
	until, letThrough bool
	freezeTargets     bool
	// parent makes the verb a subcommand of the group's verb of that name
	// (admin subcommands).
	parent string
	// idHelp replaces the help of --id.
	idHelp string
}

// crud returns the create/get/list/update/delete verbs of a resource whose
// manifest kind is kind.
func crud(noun, kind string, create, get, list, update, del cli.Command) []verb {
	return []verb{
		{use: "create", short: fmt.Sprintf("Create a %s from a %s manifest (-f); prints its server-assigned id", noun, kind),
			run: create, args: cobra.NoArgs, input: true, wait: true},
		{use: "get <id>", short: "Show a " + noun + "; --live reads its status now instead of the one last recorded", run: get, args: cobra.ExactArgs(1), live: true},
		{use: "list", short: "List " + noun + "s", run: list, args: cobra.NoArgs, page: true},
		{use: "update [id]", short: fmt.Sprintf("Update a %s from a %s manifest (-f): what the manifest changes is shown, then applied; the id is --id or metadata.id", noun, kind),
			run: update, args: cobra.MaximumNArgs(1), input: true, wait: true},
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
	if use != "admin" {
		pf.StringVar(&o.Tenant, "tenant", "", "tenant ID (default $"+cli.EnvTenant+"; a manifest's metadata.tenant wins over $"+cli.EnvTenant+")")
	}
	pf.StringVarP(&output, "output", "o", cli.OutputTable, cli.OutputHelp)
	for _, s := range config.ClientSettings() {
		pf.String(s.Flag, s.Default, fmt.Sprintf("%s [$%s]", s.Usage, s.Env))
	}

	byName := map[string]*cobra.Command{}
	for _, v := range verbs {
		usage, takesID, idRequired := cli.IDVerb(v.use)
		if takesID {
			// The object acted on is --id; a positional ID still works (hidden).
			v.use, v.args, v.id = usage, cobra.MaximumNArgs(1), true
		}
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
				opts.RequestTimeout = cfg.RequestTimeout
				opts.Server = cfg.Server
				opts.Terminal = onTerminal()
				if use != "admin" { // Admin commands do not use a tenant.
					opts.DefaultTenant = os.Getenv(cli.EnvTenant)
				}
				if opts.ID, err = cli.ResolveID(opts.ID, args); err != nil {
					return err
				}
				if idRequired {
					if err := cli.RequireID(opts.ID, use); err != nil {
						return err
					}
				}
				return cli.Run(cmd.Context(), v.run, cli.NewClients(cfg.Server, nil, cli.WithToken(apiToken)), opts, cmd.OutOrStdout(), output)
			},
		}
		f := cmd.Flags()
		if v.id {
			help := v.idHelp
			if help == "" {
				help = fmt.Sprintf("the %s's ID, as %s create and %s list print it", use, use, use)
			}
			f.StringVar(&o.ID, "id", "", help)
		}

		if v.until {
			f.StringVar(&o.Until, "until", "", "end it by itself at this time: RFC 3339 (2026-12-31T18:00:00Z) or a date (2026-12-31, its start in UTC)")
		}
		if v.freezeTargets {
			f.BoolVar(&o.Platform, "platform", false, "target the entire platform; cannot combine with tenant, network or cluster")
			f.StringVar(&o.Tenant, "tenant", "", "target this tenant and all its networks and clusters")
		}
		if v.letThrough {
			f.StringVar(&o.Cluster, "cluster", "", "target this cluster only; its shared network keeps its own freeze state")
			f.StringVar(&o.PauseNetwork, "network", "", "target this network and all its clusters; not with --cluster (operator commands also exclude --tenant)")
		}
		if v.input {
			f.StringVarP(&o.Input, "file", "f", "", "YAML or JSON manifest: apiVersion, kind, metadata, spec")
		}
		if v.etag {
			f.StringVar(&o.ETag, "etag", "", "fail unless the resource still has this etag")
		}
		if v.ack {
			f.BoolVar(&o.AcknowledgeNotImported, "acknowledge-not-imported", false,
				"take over even though some resources were not imported (see status.import)")
		}
		if v.noPlatform {
			f.BoolVar(&o.SkipPlatformComponents, "skip-platform-components", false,
				"keep the platform components off the cluster (it already runs its own); decided at the takeover")
		}
		if v.page {
			f.Int32Var(&o.PageSize, "page-size", 0, "maximum results per page; 0 uses the server default")
			f.StringVar(&o.PageToken, "page-token", "", "next_page_token of the previous page")
		}
		if v.plan {
			f.StringVar(&o.PlanID, "plan-id", "", "apply only the previously reviewed plan ID; refuses stale plans")
			f.BoolVar(&o.Plan, "plan", false, "only show what the update would do (a plan: creates, in-place and disruptive changes, deletes); nothing changes")
		}
		if v.force {
			f.BoolVar(&o.Force, "force", false, "tear the cluster down even if its API is unreachable: Helm releases are released without uninstalling")
		}
		if v.sort != "" {
			f.StringVar(&o.SortBy, "sort", "", "order the rows by "+v.sort+"; default: the list's own order")
			f.StringVar(&o.SortOrder, "order", "", "asc or desc; default: the order --sort usually means (largest first for sizes and counts, newest first for times)")
		}
		if v.nodes {
			f.StringVar(&o.NodeGroup, "group", "", "only this node group; default: every group the cluster reports")
			f.BoolVar(&o.Ready, "ready", false, "only Ready nodes")
			f.BoolVar(&o.NotReady, "not-ready", false, "only nodes that are not Ready")
			f.StringVar(&o.NameContains, "name", "", "only nodes whose name contains this")
			f.BoolVar(&o.Manifests, "manifests", false, "print the NodePool and EC2NodeClass YAML the platform applies for the Karpenter groups (one with --group) instead of the nodes")
			f.BoolVar(&o.NodeGroupOptions, "options", false, "print what the tenant's node groups may use (instance families and types, architectures, capacity types); needs no cluster ID")
		}
		if v.violations {
			f.StringVar(&o.Namespace, "namespace", "", "only this namespace of the cluster")
			f.StringVar(&o.Engine, "engine", "", "only this policy engine (kyverno, gatekeeper, ...)")
			f.BoolVar(&o.Events, "events", false, "the policy and admission events of the last hour instead of the violations")
		}
		if v.recommend {
			f.StringVar(&o.Namespace, "namespace", "", "only the workloads of this namespace of the cluster")
			f.BoolVar(&o.OnlyChanges, "only-changes", false, "only the containers whose recommended requests differ from theirs by a tenth or more")
		}
		if v.live {
			f.BoolVar(&o.Live, "live", false, "read the status now from the management cluster instead of the one the controller last recorded")
		}
		if v.retry {
			f.StringSliceVar(&o.RetryComponents, "component", nil, "only this component (repeat or comma-separate); default: every failed one")
			f.BoolVar(&o.RetryUpgrade, "upgrade", false, "also retry a failed or timed-out automatic upgrade (default with no --component)")
		}
		if v.removed {
			f.BoolVar(&o.OnlyRemoved, "only-removed", false, "only the objects whose API version is removed at the target version")
		}
		if v.warnings {
			f.BoolVar(&o.OnlyWarnings, "only-warnings", false, "only the warnings: failures and what needs a look")
		}
		if v.oidc {
			f.BoolVar(&o.OIDC, "oidc", false, "print a kubeconfig whose user signs in through the cluster's OIDC identity provider (spec.access.oidc), with kubectl oidc-login")
			f.BoolVar(&o.Print, "print", false, "print the IAM kubeconfig instead of installing it with the AWS CLI")
			f.StringVar(&o.KubeconfigFile, "kubeconfig-file", "", "the kubeconfig the AWS CLI writes to (default: its own, ~/.kube/config)")
		}
		if v.cluster {
			f.StringVar(&o.Cluster, "cluster", "", "only this cluster (cluster ID); default: the tenant and everything it has")
		}
		if v.resetExceptions {
			f.BoolVar(&o.ResetExceptions, "reset-exceptions", false, "freeze previously let-through clusters and networks again")
		}
		if v.reason {
			f.StringVar(&o.Reason, "reason", "", "why, shown with the pause or freeze in the status of what it holds")
		}
		if v.regions {
			f.StringSliceVar(&o.Regions, "region", nil, "also check these regions (repeat or comma-separate), e.g. the region of a network not created yet; kept for later checks")
		}

		if v.wait {
			help := "wait until the resource is ready (delete: gone; import: its plan is there), printing its progress to stderr; a resource that turns FAILED ends the wait with its message and a non-zero exit"
			if v.regions {
				help = "wait for the controller's access check, printing progress to stderr; a denied check exits non-zero"
			}
			if (use == "admin" || use == "tenant") && cmd.Name() == "freeze" {
				help = "wait for the controller to confirm Frozen after active reconciliations drain"
			}
			if (use == "admin" || use == "tenant") && cmd.Name() == "unfreeze" {
				help = "wait for controller confirmation that freeze-owned pauses in the selected scope are removed"
			}
			f.BoolVar(&o.Wait, "wait", false, help)
			f.DurationVar(&o.Timeout, "timeout", cli.DefaultWaitTimeout, "how long --wait waits")
		}
		byName[cmd.Name()] = cmd
		if parent := byName[v.parent]; parent != nil {
			parent.AddCommand(cmd)
			continue
		}
		group.AddCommand(cmd)
	}
	return group
}

// newAccountCmd is the cloud accounts of a tenant. get and verify take
// --id; the ID as the argument works too (not shown in the help).
func newAccountCmd() *cobra.Command {
	return newResourceCmd("account", "Onboard and check a tenant's cloud accounts (TenantService)", []verb{
		{use: "create", short: "Onboard the account in a CloudAccount manifest (-f): its account ProviderConfig (requires an existing Tenant and its namespace; run tenant create first), then print how to " +
			"create the platform's IAM role in it with CloudFormation (the template this server serves at /cloudformation/) and have its access checked",
			run: cli.AccountCreate, args: cobra.NoArgs, input: true},
		{use: "get", short: "Show the account --id, with its access check", run: cli.BootstrapGet, args: cobra.MaximumNArgs(1), id: true},
		{use: "list", short: "List the tenant's accounts", run: cli.BootstrapList, args: cobra.NoArgs},
		{use: "verify", short: "Ask the controller to check the account --id's access now (the tenant role in each region, and the calls the platform needs); get shows the result",
			run: cli.BootstrapVerify, args: cobra.MaximumNArgs(1), id: true, regions: true, wait: true},
		{use: "delete", short: "Delete the account --id (refused while networks belong to it): the controller removes its ProviderConfig; " +
			"the role's stack in AWS stays, yours to delete",
			run: cli.AccountDelete, args: cobra.MaximumNArgs(1), id: true, wait: true},
	})
}

func newTenantCmd() *cobra.Command {
	return newResourceCmd("tenant", "Create and list tenants, and manage their freezes (TenantService)", []verb{
		{use: "create <tenant-id>", short: "Register a tenant and create its owned namespace before connecting accounts", run: cli.TenantCreate, idHelp: "tenant ID (DNS label)", args: cobra.ExactArgs(1)},
		{use: "list", short: "List tenants visible to your identity; no --tenant required", run: cli.TenantList, args: cobra.NoArgs},
		{use: "freeze", short: "Freeze this tenant, or one of its networks or clusters", run: cli.TenantFreeze, args: cobra.NoArgs, reason: true, until: true, wait: true, letThrough: true},
		{use: "status", short: "Show this tenant's freeze confirmation", run: cli.TenantFreezeStatus, args: cobra.NoArgs, parent: "freeze", letThrough: true},
		{use: "unfreeze", short: "Resume this tenant, network or cluster unless held by the platform", run: cli.TenantUnfreeze, args: cobra.NoArgs, wait: true, letThrough: true},
	})
}

func newNetworkCmd() *cobra.Command {
	verbs := crud("network", cli.KindNetwork, cli.NetworkCreate, cli.NetworkGet, cli.NetworkList, cli.NetworkUpdate, cli.NetworkDelete)
	verbs[3].plan = true
	verbs = append(verbs,
		verb{use: "import", short: "Import an existing network read-only from a NetworkImport manifest (-f)",
			run: cli.NetworkImport, args: cobra.NoArgs, input: true, wait: true},
		verb{use: "takeover <id>", short: "Take over an imported network: the platform becomes its owner",
			run: cli.NetworkTakeOver, args: cobra.ExactArgs(1), etag: true, ack: true, wait: true},
		verb{use: "events <id>", short: "List what the controller reported about the network, newest first (kept for about an hour; get shows the last ten for good); -o table, yaml or json",
			run: cli.NetworkEvents, args: cobra.ExactArgs(1), page: true, warnings: true})
	return newResourceCmd("network", "Manage networks (NetworkService)", verbs)
}

func newAdminCmd() *cobra.Command {
	admin := newResourceCmd("admin", "Operator actions (AdminService)", []verb{
		{use: "freeze", short: "Freeze the platform, a tenant, a network or a cluster; controllers and Crossplane pause",
			run: cli.AdminFreeze, args: cobra.NoArgs, reason: true, until: true, wait: true, resetExceptions: true, letThrough: true, freezeTargets: true},
		{use: "status", short: "Show freeze confirmation, provider pause counts and let-through IDs", run: cli.AdminFreezeStatus, args: cobra.NoArgs, parent: "freeze", letThrough: true, freezeTargets: true},
		{use: "unfreeze", short: "Resume a cluster, a network and its clusters, a tenant or the entire platform",
			run: cli.AdminUnfreeze, args: cobra.NoArgs, wait: true, letThrough: true, freezeTargets: true},
	})
	admin.AddCommand(newAdminReleaseCmds()...)
	return admin
}

// newAdminReleaseCmds are the read-only views of the platform's release
// configuration: releases, and channels with their rollouts
// (AdminService).
func newAdminReleaseCmds() []*cobra.Command {
	return []*cobra.Command{
		newResourceCmd("release", "The platform's releases: what clusters run per Kubernetes minor (read-only)", []verb{
			{use: "list", short: "List the releases, newest first: shipped or published, valid, the channels offering them; -o wide adds why one is not valid",
				run: cli.AdminReleaseList, args: cobra.NoArgs, page: true},
			{use: "get <id>", short: "Show a release: each Kubernetes minor with its channel, upgrade target, end of support and whether it is ending, and the components on it; -o wide adds their locked values",
				run: cli.AdminReleaseGet, args: cobra.ExactArgs(1), idHelp: "the release's name, as release list prints it (e.g. shipped-1aed928c)"},
		}),
		newResourceCmd("channel", "The release channels alpha, beta and stable: what each offers, its rollout policy and release soak (read-only)", []verb{
			{use: "list", short: "List the channels: promotion, the release and Kubernetes minor each offers, the tier its rollout is at, its state and why it waits; -o wide adds the progress per tier and the policy",
				run: cli.AdminChannelList, args: cobra.NoArgs},
			{use: "get <id>", short: "Show a channel: its release, Kubernetes minor, release soak, rollout policy and its rollout planned now (tiers, batches, who waits and why: Kubernetes upgrades and release moves), the releases it offered, its conditions; -o wide adds every cluster with its ID and gate",
				run: cli.AdminChannelGet, args: cobra.ExactArgs(1), idHelp: "the channel: alpha, beta or stable"},
		}),
	}
}

func newClusterCmd() *cobra.Command {
	verbs := crud("cluster", cli.KindCluster, cli.ClusterCreate, cli.ClusterGet, cli.ClusterList, cli.ClusterUpdate, cli.ClusterDelete)
	verbs[3].plan, verbs[4].force = true, true
	verbs = append(verbs,
		verb{use: "import", short: "Import an existing EKS cluster read-only from a ClusterImport manifest (-f)",
			run: cli.ClusterImport, args: cobra.NoArgs, input: true, wait: true},
		verb{use: "takeover <id>", short: "Take over an imported cluster: the platform becomes its owner",
			run: cli.ClusterTakeOver, args: cobra.ExactArgs(1), etag: true, ack: true, wait: true, noPlatform: true},
		verb{use: "deprecations <id>", short: "List the cluster's objects written with deprecated or removed API versions (the controller's last scan); -o table, yaml or json",
			run: cli.ClusterDeprecations, args: cobra.ExactArgs(1), page: true, removed: true, sort: "removed-first, namespace or kind"},
		verb{use: "recommendations <id>", short: "Recommend the requests and limits of the cluster's containers from a week of recorded usage (read-only: nothing is installed); -o table, yaml or json",
			run: cli.ClusterRecommendations, args: cobra.ExactArgs(1), page: true, recommend: true, sort: "namespace, cpu-change or memory-change (the size of the change)"},
		verb{use: "nodes [id]", short: "List the nodes of the cluster's node groups (all, or --group): status, instance type, zone, capacity, kubelet, NodeClaim and requested CPU, memory and pods; --manifests prints the groups' NodePool and EC2NodeClass YAML, --options what node groups may use; -o wide, yaml or json",
			run: cli.ClusterNodes, args: cobra.MaximumNArgs(1), page: true, nodes: true, sort: "name, age, readiness, cpu-requested-percent, memory-requested-percent or pod-count"},
		verb{use: "violations <id>", short: "List the policy violations the cluster's engines report (Kyverno, Gatekeeper); --events lists the policy and admission events of the last hour instead; -o wide, yaml or json",
			run: cli.ClusterViolations, args: cobra.ExactArgs(1), page: true, violations: true, sort: "namespace, engine or severity (violations); time or count (--events)"},
		verb{use: "events <id>", short: "List what the controller reported about the cluster, newest first (kept for about an hour; get shows the last ten for good); -o table, yaml or json",
			run: cli.ClusterEvents, args: cobra.ExactArgs(1), page: true, warnings: true},
		verb{use: "kubeconfig <id>", short: "Add the cluster to your kubeconfig (runs aws eks update-kubeconfig with its name and region); --print or --oidc print a kubeconfig instead",
			run: cli.ClusterKubeconfig, args: cobra.ExactArgs(1), oidc: true},
		verb{use: "retry <id>", short: "Try again what failed on the cluster: its failed platform and tenant components and a failed automatic upgrade (or only --component, --upgrade); says per item what happens, or what to fix first",
			run: cli.ClusterRetry, args: cobra.ExactArgs(1), retry: true})
	return newResourceCmd("cluster", "Manage EKS clusters (ClusterService)", verbs)
}
