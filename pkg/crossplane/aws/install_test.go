package aws_test

import (
	"context"
	"testing"

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
	// An operator pinned the EKS provider to another version.
	pinned := &pkgv1.Provider{ObjectMeta: metav1.ObjectMeta{Name: "provider-aws-eks"},
		Spec: pkgv1.ProviderSpec{PackageSpec: pkgv1.PackageSpec{Package: "xpkg.upbound.io/upbound/provider-aws-eks:v2.8.0"}}}
	k := fakeKube(t, pinned)

	got, err := crossplaneaws.InstallProviders(ctx, k, "arn:aws:iam::111122223333:role/platform")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Created) != 5 || len(got.Existing) != 1 {
		t.Fatalf("created %v, existing %v; want the activation policy, runtime config, ec2, iam and helm created, eks existing", got.Created, got.Existing)
	}
	helm := &pkgv1.Provider{}
	if err := k.Get(ctx, client.ObjectKey{Name: crossplane.HelmProviderName}, helm); err != nil ||
		helm.Spec.Package != "xpkg.crossplane.io/crossplane-contrib/provider-helm:"+crossplane.HelmProviderVersion ||
		helm.Spec.RuntimeConfigReference != nil {
		t.Errorf("provider-helm = %+v, %v; want the default runtime config (no AWS identity)", helm.Spec, err)
	}
	eks := &pkgv1.Provider{}
	if err := k.Get(ctx, client.ObjectKey{Name: "provider-aws-eks"}, eks); err != nil || eks.Spec.Package != pinned.Spec.Package {
		t.Errorf("existing provider changed: %v %v", eks.Spec.Package, err)
	}
	ec2 := &pkgv1.Provider{}
	if err := k.Get(ctx, client.ObjectKey{Name: "provider-aws-ec2"}, ec2); err != nil ||
		ec2.Spec.Package != "xpkg.upbound.io/upbound/provider-aws-ec2:"+crossplaneaws.ProviderVersion ||
		ec2.Spec.RuntimeConfigReference.Name != crossplaneaws.RuntimeConfigName {
		t.Errorf("provider-aws-ec2 = %+v, %v", ec2.Spec, err)
	}

	// Second start: nothing to create.
	again, err := crossplaneaws.InstallProviders(ctx, k, "arn:aws:iam::111122223333:role/platform")
	if err != nil || len(again.Created) != 0 || len(again.Existing) != 6 {
		t.Errorf("second run: %+v, %v", again, err)
	}
}

func TestInstallProvidersWithoutManagementRole(t *testing.T) {
	ctx := context.Background()
	k := fakeKube(t)
	if _, err := crossplaneaws.InstallProviders(ctx, k, ""); err != nil {
		t.Fatal(err)
	}
	drc := &pkgv1beta1.DeploymentRuntimeConfig{}
	if err := k.Get(ctx, client.ObjectKey{Name: crossplaneaws.RuntimeConfigName}, drc); err == nil {
		t.Error("runtime config created without a management role")
	}
	iam := &pkgv1.Provider{}
	if err := k.Get(ctx, client.ObjectKey{Name: "provider-aws-iam"}, iam); err != nil || iam.Spec.RuntimeConfigReference != nil {
		t.Errorf("provider-aws-iam = %+v, %v; want Crossplane's default runtime config", iam.Spec, err)
	}
}
