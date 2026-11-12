package network

import (
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// Account is what rendering needs of the cloud account a network lives
// in.
type Account struct {
	// Namespace is the tenant namespace in the management cluster;
	// ProviderConfigName the account's ProviderConfig in it.
	Namespace          string
	ProviderConfigName string
	// AccountID is the AWS account ID.
	AccountID string
}

// Identity names a network: who it belongs to, its ID, its client-chosen
// name and its labels (the AWS tags of every resource).
type Identity struct {
	TenantID string
	ID       string
	Name     string
	Labels   map[string]string
}

// RenderPlan maps a network's resolved spec and layout onto the plan its
// managed resources are rendered from (crossplaneaws.NetworkSet).
func RenderPlan(n Identity, spec *cloudv1.NetworkSpec, l Layout, a Account) crossplaneaws.NetworkPlan {
	return crossplaneaws.NetworkPlan{
		Namespace:          a.Namespace,
		ProviderConfigName: a.ProviderConfigName,
		TenantID:           n.TenantID,
		NetworkID:          n.ID,
		NetworkName:        n.Name,
		Prefix:             crossplaneaws.NetworkPrefix(n.TenantID, n.ID, n.Name),
		Region:             spec.GetRegion(),
		CIDRBlock:          l.CIDRBlock,
		Subnets:            l.Subnets,
		NAT:                l.NAT,
		Tags:               n.Labels,
		AccountID:          a.AccountID,
		FlowLogs:           FlowLogPlan(spec.GetFlowLogs()),
	}
}
