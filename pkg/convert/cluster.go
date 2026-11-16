package convert

import (
	"maps"
	"slices"

	"k8s.io/utils/ptr"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// ClusterSpecToAPI is the kind's spec for a contract spec. networkName is
// the Network object the spec's network.network_id stands for. The fields
// of the kind that are not in the contract's spec (displayName, tags,
// importFrom) are the caller's to set.
//
// The remainder is what the kind has no field for. Every field of the
// contract's ClusterSpec has one now, so it is always nil; it stays in the
// signature so the store's remainder annotation keeps one shape for
// networks and clusters.
func ClusterSpecToAPI(p *cloudv1.ClusterSpec, networkName string) (cloudv1alpha1.ClusterSpec, *cloudv1.ClusterSpec) {
	n := p.GetNetwork()
	spec := cloudv1alpha1.ClusterSpec{
		Region: p.GetRegion(),
		Network: cloudv1alpha1.ClusterNetwork{
			NetworkRef: cloudv1alpha1.LocalReference{Name: networkName},
			ZoneIDs:    slices.Clone(n.GetZoneIds()), PodCIDR: n.GetPodCidr(), ServiceCIDR: n.GetServiceCidr(),
		},
		ControlPlane:      controlPlaneToAPI(p.GetControlPlane()),
		DefaultNodePool:   nodePoolToAPI(p.GetDefaultNodePool()),
		Access:            accessToAPI(p.GetAccess()),
		ReleaseChannel:    cloudv1alpha1.ReleaseChannelName(camel(p.GetReleaseChannel(), prefixReleaseChannel)),
		UpgradeMode:       cloudv1alpha1.UpgradeMode(camel(p.GetUpgradeMode(), prefixUpgradeMode)),
		MaintenanceWindow: windowToAPI(p.GetMaintenanceWindow()),
		NaaS:              p.Naas,
		AWS:               awsToAPI(p.GetAws()),
	}
	return spec, nil
}

func windowToAPI(w *cloudv1.MaintenanceWindow) *cloudv1alpha1.MaintenanceWindow {
	if w == nil {
		return nil
	}
	out := &cloudv1alpha1.MaintenanceWindow{Start: w.GetStart(), Duration: w.GetDuration()}
	for _, d := range w.GetDays() {
		out.Days = append(out.Days, cloudv1alpha1.Weekday(camel(d, prefixWeekday)))
	}
	return out
}

func windowFromAPI(w *cloudv1alpha1.MaintenanceWindow) *cloudv1.MaintenanceWindow {
	if w == nil {
		return nil
	}
	out := &cloudv1.MaintenanceWindow{Start: w.Start, Duration: w.Duration}
	for _, d := range w.Days {
		out.Days = append(out.Days, cloudv1.Weekday(uncamel(cloudv1.Weekday(0).Descriptor(), prefixWeekday, string(d))))
	}
	return out
}

// ClusterSpecFromAPI is the contract spec of a kind's spec. networkID is
// the row ID of the network the spec's networkRef names. The last
// argument is what ClusterSpecToAPI returned as the remainder; it has
// nothing to carry (see there) and is ignored.
func ClusterSpecFromAPI(s cloudv1alpha1.ClusterSpec, networkID string, _ *cloudv1.ClusterSpec) *cloudv1.ClusterSpec {
	p := &cloudv1.ClusterSpec{
		Region: s.Region,
		Network: &cloudv1.ClusterNetwork{
			NetworkId: networkID, ZoneIds: slices.Clone(s.Network.ZoneIDs),
			PodCidr: s.Network.PodCIDR, ServiceCidr: s.Network.ServiceCIDR,
		},
		ControlPlane:    controlPlaneFromAPI(s.ControlPlane),
		DefaultNodePool: nodePoolFromAPI(s.DefaultNodePool),
		Access:          accessFromAPI(s.Access),
		ReleaseChannel: cloudv1.ReleaseChannel(uncamel(
			cloudv1.ReleaseChannel(0).Descriptor(), prefixReleaseChannel, string(s.ReleaseChannel))),

		UpgradeMode: cloudv1.UpgradeMode(uncamel(
			cloudv1.UpgradeMode(0).Descriptor(), prefixUpgradeMode, string(s.UpgradeMode))),
		MaintenanceWindow: windowFromAPI(s.MaintenanceWindow),
		Naas:              s.NaaS,
	}
	if s.AWS != nil {
		p.Provider = &cloudv1.ClusterSpec_Aws{Aws: awsFromAPI(s.AWS)}
	}
	return p
}

func controlPlaneToAPI(p *cloudv1.ControlPlaneSpec) *cloudv1alpha1.ControlPlaneSpec {
	if p == nil {
		return nil
	}
	out := &cloudv1alpha1.ControlPlaneSpec{KubernetesVersion: p.GetKubernetesVersion()}
	if a := p.GetEndpointAccess(); a != nil {
		out.EndpointAccess = &cloudv1alpha1.EndpointAccess{
			PublicAccessCIDRs: slices.Clone(a.GetPublicAccessCidrs()),
			PrivateAccess:     a.EndpointPrivateAccess, PublicAccess: a.EndpointPublicAccess,
		}
	}
	if e := p.GetSecretsEncryption(); e != nil {
		out.SecretsEncryption = &cloudv1alpha1.SecretsEncryption{Enabled: e.GetEnabled(), KMSKeyARN: e.GetKmsKeyArn()}
	}
	return out
}

func controlPlaneFromAPI(c *cloudv1alpha1.ControlPlaneSpec) *cloudv1.ControlPlaneSpec {
	if c == nil {
		return nil
	}
	out := &cloudv1.ControlPlaneSpec{KubernetesVersion: c.KubernetesVersion}
	if a := c.EndpointAccess; a != nil {
		out.EndpointAccess = &cloudv1.EndpointAccess{
			PublicAccessCidrs:     slices.Clone(a.PublicAccessCIDRs),
			EndpointPrivateAccess: a.PrivateAccess, EndpointPublicAccess: a.PublicAccess,
		}
	}
	if e := c.SecretsEncryption; e != nil {
		out.SecretsEncryption = &cloudv1.SecretsEncryption{Enabled: e.Enabled, KmsKeyArn: e.KMSKeyARN}
	}
	return out
}

func nodePoolToAPI(p *cloudv1.NodePoolSpec) *cloudv1alpha1.NodePoolSpec {
	if p == nil {
		return nil
	}
	out := &cloudv1alpha1.NodePoolSpec{
		InstanceTypes: slices.Clone(p.GetInstanceTypes()),
		CapacityType:  cloudv1alpha1.CapacityType(camel(p.GetCapacityType(), prefixCapacity)),
		Labels:        maps.Clone(p.GetLabels()),
	}
	if sc := p.GetScalingConfig(); sc != nil {
		out.Scaling = &cloudv1alpha1.ScalingConfig{MinSize: toInt32(sc.GetMinSize()), MaxSize: toInt32(sc.GetMaxSize())}
		if sc.DesiredSize != nil {
			out.Scaling.DesiredSize = ptr.To(toInt32(sc.GetDesiredSize()))
		}
	}
	if d := p.GetDiskSizeGb(); d != 0 {
		out.DiskSizeGB = ptr.To(toInt32(d))
	}
	for _, t := range p.GetTaints() {
		out.Taints = append(out.Taints, cloudv1alpha1.Taint{Key: t.GetKey(), Value: t.GetValue(),
			Effect: cloudv1alpha1.TaintEffect(camel(t.GetEffect(), prefixTaintEffect))})
	}
	return out
}

func nodePoolFromAPI(n *cloudv1alpha1.NodePoolSpec) *cloudv1.NodePoolSpec {
	if n == nil {
		return nil
	}
	out := &cloudv1.NodePoolSpec{
		InstanceTypes: slices.Clone(n.InstanceTypes),
		CapacityType: cloudv1.CapacityType(uncamel(
			cloudv1.CapacityType(0).Descriptor(), prefixCapacity, string(n.CapacityType))),
		DiskSizeGb: toUint32(ptr.Deref(n.DiskSizeGB, 0)),
		Labels:     maps.Clone(n.Labels),
	}
	if sc := n.Scaling; sc != nil {
		out.ScalingConfig = &cloudv1.ScalingConfig{MinSize: toUint32(sc.MinSize), MaxSize: toUint32(sc.MaxSize)}
		if sc.DesiredSize != nil {
			out.ScalingConfig.DesiredSize = ptr.To(toUint32(*sc.DesiredSize))
		}
	}
	for _, t := range n.Taints {
		out.Taints = append(out.Taints, &cloudv1.Taint{Key: t.Key, Value: t.Value, Effect: cloudv1.TaintEffect(uncamel(
			cloudv1.TaintEffect(0).Descriptor(), prefixTaintEffect, string(t.Effect)))})
	}
	return out
}

func accessToAPI(p *cloudv1.AccessSpec) *cloudv1alpha1.AccessSpec {
	if p == nil {
		return nil
	}
	out := &cloudv1alpha1.AccessSpec{AdminPrincipals: slices.Clone(p.GetAdminPrincipals())}
	for _, e := range p.GetAccessEntries() {
		entry := cloudv1alpha1.AccessEntry{PrincipalARN: e.GetPrincipalArn(),
			Type: cloudv1alpha1.AccessEntryType(awsName(e.GetType()))}
		for _, policy := range e.GetPolicies() {
			entry.Policies = append(entry.Policies, cloudv1alpha1.AccessPolicy{
				PolicyARN:  policy.GetPolicyArn(),
				Scope:      cloudv1alpha1.AccessPolicyScope(camel(policy.GetScope(), prefixPolicyScope)),
				Namespaces: slices.Clone(policy.GetNamespaces()),
			})
		}
		out.AccessEntries = append(out.AccessEntries, entry)
	}
	if o := p.GetOidc(); o != nil {
		out.OIDC = &cloudv1alpha1.OIDCIdentityProvider{Name: o.GetName(), IssuerURL: o.GetIssuerUrl(), ClientID: o.GetClientId(),
			UsernameClaim: o.GetUsernameClaim(), UsernamePrefix: o.GetUsernamePrefix(),
			GroupsClaim: o.GetGroupsClaim(), GroupsPrefix: o.GetGroupsPrefix(),
			RequiredClaims: maps.Clone(o.GetRequiredClaims())}
		if e := o.GetEntra(); e != nil {
			out.OIDC.Entra = &cloudv1alpha1.EntraIdentityProvider{TenantID: e.GetTenantId(), ClientID: e.GetClientId()}
		}
	}
	return out
}

func accessFromAPI(a *cloudv1alpha1.AccessSpec) *cloudv1.AccessSpec {
	if a == nil {
		return nil
	}
	out := &cloudv1.AccessSpec{AdminPrincipals: slices.Clone(a.AdminPrincipals)}
	for _, e := range a.AccessEntries {
		entry := &cloudv1.AccessEntry{PrincipalArn: e.PrincipalARN,
			Type: cloudv1.AccessEntryType(fromAWSName(cloudv1.AccessEntryType(0).Descriptor(), string(e.Type)))}
		for _, policy := range e.Policies {
			entry.Policies = append(entry.Policies, &cloudv1.AccessPolicy{
				PolicyArn: policy.PolicyARN,
				Scope: cloudv1.AccessPolicyScope(uncamel(
					cloudv1.AccessPolicyScope(0).Descriptor(), prefixPolicyScope, string(policy.Scope))),
				Namespaces: slices.Clone(policy.Namespaces),
			})
		}
		out.AccessEntries = append(out.AccessEntries, entry)
	}
	if o := a.OIDC; o != nil {
		out.Oidc = &cloudv1.OidcIdentityProvider{Name: o.Name, IssuerUrl: o.IssuerURL, ClientId: o.ClientID,
			UsernameClaim: o.UsernameClaim, UsernamePrefix: o.UsernamePrefix,
			GroupsClaim: o.GroupsClaim, GroupsPrefix: o.GroupsPrefix,
			RequiredClaims: maps.Clone(o.RequiredClaims)}
		if e := o.Entra; e != nil {
			out.Oidc.Entra = &cloudv1.EntraIdentityProvider{TenantId: e.TenantID, ClientId: e.ClientID}
		}
	}
	return out
}

func awsToAPI(p *cloudv1.AwsClusterSpec) *cloudv1alpha1.AWSClusterSpec {
	if p == nil {
		return nil
	}
	out := &cloudv1alpha1.AWSClusterSpec{
		IRSA:                       p.Irsa,
		AdditionalSecurityGroupIDs: slices.Clone(p.GetAdditionalSecurityGroupIds()),
		DefaultNodePool:            awsNodePoolToAPI(p.GetDefaultNodePool()),
	}
	if c := p.GetAccessConfig(); c != nil {
		out.AccessConfig = &cloudv1alpha1.AWSAccessConfig{
			AuthenticationMode: cloudv1alpha1.AWSAuthenticationMode(awsName(c.GetAuthenticationMode()))}
	}
	for _, t := range p.GetEnabledClusterLogTypes() {
		out.EnabledClusterLogTypes = append(out.EnabledClusterLogTypes, cloudv1alpha1.AWSClusterLogType(awsName(t)))
	}
	if u := p.GetUpgradePolicy(); u != nil {
		out.UpgradePolicy = &cloudv1alpha1.AWSUpgradePolicy{SupportType: cloudv1alpha1.AWSSupportType(awsName(u.GetSupportType()))}
	}
	for _, a := range p.GetAddons() {
		out.Addons = append(out.Addons, cloudv1alpha1.AWSAddon{Name: a.GetName(), Version: a.GetVersion(),
			ConfigurationValues: a.GetConfigurationValues(), S3BucketNames: slices.Clone(a.GetS3BucketNames())})
	}
	if lg := p.GetControlPlaneLogGroup(); lg != nil {
		out.ControlPlaneLogGroup = &cloudv1alpha1.AWSControlPlaneLogGroup{Enabled: lg.Enabled,
			RetentionDays: toInt32(lg.GetRetentionDays())}
	}
	return out
}

func awsNodePoolToAPI(p *cloudv1.AwsNodePoolSpec) *cloudv1alpha1.AWSNodePoolSpec {
	if p == nil {
		return nil
	}
	out := &cloudv1alpha1.AWSNodePoolSpec{AMIType: cloudv1alpha1.AWSAMIType(awsName(p.GetAmiType()))}
	if lt := p.GetLaunchTemplate(); lt != nil {
		out.LaunchTemplate = &cloudv1alpha1.AWSLaunchTemplate{ID: lt.GetId(), Name: lt.GetName(), Version: lt.GetVersion(),
			SecurityGroupIDs: slices.Clone(lt.GetSecurityGroupIds()), ImageID: lt.GetImageId(), UserData: lt.GetUserData()}
	}
	return out
}

func awsFromAPI(a *cloudv1alpha1.AWSClusterSpec) *cloudv1.AwsClusterSpec {
	out := &cloudv1.AwsClusterSpec{
		Irsa:                       a.IRSA,
		AdditionalSecurityGroupIds: slices.Clone(a.AdditionalSecurityGroupIDs),
		DefaultNodePool:            awsNodePoolFromAPI(a.DefaultNodePool),
	}
	if c := a.AccessConfig; c != nil {
		out.AccessConfig = &cloudv1.AwsAccessConfig{AuthenticationMode: cloudv1.AwsAuthenticationMode(fromAWSName(
			cloudv1.AwsAuthenticationMode(0).Descriptor(), string(c.AuthenticationMode)))}
	}
	for _, t := range a.EnabledClusterLogTypes {
		out.EnabledClusterLogTypes = append(out.EnabledClusterLogTypes,
			cloudv1.AwsClusterLogType(fromAWSName(cloudv1.AwsClusterLogType(0).Descriptor(), string(t))))
	}
	if u := a.UpgradePolicy; u != nil {
		out.UpgradePolicy = &cloudv1.AwsUpgradePolicy{SupportType: cloudv1.AwsSupportType(fromAWSName(
			cloudv1.AwsSupportType(0).Descriptor(), string(u.SupportType)))}
	}
	for _, addon := range a.Addons {
		out.Addons = append(out.Addons, &cloudv1.AwsAddon{Name: addon.Name, Version: addon.Version,
			ConfigurationValues: addon.ConfigurationValues, S3BucketNames: slices.Clone(addon.S3BucketNames)})
	}
	if lg := a.ControlPlaneLogGroup; lg != nil {
		out.ControlPlaneLogGroup = &cloudv1.AwsControlPlaneLogGroup{Enabled: lg.Enabled, RetentionDays: toUint32(lg.RetentionDays)}
	}
	return out
}

func awsNodePoolFromAPI(n *cloudv1alpha1.AWSNodePoolSpec) *cloudv1.AwsNodePoolSpec {
	if n == nil {
		return nil
	}
	out := &cloudv1.AwsNodePoolSpec{AmiType: cloudv1.AwsAmiType(fromAWSName(cloudv1.AwsAmiType(0).Descriptor(), string(n.AMIType)))}
	if lt := n.LaunchTemplate; lt != nil {
		out.LaunchTemplate = &cloudv1.AwsLaunchTemplate{Id: lt.ID, Name: lt.Name, Version: lt.Version,
			SecurityGroupIds: slices.Clone(lt.SecurityGroupIDs), ImageId: lt.ImageID, UserData: lt.UserData}
	}
	return out
}
