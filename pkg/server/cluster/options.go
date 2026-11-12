package cluster

import (
	"context"
	"errors"
	"fmt"

	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	awskms "github.com/upbound/provider-aws/v2/apis/namespaced/kms/v1beta1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// resolveOptions fills the defaults of the optional settings that are in
// use (the log group's retention, an access entry's type and scope, the
// AMI type of a custom AMI) and checks what the schema cannot. Settings
// that are not in use are left as they are: the stored spec of a cluster
// without them does not change.
func resolveOptions(spec *cloudv1.ClusterSpec) error {
	access := spec.GetControlPlane().GetEndpointAccess()
	if publicEndpoint(spec) && len(access.GetPublicAccessCidrs()) == 0 {
		return errNoPublicAccessCIDRs
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

// publicEndpoint reports whether the API server's public endpoint is on
// (the default).
func publicEndpoint(spec *cloudv1.ClusterSpec) bool {
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
		entry := crossplaneaws.AccessEntryPlan{Principal: e.GetPrincipalArn(), Type: crossplaneName(e.GetType(), "STANDARD")}
		for _, policy := range e.GetPolicies() {
			entry.Policies = append(entry.Policies, crossplaneaws.AccessPolicyPlan{PolicyARN: policy.GetPolicyArn(),
				Scope: crossplaneName(policy.GetScope(), "cluster"), Namespaces: policy.GetNamespaces()})
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
func encryption(spec *cloudv1.ClusterSpec) string {
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

// encryptionKept reports whether an update leaves a cluster's secrets
// encryption as EKS needs it: it can be turned on, never off, and its key
// never changes.
func encryptionKept(before, after string) bool { return before == "" || before == after }

// fillLive sets the plan's values that only exist once other resources do,
// from the cluster's managed resources: the ARN of the rendered KMS key,
// the EKS cluster's OIDC issuer and security group (once it is ACTIVE),
// and ClusterHeld while a new cluster's log group is not READY yet. What
// is still missing keeps the objects that need it out of the render
// (ClusterPlan.Pending).
func (s *ClusterService) fillLive(ctx context.Context, row *model.Cluster, plan *crossplaneaws.ClusterPlan) error {
	records, err := s.deps.Store.ManagedResources().ListByOwner(ctx, model.OwnerCluster, row.ID)
	if err != nil {
		return fmt.Errorf("list cluster resources: %w", err)
	}
	clusterRecorded, logGroupReady := false, false
	for _, r := range records {
		key := client.ObjectKey{Namespace: r.Namespace, Name: r.Name}
		switch r.Kind {
		case "Cluster":
			clusterRecorded = true
			eks := &awseks.Cluster{}
			if err := s.getIgnoreMissing(ctx, key, eks); err != nil {
				return fmt.Errorf("read EKS cluster %s: %w", r.Name, err)
			}
			if ptr.Deref(eks.Status.AtProvider.Status, "") != eksActiveStatus {
				continue
			}
			st, aws := &cloudv1.ClusterStatus{}, &cloudv1.AwsClusterStatus{}
			fromEKSCluster(eks.Status.AtProvider, st, aws)
			plan.OIDCIssuer, plan.ClusterSecurityGroupID = st.GetOidcIssuerUrl(), aws.GetClusterSecurityGroupId()
		case "Key":
			if !plan.CreateEncryptionKey {
				continue
			}
			k := &awskms.Key{}
			if err := s.getIgnoreMissing(ctx, key, k); err != nil {
				return fmt.Errorf("read KMS key %s: %w", r.Name, err)
			}
			plan.EncryptionKeyARN = ptr.Deref(k.Status.AtProvider.Arn, "")
		case "Group":
			views, err := s.deps.Observe(ctx, []model.ManagedResource{r})
			if err != nil {
				return err
			}
			logGroupReady = len(views) == 1 && views[0].GetState() == cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY
		}
	}
	// A new cluster waits for its log group, or EKS creates the group
	// itself. An existing cluster's group is simply adopted.
	plan.ClusterHeld = plan.LogGroupRetentionDays > 0 && !clusterRecorded && !logGroupReady
	return nil
}
