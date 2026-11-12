package cluster

import (
	"errors"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// ResolveOptions fills the defaults of the optional settings that are in
// use (the log group's retention, an access entry's type and scope, the
// AMI type of a custom AMI) and checks what the schema cannot. Settings
// that are not in use are left as they are: the stored spec of a cluster
// without them does not change.
func ResolveOptions(spec *cloudv1.ClusterSpec) error {
	access := spec.GetControlPlane().GetEndpointAccess()
	if PublicEndpoint(spec) && len(access.GetPublicAccessCidrs()) == 0 {
		return ErrNoPublicAccessCIDRs
	}
	aws := spec.GetAws()
	if lg := aws.GetControlPlaneLogGroup(); lg.GetEnabled() && lg.GetRetentionDays() == 0 {
		lg.RetentionDays = crossplaneaws.DefaultControlPlaneLogRetentionDays
	}
	for _, e := range spec.GetAccess().GetAccessEntries() {
		if e.GetType() == cloudv1.AccessEntryType_ACCESS_ENTRY_TYPE_UNSPECIFIED {
			e.Type = cloudv1.AccessEntryType_ACCESS_ENTRY_TYPE_STANDARD
		}
		for _, p := range e.GetPolicies() {
			if p.GetScope() == cloudv1.AccessPolicyScope_ACCESS_POLICY_SCOPE_UNSPECIFIED {
				p.Scope = cloudv1.AccessPolicyScope_ACCESS_POLICY_SCOPE_CLUSTER
			}
		}
	}
	lt := aws.GetDefaultNodePool().GetLaunchTemplate()
	if lt == nil {
		return nil
	}
	if spec.GetDefaultNodePool().GetDiskSizeGb() != 0 {
		return errors.New("spec.default_node_pool.disk_size_gb cannot be set with a launch template: EKS takes the disk from the template")
	}
	if lt.GetImageId() != "" {
		aws.DefaultNodePool.AmiType = cloudv1.AwsAmiType_AWS_AMI_TYPE_CUSTOM
	}
	return nil
}

// PublicEndpoint reports whether the API server's public endpoint is on
// (the default).
func PublicEndpoint(spec *cloudv1.ClusterSpec) bool {
	access := spec.GetControlPlane().GetEndpointAccess()
	return access == nil || access.EndpointPublicAccess == nil || access.GetEndpointPublicAccess()
}

// optionsPlan maps the optional settings of a resolved spec onto the plan.
// Unset, they leave the plan as it was before they existed.
func optionsPlan(spec *cloudv1.ClusterSpec, p *crossplaneaws.ClusterPlan) {
	if access := spec.GetControlPlane().GetEndpointAccess(); access != nil {
		p.EndpointPrivateAccess, p.EndpointPublicAccess = access.EndpointPrivateAccess, access.EndpointPublicAccess
	}
	aws := spec.GetAws()
	p.AdditionalSecurityGroupIDs = aws.GetAdditionalSecurityGroupIds()
	if lg := aws.GetControlPlaneLogGroup(); lg.GetEnabled() {
		p.LogGroupRetentionDays = int(lg.GetRetentionDays())
	}
	if enc := spec.GetControlPlane().GetSecretsEncryption(); enc.GetKmsKeyArn() != "" {
		p.EncryptionKeyARN = enc.GetKmsKeyArn()
	} else if enc.GetEnabled() {
		p.CreateEncryptionKey = true
	}
	p.IRSA = aws.GetIrsa()
	for _, e := range spec.GetAccess().GetAccessEntries() {
		entry := crossplaneaws.AccessEntryPlan{Principal: e.GetPrincipalArn(), Type: CrossplaneName(e.GetType(), "STANDARD")}
		for _, policy := range e.GetPolicies() {
			entry.Policies = append(entry.Policies, crossplaneaws.AccessPolicyPlan{PolicyARN: policy.GetPolicyArn(),
				Scope: CrossplaneName(policy.GetScope(), "cluster"), Namespaces: policy.GetNamespaces()})
		}
		p.AccessEntries = append(p.AccessEntries, entry)
	}
	if lt := aws.GetDefaultNodePool().GetLaunchTemplate(); lt != nil {
		p.NodePool.LaunchTemplate = &crossplaneaws.LaunchTemplatePlan{ID: lt.GetId(), Name: lt.GetName(), Version: lt.GetVersion(),
			SecurityGroupIDs: lt.GetSecurityGroupIds(), ImageID: lt.GetImageId(), UserData: lt.GetUserData()}
	}
}

// encryption identifies a spec's secrets encryption: "" (off), "platform"
// (a key the platform creates) or the existing key's ARN.
func Encryption(spec *cloudv1.ClusterSpec) string {
	enc := spec.GetControlPlane().GetSecretsEncryption()
	switch {
	case enc.GetKmsKeyArn() != "":
		return enc.GetKmsKeyArn()
	case enc.GetEnabled():
		return "platform"
	default:
		return ""
	}
}

// EncryptionKept reports whether an update leaves a cluster's secrets
// encryption as EKS needs it: it can be turned on, never off, and its key
// never changes.
func EncryptionKept(before, after string) bool { return before == "" || before == after }
