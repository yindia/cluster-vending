package cluster

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"buf.build/go/protovalidate"
	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	awsiam "github.com/upbound/provider-aws/v2/apis/namespaced/iam/v1beta1"
	"google.golang.org/protobuf/reflect/protoreflect"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	awscloud "github.com/yindia/cluster-vending/pkg/cloud/aws"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/kube"
)

// specAddons are the add-ons the contract has a name for (AwsAddon.name),
// without aws-mountpoint-s3-csi-driver: its buckets cannot be read back.
var specAddons = []string{"vpc-cni", "coredns", "kube-proxy", "eks-pod-identity-agent", "aws-ebs-csi-driver",
	"aws-efs-csi-driver", "aws-fsx-csi-driver", "snapshot-controller", "cert-manager", "metrics-server", "kube-state-metrics"}

// importInput is what an import plan is made from.
type importInput struct {
	found *awscloud.Cluster
	// name is the cluster's (and EKS cluster's) name; nodeGroup the node
	// group that becomes the default node pool.
	name, nodeGroup string
	networkID       string
	region          string
	// subnets are the network's private subnets.
	subnets []privateSubnet
	// base carries the namespace, names and account of the cluster.
	base   crossplaneaws.ClusterPlan
	scheme *runtime.Scheme
}

// clusterImport is what ImportCluster does with a discovered cluster.
type clusterImport struct {
	// spec is the cluster's spec, derived from what was found.
	spec     *cloudv1.ClusterSpec
	adoption crossplaneaws.Adoption
	// objects are the Observe-only objects that import it, in creation
	// order; podIdentities the external names (association IDs) of the
	// PodIdentityAssociation objects among them, by object name: the
	// provider assigns those, so they are not rendered (Adoption) but set
	// once.
	objects       []client.Object
	podIdentities map[string]string
	lines         []*cloudv1.ImportedResource
}

func (p clusterImport) blocked() bool {
	return slices.ContainsFunc(p.lines, func(l *cloudv1.ImportedResource) bool {
		return l.GetDisposition() == cloudv1.Disposition_DISPOSITION_BLOCKING
	})
}

func notImported(lines []*cloudv1.ImportedResource) []*cloudv1.ImportedResource {
	var out []*cloudv1.ImportedResource
	for _, l := range lines {
		if l.GetDisposition() == cloudv1.Disposition_DISPOSITION_NOT_IMPORTED {
			out = append(out, l)
		}
	}
	return out
}

const eksAPIVersion = "eks.aws.m.upbound.io/v1beta1"

// planClusterImport maps a discovered cluster onto the contract. The spec
// is derived from it, rendered like any cluster's, and each rendered
// object adopts the cloud resource it describes (crossplaneaws.Adopt), so
// the cluster is rendered the same way before and after the takeover.
// One plan line per object:
//
//	MATCHES       adopted; the render equals what the cloud has
//	WILL_UPDATE   adopted; the takeover changes it (the platform's tags, a
//	              field the contract renders differently)
//	WILL_CREATE   the platform adds it at the takeover (the kubeconfig
//	              object, default add-ons the cluster lacks, ...)
//	CARRIED_OVER  no spec field describes it (extra role policies, the OIDC
//	              provider, the log group, the KMS key, identity provider
//	              configs, other add-ons and pod identities, the control
//	              plane's additional security groups with their rules):
//	              imported as it is
//	NOT_IMPORTED  stays unmanaged (other node groups, EKS-managed access
//	              entries, the node group's launch template and security
//	              groups)
//	BLOCKING      not expressible: a field EKS cannot change differs from
//	              the render, or the spec is not valid
func planClusterImport(in importInput) clusterImport {
	p := &importPlanner{in: in}
	d := in.found
	group := p.preconditions()
	if p.plan.blocked() {
		return p.plan
	}
	zones, subnetIDs, subnetObjects := p.subnets()
	if p.plan.blocked() {
		return p.plan
	}
	spec, err := resolveSpec(p.deriveSpec(group, zones), zones)
	if err == nil {
		err = protovalidate.Validate(&cloudv1.Cluster{Name: in.name, Spec: spec})
	}
	if err != nil {
		p.block("Cluster", d.Name, "the cluster cannot be expressed in the contract: "+err.Error())
		return p.plan
	}
	p.plan.spec = spec
	p.plan.adoption = crossplaneaws.Adoption{ClusterRoleName: d.Role.Name, ClusterRoleTrust: d.Role.AssumeRolePolicy,
		NodeRoleName: d.NodeRole.Name, NodeRoleTrust: d.NodeRole.AssumeRolePolicy, ExternalNames: map[string]string{},
		BootstrapAdmin: d.BootstrapAdmin}
	base := in.base
	base.Adoption = p.plan.adoption
	render := clusterPlan(spec, subnetIDs, base)
	render.SubnetObjects = subnetObjects
	p.adopt(render, group)
	p.carryOver(render)
	p.leftAlone(group)
	return p.plan
}

type importPlanner struct {
	in   importInput
	plan clusterImport
}

func (p *importPlanner) line(apiVersion, kind, name, externalName, specPath string, d cloudv1.Disposition, detail string) {
	p.plan.lines = append(p.plan.lines, &cloudv1.ImportedResource{ApiVersion: apiVersion, Kind: kind, Name: name,
		ExternalName: externalName, SpecPath: specPath, Disposition: d, Detail: detail})
}

func (p *importPlanner) block(kind, externalName, detail string) {
	p.line(eksAPIVersion, kind, "", externalName, "", cloudv1.Disposition_DISPOSITION_BLOCKING, detail)
}

// preconditions checks what must hold before anything is derived and
// returns the default node group.
func (p *importPlanner) preconditions() awscloud.NodeGroup {
	d := p.in.found
	if d.Status != eksActiveStatus {
		p.block("Cluster", d.Name, fmt.Sprintf("the EKS cluster is %s; it must be ACTIVE", d.Status))
	}
	if d.IPFamily != "" && d.IPFamily != "ipv4" {
		p.block("Cluster", d.Name, "IP family "+d.IPFamily+": only IPv4 clusters are supported")
	}
	if d.Role.Unreadable != "" {
		p.block("Role", d.Role.ARN, "the cluster's role cannot be read: "+d.Role.Unreadable)
	}
	i := slices.IndexFunc(d.NodeGroups, func(g awscloud.NodeGroup) bool { return g.Name == p.in.nodeGroup })
	if i < 0 {
		p.block("NodeGroup", p.in.nodeGroup, fmt.Sprintf("the cluster has no node group %q to adopt as the default node pool", p.in.nodeGroup))
		return awscloud.NodeGroup{}
	}
	if d.NodeRole.Unreadable != "" {
		p.block("Role", d.NodeRole.ARN, "the node group's role cannot be read: "+d.NodeRole.Unreadable)
	}
	return d.NodeGroups[i]
}

// subnets maps the cluster's subnets onto the network: its zones, and the
// network's private subnets in them (what a cluster of the network
// renders).
func (p *importPlanner) subnets() (zones, ids, objects []string) {
	byID := map[string]privateSubnet{}
	for _, s := range p.in.subnets {
		byID[s.id] = s
	}
	for _, id := range p.in.found.SubnetIDs {
		s, ok := byID[id]
		if !ok {
			p.line("ec2.aws.m.upbound.io/v1beta1", "Subnet", "", id, "spec.network.network_id", cloudv1.Disposition_DISPOSITION_BLOCKING,
				fmt.Sprintf("the cluster's subnet %s is not a private subnet of network %s; import the cluster's network first", id, p.in.networkID))
			continue
		}
		if !slices.Contains(zones, s.zone) {
			zones = append(zones, s.zone)
		}
	}
	if p.plan.blocked() {
		return nil, nil, nil
	}
	sort.Strings(zones)
	zones, ids, err := chooseSubnets(p.in.subnets, zones)
	if err != nil {
		p.block("Cluster", p.in.found.Name, err.Error())
		return nil, nil, nil
	}
	return zones, ids, objectNames(p.in.subnets, ids)
}

// enumFor is the enum value whose provider-side spelling (its
// cloud.v1.crossplane option) is name.
func enumFor[E interface {
	~int32
	protoreflect.Enum
}](name string) (E, bool) {
	var zero E
	values := zero.Descriptor().Values()
	for i := range values.Len() {
		v := E(values.Get(i).Number())
		if name != "" && crossplaneName(v, "") == name {
			return v, true
		}
	}
	return zero, false
}

// deriveSpec is the spec that says what the cluster is.
func (p *importPlanner) deriveSpec(g awscloud.NodeGroup, zones []string) *cloudv1.ClusterSpec {
	d := p.in.found
	access := &cloudv1.EndpointAccess{}
	if !d.EndpointPrivateAccess {
		access.EndpointPrivateAccess = ptr.To(false)
	}
	if d.EndpointPublicAccess {
		access.PublicAccessCidrs = d.PublicAccessCIDRs
	} else {
		access.EndpointPublicAccess = ptr.To(false)
	}
	controlPlane := &cloudv1.ControlPlaneSpec{KubernetesVersion: d.Version, EndpointAccess: access}
	if d.EncryptionKeyARN != "" {
		controlPlane.SecretsEncryption = &cloudv1.SecretsEncryption{KmsKeyArn: d.EncryptionKeyARN}
	}
	aws := &cloudv1.AwsClusterSpec{AccessConfig: &cloudv1.AwsAccessConfig{}, AdditionalSecurityGroupIds: d.SecurityGroupIDs,
		DefaultNodePool: &cloudv1.AwsNodePoolSpec{}}
	aws.AccessConfig.AuthenticationMode, _ = enumFor[cloudv1.AwsAuthenticationMode](d.AuthenticationMode)
	if st, ok := enumFor[cloudv1.AwsSupportType](d.SupportType); ok {
		aws.UpgradePolicy = &cloudv1.AwsUpgradePolicy{SupportType: st}
	}
	for _, t := range d.LogTypes {
		if lt, ok := enumFor[cloudv1.AwsClusterLogType](t); ok {
			aws.EnabledClusterLogTypes = append(aws.EnabledClusterLogTypes, lt)
		}
	}
	for _, a := range d.Addons {
		if slices.Contains(specAddons, a.Name) {
			aws.Addons = append(aws.Addons, &cloudv1.AwsAddon{Name: a.Name, Version: a.Version, ConfigurationValues: a.ConfigurationValues})
		}
	}
	spec := &cloudv1.ClusterSpec{Region: p.in.region, ControlPlane: controlPlane,
		Network:         &cloudv1.ClusterNetwork{NetworkId: p.in.networkID, ZoneIds: zones, ServiceCidr: d.ServiceCIDR},
		DefaultNodePool: p.nodePool(g, aws.DefaultNodePool),
		Provider:        &cloudv1.ClusterSpec_Aws{Aws: aws}}
	for _, e := range d.AccessEntries {
		if p.eksManaged(e) {
			continue
		}
		entry := &cloudv1.AccessEntry{PrincipalArn: e.PrincipalARN}
		entry.Type, _ = enumFor[cloudv1.AccessEntryType](e.Type)
		for _, policy := range e.Policies {
			scope, _ := enumFor[cloudv1.AccessPolicyScope](policy.Scope)
			entry.Policies = append(entry.Policies, &cloudv1.AccessPolicy{PolicyArn: policy.PolicyARN, Scope: scope, Namespaces: policy.Namespaces})
		}
		if spec.Access == nil {
			spec.Access = &cloudv1.AccessSpec{}
		}
		spec.Access.AccessEntries = append(spec.Access.AccessEntries, entry)
	}
	return spec
}

// nodePool is the node group as a NodePoolSpec; its AWS-only settings go
// to aws.
func (p *importPlanner) nodePool(g awscloud.NodeGroup, aws *cloudv1.AwsNodePoolSpec) *cloudv1.NodePoolSpec {
	pool := &cloudv1.NodePoolSpec{InstanceTypes: g.InstanceTypes, Labels: g.Labels,
		ScalingConfig: &cloudv1.ScalingConfig{MinSize: uint32(g.MinSize), MaxSize: uint32(g.MaxSize), DesiredSize: ptr.To(uint32(g.DesiredSize))}} //nolint:gosec // EKS sizes are small and not negative
	pool.CapacityType, _ = enumFor[cloudv1.CapacityType](g.CapacityType)
	ami, ok := enumFor[cloudv1.AwsAmiType](g.AMIType)
	if !ok {
		p.block("NodeGroup", g.Name, fmt.Sprintf("AMI type %s is not one the contract has", g.AMIType))
	}
	aws.AmiType = ami
	if g.LaunchTemplateID != "" {
		aws.LaunchTemplate = &cloudv1.AwsLaunchTemplate{Id: g.LaunchTemplateID, Version: g.LaunchTemplateVersion}
	} else {
		pool.DiskSizeGb = uint32(g.DiskSizeGB) //nolint:gosec // 20 to 16384
	}
	for _, t := range g.Taints {
		effect, _ := enumFor[cloudv1.TaintEffect](t.Effect)
		pool.Taints = append(pool.Taints, &cloudv1.Taint{Key: t.Key, Value: t.Value, Effect: effect})
	}
	return pool
}

// eksManaged reports whether EKS itself owns an access entry: the entries
// of managed node groups' roles and of service-linked roles.
func (p *importPlanner) eksManaged(e awscloud.AccessEntry) bool {
	return strings.Contains(e.PrincipalARN, ":role/aws-service-role/") ||
		slices.ContainsFunc(p.in.found.NodeGroups, func(g awscloud.NodeGroup) bool { return g.NodeRoleARN == e.PrincipalARN })
}

// identities are the cloud identities the rendered objects adopt.
func (p *importPlanner) identities(prefix string, g awscloud.NodeGroup) crossplaneaws.ClusterIdentities {
	d := p.in.found
	ids := crossplaneaws.ClusterIdentities{ClusterName: d.Name, NodeGroupName: g.Name,
		RoleNames:        map[string]string{prefix + "-cluster-role": d.Role.Name, prefix + "-node-role": d.NodeRole.Name},
		AttachedPolicies: map[string][]string{d.Role.Name: d.Role.PolicyARNs, d.NodeRole.Name: d.NodeRole.PolicyARNs},
		AccessEntries:    map[string][]crossplaneaws.AccessPolicyPlan{}, PodIdentities: map[string]string{}}
	for _, a := range d.Addons {
		ids.Addons = append(ids.Addons, a.Name)
	}
	for _, e := range d.AccessEntries {
		policies := []crossplaneaws.AccessPolicyPlan{}
		for _, policy := range e.Policies {
			policies = append(policies, crossplaneaws.AccessPolicyPlan{PolicyARN: policy.PolicyARN, Scope: policy.Scope, Namespaces: policy.Namespaces})
		}
		ids.AccessEntries[e.PrincipalARN] = policies
	}
	for _, a := range d.PodIdentityAssociations {
		ids.PodIdentities[a.Namespace+"/"+a.ServiceAccount] = a.ID
	}
	return ids
}

// adopt renders the cluster and lets each object adopt its cloud resource:
// MATCHES or WILL_UPDATE (BLOCKING for a field EKS cannot change), and
// WILL_CREATE for what the cloud does not have. Usages are not listed:
// they only order deletions.
func (p *importPlanner) adopt(render crossplaneaws.ClusterPlan, g awscloud.NodeGroup) {
	ids := p.identities(render.Prefix, g)
	p.plan.podIdentities = map[string]string{}
	// Rendered twice: the external names found in the first pass are part
	// of the second render, as of every later one.
	for _, obj := range crossplaneaws.ClusterResources(render) {
		if name, ok := crossplaneaws.Adopt(obj, ids); ok {
			if _, provider := obj.(*awseks.PodIdentityAssociation); provider {
				p.plan.podIdentities[obj.GetName()] = name
				continue
			}
			p.plan.adoption.ExternalNames[obj.GetName()] = name
		}
	}
	render.Adoption = p.plan.adoption
	for _, obj := range crossplaneaws.ClusterResources(render) {
		u, err := kube.ApplyObject(obj, p.in.scheme)
		if err != nil || u.GetKind() == "Usage" {
			continue
		}
		name, exists := crossplaneaws.Adopt(obj, ids)
		if !exists {
			p.line(u.GetAPIVersion(), u.GetKind(), u.GetName(), "", "", cloudv1.Disposition_DISPOSITION_WILL_CREATE,
				"the platform adds it at the takeover")
			continue
		}
		p.plan.objects = append(p.plan.objects, obj)
		disposition, detail := p.compare(u, g)
		p.line(u.GetAPIVersion(), u.GetKind(), u.GetName(), name, "", disposition, detail)
	}
}

// compare tells what the takeover changes on an adopted object: the
// rendered forProvider against what the cloud has.
func (p *importPlanner) compare(rendered *unstructured.Unstructured, g awscloud.NodeGroup) (cloudv1.Disposition, string) {
	actual := p.actual(rendered, g)
	if actual == nil {
		return cloudv1.Disposition_DISPOSITION_MATCHES, ""
	}
	forProvider, _, _ := unstructured.NestedMap(rendered.Object, "spec", "forProvider")
	want := crossplaneaws.FlattenFields(forProvider)
	paths := make([]string, 0, len(want))
	for path := range want {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	var changes, refused []string
	tags := 0
	for _, path := range paths {
		have, known := actual[path]
		before, after := crossplaneaws.FieldString(have), crossplaneaws.FieldString(want[path])
		switch {
		case strings.HasPrefix(path, "tags."):
			if before != after {
				tags++
			}
		case !known || before == after:
		default:
			change := fmt.Sprintf("%s: %s -> %s", path, before, after)
			if class, ok := crossplaneaws.ClassifyField(rendered.GetKind(), path); !ok || class == crossplaneaws.ChangeImmutable || class == crossplaneaws.ChangeAddOnly {
				refused = append(refused, change)
			} else {
				changes = append(changes, change)
			}
		}
	}
	if tags > 0 {
		changes = append(changes, fmt.Sprintf("adds %d tags (the platform's and the cluster's labels); existing tags are kept", tags))
	}
	if rendered.GetKind() == "Addon" {
		// Not readable from EKS, so always said: the platform's add-ons
		// resolve conflicts by overwriting.
		changes = append(changes, "managed with conflict resolution OVERWRITE from now on: add-on settings changed by hand in the cluster are reset when the add-on is next updated")
	}
	switch {
	case len(refused) > 0:
		return cloudv1.Disposition_DISPOSITION_BLOCKING, "EKS cannot change " + strings.Join(refused, "; ")
	case len(changes) > 0:
		return cloudv1.Disposition_DISPOSITION_WILL_UPDATE, strings.Join(changes, "; ")
	default:
		return cloudv1.Disposition_DISPOSITION_MATCHES, ""
	}
}

// actual is what the cloud has for an adopted object, by forProvider path;
// nil for kinds that are not compared (their render has nothing the import
// could differ in).
func (p *importPlanner) actual(rendered *unstructured.Unstructured, g awscloud.NodeGroup) map[string]any {
	d := p.in.found
	out := map[string]any{}
	tags := func(m map[string]string) {
		for k, v := range m {
			out["tags."+k] = v
		}
	}
	switch rendered.GetKind() {
	case "Cluster":
		out["version"] = d.Version
		out["vpcConfig.subnetIds"] = list(d.SubnetIDs)
		out["vpcConfig.endpointPrivateAccess"], out["vpcConfig.endpointPublicAccess"] = d.EndpointPrivateAccess, d.EndpointPublicAccess
		out["vpcConfig.publicAccessCidrs"] = list(d.PublicAccessCIDRs)
		out["vpcConfig.securityGroupIds"] = list(d.SecurityGroupIDs)
		out["accessConfig.authenticationMode"] = d.AuthenticationMode
		if d.BootstrapAdmin != nil {
			out["accessConfig.bootstrapClusterCreatorAdminPermissions"] = *d.BootstrapAdmin
		}
		out["enabledClusterLogTypes"] = list(d.LogTypes)
		out["upgradePolicy.supportType"] = d.SupportType
		out["kubernetesNetworkConfig.serviceIpv4Cidr"] = d.ServiceCIDR
		out["encryptionConfig.provider.keyArn"] = d.EncryptionKeyARN
		tags(d.Tags)
	case "NodeGroup":
		out["version"] = g.Version
		out["subnetIds"], out["instanceTypes"] = list(g.SubnetIDs), list(g.InstanceTypes)
		out["capacityType"], out["amiType"] = g.CapacityType, g.AMIType
		out["diskSize"] = g.DiskSizeGB
		out["scalingConfig.minSize"], out["scalingConfig.maxSize"] = g.MinSize, g.MaxSize
		out["launchTemplate.id"], out["launchTemplate.name"], out["launchTemplate.version"] = g.LaunchTemplateID, g.LaunchTemplateName, g.LaunchTemplateVersion
		for k, v := range g.Labels {
			out["labels."+k] = v
		}
		taints := make([]any, 0, len(g.Taints))
		for _, t := range g.Taints {
			taints = append(taints, map[string]any{"key": t.Key, "value": t.Value, "effect": t.Effect})
		}
		out["taint"] = taints
		tags(g.Tags)
	case "Addon":
		name, _, _ := unstructured.NestedString(rendered.Object, "spec", "forProvider", "addonName")
		for _, a := range d.Addons {
			if a.Name == name {
				out["addonVersion"] = a.Version
				if a.ConfigurationValues != "" {
					out["configurationValues"] = a.ConfigurationValues
				}
				tags(a.Tags)
			}
		}
	case "Role":
		for _, role := range []awscloud.ClusterRole{d.Role, d.NodeRole} {
			if role.Name != "" && role.Name == rendered.GetAnnotations()[crossplaneaws.ExternalNameAnnotation] {
				tags(role.Tags)
			}
		}
	case "AccessEntry":
		principal, _, _ := unstructured.NestedString(rendered.Object, "spec", "forProvider", "principalArn")
		for _, e := range d.AccessEntries {
			if e.PrincipalARN == principal {
				tags(e.Tags)
			}
		}
	case "PodIdentityAssociation":
		// Its tags are not looked up: the ones rendered count as added.
	default:
		return nil
	}
	return out
}

func list(in []string) []any {
	out := make([]any, 0, len(in))
	for _, s := range in {
		out = append(out, s)
	}
	return out
}

// carryOver imports what no spec field describes, as it is.
func (p *importPlanner) carryOver(render crossplaneaws.ClusterPlan) {
	d := p.in.found
	c := crossplaneaws.CarriedOverResources{OIDCProviderARN: d.OIDCProviderARN, OIDCIssuer: d.OIDCIssuer, EncryptionKeyARN: d.EncryptionKeyARN,
		RoleNames:     map[string]string{"cluster-role": d.Role.Name, "node-role": d.NodeRole.Name},
		ExtraPolicies: map[string][]string{"cluster-role": d.Role.PolicyARNs, "node-role": d.NodeRole.PolicyARNs}}
	if d.LogGroup != nil {
		c.LogGroupName, c.LogGroupRetentionDays = d.LogGroup.Name, d.LogGroup.RetentionDays
	}
	rendered := crossplaneaws.ClusterResources(render)
	// Policies and pod identities the render has are not carried over.
	identities := map[string]bool{}
	for _, obj := range rendered {
		switch o := obj.(type) {
		case *awsiam.RolePolicyAttachment:
			if ref := o.Spec.ForProvider.RoleRef; ref != nil && o.Spec.ForProvider.PolicyArn != nil {
				role := strings.TrimPrefix(ref.Name, render.Prefix+"-")
				c.ExtraPolicies[role] = slices.DeleteFunc(slices.Clone(c.ExtraPolicies[role]), func(arn string) bool { return arn == *o.Spec.ForProvider.PolicyArn })
			}
		case *awseks.PodIdentityAssociation:
			identities[ptr.Deref(o.Spec.ForProvider.Namespace, "")+"/"+ptr.Deref(o.Spec.ForProvider.ServiceAccount, "")] = true
		}
	}
	for _, a := range d.PodIdentityAssociations {
		if !identities[a.Namespace+"/"+a.ServiceAccount] {
			c.PodIdentities = append(c.PodIdentities, crossplaneaws.PodIdentityPlan{ID: a.ID, Namespace: a.Namespace, ServiceAccount: a.ServiceAccount})
		}
	}
	for _, a := range d.Addons {
		if !slices.Contains(specAddons, a.Name) {
			c.Addons = append(c.Addons, crossplaneaws.AddonPlan{Name: a.Name, Version: a.Version, ConfigurationValues: a.ConfigurationValues})
		}
	}
	for _, idp := range d.IdentityProviderConfigs {
		c.IdentityProviders = append(c.IdentityProviders, crossplaneaws.IdentityProviderPlan{Name: idp.Name, IssuerURL: idp.IssuerURL,
			ClientID: idp.ClientID, GroupsClaim: idp.GroupsClaim, GroupsPrefix: idp.GroupsPrefix, UsernameClaim: idp.UsernameClaim,
			UsernamePrefix: idp.UsernamePrefix, RequiredClaims: idp.RequiredClaims})
	}
	for _, g := range d.SecurityGroups {
		if g.Unreadable != "" {
			continue // listed NOT_IMPORTED (leftAlone)
		}
		sg := crossplaneaws.SecurityGroupPlan{ID: g.ID, Name: g.Name, Description: g.Description, VPCID: g.VPCID, Tags: g.Tags, Shared: g.Shared}
		for _, r := range g.Rules {
			sg.Rules = append(sg.Rules, crossplaneaws.SecurityGroupRulePlan{ID: r.ID, Egress: r.Egress, Protocol: r.Protocol,
				FromPort: r.FromPort, ToPort: r.ToPort, CIDRIPv4: r.CIDRIPv4, CIDRIPv6: r.CIDRIPv6, PrefixListID: r.PrefixListID,
				ReferencedGroupID: r.ReferencedGroupID, Description: r.Description, Tags: r.Tags})
		}
		c.SecurityGroups = append(c.SecurityGroups, sg)
	}
	for _, obj := range crossplaneaws.CarriedOver(render, c) {
		u, err := kube.ApplyObject(obj, p.in.scheme)
		if err != nil {
			continue
		}
		detail := ""
		if obj.GetAnnotations()[crossplaneaws.RetainAnnotation] == "true" {
			detail = "owned without Delete: something else uses it too, or the platform cannot tell, so it stays when the cluster is deleted"
		}
		p.plan.objects = append(p.plan.objects, obj)
		p.line(u.GetAPIVersion(), u.GetKind(), u.GetName(), obj.GetAnnotations()[crossplaneaws.ExternalNameAnnotation], "",
			cloudv1.Disposition_DISPOSITION_CARRIED_OVER, detail)
	}
}

// leftAlone lists what stays unmanaged.
func (p *importPlanner) leftAlone(g awscloud.NodeGroup) {
	d := p.in.found
	for _, other := range d.NodeGroups {
		if other.Name != g.Name {
			p.line(eksAPIVersion, "NodeGroup", "", other.Name, "", cloudv1.Disposition_DISPOSITION_NOT_IMPORTED,
				"only the default node group is imported: this one stays unmanaged and blocks deleting the cluster until it is removed")
		}
	}
	for _, e := range d.AccessEntries {
		if p.eksManaged(e) {
			p.line(eksAPIVersion, "AccessEntry", "", d.Name+":"+e.PrincipalARN, "", cloudv1.Disposition_DISPOSITION_NOT_IMPORTED,
				"EKS manages the access entries of node group roles and service-linked roles")
		}
	}
	for _, sg := range d.SecurityGroups {
		if sg.Unreadable != "" {
			p.line("ec2.aws.m.upbound.io/v1beta1", "SecurityGroup", "", sg.ID, "spec.aws.additional_security_group_ids",
				cloudv1.Disposition_DISPOSITION_NOT_IMPORTED, "the cluster uses it, but "+sg.Unreadable)
		}
	}
	if g.LaunchTemplateID != "" {
		p.line("ec2.aws.m.upbound.io/v1beta1", "LaunchTemplate", "", g.LaunchTemplateID, "spec.aws.default_node_pool.launch_template",
			cloudv1.Disposition_DISPOSITION_NOT_IMPORTED, "the node group's launch template stays with its owner; the node group refers to it by ID and version")
	}
}
