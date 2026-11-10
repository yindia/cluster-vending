// Package aws builds the Crossplane objects of the AWS provider
// (provider-upjet-aws) with their Go types: the providers, their runtime
// config and a tenant's ProviderConfig. pkg/kube applies them.
package aws

import (
	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	pkgv1 "github.com/crossplane/crossplane/apis/v2/pkg/v1"
	pkgv1beta1 "github.com/crossplane/crossplane/apis/v2/pkg/v1beta1"
	awsv1beta1 "github.com/upbound/provider-aws/v2/apis/namespaced/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/yindia/cluster-vending/pkg/crossplane"
)

// ProviderVersion is the provider-upjet-aws release the platform uses.
// It must match the Go API module (github.com/upbound/provider-aws/v2) in
// go.mod.
const ProviderVersion = "v2.9.0"

// ProviderPackages are the provider-upjet-aws service providers the
// platform needs; the family provider comes as their dependency.
var ProviderPackages = []string{"provider-aws-ec2", "provider-aws-eks", "provider-aws-iam"}

const (
	// RuntimeConfigName is the DeploymentRuntimeConfig every AWS provider
	// runs with.
	RuntimeConfigName = "aws-irsa"

	// irsaRoleAnnotation is read by the EKS pod identity webhook.
	irsaRoleAnnotation = "eks.amazonaws.com/role-arn"
	awsPackageRegistry = "xpkg.upbound.io/upbound/"
)

// credentialsSourceIRSA is the AWS-only credentials source; Crossplane core
// has no constant for it.
const credentialsSourceIRSA xpv2.CredentialsSource = "IRSA"

// RuntimeConfig is the DeploymentRuntimeConfig of the AWS providers: their
// service account carries managementRoleARN, so on EKS the providers
// authenticate as it (IRSA). Cluster-scoped and shared by every tenant.
func RuntimeConfig(managementRoleARN string) *pkgv1beta1.DeploymentRuntimeConfig {
	return &pkgv1beta1.DeploymentRuntimeConfig{
		TypeMeta:   metav1.TypeMeta{APIVersion: pkgv1beta1.SchemeGroupVersion.String(), Kind: pkgv1beta1.DeploymentRuntimeConfigKind},
		ObjectMeta: metav1.ObjectMeta{Name: RuntimeConfigName, Labels: crossplane.Labels("")},
		Spec: pkgv1beta1.DeploymentRuntimeConfigSpec{
			ServiceAccountTemplate: &pkgv1beta1.ServiceAccountTemplate{
				Metadata: &pkgv1beta1.ObjectMeta{
					Annotations: map[string]string{irsaRoleAnnotation: managementRoleARN},
				},
			},
		},
	}
}

// Providers are the Provider packages of ProviderPackages at
// ProviderVersion, running with RuntimeConfig. Cluster-scoped and
// shared by every tenant.
func Providers() []*pkgv1.Provider {
	out := make([]*pkgv1.Provider, 0, len(ProviderPackages))
	for _, name := range ProviderPackages {
		out = append(out, &pkgv1.Provider{
			TypeMeta:   metav1.TypeMeta{APIVersion: pkgv1.SchemeGroupVersion.String(), Kind: pkgv1.ProviderKind},
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: crossplane.Labels("")},
			Spec: pkgv1.ProviderSpec{
				PackageSpec: pkgv1.PackageSpec{Package: awsPackageRegistry + name + ":" + ProviderVersion},
				PackageRuntimeSpec: pkgv1.PackageRuntimeSpec{
					RuntimeConfigReference: &pkgv1.RuntimeConfigReference{Name: RuntimeConfigName},
				},
			},
		})
	}
	return out
}

// ProviderConfig is the ProviderConfig of a tenant's AWS account: the
// provider authenticates with IRSA as the management role (RuntimeConfig),
// then assumes tenantRoleARN in the tenant's account. No static credentials
// are stored.
//
// name is the account's stored ProviderConfig name
// (model.CloudAccount.ProviderConfigName): a tenant has one ProviderConfig
// per account, and the managed resources in that account reference it.
func ProviderConfig(namespace, name, tenantID, tenantRoleARN string) *awsv1beta1.ProviderConfig {
	return &awsv1beta1.ProviderConfig{
		TypeMeta: metav1.TypeMeta{APIVersion: awsv1beta1.SchemeGroupVersion.String(), Kind: awsv1beta1.ProviderConfigKind},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    crossplane.Labels(tenantID),
		},
		Spec: awsv1beta1.ProviderConfigSpec{
			Credentials:     awsv1beta1.ProviderCredentials{Source: credentialsSourceIRSA},
			AssumeRoleChain: []awsv1beta1.AssumeRoleOptions{{RoleARN: ptr.To(tenantRoleARN)}},
		},
	}
}

// ProviderConfigRef is the spec.providerConfigRef of a namespaced managed
// resource that uses the ProviderConfig name in its own namespace.
func ProviderConfigRef(name string) *xpv2.ProviderConfigReference {
	return &xpv2.ProviderConfigReference{Kind: awsv1beta1.ProviderConfigKind, Name: name}
}
