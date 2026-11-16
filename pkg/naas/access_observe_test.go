package naas

import (
	"testing"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/crossplane"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/kube"
)

func TestAccessOIDCRequiresActiveObservedExactPhysicalMapping(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*cloudv1.ClusterSpec, *awseks.IdentityProviderConfig)
		ok     bool
	}{
		{name: "active exact observed mapping", ok: true},
		{name: "documented username defaults", ok: true, change: func(_ *cloudv1.ClusterSpec, idp *awseks.IdentityProviderConfig) {
			idp.Status.AtProvider.Oidc.UsernameClaim = ptr.To("sub")
			idp.Status.AtProvider.Oidc.UsernamePrefix = ptr.To("https://login.example.test#")
		}},
		{name: "email has no default prefix", ok: true, change: func(spec *cloudv1.ClusterSpec, idp *awseks.IdentityProviderConfig) {
			spec.Access.Oidc.UsernameClaim = "email"
			idp.Spec.ForProvider.Oidc.UsernameClaim = ptr.To("email")
			idp.Status.AtProvider.Oidc.UsernameClaim = ptr.To("email")
		}},
		{name: "explicit dash disables username prefix", ok: true, change: func(spec *cloudv1.ClusterSpec, idp *awseks.IdentityProviderConfig) {
			spec.Access.Oidc.UsernamePrefix = "-"
			idp.Spec.ForProvider.Oidc.UsernamePrefix = ptr.To("-")
			idp.Status.AtProvider.Oidc.UsernameClaim = ptr.To("sub")
			idp.Status.AtProvider.Oidc.UsernamePrefix = ptr.To("")
		}},
		{name: "stale desired mapping", change: func(spec *cloudv1.ClusterSpec, _ *awseks.IdentityProviderConfig) {
			spec.Access.Oidc.GroupsPrefix = "new:"
		}},
		{name: "incorrect observed audience", change: func(_ *cloudv1.ClusterSpec, idp *awseks.IdentityProviderConfig) {
			idp.Status.AtProvider.Oidc.ClientID = ptr.To("foreign-client")
		}},
		{name: "incorrect observed issuer", change: func(_ *cloudv1.ClusterSpec, idp *awseks.IdentityProviderConfig) {
			idp.Status.AtProvider.Oidc.IssuerURL = ptr.To("https://foreign.example.test")
		}},
		{name: "incorrect observed groups claim", change: func(_ *cloudv1.ClusterSpec, idp *awseks.IdentityProviderConfig) {
			idp.Status.AtProvider.Oidc.GroupsClaim = ptr.To("foreign-claim")
		}},
		{name: "incorrect observed required claims", change: func(_ *cloudv1.ClusterSpec, idp *awseks.IdentityProviderConfig) {
			idp.Status.AtProvider.Oidc.RequiredClaims = map[string]*string{"unapproved": ptr.To("value")}
		}},
		{name: "not active", change: func(_ *cloudv1.ClusterSpec, idp *awseks.IdentityProviderConfig) {
			idp.Status.AtProvider.Status = ptr.To("ASSOCIATING")
		}},
		{name: "not ready", change: func(_ *cloudv1.ClusterSpec, idp *awseks.IdentityProviderConfig) {
			idp.SetConditions(xpv2.Unavailable())
		}},
		{name: "not synced", change: func(_ *cloudv1.ClusterSpec, idp *awseks.IdentityProviderConfig) {
			idp.Status.Conditions = idp.Status.Conditions[:1]
		}},
		{name: "foreign physical cluster", change: func(_ *cloudv1.ClusterSpec, idp *awseks.IdentityProviderConfig) {
			idp.Status.AtProvider.ClusterName = ptr.To("foreign")
		}},
		{name: "foreign AWS account", change: func(_ *cloudv1.ClusterSpec, idp *awseks.IdentityProviderConfig) {
			idp.Status.AtProvider.Arn = ptr.To("arn:aws:eks:us-east-1:999999999999:identityproviderconfig/prod/oidc/name/id")
		}},
		{name: "foreign ARN partition", change: func(_ *cloudv1.ClusterSpec, idp *awseks.IdentityProviderConfig) {
			idp.Status.AtProvider.Arn = ptr.To("arn:aws-us-gov:eks:us-east-1:111122223333:identityproviderconfig/prod/oidc/name/id")
		}},
		{name: "foreign ARN region", change: func(_ *cloudv1.ClusterSpec, idp *awseks.IdentityProviderConfig) {
			idp.Status.AtProvider.Arn = ptr.To("arn:aws:eks:us-west-2:111122223333:identityproviderconfig/prod/oidc/name/id")
		}},
		{name: "foreign ARN physical name", change: func(_ *cloudv1.ClusterSpec, idp *awseks.IdentityProviderConfig) {
			idp.Status.AtProvider.Arn = ptr.To("arn:aws:eks:us-east-1:111122223333:identityproviderconfig/prod-other/oidc/name/id")
		}},
		{name: "missing observed ARN", change: func(_ *cloudv1.ClusterSpec, idp *awseks.IdentityProviderConfig) {
			idp.Status.AtProvider.Arn = nil
		}},
		{name: "malformed observed ARN", change: func(_ *cloudv1.ClusterSpec, idp *awseks.IdentityProviderConfig) {
			idp.Status.AtProvider.Arn = ptr.To("not-an-arn:identityproviderconfig/prod/oidc/name/id")
		}},
		{name: "wrong desired physical binding", change: func(_ *cloudv1.ClusterSpec, idp *awseks.IdentityProviderConfig) {
			idp.Spec.ForProvider.ClusterName = ptr.To("foreign")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec := accessCluster()
			spec.Region = "us-east-1"
			parent := &cloud.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "prod", Namespace: "tenant-acme", Labels: map[string]string{cloudstore.LabelTenant: "acme", cloudstore.LabelID: "cluster-id"}}, Status: cloud.ClusterStatus{AWS: &cloud.AWSClusterStatus{ClusterARN: "arn:aws:eks:us-east-1:111122223333:cluster/prod"}}}
			idp := &awseks.IdentityProviderConfig{ObjectMeta: metav1.ObjectMeta{Name: "idp", Namespace: parent.Namespace, UID: "association-uid", Labels: crossplane.ClusterLabels("acme", "cluster-id")},
				Spec:   awseks.IdentityProviderConfigSpec{ForProvider: awseks.IdentityProviderConfigParameters{Region: ptr.To(spec.Region), ClusterName: ptr.To("prod"), Oidc: &awseks.IdentityProviderConfigOidcParameters{IssuerURL: ptr.To("https://login.example.test"), ClientID: ptr.To("kubernetes"), GroupsClaim: ptr.To("groups"), GroupsPrefix: ptr.To("oidc:")}}},
				Status: awseks.IdentityProviderConfigStatus{AtProvider: awseks.IdentityProviderConfigObservation{Status: ptr.To("ACTIVE"), ClusterName: ptr.To("prod"), Arn: ptr.To("arn:aws:eks:us-east-1:111122223333:identityproviderconfig/prod/oidc/name/id"), Oidc: &awseks.IdentityProviderConfigOidcObservation{IssuerURL: ptr.To("https://login.example.test"), ClientID: ptr.To("kubernetes"), GroupsClaim: ptr.To("groups"), GroupsPrefix: ptr.To("oidc:")}}}}
			idp.SetConditions(xpv2.Available(), xpv2.ReconcileSuccess())
			if test.change != nil {
				test.change(spec, idp)
			}
			scheme, err := kube.Scheme()
			if err != nil {
				t.Fatal(err)
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(idp).Build()
			uid, err := ObserveAccessOIDC(t.Context(), c, parent, spec)
			if (err == nil) != test.ok || test.ok && uid != "association-uid" {
				t.Fatalf("uid=%s err=%v, expected ok=%t", uid, err, test.ok)
			}
		})
	}
}

func TestProvisioningPayloadReadinessLagDoesNotAffectSharedNamespace(t *testing.T) {
	ordinary := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "naas-system", "labels": map[string]any{"pod-security.kubernetes.io/enforce": "restricted"}}}}
	if !ProvisioningPayloadMatches(ordinary, ordinary) {
		t.Fatal("shared namespace incorrectly requires tenant access readiness")
	}
	expected := ordinary.DeepCopy()
	expected.SetLabels(map[string]string{"cloud.cluster-vending.io/naas-ready": "true", "pod-security.kubernetes.io/enforce": "restricted"})
	actual := expected.DeepCopy()
	actual.SetLabels(map[string]string{"cloud.cluster-vending.io/naas-ready": "false", "pod-security.kubernetes.io/enforce": "restricted"})
	if !ProvisioningPayloadMatches(actual, expected) {
		t.Fatal("asynchronous access-label propagation invalidated provisioning")
	}
	actual.SetLabels(map[string]string{"cloud.cluster-vending.io/naas-ready": "false", "pod-security.kubernetes.io/enforce": "privileged"})
	if ProvisioningPayloadMatches(actual, expected) {
		t.Fatal("security policy drift was normalized away")
	}
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	objects := []client.Object{}
	for _, object := range mustLoad(t).Objects() {
		objects = append(objects, object)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	if _, err := VerifyTenantBootstrap(t.Context(), c); err != nil {
		t.Fatal("real bootstrap objects failed shared-namespace observation", err)
	}
}
