// Package aws builds the Crossplane objects of the AWS provider
// (provider-upjet-aws) with their Go types: the providers, their runtime
// config and a tenant's ProviderConfig. pkg/kube applies them.
package aws

import (
	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	awsv1beta1 "github.com/upbound/provider-aws/v2/apis/namespaced/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/yindia/cluster-vending/pkg/crossplane"
)

// ProviderVersion is the provider-upjet-aws release the platform uses.
// It must match the Go API module (github.com/upbound/provider-aws/v2) in
// go.mod.
const ProviderVersion = "v2.9.0"

// RenderVersion is the version of what the renderers in this package
// output. Bump it whenever a renderer's output changes for an unchanged
// plan (a new object, field or name): the controller then applies
// existing networks and clusters again, one by one, up to its target
// version (see reconcile.Deps.RenderTarget).
//
//	1  Usages order deletions; desiredSize in initProvider only
//	2  Usages of the EKS cluster are named "...-cluster-..." (they ended in
//	   "-by-", a name the API server refuses)
//	3  a provider-helm ProviderConfig per cluster (<prefix>-helm, through
//	   the ClusterAuth's kubeconfig), with a Usage keeping the ClusterAuth
//	   while it exists
//	4  coredns and the EBS CSI controller follow the default node pool:
//	   its labels as their nodeSelector, a toleration per taint (only for
//	   clusters whose default pool has labels or taints)
//	5  stateful objects (VPC, subnets, gateways, EIPs, route tables, the
//	   EKS cluster and node groups, keys, roles, policies, the OIDC
//	   provider, log groups) are managed without Delete while their owner
//	   exists (protect)
//	6  Karpenter node groups: the default node group is fixed (system label
//	   and taint, two nodes), the karpenter node role with its EC2_LINUX
//	   access entry and the interruption queue with its EventBridge rules
//	   are rendered, and the EBS CSI controller follows the platform pool
//	   (docs/architecture.md#node-layout)
//	8  one platform node group, "system": the default node group is sized by
//	   the admin (nodeGroups.system.size) and the EBS CSI controller runs on
//	   it like every other platform pod; there is no platform pool
//	7  the bucket of the cluster's scan results with its public access
//	   block, ownership controls, encryption, TLS-only policy and lifecycle
//	   (results_bucket.go), for clusters the platform created
//	9  metrics-server, kube-state-metrics, snapshot-controller, cert-manager
//	   (with its cainjector and webhook) and the controllers of the EFS, FSx
//	   and Mountpoint S3 CSI drivers follow the system node group like
//	   coredns (addon_placement.go). Add-ons are Build items, which the
//	   code-change fingerprint counts by name only: a change to their
//	   render needs this bump (addons_render_version_test.go)
//	10 the system node group has a static size (min = max = desired,
//	   desired applied in place) and no cluster-autoscaler tags: Karpenter
//	   is the only scaler
//	11 a platform component's Helm Release has wait: false and
//	   rollbackLimit 3: Helm applies and records the chart within seconds,
//	   and the platform judges the rollout (Helm deployed and the
//	   release's workloads available, within the install timeout); the
//	   wait is part of the Release digest (helmrel.Spec.Wait), so every
//	   Release is written once more
//	12 vpc-cni enforces NetworkPolicy on every cluster (imported ones
//	   too): enableNetworkPolicy "true" and nodeAgent.enabled true in its
//	   configurationValues, written over the tenant's (configuration in
//	   addons.yaml, addon_configuration.go). Sub-tenant isolation relies
//	   on it; a sub-tenant's access waits for it
const RenderVersion = 12

const (
	// irsaRoleAnnotation is read by the EKS pod identity webhook.
	irsaRoleAnnotation = "eks.amazonaws.com/role-arn"
	// The open-source builds of provider-upjet-aws (same code as Upbound's).
	awsPackageRegistry = "xpkg.crossplane.io/crossplane-contrib/"
)

// credentialsSourceIRSA is the AWS-only credentials source; Crossplane core
// has no constant for it.
const credentialsSourceIRSA xpv2.CredentialsSource = "IRSA"

// CredentialsSecret names the Secret key that holds AWS keys in the
// shared credentials file format ([default] aws_access_key_id = ...): the
// providers' own credentials where there is no IRSA (a local kind
// cluster). The providers read it themselves; the platform never does.
type CredentialsSecret struct {
	Namespace, Name, Key string
	// Direct: the providers act with these keys as they are, without
	// assuming the tenant role (local testing, for an account whose
	// policies forbid assuming roles).
	Direct bool
}

// ProviderConfig is the ProviderConfig of a tenant's AWS account: the
// provider authenticates with its own credentials, then assumes
// tenantRoleARN in the tenant's account. Its own credentials are IRSA as
// the management role (RuntimeConfig), or, with secret set, the keys in
// that Secret (local testing); the tenant role is assumed either way, so a
// local run exercises the permissions production has. No keys are stored
// in the ProviderConfig.
//
// name is the account's stored ProviderConfig name
// (model.CloudAccount.ProviderConfigName): a tenant has one ProviderConfig
// per account, and the managed resources in that account reference it.
func ProviderConfig(namespace, name, tenantID, tenantRoleARN, externalID string, secret *CredentialsSecret) *awsv1beta1.ProviderConfig {
	credentials := awsv1beta1.ProviderCredentials{Source: credentialsSourceIRSA}
	if secret != nil {
		credentials = awsv1beta1.ProviderCredentials{Source: xpv2.CredentialsSourceSecret,
			CommonCredentialSelectors: xpv2.CommonCredentialSelectors{SecretRef: &xpv2.SecretKeySelector{
				SecretReference: xpv2.SecretReference{Namespace: secret.Namespace, Name: secret.Name}, Key: secret.Key}}}
	}
	return &awsv1beta1.ProviderConfig{
		TypeMeta: metav1.TypeMeta{APIVersion: awsv1beta1.SchemeGroupVersion.String(), Kind: awsv1beta1.ProviderConfigKind},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    crossplane.Labels(tenantID),
		},
		Spec: awsv1beta1.ProviderConfigSpec{
			Credentials:     credentials,
			AssumeRoleChain: assumeRoleChain(tenantRoleARN, externalID, secret),
		},
	}
}

// assumeRoleChain is the tenant role, passing the account's external ID
// when it has one (sts:ExternalId, which the role's trust requires once
// its stack is created with it), unless the credentials Secret is used
// directly.
func assumeRoleChain(tenantRoleARN, externalID string, secret *CredentialsSecret) []awsv1beta1.AssumeRoleOptions {
	if secret != nil && secret.Direct {
		return nil
	}
	role := awsv1beta1.AssumeRoleOptions{RoleARN: ptr.To(tenantRoleARN)}
	if externalID != "" {
		role.ExternalID = ptr.To(externalID)
	}
	return []awsv1beta1.AssumeRoleOptions{role}
}

// ProviderConfigRef is the spec.providerConfigRef of a namespaced managed
// resource that uses the ProviderConfig name in its own namespace.
func ProviderConfigRef(name string) *xpv2.ProviderConfigReference {
	return &xpv2.ProviderConfigReference{Kind: awsv1beta1.ProviderConfigKind, Name: name}
}
