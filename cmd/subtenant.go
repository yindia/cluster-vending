package cmd

import (
	"github.com/spf13/cobra"

	"github.com/yindia/cluster-vending/pkg/cli"
)

// newSubTenantCmd is the sub-tenants of a cluster that runs Capsule
// (spec.naas): the console's Sub Tenants page. The manifest is the
// console's (examples/subtenant.yaml).
func newSubTenantCmd() *cobra.Command {
	group := newResourceCmd("subtenant", "Manage the sub-tenants of a cluster with NaaS: namespace allocations with a quota (SubTenantService)", []verb{
		{use: "create", short: "Create a sub-tenant from a SubTenant manifest (-f; the cluster is metadata.clusterId or --cluster); prints its server-assigned id",
			run: cli.SubTenantCreate, args: cobra.NoArgs, input: true, wait: true, cluster: true},
		{use: "get <id>", short: "Show a sub-tenant: its quota, access, namespaces, deletion blockers and conditions",
			run: cli.SubTenantGet, args: cobra.ExactArgs(1), cluster: true},
		{use: "list", short: "List the tenant's sub-tenants, or one cluster's with --cluster; -o wide adds the status message",
			run: cli.SubTenantList, args: cobra.NoArgs, page: true, cluster: true},
		{use: "update [id]", short: "Update a sub-tenant from a SubTenant manifest (-f): what the manifest changes is shown, then applied; the id is --id or metadata.id. " +
			"spec.name is fixed and namespace keys can only be added; set spec.deletionProtection: false before a delete",
			run: cli.SubTenantUpdate, args: cobra.MaximumNArgs(1), input: true, wait: true, cluster: true},
		{use: "delete <id>", short: "Delete a sub-tenant whose deletion protection is off; refused while it has deletion blockers (get lists them)",
			run: cli.SubTenantDelete, args: cobra.ExactArgs(1), etag: true, wait: true, cluster: true},
	})
	// --cluster is shared with the freeze commands, whose help says something
	// else: here it is the sub-tenant's cluster.
	for _, c := range group.Commands() {
		if f := c.Flags().Lookup("cluster"); f != nil {
			f.Usage = "the sub-tenant's cluster (cluster ID); create and update: or metadata.clusterId; list: optional filter"
		}
	}
	return group
}
