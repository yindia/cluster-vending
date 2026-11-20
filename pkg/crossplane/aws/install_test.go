package aws_test

import (
	"context"
	"slices"
	"testing"

	apiextv1alpha1 "github.com/crossplane/crossplane/apis/v2/apiextensions/v1alpha1"
	pkgv1 "github.com/crossplane/crossplane/apis/v2/pkg/v1"
	pkgv1beta1 "github.com/crossplane/crossplane/apis/v2/pkg/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/yindia/cluster-vending/pkg/crossplane"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/kube"
)

func fakeKube(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func TestInstallProvidersCreatesMissingAndKeepsExisting(t *testing.T) {
	ctx := context.Background()
	withRole := crossplaneaws.DefaultProviderRuntime()
	withRole.ManagementRoleARN = "arn:aws:iam::111122223333:role/platform"
	// An operator pinned the EKS provider to another version.
	pinned := &pkgv1.Provider{ObjectMeta: metav1.ObjectMeta{Name: "provider-aws-eks"},
		Spec: pkgv1.ProviderSpec{PackageSpec: pkgv1.PackageSpec{Package: "xpkg.upbound.io/upbound/provider-aws-eks:v2.8.0"}}}
	k := fakeKube(t, pinned)

	got, err := crossplaneaws.InstallProviders(ctx, k, withRole)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Created) != 12 || len(got.Existing) != 1 {
		t.Fatalf("created %v, existing %v; want the activation policy, 3 runtime configs, ec2, iam, cloudwatchlogs, kms, sqs, cloudwatchevents, s3 and helm created, eks existing", got.Created, got.Existing)
	}
	helm := &pkgv1.Provider{}
	if err := k.Get(ctx, client.ObjectKey{Name: crossplane.HelmProviderName}, helm); err != nil ||
		helm.Spec.Package != "xpkg.crossplane.io/crossplane-contrib/provider-helm:"+crossplane.HelmProviderVersion ||
		helm.Spec.RuntimeConfigReference == nil || helm.Spec.RuntimeConfigReference.Name != crossplaneaws.RuntimeConfigHelmName {
		t.Errorf("provider-helm = %+v, %v; want its own runtime config (no AWS identity)", helm.Spec, err)
	}
	helmConfig := &pkgv1beta1.DeploymentRuntimeConfig{}
	if err := k.Get(ctx, client.ObjectKey{Name: crossplaneaws.RuntimeConfigHelmName}, helmConfig); err != nil || helmConfig.Spec.ServiceAccountTemplate != nil {
		t.Errorf("provider-helm runtime config: %v, service account template %+v; want one without IRSA", err, helmConfig.Spec.ServiceAccountTemplate)
	}
	eks := &pkgv1.Provider{}
	if err := k.Get(ctx, client.ObjectKey{Name: "provider-aws-eks"}, eks); err != nil || eks.Spec.Package != pinned.Spec.Package {
		t.Errorf("existing provider changed: %v %v", eks.Spec.Package, err)
	}
	ec2 := &pkgv1.Provider{}
	if err := k.Get(ctx, client.ObjectKey{Name: "provider-aws-ec2"}, ec2); err != nil ||
		ec2.Spec.Package != "xpkg.crossplane.io/crossplane-contrib/provider-aws-ec2:"+crossplaneaws.ProviderVersion ||
		ec2.Spec.RuntimeConfigReference.Name != crossplaneaws.RuntimeConfigEC2Name {
		t.Errorf("provider-aws-ec2 = %+v, %v", ec2.Spec, err)
	}

	// Second start: nothing to create.
	again, err := crossplaneaws.InstallProviders(ctx, k, withRole)
	if err != nil || len(again.Created) != 0 || len(again.Existing) != 13 || len(again.Updated) != 0 {
		t.Errorf("second run: %+v, %v", again, err)
	}
}

// A management cluster set up by an older server has an activation policy
// without the kinds this build added: they are added, nothing is removed.
func TestInstallProvidersAddsMissingActivations(t *testing.T) {
	ctx := context.Background()
	old := crossplaneaws.ActivationPolicy()
	old.Spec.Activations = []apiextv1alpha1.ActivationPolicy{"vpcs.ec2.aws.m.upbound.io", "vaults.glacier.aws.m.upbound.io"}
	k := fakeKube(t, old)
	got, err := crossplaneaws.InstallProviders(ctx, k, crossplaneaws.DefaultProviderRuntime())
	if err != nil || len(got.Updated) != 1 {
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

func TestInstallProvidersWithoutManagementRole(t *testing.T) {
	ctx := context.Background()
	k := fakeKube(t)
	if _, err := crossplaneaws.InstallProviders(ctx, k, crossplaneaws.DefaultProviderRuntime()); err != nil {
		t.Fatal(err)
	}
	// Without a role the runtime config still tunes the provider; it only
	// carries no IRSA annotation.
	drc := &pkgv1beta1.DeploymentRuntimeConfig{}
	if err := k.Get(ctx, client.ObjectKey{Name: crossplaneaws.RuntimeConfigName}, drc); err != nil || drc.Spec.ServiceAccountTemplate != nil {
		t.Errorf("runtime config: %v, service account template %+v; want one without IRSA", err, drc.Spec.ServiceAccountTemplate)
	}
	iam := &pkgv1.Provider{}
	if err := k.Get(ctx, client.ObjectKey{Name: "provider-aws-iam"}, iam); err != nil ||
		iam.Spec.RuntimeConfigReference == nil || iam.Spec.RuntimeConfigReference.Name != crossplaneaws.RuntimeConfigName {
		t.Errorf("provider-aws-iam = %+v, %v; want the platform's runtime config", iam.Spec, err)
	}
}
