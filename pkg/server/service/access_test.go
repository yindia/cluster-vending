package service

import (
	"testing"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	awsv1beta1 "github.com/upbound/provider-aws/v2/apis/namespaced/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"connectrpc.com/connect/v2"

	"github.com/yindia/cluster-vending/pkg/config"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/kube"
)

var testAccount = &model.CloudAccount{AccountID: "444455556666", Namespace: "tenant-acme", ProviderConfigName: "aws-1"}

const tenantRole = "arn:aws:iam::444455556666:role/provisioner"

func deps(t *testing.T, objects ...client.Object) Deps {
	t.Helper()
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	return Deps{Kube: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()}
}

func providerConfig(source xpv2.CredentialsSource, secret *xpv2.SecretKeySelector) *awsv1beta1.ProviderConfig {
	pc := &awsv1beta1.ProviderConfig{ObjectMeta: metav1.ObjectMeta{Name: "aws-1", Namespace: "tenant-acme"},
		Spec: awsv1beta1.ProviderConfigSpec{AssumeRoleChain: []awsv1beta1.AssumeRoleOptions{{RoleARN: ptr.To(tenantRole)}}}}
	pc.Spec.Credentials.Source = source
	pc.Spec.Credentials.SecretRef = secret
	return pc
}

func TestAWSAccessPrefersEnvironmentKeys(t *testing.T) {
	d := deps(t) // no ProviderConfig needed
	d.AWS.StaticKeys = &config.StaticKeys{AccessKeyID: "AKIDENV", SecretAccessKey: "s"}
	a, err := d.AWSAccess(t.Context(), testAccount)
	if err != nil || a.Static == nil || a.Static.AccessKeyID != "AKIDENV" || a.RoleARN != "" {
		t.Fatalf("access = %+v, %v; want the environment keys, no role", a, err)
	}
}

func TestAWSAccessFollowsProviderConfig(t *testing.T) {
	// IRSA: the server's own credentials, then the tenant role.
	a, err := deps(t, providerConfig("IRSA", nil)).AWSAccess(t.Context(), testAccount)
	if err != nil || a.Static != nil || a.RoleARN != tenantRole {
		t.Fatalf("IRSA access = %+v, %v", a, err)
	}

	// Secret: the Secret's keys, then the tenant role.
	ref := &xpv2.SecretKeySelector{SecretReference: xpv2.SecretReference{Name: "creds", Namespace: "tenant-acme"}, Key: "credentials"}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: "tenant-acme"},
		Data: map[string][]byte{"credentials": []byte("[other]\naws_access_key_id = WRONG\n\n[default]\naws_access_key_id = AKIDSECRET\naws_secret_access_key = s3cr3t\n")}}
	a, err = deps(t, providerConfig("Secret", ref), secret).AWSAccess(t.Context(), testAccount)
	if err != nil || a.Static == nil || a.Static.AccessKeyID != "AKIDSECRET" || a.Static.SecretAccessKey != "s3cr3t" || a.RoleARN != tenantRole {
		t.Fatalf("Secret access = %+v, %v", a, err)
	}

	for name, d := range map[string]Deps{
		"no ProviderConfig": deps(t),
		"no Secret":         deps(t, providerConfig("Secret", ref)),
		"empty Secret": deps(t, providerConfig("Secret", ref), &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: "tenant-acme"}}),
	} {
		if _, err := d.AWSAccess(t.Context(), testAccount); connect.CodeOf(err) != connect.CodeFailedPrecondition {
			t.Errorf("%s: %v, want FAILED_PRECONDITION", name, err)
		}
	}
}
