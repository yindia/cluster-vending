// Package aws builds the Crossplane objects of the AWS provider
// (provider-upjet-aws) with their Go types: the providers, their runtime
// config and a tenant's ProviderConfig. pkg/kube applies them.
package aws

import (
	"fmt"
	"time"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	pkgv1 "github.com/crossplane/crossplane/apis/v2/pkg/v1"
	pkgv1beta1 "github.com/crossplane/crossplane/apis/v2/pkg/v1beta1"
	awsv1beta1 "github.com/upbound/provider-aws/v2/apis/namespaced/v1beta1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
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
const RenderVersion = 4

// ProviderPackages are the provider-upjet-aws service providers the
// platform needs; the family provider comes as their dependency.
// provider-aws-sqs and provider-aws-cloudwatchevents are for Karpenter's
// interruption queue and the EventBridge rules feeding it (components).
var ProviderPackages = []string{"provider-aws-ec2", "provider-aws-eks", "provider-aws-iam", "provider-aws-cloudwatchlogs",
	"provider-aws-kms", "provider-aws-sqs", "provider-aws-cloudwatchevents"}

const (
	// RuntimeConfigName is the DeploymentRuntimeConfig the AWS providers
	// run with; RuntimeConfigEC2Name the one of provider-aws-ec2;
	// RuntimeConfigHelmName the one of provider-helm.
	RuntimeConfigName     = "cluster-vending-aws"
	RuntimeConfigEC2Name  = "cluster-vending-aws-ec2"
	RuntimeConfigHelmName = "cluster-vending-helm"
	providerEC2           = "provider-aws-ec2"

	// irsaRoleAnnotation is read by the EKS pod identity webhook.
	irsaRoleAnnotation = "eks.amazonaws.com/role-arn"
	// The open-source builds of provider-upjet-aws (same code as Upbound's).
	awsPackageRegistry = "xpkg.crossplane.io/crossplane-contrib/"
)

// credentialsSourceIRSA is the AWS-only credentials source; Crossplane core
// has no constant for it.
const credentialsSourceIRSA xpv2.CredentialsSource = "IRSA"

// runtimeContainer is the provider container in a package's Deployment.
const runtimeContainer = "package-runtime"

// ProviderRuntime is how the AWS providers' pods run.
type ProviderRuntime struct {
	// ManagementRoleARN is the role the providers authenticate as on EKS
	// (IRSA: an annotation on their service account). Empty: none (local
	// clusters, credentials from the ProviderConfig Secret).
	ManagementRoleARN string
	// Poll is how often a provider checks each resource for drift; EC2Poll
	// the same for provider-aws-ec2, whose network resources are many and
	// change rarely.
	Poll, EC2Poll time.Duration
	// MaxReconcileRate caps a provider's reconciles per second (and so its
	// AWS API calls).
	MaxReconcileRate int
	// Resource requests and limits of a provider's container (quantities;
	// empty: none).
	CPURequest, MemoryRequest, CPULimit, MemoryLimit string
	// Replicas of each provider's Deployment (0: Crossplane's default,
	// one). The providers run with leader election (--leader-election), so
	// only one replica reconciles; the others are hot standbys that take
	// over the lease when it dies, without waiting for a new pod.
	Replicas int32
}

// DefaultProviderRuntime is the providers' runtime for ~100 clusters of
// ~80 managed resources each on one management cluster. Poll 10m (the
// providers' own default) and 30m for EC2's many, rarely changing network
// resources; at most 10 reconciles a second per provider (the providers
// default to 100, which AWS API throttling punishes at this size). Two
// replicas with leader election: one active, one hot standby.
func DefaultProviderRuntime() ProviderRuntime {
	return ProviderRuntime{Poll: 10 * time.Minute, EC2Poll: 30 * time.Minute, MaxReconcileRate: 10,
		CPURequest: "1", MemoryRequest: "2Gi", CPULimit: "4", MemoryLimit: "4Gi", Replicas: 2}
}

// leaderElectionArg turns on leader election in provider-upjet-aws
// (v2.9.0) and provider-helm (v1.5.0): both read the same flag (default
// false). Without it two replicas would both reconcile every resource.
const leaderElectionArg = "--leader-election"

// RuntimeConfigs are the DeploymentRuntimeConfigs of the providers:
// RuntimeConfigName for the AWS providers, RuntimeConfigEC2Name (its own
// poll interval) for provider-aws-ec2, and RuntimeConfigHelmName for
// provider-helm (no AWS identity: it reaches the workload clusters with
// their kubeconfigs). Cluster-scoped and shared by every tenant.
func RuntimeConfigs(rt ProviderRuntime) ([]*pkgv1beta1.DeploymentRuntimeConfig, error) {
	resources, err := rt.resources()
	if err != nil {
		return nil, err
	}
	config := func(name string, poll time.Duration, irsa bool) *pkgv1beta1.DeploymentRuntimeConfig {
		spec := &appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{}, // required by the type; Crossplane sets it
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: runtimeContainer, Resources: resources,
				Args: []string{"--poll=" + poll.String(), fmt.Sprintf("--max-reconcile-rate=%d", rt.MaxReconcileRate), leaderElectionArg},
			}}}},
		}
		if rt.Replicas > 0 {
			spec.Replicas = ptr.To(rt.Replicas)
		}
		c := &pkgv1beta1.DeploymentRuntimeConfig{
			TypeMeta:   metav1.TypeMeta{APIVersion: pkgv1beta1.SchemeGroupVersion.String(), Kind: pkgv1beta1.DeploymentRuntimeConfigKind},
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: crossplane.Labels("")},
			Spec:       pkgv1beta1.DeploymentRuntimeConfigSpec{DeploymentTemplate: &pkgv1beta1.DeploymentTemplate{Spec: spec}},
		}
		if irsa && rt.ManagementRoleARN != "" {
			c.Spec.ServiceAccountTemplate = &pkgv1beta1.ServiceAccountTemplate{Metadata: &pkgv1beta1.ObjectMeta{
				Annotations: map[string]string{irsaRoleAnnotation: rt.ManagementRoleARN}}}
		}
		return c
	}
	return []*pkgv1beta1.DeploymentRuntimeConfig{config(RuntimeConfigName, rt.Poll, true), config(RuntimeConfigEC2Name, rt.EC2Poll, true),
		config(RuntimeConfigHelmName, rt.Poll, false)}, nil
}

// HelmProvider is provider-helm (crossplane.HelmProvider) running with
// RuntimeConfigHelmName.
func HelmProvider() *pkgv1.Provider {
	p := crossplane.HelmProvider()
	p.Spec.RuntimeConfigReference = &pkgv1.RuntimeConfigReference{Name: RuntimeConfigHelmName}
	return p
}

func (rt ProviderRuntime) resources() (corev1.ResourceRequirements, error) {
	out := corev1.ResourceRequirements{}
	for _, q := range []struct {
		list  *corev1.ResourceList
		name  corev1.ResourceName
		value string
	}{
		{&out.Requests, corev1.ResourceCPU, rt.CPURequest}, {&out.Requests, corev1.ResourceMemory, rt.MemoryRequest},
		{&out.Limits, corev1.ResourceCPU, rt.CPULimit}, {&out.Limits, corev1.ResourceMemory, rt.MemoryLimit},
	} {
		if q.value == "" {
			continue
		}
		quantity, err := resource.ParseQuantity(q.value)
		if err != nil {
			return out, fmt.Errorf("provider %s %q: %w", q.name, q.value, err)
		}
		if *q.list == nil {
			*q.list = corev1.ResourceList{}
		}
		(*q.list)[q.name] = quantity
	}
	return out, nil
}

// Providers are the Provider packages of ProviderPackages at
// ProviderVersion, each running with its runtime config (RuntimeConfigs).
// Cluster-scoped and shared by every tenant.
func Providers() []*pkgv1.Provider {
	out := make([]*pkgv1.Provider, 0, len(ProviderPackages))
	for _, name := range ProviderPackages {
		config := RuntimeConfigName
		if name == providerEC2 {
			config = RuntimeConfigEC2Name
		}
		out = append(out, &pkgv1.Provider{
			TypeMeta:   metav1.TypeMeta{APIVersion: pkgv1.SchemeGroupVersion.String(), Kind: pkgv1.ProviderKind},
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: crossplane.Labels("")},
			Spec: pkgv1.ProviderSpec{
				PackageSpec: pkgv1.PackageSpec{Package: awsPackageRegistry + name + ":" + ProviderVersion},
				PackageRuntimeSpec: pkgv1.PackageRuntimeSpec{
					RuntimeConfigReference: &pkgv1.RuntimeConfigReference{Name: config},
				},
			},
		})
	}
	return out
}

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
func ProviderConfig(namespace, name, tenantID, tenantRoleARN string, secret *CredentialsSecret) *awsv1beta1.ProviderConfig {
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
			AssumeRoleChain: assumeRoleChain(tenantRoleARN, secret),
		},
	}
}

// assumeRoleChain is the tenant role, unless the credentials Secret is
// used directly.
func assumeRoleChain(tenantRoleARN string, secret *CredentialsSecret) []awsv1beta1.AssumeRoleOptions {
	if secret != nil && secret.Direct {
		return nil
	}
	return []awsv1beta1.AssumeRoleOptions{{RoleARN: ptr.To(tenantRoleARN)}}
}

// ProviderConfigRef is the spec.providerConfigRef of a namespaced managed
// resource that uses the ProviderConfig name in its own namespace.
func ProviderConfigRef(name string) *xpv2.ProviderConfigReference {
	return &xpv2.ProviderConfigReference{Kind: awsv1beta1.ProviderConfigKind, Name: name}
}
