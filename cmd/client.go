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
	// reason adds --reason (admin pause), pauseNetwork --network (admin
	// pause and resume), regions --region (account verify).
	reason, pauseNetwork, regions bool
	// createRole adds --create-role, --profile and --region (account
	// create), and makes --wait default to true.
	createRole bool
	// deleteRole adds --delete-role, --profile and --region (account
	// delete).
	deleteRole bool
	// removed adds --only-removed, oidc --oidc, warnings --only-warnings,
	// noPlatform --skip-platform-components.
	removed, oidc, warnings, noPlatform bool
	// live adds --live: read the status now instead of the recorded one.
	live bool
	// retry adds --component and --upgrade (cluster retry).
	retry bool
	// recommend adds --namespace and --only-changes.
	recommend bool
	// rollout adds --channel and --tier (admin rollout get).
	rollout bool
	// promote adds --release (admin channel promote).
	promote bool
	// pauseTargets adds --channel, --tier and --only (admin pause and
	// resume), until --until (admin pause).
	pauseTargets, until bool
	// parent makes the verb a subcommand of the group's verb of that name
	// (admin pause list).
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
			run: update, args: cobra.MaximumNArgs(1), input: true, etag: true, wait: true},
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
	pf.StringVar(&o.Tenant, "tenant", "", "tenant ID (default $"+cli.EnvTenant+"; a manifest's metadata.tenant wins over $"+cli.EnvTenant+")")
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
				if use != "admin" { // admin's --tenant is a pause target: never defaulted
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
		if v.promote {
			f.StringVar(&o.PromoteRelease, "release", "", "the release to take now; default: the upstream channel's (alpha's for beta, beta's for stable)")
		}
		if v.rollout {
			f.StringVar(&o.Channel, "channel", "", "the channel: alpha, beta or stable (required)")
			f.StringVar(&o.Tier, "tier", "", "only this tier's clusters: 0, 1 or 2; default: all")
		}
		if v.pauseTargets {
			f.StringVar(&o.Channel, "channel", "", "every cluster on this release channel, of every tenant: alpha, beta or stable; not with --tenant")
			f.StringVar(&o.Tier, "tier", "", "only the channel's clusters of this tier: 0, 1 or 2 (a cluster with no tier is 0)")
			f.StringVar(&o.Only, "only", "", "upgrades: pause only the automatic upgrades; the clusters are reconciled as usual")
		}
		if v.until {
			f.StringVar(&o.Until, "until", "", "end the pause by itself at this time: RFC 3339 (2026-12-31T18:00:00Z) or a date (2026-12-31, its start in UTC)")
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
			f.BoolVar(&o.Plan, "plan", false, "only show what the update would do (a plan: creates, in-place and disruptive changes, deletes); nothing changes")
			f.StringVar(&o.PlanID, "plan-id", "", "apply only if this plan (planId of --plan) still holds")
		}
		if v.force {
			f.BoolVar(&o.Force, "force", false, "tear the cluster down even if its API is unreachable: Helm releases are released without uninstalling")
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
		if v.reason {
			f.StringVar(&o.Reason, "reason", "", "why, shown with the pause in the status of what is paused")
		}
		if v.regions {
			f.StringSliceVar(&o.Regions, "region", nil, "also check these regions (repeat or comma-separate), e.g. the region of a network not created yet; kept for later checks")
		}
		if v.pauseNetwork {
			f.StringVar(&o.PauseNetwork, "network", "", "only this network (network ID): the network itself, not its clusters; not with --cluster")
		}
		if v.createRole {
			f.StringVar(&o.CreateRole, "create-role", "", "how to create the platform's role (spec.aws.role) in the AWS account with CloudFormation: "+
				"terminal (here, with your AWS credentials of the account), browser (the CloudFormation console) or skip (it exists); asked when at a terminal")
			f.StringVar(&o.Profile, "profile", "", "the AWS profile with credentials of the account (default: AWS_PROFILE and the SDK's default chain)")
			f.StringVar(&o.StackRegion, "region", "", "the AWS region of the role's stack (IAM is global), also checked by the account's access check; default us-east-1")
		}
		if v.deleteRole {
			f.BoolVar(&o.DeleteRole, "delete-role", false, "also delete the CloudFormation stack of the account's role (spec.aws.role), "+
				"with your AWS credentials of the account, once the account is gone: only a stack account create made (its "+
				"cluster-vending.io/managed-by tag); any other is refused before anything is deleted")
			f.StringVar(&o.Profile, "profile", "", "the AWS profile with credentials of the account (default: AWS_PROFILE and the SDK's default chain)")
			f.StringVar(&o.StackRegion, "region", "", "the AWS region of the role's stack; default us-east-1")
		}
		if v.wait {
			help := "wait until the resource is ready (delete: gone; import: its plan is there), printing its progress to stderr; a resource that turns FAILED ends the wait with its message and a non-zero exit"
			if v.createRole {
				help = "wait for the controller's access check of the account, printing progress to stderr; a denied check exits non-zero; --wait=false returns once the account is onboarded (a stack the CLI creates is followed to its end either way)"
			}
			f.BoolVar(&o.Wait, "wait", v.createRole, help)
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
		{use: "create", short: "Onboard the account in a CloudAccount manifest (-f): create the platform's IAM role in it with CloudFormation " +
			"(--create-role terminal or browser), then its namespace and ProviderConfig, and wait for its access check",
			run: cli.AccountCreate, args: cobra.NoArgs, input: true, wait: true, createRole: true},
		{use: "get", short: "Show the account --id, with its access check", run: cli.BootstrapGet, args: cobra.MaximumNArgs(1), id: true},
		{use: "list", short: "List the tenant's accounts", run: cli.BootstrapList, args: cobra.NoArgs},
		{use: "verify", short: "Ask the controller to check the account --id's access now (the tenant role in each region, and the calls the platform needs); get shows the result",
			run: cli.BootstrapVerify, args: cobra.MaximumNArgs(1), id: true, regions: true},
		{use: "delete", short: "Delete the account --id (refused while networks belong to it): the controller removes its ProviderConfig; " +
			"--delete-role also deletes its role's CloudFormation stack, when account create made it",
			run: cli.AccountDelete, args: cobra.MaximumNArgs(1), id: true, wait: true, deleteRole: true},
	})
}

func newNetworkCmd() *cobra.Command {
	verbs := crud("network", cli.KindNetwork, cli.NetworkCreate, cli.NetworkGet, cli.NetworkList, cli.NetworkUpdate, cli.NetworkDelete)
	verbs = append(verbs,
		verb{use: "import", short: "Import an existing network read-only from a NetworkImport manifest (-f)",
			run: cli.NetworkImport, args: cobra.NoArgs, input: true, wait: true},
		verb{use: "takeover <id>", short: "Take over an imported network: the platform becomes its owner",
			run: cli.NetworkTakeOver, args: cobra.ExactArgs(1), etag: true, ack: true, wait: true},
		verb{use: "events <id>", short: "List what the controller reported about the network, newest first (kept for about an hour; get shows the last ten for good); -o table, yaml or json",
			run: cli.NetworkEvents, args: cobra.ExactArgs(1), page: true, warnings: true},
		watchVerb("network", cli.NetworkWatch))
	return newResourceCmd("network", "Manage networks (NetworkService)", verbs)
}

func newAdminCmd() *cobra.Command {
	admin := newResourceCmd("admin", "Operator actions (AdminService)", []verb{
		{use: "pause", short: "Pause a tenant, one network or cluster, or a channel's clusters (--channel, --tier): everything (the platform and the Crossplane providers stop acting on it, e.g. during a provider upgrade; what is asked for meanwhile is done after resume), or with --only upgrades its automatic upgrades only",
			run: cli.AdminPause, args: cobra.NoArgs, cluster: true, reason: true, pauseNetwork: true, pauseTargets: true, until: true},
		{use: "list", short: "List every pause in effect: what it covers, whether everything or only upgrades, who, since and until when, why, how many clusters",
			run: cli.AdminPauseList, args: cobra.NoArgs, parent: "pause"},
		{use: "resume", short: "Resume what pause paused, with the same target and --only: a tenant (a network or cluster also paused on its own stays paused), one network or cluster, or a channel's clusters",
			run: cli.AdminResume, args: cobra.NoArgs, cluster: true, pauseNetwork: true, pauseTargets: true},
	})
	admin.AddCommand(newAdminReleaseCmds()...)
	return admin
}

// newAdminReleaseCmds are the read-only views of the platform's release
// configuration: releases, channels and the rollout of each channel's
// Kubernetes minor (AdminService).
func newAdminReleaseCmds() []*cobra.Command {
	return []*cobra.Command{
		newResourceCmd("release", "The platform's releases: what clusters run per Kubernetes minor (read-only)", []verb{
			{use: "list", short: "List the releases, newest first: shipped or published, valid, the channels offering them; -o wide adds why one is not valid",
				run: cli.AdminReleaseList, args: cobra.NoArgs, page: true},
			{use: "get <id>", short: "Show a release: each Kubernetes minor with its channel, upgrade target, end of support and whether it is ending, and the components on it; -o wide adds their locked values",
				run: cli.AdminReleaseGet, args: cobra.ExactArgs(1), idHelp: "the release's name, as release list prints it (e.g. shipped-1aed928c)"},
		}),
		newResourceCmd("channel", "The release channels alpha, beta and stable: what each offers, its rollout policy and release soak; promote takes a release now", []verb{
			{use: "list", short: "List the channels: promotion, the release and Kubernetes minor each offers, the tier its rollout is at, its state and why it waits; -o wide adds the progress per tier and the policy",
				run: cli.AdminChannelList, args: cobra.NoArgs},
			{use: "get <id>", short: "Show a channel: its release, Kubernetes minor, rollout policy and progress, the releases it offered, its conditions",
				run: cli.AdminChannelGet, args: cobra.ExactArgs(1), idHelp: "the channel: alpha, beta or stable"},
			{use: "promote <id>", short: "Make beta or stable take a release now, before its release soak ends: its upstream channel's, or --release",
				run: cli.AdminChannelPromote, args: cobra.ExactArgs(1), promote: true, idHelp: "the channel: beta or stable"},
		}),
		newResourceCmd("rollout", "The rollout of each channel's Kubernetes minor to its automatic clusters, tier 2 first (read-only)", []verb{
			{use: "get", short: "Show one channel's rollout cluster by cluster: what each runs, the minor it moves to, its state and why it is blocked; -o wide adds cluster IDs and gate codes",
				run: cli.AdminRolloutGet, args: cobra.NoArgs, rollout: true, page: true},
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
			run: cli.ClusterDeprecations, args: cobra.ExactArgs(1), page: true, removed: true},
		verb{use: "recommendations <id>", short: "Recommend the requests and limits of the cluster's containers from a week of recorded usage (read-only: nothing is installed); -o table, yaml or json",
			run: cli.ClusterRecommendations, args: cobra.ExactArgs(1), page: true, recommend: true},
		verb{use: "events <id>", short: "List what the controller reported about the cluster, newest first (kept for about an hour; get shows the last ten for good); -o table, yaml or json",
			run: cli.ClusterEvents, args: cobra.ExactArgs(1), page: true, warnings: true},
		verb{use: "kubeconfig <id>", short: "Add the cluster to your kubeconfig (runs aws eks update-kubeconfig with its name and region); --print or --oidc print a kubeconfig instead",
			run: cli.ClusterKubeconfig, args: cobra.ExactArgs(1), oidc: true},
		verb{use: "retry <id>", short: "Try again what failed on the cluster: its failed platform and tenant components and a failed automatic upgrade (or only --component, --upgrade); says per item what happens, or what to fix first",
			run: cli.ClusterRetry, args: cobra.ExactArgs(1), retry: true},
		watchVerb("cluster", cli.ClusterWatch))
	return newResourceCmd("cluster", "Manage EKS clusters (ClusterService)", verbs)
}

// watchVerb streams a resource's state and its managed resources' events.
func watchVerb(noun string, run cli.Command) verb {
	return verb{use: "watch <id>",
		short: fmt.Sprintf("Stream a %s's state and its resources' events (+ added, ~ changed, - deleted) until it is deleted; -o json for JSON lines", noun),
		run:   run, args: cobra.ExactArgs(1)}
}
