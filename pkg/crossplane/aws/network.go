package aws

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	awscwl "github.com/upbound/provider-aws/v2/apis/namespaced/cloudwatchlogs/v1beta1"
	awsec2 "github.com/upbound/provider-aws/v2/apis/namespaced/ec2/v1beta1"
	awsiam "github.com/upbound/provider-aws/v2/apis/namespaced/iam/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/crossplane"
	"github.com/yindia/cluster-vending/pkg/crossplane/sequence"
)

// SubnetRole is the role of a planned subnet.
type SubnetRole string

// Subnet roles.
const (
	SubnetPublic  SubnetRole = "public"
	SubnetPrivate SubnetRole = "private"
)

// NATMode is how many NAT gateways a network gets.
type NATMode string

// NAT modes (cloud.v1 NatGatewayMode).
const (
	// NATSingle is one NAT gateway, in the first zone's public subnet.
	NATSingle NATMode = "single"
	// NATPerZone is one NAT gateway per zone; private subnets route to
	// their zone's.
	NATPerZone NATMode = "per-zone"
)

// Tags the platform puts on every AWS resource, on top of the network's.
const (
	TagName        = "Name"
	TagTenant      = "cluster-vending.io/tenant"
	TagNetwork     = "cluster-vending.io/network"
	TagNetworkName = "cluster-vending.io/network-name"
	TagELB         = "kubernetes.io/role/elb"
	TagInternalELB = "kubernetes.io/role/internal-elb"
	// TagKarpenterDiscovery is Karpenter's discovery tag, valued with the
	// EKS cluster name: on the cluster's resources, and added by the
	// cluster to the private subnets it uses. An EC2NodeClass selects
	// subnets with {karpenter.sh/discovery: <cluster>} and the cluster
	// security group with {aws:eks:cluster-name: <cluster>}, a tag EKS sets
	// itself.
	TagKarpenterDiscovery = "karpenter.sh/discovery"
	// TagKubernetesClusterPrefix + <EKS name> = "shared" marks a subnet a
	// cluster uses (EKS, the AWS load balancer controller); one key per
	// cluster, so it works for every cluster of a network.
	TagKubernetesClusterPrefix = "kubernetes.io/cluster/"
	anyIPv4                    = "0.0.0.0/0"
)

// SubnetPlan is one subnet of a NetworkPlan.
type SubnetPlan struct {
	Role   SubnetRole
	CIDR   string
	ZoneID string
}

// NetworkPlan is a network resolved to concrete values: defaults applied,
// CIDRs carved, zones chosen and validated. NetworkResources only renders
// it.
type NetworkPlan struct {
	// Namespace is the tenant namespace; ProviderConfigName the account's
	// ProviderConfig in it.
	Namespace          string
	ProviderConfigName string
	TenantID           string
	// NetworkID is the cloud.v1 Network.id (labels and tags); NetworkName
	// its client-chosen name (tag).
	NetworkID   string
	NetworkName string
	// Prefix starts every managed resource and AWS Name tag:
	// "<Prefix>-<suffix>". NetworkPrefix builds it.
	Prefix    string
	Region    string
	CIDRBlock string
	Subnets   []SubnetPlan
	NAT       NATMode
	// Tags are the network's own tags (NetworkSpec.tags).
	Tags map[string]string
	// AccountID is the AWS account of the network; only flow logs need it.
	AccountID string
	// FlowLogs, when set, adds a flow log on the VPC with its log group and
	// delivery role. Nil renders none.
	FlowLogs *FlowLogPlan
}

// Zones returns the plan's zones, in order of first appearance.
func (p NetworkPlan) Zones() []string {
	var zones []string
	seen := map[string]bool{}
	for _, s := range p.Subnets {
		if !seen[s.ZoneID] {
			seen[s.ZoneID] = true
			zones = append(zones, s.ZoneID)
		}
	}
	return zones
}

// NetworkPrefix is "<name>-<hash8>", hash8 being the first 8 hex chars of
// sha256(tenantID/networkID): readable, and unique even when two networks
// of a tenant share a name.
func NetworkPrefix(tenantID, networkID, name string) string {
	sum := sha256.Sum256([]byte(tenantID + "/" + networkID))
	return name + "-" + hex.EncodeToString(sum[:4])
}

// NetworkResources renders the plan as managed resources, all in the
// tenant namespace, using the account's ProviderConfig, labelled with the
// tenant and network (crossplane.NetworkLabels) and referencing each other
// by name:
//
//	VPC, InternetGateway
//	Subnet per planned subnet
//	EIP + NATGateway per NAT (in a public subnet)
//	RouteTable "public" + Route 0.0.0.0/0 -> igw + association per public subnet
//	RouteTable per private route (one, or one per zone) + Route 0.0.0.0/0 -> nat
//	  + association per private subnet
//	with FlowLogs: the flow log, its log group and delivery role (flowLogs)
func NetworkResources(p NetworkPlan) ([]client.Object, error) {
	b := networkBuilder{p: p}
	vpc := b.vpc()
	objects := []client.Object{vpc, b.internetGateway()}

	subnetNames := map[SubnetRole]map[string][]string{SubnetPublic: {}, SubnetPrivate: {}} // role -> zone -> names
	for i, s := range p.Subnets {
		sub := b.subnet(i, s)
		subnetNames[s.Role][s.ZoneID] = append(subnetNames[s.Role][s.ZoneID], sub.Name)
		objects = append(objects, sub)
	}

	// NAT gateways: zone -> NAT name.
	natByZone := map[string]string{}
	natZones := p.Zones()
	if p.NAT == NATSingle {
		natZones = natZones[:1]
	}
	for _, zone := range natZones {
		public := subnetNames[SubnetPublic][zone]
		if len(public) == 0 {
			return nil, fmt.Errorf("NAT gateway in zone %s needs a public subnet there", zone)
		}
		eip, nat := b.nat(zone, public[0])
		objects = append(objects, eip, nat)
		natByZone[zone] = nat.Name
	}

	// Public routing.
	publicRT := b.routeTable("public")
	objects = append(objects, publicRT, b.route("public", publicRT.Name, routeTarget{gateway: b.name("igw")}))
	for _, zone := range p.Zones() {
		for _, sub := range subnetNames[SubnetPublic][zone] {
			objects = append(objects, b.association(sub, publicRT.Name))
		}
	}

	// Private routing: one table per NAT gateway.
	for _, zone := range p.Zones() {
		privates := subnetNames[SubnetPrivate][zone]
		if len(privates) == 0 {
			continue
		}
		natZone := zone
		if p.NAT == NATSingle {
			natZone = natZones[0]
		}
		key := "private-" + natZone
		rt := b.routeTable(key)
		if !containsName(objects, rt.Name) {
			objects = append(objects, rt, b.route(key, rt.Name, routeTarget{nat: natByZone[natZone]}))
		}
		for _, sub := range privates {
			objects = append(objects, b.association(sub, rt.Name))
		}
	}
	if p.FlowLogs != nil {
		flowLogs, err := b.flowLogs()
		if err != nil {
			return nil, err
		}
		objects = append(objects, flowLogs...)
	}
	return objects, nil
}

// NetworkDeleteOrder deletes a network from the inside out, the order AWS
// accepts: the flow log, network ACL rules, routes and subnet
// associations; then the flow log's log group and its role's policy
// attachment, network ACLs, route tables and NAT gateways; then the
// delivery role and its policy and the NAT gateways' EIPs; then subnets,
// the internet gateway (it detaches only once no public IP is left) and
// the default network ACL (only its object goes: it is owned without
// Delete); then the VPC.
//
// The flow log's Usages (they hold its log group and role while it
// exists) sit in the tier after it: Kubernetes garbage-collects them with
// the flow log, and a Usage is never deleted before its user.
var NetworkDeleteOrder = sequence.DeleteOrder{
	{"FlowLog", "NetworkACLRule", "RouteTableAssociation", "Route"},
	{"Usage", "Group", "RolePolicyAttachment", "NetworkACL", "RouteTable", "NATGateway"},
	{"Role", "Policy", "EIP"},
	{"Subnet", "InternetGateway", "DefaultNetworkACL"},
	{"VPC"},
}

// NetworkSet is NetworkResources as a set to apply in order: a public NAT
// gateway can only be created in a VPC whose internet gateway is attached
// (created earlier it fails), so the NAT gateways wait for the
// InternetGateway to be Ready (the provider creates and attaches it in
// one step).
func NetworkSet(p NetworkPlan) (sequence.Set, error) {
	objects, err := NetworkResources(p)
	if err != nil {
		return nil, err
	}
	var gateway client.Object
	for _, obj := range objects {
		if _, ok := obj.(*awsec2.InternetGateway); ok {
			gateway = obj
		}
	}
	set := sequence.Of(objects...)
	for i, item := range set {
		if _, ok := item.Object.(*awsec2.NATGateway); ok && gateway != nil {
			set[i].After = []sequence.Condition{sequence.Ready(gateway)}
		}
	}
	return protected(set), nil
}

func containsName(objects []client.Object, name string) bool {
	for _, o := range objects {
		if o.GetName() == name {
			return true
		}
	}
	return false
}

type networkBuilder struct{ p NetworkPlan }

func (b networkBuilder) name(suffix string) string { return b.p.Prefix + "-" + suffix }

func (b networkBuilder) meta(suffix string, extra map[string]string) metav1.ObjectMeta {
	labels := crossplane.NetworkLabels(b.p.TenantID, b.p.NetworkID)
	maps.Copy(labels, extra)
	return metav1.ObjectMeta{Name: b.name(suffix), Namespace: b.p.Namespace, Labels: labels}
}

func (b networkBuilder) spec() xpv2.ManagedResourceSpec {
	return xpv2.ManagedResourceSpec{ProviderConfigReference: ProviderConfigRef(b.p.ProviderConfigName)}
}

// tags returns the AWS tags of one resource: the network's, then the
// platform's (which win), then extra.
func (b networkBuilder) tags(suffix string, extra map[string]string) map[string]*string {
	out := map[string]*string{}
	for k, v := range b.p.Tags {
		out[k] = ptr.To(v)
	}
	out[TagName] = ptr.To(b.name(suffix))
	out[TagTenant] = ptr.To(b.p.TenantID)
	out[TagNetwork] = ptr.To(b.p.NetworkID)
	out[TagNetworkName] = ptr.To(b.p.NetworkName)
	for k, v := range extra {
		out[k] = ptr.To(v)
	}
	return out
}

func (b networkBuilder) ref(suffix string) *xpv2.NamespacedReference {
	return &xpv2.NamespacedReference{Name: suffix}
}

func (b networkBuilder) vpc() *awsec2.VPC {
	return &awsec2.VPC{
		ObjectMeta: b.meta("vpc", nil),
		Spec: awsec2.VPCSpec{
			ManagedResourceSpec: b.spec(),
			ForProvider: awsec2.VPCParameters_2{
				Region:             ptr.To(b.p.Region),
				CidrBlock:          ptr.To(b.p.CIDRBlock),
				EnableDNSHostnames: ptr.To(true), // EKS needs both
				EnableDNSSupport:   ptr.To(true),
				Tags:               b.tags("vpc", nil),
			},
		},
	}
}

func (b networkBuilder) internetGateway() *awsec2.InternetGateway {
	return &awsec2.InternetGateway{
		ObjectMeta: b.meta("igw", nil),
		Spec: awsec2.InternetGatewaySpec{
			ManagedResourceSpec: b.spec(),
			ForProvider: awsec2.InternetGatewayParameters_2{
				Region:   ptr.To(b.p.Region),
				VPCIDRef: b.ref(b.name("vpc")),
				Tags:     b.tags("igw", nil),
			},
		},
	}
}

// subnet names a subnet "<network>-<role>-<zone>", with "-<n>" for the
// second and later subnet of a role in one zone.
func (b networkBuilder) subnet(i int, s SubnetPlan) *awsec2.Subnet {
	suffix := string(s.Role) + "-" + s.ZoneID
	if n := b.ordinal(i, s); n > 0 {
		suffix = fmt.Sprintf("%s-%d", suffix, n+1)
	}
	elbTag := map[string]string{TagInternalELB: "1"}
	if s.Role == SubnetPublic {
		elbTag = map[string]string{TagELB: "1"}
	}
	return &awsec2.Subnet{
		ObjectMeta: b.meta(suffix, map[string]string{
			crossplane.LabelSubnetRole: string(s.Role),
			crossplane.LabelZone:       s.ZoneID,
		}),
		Spec: awsec2.SubnetSpec{
			ManagedResourceSpec: b.spec(),
			ForProvider: awsec2.SubnetParameters_2{
				Region:              ptr.To(b.p.Region),
				VPCIDRef:            b.ref(b.name("vpc")),
				CidrBlock:           ptr.To(s.CIDR),
				AvailabilityZoneID:  ptr.To(s.ZoneID),
				MapPublicIPOnLaunch: ptr.To(s.Role == SubnetPublic),
				Tags:                b.tags(suffix, elbTag),
			},
		},
	}
}

// ordinal counts the earlier subnets with the same role and zone.
func (b networkBuilder) ordinal(i int, s SubnetPlan) int {
	n := 0
	for _, o := range b.p.Subnets[:i] {
		if o.Role == s.Role && o.ZoneID == s.ZoneID {
			n++
		}
	}
	return n
}

func (b networkBuilder) nat(zone, publicSubnet string) (*awsec2.EIP, *awsec2.NATGateway) {
	eip := &awsec2.EIP{
		ObjectMeta: b.meta("eip-"+zone, map[string]string{crossplane.LabelZone: zone}),
		Spec: awsec2.EIPSpec{
			ManagedResourceSpec: b.spec(),
			ForProvider: awsec2.EIPParameters{
				Region: ptr.To(b.p.Region),
				Domain: ptr.To("vpc"),
				Tags:   b.tags("eip-"+zone, nil),
			},
		},
	}
	nat := &awsec2.NATGateway{
		ObjectMeta: b.meta("nat-"+zone, map[string]string{crossplane.LabelZone: zone}),
		Spec: awsec2.NATGatewaySpec{
			ManagedResourceSpec: b.spec(),
			ForProvider: awsec2.NATGatewayParameters_2{
				Region:           ptr.To(b.p.Region),
				AvailabilityMode: ptr.To("zonal"),
				ConnectivityType: ptr.To("public"),
				AllocationIDRef:  b.ref(eip.Name),
				SubnetIDRef:      b.ref(publicSubnet),
				Tags:             b.tags("nat-"+zone, nil),
			},
		},
	}
	return eip, nat
}

func (b networkBuilder) routeTable(key string) *awsec2.RouteTable {
	return &awsec2.RouteTable{
		ObjectMeta: b.meta("rt-"+key, nil),
		Spec: awsec2.RouteTableSpec{
			ManagedResourceSpec: b.spec(),
			ForProvider: awsec2.RouteTableParameters_2{
				Region:   ptr.To(b.p.Region),
				VPCIDRef: b.ref(b.name("vpc")),
				Tags:     b.tags("rt-"+key, nil),
			},
		},
	}
}

type routeTarget struct{ gateway, nat string }

func (b networkBuilder) route(key, routeTable string, to routeTarget) *awsec2.Route {
	params := awsec2.RouteParameters_2{
		Region:               ptr.To(b.p.Region),
		RouteTableIDRef:      b.ref(routeTable),
		DestinationCidrBlock: ptr.To(anyIPv4),
	}
	if to.gateway != "" {
		params.GatewayIDRef = b.ref(to.gateway)
	}
	if to.nat != "" {
		params.NATGatewayIDRef = b.ref(to.nat)
	}
	return &awsec2.Route{
		ObjectMeta: b.meta("route-"+key, nil),
		Spec:       awsec2.RouteSpec{ManagedResourceSpec: b.spec(), ForProvider: params},
	}
}

func (b networkBuilder) association(subnet, routeTable string) *awsec2.RouteTableAssociation {
	// The subnet name already holds the prefix: "<prefix>-<role>-<zone>".
	return &awsec2.RouteTableAssociation{
		ObjectMeta: metav1.ObjectMeta{
			Name:      subnet + "-rta",
			Namespace: b.p.Namespace,
			Labels:    crossplane.NetworkLabels(b.p.TenantID, b.p.NetworkID),
		},
		Spec: awsec2.RouteTableAssociationSpec{
			ManagedResourceSpec: b.spec(),
			ForProvider: awsec2.RouteTableAssociationParameters{
				Region:          ptr.To(b.p.Region),
				SubnetIDRef:     b.ref(subnet),
				RouteTableIDRef: b.ref(routeTable),
			},
		},
	}
}

// NetworkLists are empty lists of every managed resource kind a network
// has (NetworkResources, ImportedNetworkResources), for listing (status)
// and deleting a network's resources by label.
func NetworkLists() []client.ObjectList {
	return []client.ObjectList{
		&awsec2.NetworkACLRuleList{}, &awsec2.NetworkACLList{}, &awsec2.DefaultNetworkACLList{},
		&awsec2.FlowLogList{}, &awsiam.RolePolicyAttachmentList{}, &awsiam.PolicyList{}, &awsiam.RoleList{},
		&awscwl.GroupList{},
		&awsec2.RouteTableAssociationList{}, &awsec2.RouteList{}, &awsec2.RouteTableList{},
		&awsec2.NATGatewayList{}, &awsec2.EIPList{}, &awsec2.SubnetList{},
		&awsec2.InternetGatewayList{}, &awsec2.VPCList{},
	}
}

// NetworkObjects are empty objects of the kinds in NetworkLists, in the
// same order, for DeleteAllOf.
func NetworkObjects() []client.Object {
	return []client.Object{
		&awsec2.NetworkACLRule{}, &awsec2.NetworkACL{}, &awsec2.DefaultNetworkACL{},
		&awsec2.FlowLog{}, &awsiam.RolePolicyAttachment{}, &awsiam.Policy{}, &awsiam.Role{},
		&awscwl.Group{},
		&awsec2.RouteTableAssociation{}, &awsec2.Route{}, &awsec2.RouteTable{},
		&awsec2.NATGateway{}, &awsec2.EIP{}, &awsec2.Subnet{},
		&awsec2.InternetGateway{}, &awsec2.VPC{},
	}
}
