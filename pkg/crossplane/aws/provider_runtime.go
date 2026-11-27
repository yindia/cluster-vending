package aws

import (
	"fmt"
	"slices"
	"strings"
	"time"

	pkgv1 "github.com/crossplane/crossplane/apis/v2/pkg/v1"
	pkgv1beta1 "github.com/crossplane/crossplane/apis/v2/pkg/v1beta1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/yindia/cluster-vending/pkg/crossplane"
)

// ProviderPackages are the provider-upjet-aws service providers the
// platform needs; the family provider comes as their dependency.
// provider-aws-sqs and provider-aws-cloudwatchevents are for Karpenter's
// interruption queue and the EventBridge rules feeding it (karpenter.go).
// provider-aws-s3 is for the bucket of a cluster's scan results
// (results_bucket.go, docs/architecture.md#results-in-s3).
var ProviderPackages = []string{"provider-aws-ec2", "provider-aws-eks", "provider-aws-iam", "provider-aws-cloudwatchlogs",
	"provider-aws-kms", "provider-aws-sqs", "provider-aws-cloudwatchevents", "provider-aws-s3"}

const (
	providerEC2 = "provider-aws-ec2"
	providerEKS = "provider-aws-eks"

	// RuntimeConfigEC2Name is the DeploymentRuntimeConfig of
	// provider-aws-ec2 and RuntimeConfigHelmName the one of provider-helm
	// (RuntimeConfigNameOf).
	RuntimeConfigEC2Name  = "cluster-vending-aws-ec2"
	RuntimeConfigHelmName = "cluster-vending-helm"
	// LegacyRuntimeConfigName is the runtime config earlier builds shared
	// between every AWS provider but EC2. InstallProviders moves the
	// providers off it and deletes it when nothing uses it any more.
	LegacyRuntimeConfigName = "cluster-vending-aws"
	runtimeConfigPrefix     = "cluster-vending-"
)

// RuntimeProviders are the providers the platform installs and tunes:
// ProviderPackages and provider-helm. The keys of ProviderRuntime.Providers.
func RuntimeProviders() []string {
	return append(slices.Clone(ProviderPackages), crossplane.HelmProviderName)
}

// RuntimeConfigNameOf is the DeploymentRuntimeConfig of a provider: one per
// provider, so each can be tuned on its own ("cluster-vending-aws-eks" for
// provider-aws-eks, "cluster-vending-helm" for provider-helm).
func RuntimeConfigNameOf(provider string) string {
	return runtimeConfigPrefix + strings.TrimPrefix(provider, "provider-")
}

// runtimeContainer is the provider container in a package's Deployment.
const runtimeContainer = "package-runtime"

// ProviderTuning is how one provider's pods run. In an override
// (ProviderRuntime.Providers) a zero field takes the default's.
type ProviderTuning struct {
	// Replicas of the provider's Deployment. The providers run with leader
	// election (--leader-election), so only one replica reconciles; more
	// are hot standbys that take over the lease when it dies.
	Replicas int32
	// Poll is how often the provider checks each resource for drift.
	Poll time.Duration
	// MaxReconcileRate caps the provider's reconciles per second (and so
	// its AWS API calls).
	MaxReconcileRate int
	// Resource requests and limits of the provider's container
	// (quantities; empty: none).
	CPURequest, MemoryRequest, CPULimit, MemoryLimit string
}

// over is t with every non-zero field of o written over it.
func (t ProviderTuning) over(o ProviderTuning) ProviderTuning {
	if o.Replicas > 0 {
		t.Replicas = o.Replicas
	}
	if o.Poll > 0 {
		t.Poll = o.Poll
	}
	if o.MaxReconcileRate > 0 {
		t.MaxReconcileRate = o.MaxReconcileRate
	}
	for _, f := range []struct {
		dst *string
		src string
	}{
		{&t.CPURequest, o.CPURequest}, {&t.MemoryRequest, o.MemoryRequest}, {&t.CPULimit, o.CPULimit}, {&t.MemoryLimit, o.MemoryLimit},
	} {
		if f.src != "" {
			*f.dst = f.src
		}
	}
	return t
}

// ProviderRuntime is how the providers' pods run.
type ProviderRuntime struct {
	// ManagementRoleARN is the role the AWS providers authenticate as on
	// EKS (IRSA: an annotation on their service account). Empty: none
	// (local clusters, credentials from the ProviderConfig Secret).
	ManagementRoleARN string
	// Default is every provider's tuning; Providers, by provider name
	// (RuntimeProviders), what one provider runs with instead, field by
	// field over Default.
	Default   ProviderTuning
	Providers map[string]ProviderTuning
}

// For is the tuning provider runs with: Default with its override over it.
func (rt ProviderRuntime) For(provider string) ProviderTuning {
	return rt.Default.over(rt.Providers[provider])
}

// Over is rt with the tuning o configures written over it: o's Default
// over rt's for every provider, o's overrides over rt's provider by
// provider (the platform config's providers section over the platform's
// defaults). rt is not changed.
func (rt ProviderRuntime) Over(o ProviderRuntime) ProviderRuntime {
	out := ProviderRuntime{ManagementRoleARN: rt.ManagementRoleARN, Default: rt.Default.over(o.Default), Providers: map[string]ProviderTuning{}}
	if o.ManagementRoleARN != "" {
		out.ManagementRoleARN = o.ManagementRoleARN
	}
	for name, t := range rt.Providers {
		out.Providers[name] = t.over(o.Default)
	}
	for name, t := range o.Providers {
		out.Providers[name] = out.Providers[name].over(t)
	}
	return out
}

const (
	// DefaultEC2Poll is how often provider-aws-ec2 checks each resource
	// unasked (DefaultProviderRuntime). Whatever waits for that provider
	// to have observed a change without a signal from it (the network's
	// observation bound) is derived from it.
	DefaultEC2Poll = 30 * time.Minute
	// DefaultHelmPoll is how often provider-helm looks at each Release
	// unasked. A spec change (a new digest, a retry's annotation, a pause
	// lifted) is reconciled at once through provider-helm's watch; the
	// poll is what brings a Release's status (a failed install, drift in
	// the workload cluster) to the platform, which watches Releases, so a
	// status shows within about this long. Each poll is a remote Helm
	// status call: at 30s, ~13 Releases on 100 clusters make ~43 a second.
	DefaultHelmPoll = 30 * time.Second
	// defaultHelmMaxReconcileRate caps provider-helm's reconciles a second.
	// 100: the poll takes ~43 a second on 100 clusters, which leaves room
	// for bursts (a fleet-wide release move rewrites ~1300 Releases, done in
	// about 25s). With wait: false a reconcile takes seconds, not minutes.
	defaultHelmMaxReconcileRate = 100
)

// DefaultProviderRuntime is the providers' runtime for ~100 clusters of
// ~80 managed resources each on one management cluster.
//
// Poll 10m (the providers' own default), 30m for EC2's many, rarely
// changing network resources, and DefaultHelmPoll for provider-helm. At
// most 10 reconciles a second per AWS provider (the providers default to
// 100, which AWS API throttling punishes at this size) and 50 for
// provider-helm, which reads only the workload clusters.
//
// One replica each: a provider that restarts loses no state (every
// resource is re-observed) and nothing waits on it within seconds, so a
// hot standby doubled the requests for little. Leader election stays on,
// so replicas can be raised for HA in the platform config.
//
// Requests are sized by what each provider holds: EC2 (VPCs, subnets,
// routes, security groups, launch templates) and EKS (clusters, node
// groups, add-ons, access entries) have the most resources and the
// largest schemas; IAM many small ones; KMS, SQS, EventBridge, CloudWatch
// Logs and S3 a few per cluster. Limits leave room for a resync after a
// restart. Before, every provider asked for 1 CPU and 2Gi, twice: 18 CPUs
// and 36Gi requested for providers that mostly idle.
func DefaultProviderRuntime() ProviderRuntime {
	small := ProviderTuning{CPURequest: "50m", MemoryRequest: "256Mi", CPULimit: "500m", MemoryLimit: "768Mi"}
	large := ProviderTuning{CPURequest: "500m", MemoryRequest: "1Gi", CPULimit: "2", MemoryLimit: "2Gi"}
	ec2 := large
	ec2.Poll = DefaultEC2Poll
	return ProviderRuntime{
		Default: ProviderTuning{Replicas: 1, Poll: 10 * time.Minute, MaxReconcileRate: 10,
			CPURequest: "200m", MemoryRequest: "512Mi", CPULimit: "1", MemoryLimit: "1Gi"},
		Providers: map[string]ProviderTuning{
			providerEC2: ec2, providerEKS: large,
			"provider-aws-kms": small, "provider-aws-sqs": small, "provider-aws-cloudwatchevents": small,
			"provider-aws-cloudwatchlogs": small, "provider-aws-s3": small,
			crossplane.HelmProviderName: {Poll: DefaultHelmPoll, MaxReconcileRate: defaultHelmMaxReconcileRate,
				CPURequest: "100m", MemoryRequest: "256Mi", CPULimit: "1", MemoryLimit: "1Gi"},
		},
	}
}

// leaderElectionArg turns on leader election in provider-upjet-aws
// (v2.9.0) and provider-helm (v1.5.0): both read the same flag (default
// false). Without it two replicas would both reconcile every resource.
const leaderElectionArg = "--leader-election"

// RuntimeConfigs are the DeploymentRuntimeConfigs of the providers, one
// per provider of RuntimeProviders, in that order (RuntimeConfigNameOf).
// The AWS providers' carry the management role (IRSA); provider-helm's
// none: it reaches the workload clusters with their kubeconfigs.
// Cluster-scoped and shared by every tenant.
func RuntimeConfigs(rt ProviderRuntime) ([]*pkgv1beta1.DeploymentRuntimeConfig, error) {
	for name := range rt.Providers {
		if !slices.Contains(RuntimeProviders(), name) {
			return nil, fmt.Errorf("provider %q is not one the platform runs (%s)", name, strings.Join(RuntimeProviders(), ", "))
		}
	}
	out := make([]*pkgv1beta1.DeploymentRuntimeConfig, 0, len(RuntimeProviders()))
	for _, name := range RuntimeProviders() {
		c, err := runtimeConfig(name, rt.For(name), name != crossplane.HelmProviderName, rt.ManagementRoleARN)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

func runtimeConfig(provider string, t ProviderTuning, aws bool, roleARN string) (*pkgv1beta1.DeploymentRuntimeConfig, error) {
	if t.Poll <= 0 || t.MaxReconcileRate <= 0 || t.Replicas < 0 {
		return nil, fmt.Errorf("provider %s: poll and maxReconcileRate must be positive, replicas not negative", provider)
	}
	if provider == providerEC2 && t.Poll > DefaultEC2Poll {
		return nil, fmt.Errorf("provider %s: poll %s is longer than %s: the network's observation bound (reconcile/network) is derived from %s, so a longer poll could count a change as observed before the provider saw it",
			provider, t.Poll, DefaultEC2Poll, DefaultEC2Poll)
	}
	resources, err := t.resources()
	if err != nil {
		return nil, fmt.Errorf("provider %s: %w", provider, err)
	}
	spec := &appsv1.DeploymentSpec{
		Selector: &metav1.LabelSelector{}, // required by the type; Crossplane sets it
		Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: runtimeContainer, Resources: resources,
			Args: []string{"--poll=" + t.Poll.String(), fmt.Sprintf("--max-reconcile-rate=%d", t.MaxReconcileRate), leaderElectionArg},
		}}}},
	}
	if t.Replicas > 0 {
		spec.Replicas = ptr.To(t.Replicas)
	}
	c := &pkgv1beta1.DeploymentRuntimeConfig{
		TypeMeta:   metav1.TypeMeta{APIVersion: pkgv1beta1.SchemeGroupVersion.String(), Kind: pkgv1beta1.DeploymentRuntimeConfigKind},
		ObjectMeta: metav1.ObjectMeta{Name: RuntimeConfigNameOf(provider), Labels: crossplane.Labels("")},
		Spec:       pkgv1beta1.DeploymentRuntimeConfigSpec{DeploymentTemplate: &pkgv1beta1.DeploymentTemplate{Spec: spec}},
	}
	if aws && roleARN != "" {
		c.Spec.ServiceAccountTemplate = &pkgv1beta1.ServiceAccountTemplate{Metadata: &pkgv1beta1.ObjectMeta{
			Annotations: map[string]string{irsaRoleAnnotation: roleARN}}}
	}
	return c, nil
}

func (t ProviderTuning) resources() (corev1.ResourceRequirements, error) {
	out := corev1.ResourceRequirements{}
	for _, q := range []struct {
		list  *corev1.ResourceList
		name  corev1.ResourceName
		value string
	}{
		{&out.Requests, corev1.ResourceCPU, t.CPURequest}, {&out.Requests, corev1.ResourceMemory, t.MemoryRequest},
		{&out.Limits, corev1.ResourceCPU, t.CPULimit}, {&out.Limits, corev1.ResourceMemory, t.MemoryLimit},
	} {
		if q.value == "" {
			continue
		}
		quantity, err := resource.ParseQuantity(q.value)
		if err != nil {
			return out, fmt.Errorf("%s %q: %w", q.name, q.value, err)
		}
		if *q.list == nil {
			*q.list = corev1.ResourceList{}
		}
		(*q.list)[q.name] = quantity
	}
	return out, nil
}

// HelmProvider is provider-helm (crossplane.HelmProvider) running with
// RuntimeConfigHelmName.
func HelmProvider() *pkgv1.Provider {
	p := crossplane.HelmProvider()
	p.Spec.RuntimeConfigReference = &pkgv1.RuntimeConfigReference{Name: RuntimeConfigHelmName}
	return p
}

// Providers are the Provider packages of ProviderPackages at
// ProviderVersion, each running with its own runtime config
// (RuntimeConfigNameOf). Cluster-scoped and shared by every tenant.
func Providers() []*pkgv1.Provider {
	out := make([]*pkgv1.Provider, 0, len(ProviderPackages))
	for _, name := range ProviderPackages {
		out = append(out, &pkgv1.Provider{
			TypeMeta:   metav1.TypeMeta{APIVersion: pkgv1.SchemeGroupVersion.String(), Kind: pkgv1.ProviderKind},
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: crossplane.Labels("")},
			Spec: pkgv1.ProviderSpec{
				PackageSpec: pkgv1.PackageSpec{Package: awsPackageRegistry + name + ":" + ProviderVersion},
				PackageRuntimeSpec: pkgv1.PackageRuntimeSpec{
					RuntimeConfigReference: &pkgv1.RuntimeConfigReference{Name: RuntimeConfigNameOf(name)},
				},
			},
		})
	}
	return out
}
