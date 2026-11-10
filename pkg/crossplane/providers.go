package crossplane

import (
	pkgv1 "github.com/crossplane/crossplane/apis/v2/pkg/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// HelmProviderVersion is the crossplane-contrib provider-helm release the
// platform installs: v1, with the namespaced helm.m.crossplane.io API
// (Release and ProviderConfig in tenant namespaces).
const HelmProviderVersion = "v1.5.0"

// HelmProviderName is the Provider object of provider-helm.
const HelmProviderName = "provider-helm"

// HelmProvider is the provider-helm package. It installs charts into
// tenant clusters through their kubeconfig Secrets (ClusterAuth) and needs
// no cloud identity, so it runs with Crossplane's default runtime config.
func HelmProvider() *pkgv1.Provider {
	return &pkgv1.Provider{
		TypeMeta:   metav1.TypeMeta{APIVersion: pkgv1.SchemeGroupVersion.String(), Kind: pkgv1.ProviderKind},
		ObjectMeta: metav1.ObjectMeta{Name: HelmProviderName, Labels: Labels("")},
		Spec: pkgv1.ProviderSpec{PackageSpec: pkgv1.PackageSpec{
			Package: "xpkg.crossplane.io/crossplane-contrib/provider-helm:" + HelmProviderVersion,
		}},
	}
}
