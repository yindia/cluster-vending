package cli

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/proto"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// freezeText is what a freeze covers, for people: "the platform", "tenant
// acme", "channel beta tier 0".
func freezeText(o Options, t pauseTarget) string {
	switch {
	case o.Tenant != "":
		return "tenant " + o.Tenant
	case t.channel != cloudv1.ReleaseChannel_RELEASE_CHANNEL_UNSPECIFIED:
		return t.text()
	}
	return "the platform"
}

// AdminFreeze calls AdminService.Freeze for the platform, or --tenant, or
// --channel (and --tier), with --reason and --until: no automatic change
// of what it covers starts until admin unfreeze.
func AdminFreeze(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if o.Cluster != "" || o.PauseNetwork != "" {
		return nil, fmt.Errorf("admin freeze takes no --cluster or --network: freeze, then let one through with admin unfreeze --cluster")
	}
	t, err := freezeTargetOf(o)
	if err != nil {
		return nil, err
	}
	until, err := parseUntil(o.Until)
	if err != nil {
		return nil, err
	}
	res, err := call(ctx, &cloudv1.FreezeRequest{TenantId: o.Tenant, Channel: t.channel, Tier: t.tier, Until: until, Reason: o.Reason},
		c.Admin.Freeze)
	if err != nil || !isTable(o.Output) {
		return res, err
	}
	_, err = fmt.Fprintf(o.Out, "%s is frozen: %d clusters and their networks start no automatic change "+
		"(Kubernetes upgrades, component and add-on upgrades, code changes) until admin unfreeze\n",
		freezeText(o, t), res.(*cloudv1.FreezeResponse).GetClusters())
	return nil, err
}

// AdminUnfreeze calls AdminService.Unfreeze for the freeze its target
// flags name (as admin freeze was given them): lifts it, or with
// --cluster or --network lets that one through.
func AdminUnfreeze(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	t, err := freezeTargetOf(o)
	if err != nil {
		return nil, err
	}
	res, err := call(ctx, &cloudv1.UnfreezeRequest{TenantId: o.Tenant, Channel: t.channel, Tier: t.tier,
		ClusterId: o.Cluster, NetworkId: o.PauseNetwork}, c.Admin.Unfreeze)
	if err != nil || !isTable(o.Output) {
		return res, err
	}
	_, err = fmt.Fprintln(o.Out, res.(*cloudv1.UnfreezeResponse).GetNote())
	return nil, err
}
