package aws_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	apiextv1alpha1 "github.com/crossplane/crossplane/apis/v2/apiextensions/v1alpha1"
	pkgv1 "github.com/crossplane/crossplane/apis/v2/pkg/v1"
	pkgv1beta1 "github.com/crossplane/crossplane/apis/v2/pkg/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/yindia/cluster-vending/pkg/crossplane"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/kube"
)

// fakeKube is a fake management cluster that tracks field managers
// (server-side apply) and returns them.
func fakeKube(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).WithReturnManagedFields().Build()
}

// legacy creates obj as an earlier build did: a plain Create, its fields
// owned by the platform's user agent (cluster-vending).
func legacy(t *testing.T, k client.Client, obj client.Object) {
	t.Helper()
	if err := k.Create(t.Context(), obj, client.FieldOwner(kube.FieldOwner)); err != nil {
		t.Fatal(err)
	}
}

func runtimeConfig(t *testing.T, k client.Client, name string) *pkgv1beta1.DeploymentRuntimeConfig {
	t.Helper()
	rc := &pkgv1beta1.DeploymentRuntimeConfig{}
	if err := k.Get(t.Context(), client.ObjectKey{Name: name}, rc); err != nil {
		t.Fatal(err)
	}
	return rc
}

func provider(t *testing.T, k client.Client, name string) *pkgv1.Provider {
	t.Helper()
	p := &pkgv1.Provider{}
	if err := k.Get(t.Context(), client.ObjectKey{Name: name}, p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestInstallProvidersCreatesEverything(t *testing.T) {
	rt := crossplaneaws.DefaultProviderRuntime()
	rt.ManagementRoleARN = mgmtRole
	k := fakeKube(t)

	got, err := crossplaneaws.InstallProviders(t.Context(), k, rt)

	if err != nil {
		t.Fatal(err)
	}
	// The policy, 9 runtime configs, 8 AWS providers and provider-helm.
	if len(got.Created) != 19 || len(got.Existing) != 0 {
		t.Fatalf("created %v, existing %v", got.Created, got.Existing)
	}
	helm := provider(t, k, crossplane.HelmProviderName)
	if helm.Spec.Package != "xpkg.crossplane.io/crossplane-contrib/provider-helm:"+crossplane.HelmProviderVersion ||
		helm.Spec.RuntimeConfigReference.Name != crossplaneaws.RuntimeConfigHelmName {
		t.Errorf("provider-helm = %+v", helm.Spec)
	}
	if runtimeConfig(t, k, crossplaneaws.RuntimeConfigHelmName).Spec.ServiceAccountTemplate != nil {
		t.Error("provider-helm's runtime config has an AWS identity")
	}

	// Second start: nothing changes.
	again, err := crossplaneaws.InstallProviders(t.Context(), k, rt)
	if err != nil || len(again.Created) != 0 || len(again.Existing) != 19 || len(again.Updated) != 0 || len(again.Kept) != 0 {
		t.Errorf("second run: %+v, %v", again, err)
	}
}

// An install of an earlier build (one shared runtime config, two replicas
// of 1 CPU and 2Gi, created with Create) is brought to this build's
// tuning on start: its runtime configs are applied as the platform's
// field owner, the providers move to their own and the shared one goes.
// A provider's pinned package stays.
func TestInstallProvidersUpdatesAnEarlierInstall(t *testing.T) {
	k := fakeKube(t)
	old := oldRuntimeConfig(crossplaneaws.LegacyRuntimeConfigName)
	legacy(t, k, old)
	legacy(t, k, oldRuntimeConfig(crossplaneaws.RuntimeConfigEC2Name))
	legacy(t, k, &pkgv1.Provider{ObjectMeta: metav1.ObjectMeta{Name: "provider-aws-eks", Labels: crossplane.Labels("")},
		Spec: pkgv1.ProviderSpec{PackageSpec: pkgv1.PackageSpec{Package: "xpkg.upbound.io/upbound/provider-aws-eks:v2.8.0"},
			PackageRuntimeSpec: pkgv1.PackageRuntimeSpec{RuntimeConfigReference: &pkgv1.RuntimeConfigReference{Name: crossplaneaws.LegacyRuntimeConfigName}}}})

	got, err := crossplaneaws.InstallProviders(t.Context(), k, crossplaneaws.DefaultProviderRuntime())

	if err != nil {
		t.Fatal(err)
	}
	ec2 := runtimeConfig(t, k, crossplaneaws.RuntimeConfigEC2Name)
	if r := ec2.Spec.DeploymentTemplate.Spec.Replicas; r == nil || *r != 1 {
		t.Errorf("ec2 replicas %v, want this build's 1", r)
	}
	if req := ec2.Spec.DeploymentTemplate.Spec.Template.Spec.Containers[0].Resources.Requests; req.Cpu().String() != "500m" {
		t.Errorf("ec2 requests %v, want this build's", req)
	}
	eks := provider(t, k, "provider-aws-eks")
	if eks.Spec.Package != "xpkg.upbound.io/upbound/provider-aws-eks:v2.8.0" || eks.Spec.RuntimeConfigReference.Name != "cluster-vending-aws-eks" {
		t.Errorf("provider-aws-eks = %+v: want the pinned package on its own runtime config", eks.Spec)
	}
	if err := k.Get(t.Context(), client.ObjectKeyFromObject(old), &pkgv1beta1.DeploymentRuntimeConfig{}); !apierrors.IsNotFound(err) {
		t.Errorf("the shared runtime config: %v, want it deleted", err)
	}
	if !slices.Contains(got.Updated, "DeploymentRuntimeConfig/"+crossplaneaws.RuntimeConfigEC2Name) ||
		!slices.Contains(got.Updated, "Provider/provider-aws-eks") || len(got.Deleted) != 1 {
		t.Errorf("result %+v", got)
	}
}

// What an operator changed under another field manager (kubectl edit) is
// kept: their replicas and a provider they pointed at their own runtime
// config. Everything else is this build's: a new CPU request and poll
// reach the existing config. (The fake client keeps lists atomic, so the
// operator edits a scalar here; TestOwnedFieldsOfAListMap covers fields
// in the provider container as the API server records them.)
func TestInstallProvidersKeepsAnOperatorsFields(t *testing.T) {
	k := fakeKube(t)
	if _, err := crossplaneaws.InstallProviders(t.Context(), k, crossplaneaws.DefaultProviderRuntime()); err != nil {
		t.Fatal(err)
	}
	tuned := runtimeConfig(t, k, crossplaneaws.RuntimeConfigEC2Name)
	tuned.Spec.DeploymentTemplate.Spec.Replicas = ptr.To[int32](3)
	if err := k.Update(t.Context(), tuned, client.FieldOwner("kubectl-edit")); err != nil {
		t.Fatal(err)
	}
	s3 := provider(t, k, "provider-aws-s3")
	s3.Spec.RuntimeConfigReference.Name = "my-own"
	if err := k.Update(t.Context(), s3, client.FieldOwner("kubectl-edit")); err != nil {
		t.Fatal(err)
	}

	// A new build (or the platform config) changes the EC2 provider's
	// CPU request and poll.
	rt := crossplaneaws.DefaultProviderRuntime().Over(crossplaneaws.ProviderRuntime{
		Providers: map[string]crossplaneaws.ProviderTuning{"provider-aws-ec2": {CPURequest: "300m", Poll: 20 * time.Minute}}})
	got, err := crossplaneaws.InstallProviders(t.Context(), k, rt)

	if err != nil {
		t.Fatal(err)
	}
	after := runtimeConfig(t, k, crossplaneaws.RuntimeConfigEC2Name).Spec.DeploymentTemplate.Spec
	if *after.Replicas != 3 {
		t.Errorf("replicas %d, want the operator's 3", *after.Replicas)
	}
	ctr := after.Template.Spec.Containers[0]
	if ctr.Resources.Requests.Cpu().String() != "300m" || ctr.Args[0] != "--poll=20m0s" {
		t.Errorf("CPU request %s, args %v: want the new 300m and 20m", ctr.Resources.Requests.Cpu(), ctr.Args)
	}
	if ref := provider(t, k, "provider-aws-s3").Spec.RuntimeConfigReference.Name; ref != "my-own" {
		t.Errorf("provider-aws-s3 runs with %s, want the operator's my-own", ref)
	}
	kept := strings.Join(got.Kept, "; ")
	for _, want := range []string{"DeploymentRuntimeConfig/" + crossplaneaws.RuntimeConfigEC2Name + " (replicas)", "Provider/provider-aws-s3 (runtimeConfigRef)"} {
		if !strings.Contains(kept, want) {
			t.Errorf("kept %q, want it to name %s", kept, want)
		}
	}
	if !slices.Contains(got.Updated, "DeploymentRuntimeConfig/"+crossplaneaws.RuntimeConfigEC2Name) {
		t.Errorf("updated %v", got.Updated)
	}
}

// A shared runtime config a Provider still uses (an operator's) stays.
func TestInstallProvidersKeepsASharedConfigInUse(t *testing.T) {
	k := fakeKube(t)
	legacy(t, k, oldRuntimeConfig(crossplaneaws.LegacyRuntimeConfigName))
	legacy(t, k, &pkgv1.Provider{ObjectMeta: metav1.ObjectMeta{Name: "provider-aws-glacier"},
		Spec: pkgv1.ProviderSpec{PackageSpec: pkgv1.PackageSpec{Package: "xpkg.upbound.io/upbound/provider-aws-glacier:v2.9.0"},
			PackageRuntimeSpec: pkgv1.PackageRuntimeSpec{RuntimeConfigReference: &pkgv1.RuntimeConfigReference{Name: crossplaneaws.LegacyRuntimeConfigName}}}})

	if _, err := crossplaneaws.InstallProviders(t.Context(), k, crossplaneaws.DefaultProviderRuntime()); err != nil {
		t.Fatal(err)
	}

	runtimeConfig(t, k, crossplaneaws.LegacyRuntimeConfigName)
}

// oldRuntimeConfig is a runtime config as earlier builds rendered it.
func oldRuntimeConfig(name string) *pkgv1beta1.DeploymentRuntimeConfig {
	rt := crossplaneaws.DefaultProviderRuntime().Over(crossplaneaws.ProviderRuntime{Default: crossplaneaws.ProviderTuning{
		Replicas: 2, CPURequest: "1", MemoryRequest: "2Gi", CPULimit: "4", MemoryLimit: "4Gi"}})
	configs, err := crossplaneaws.RuntimeConfigs(rt)
	if err != nil {
		panic(err)
	}
	rc := configs[0]
	rc.Name, rc.ResourceVersion = name, ""
	return rc
}

// A management cluster set up by an older server has an activation policy
// without the kinds this build added: they are added, nothing is removed.
func TestInstallProvidersAddsMissingActivations(t *testing.T) {
	ctx := context.Background()
	old := crossplaneaws.ActivationPolicy()
	old.Spec.Activations = []apiextv1alpha1.ActivationPolicy{"vpcs.ec2.aws.m.upbound.io", "vaults.glacier.aws.m.upbound.io"}
	k := fakeKube(t, old)
	got, err := crossplaneaws.InstallProviders(ctx, k, crossplaneaws.DefaultProviderRuntime())
	if err != nil || !slices.Contains(got.Updated, "ManagedResourceActivationPolicy/"+old.Name) {
		t.Fatalf("InstallProviders = %+v, %v; want the activation policy updated", got, err)
	}
	policy := &apiextv1alpha1.ManagedResourceActivationPolicy{}
	if err := k.Get(ctx, client.ObjectKey{Name: crossplaneaws.ActivationPolicyName}, policy); err != nil {
		t.Fatal(err)
	}
	for _, want := range []apiextv1alpha1.ActivationPolicy{"vaults.glacier.aws.m.upbound.io", "buckets.s3.aws.m.upbound.io", "flowlogs.ec2.aws.m.upbound.io",
		"groups.cloudwatchlogs.aws.m.upbound.io"} {
		if !slices.Contains(policy.Spec.Activations, want) {
			t.Errorf("activations lack %s: %v", want, policy.Spec.Activations)
		}
	}
	if len(policy.Spec.Activations) != len(crossplaneaws.ActivationPolicy().Spec.Activations)+1 {
		t.Errorf("activations = %v, want this build's plus the operator's one", policy.Spec.Activations)
	}
}

// Without a role the runtime configs still tune the providers; they only
// carry no IRSA annotation.
func TestInstallProvidersWithoutManagementRole(t *testing.T) {
	k := fakeKube(t)
	if _, err := crossplaneaws.InstallProviders(t.Context(), k, crossplaneaws.DefaultProviderRuntime()); err != nil {
		t.Fatal(err)
	}
	if rc := runtimeConfig(t, k, "cluster-vending-aws-iam"); rc.Spec.ServiceAccountTemplate != nil {
		t.Errorf("service account template %+v; want none", rc.Spec.ServiceAccountTemplate)
	}
}
