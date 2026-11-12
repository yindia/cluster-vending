// Package kube connects to the Crossplane management cluster, where the
// server creates the managed resources of every tenant.
//
// The client is a controller-runtime client with typed APIs: the Kubernetes
// built-ins, Crossplane's package types (Provider, DeploymentRuntimeConfig)
// and the provider-upjet-aws namespaced types (ProviderConfig and the ec2,
// eks, iam, cloudwatchlogs and kms managed resources). pkg/crossplane builds those objects.
// The platform's own kinds (api/v1alpha1) are registered
// too: they are the store when no database is used.
// Adding a provider (GCP, Azure) means adding its Go API module and
// registering its groups in Scheme.
package kube

import (
	"fmt"

	apiextv1alpha1 "github.com/crossplane/crossplane/apis/v2/apiextensions/v1alpha1"
	pkgv1 "github.com/crossplane/crossplane/apis/v2/pkg/v1"
	pkgv1beta1 "github.com/crossplane/crossplane/apis/v2/pkg/v1beta1"
	protectionv1beta1 "github.com/crossplane/crossplane/apis/v2/protection/v1beta1"
	awscwl "github.com/upbound/provider-aws/v2/apis/namespaced/cloudwatchlogs/v1beta1"
	awsec2 "github.com/upbound/provider-aws/v2/apis/namespaced/ec2/v1beta1"
	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	awsiam "github.com/upbound/provider-aws/v2/apis/namespaced/iam/v1beta1"
	awskms "github.com/upbound/provider-aws/v2/apis/namespaced/kms/v1beta1"
	awsv1beta1 "github.com/upbound/provider-aws/v2/apis/namespaced/v1beta1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/version"
)

// Client-side rate limits. The defaults of client-go (5 QPS, burst 10) are
// too low for creating a cluster's ~30 managed resources at once.
const (
	DefaultQPS   = 50
	DefaultBurst = 100
)

// TenantNamespacePrefix prefixes every tenant namespace: one namespace per
// tenant holds all its managed resources and its ProviderConfigs.
const TenantNamespacePrefix = "tenant-"

// TenantNamespace returns the namespace of a tenant's resources.
func TenantNamespace(tenantID string) string { return TenantNamespacePrefix + tenantID }

// RESTConfig loads the management cluster's connection settings.
//
// kubeconfig names a file; empty uses $KUBECONFIG, then ~/.kube/config,
// and falls back to the in-cluster service account when neither exists.
// kubeContext selects a context; empty uses the current one.
func RESTConfig(kubeconfig, kubeContext string) (*rest.Config, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{CurrentContext: kubeContext}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("kubernetes config: %w", err)
	}
	cfg = rest.CopyConfig(cfg)
	cfg.QPS = DefaultQPS
	cfg.Burst = DefaultBurst
	cfg.UserAgent = "cluster-vending/" + version.Version
	return cfg, nil
}

// Scheme returns the types the client knows. Only the API groups the
// platform uses are registered: provider-aws alone has ~180.
func Scheme() (*runtime.Scheme, error) {
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		clientgoscheme.AddToScheme,
		pkgv1.AddToScheme,
		pkgv1beta1.AddToScheme,
		apiextv1alpha1.AddToScheme,
		protectionv1beta1.AddToScheme,
		awsv1beta1.SchemeBuilder.AddToScheme,
		awsec2.AddToScheme,
		awscwl.AddToScheme,
		awskms.AddToScheme,
		awseks.AddToScheme,
		awsiam.AddToScheme,
		cloudv1alpha1.AddToScheme,
	} {
		if err := add(s); err != nil {
			return nil, fmt.Errorf("kubernetes scheme: %w", err)
		}
	}
	return s, nil
}

// New returns a client for cfg. It does not contact the cluster: API
// discovery happens on first use of each kind.
func New(cfg *rest.Config) (client.Client, error) {
	s, err := Scheme()
	if err != nil {
		return nil, err
	}
	c, err := client.New(cfg, client.Options{Scheme: s})
	if err != nil {
		return nil, fmt.Errorf("kubernetes client: %w", err)
	}
	return c, nil
}

// Connect is RESTConfig followed by New.
func Connect(kubeconfig, kubeContext string) (client.Client, error) {
	cfg, err := RESTConfig(kubeconfig, kubeContext)
	if err != nil {
		return nil, err
	}
	return New(cfg)
}
