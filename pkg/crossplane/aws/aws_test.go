package aws_test

import (
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/yindia/cluster-vending/pkg/crossplane"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
)

const (
	mgmtRole   = "arn:aws:iam::111122223333:role/crossplane-provider-aws"
	tenantRole = "arn:aws:iam::444455556666:role/cluster-vending-provisioner"
)

// Without a credentials Secret the ProviderConfig is the IRSA one, exactly
// as it was before the Secret existed; with one, only the credentials
// source changes: the tenant role is assumed either way.
func TestAWSProviderConfigCredentials(t *testing.T) {
	irsa, err := yaml.Marshal(crossplaneaws.ProviderConfig("tenant-acme", "aws-0a1b2c3d", "acme", tenantRole, "", nil))
	if err != nil {
		t.Fatal(err)
	}
	want := `apiVersion: aws.m.upbound.io/v1beta1
kind: ProviderConfig
metadata:
  labels:
    app.kubernetes.io/managed-by: cluster-vending
    cluster-vending.io/tenant-id: acme
  name: aws-0a1b2c3d
  namespace: tenant-acme
spec:
  assumeRoleChain:
  - roleARN: ` + tenantRole + `
  credentials:
    source: IRSA
status: {}
`
	if string(irsa) != want {
		t.Errorf("IRSA ProviderConfig changed:\n%s\nwant:\n%s", irsa, want)
	}

	local := crossplaneaws.ProviderConfig("tenant-acme", "aws-0a1b2c3d", "acme", tenantRole, "",
		&crossplaneaws.CredentialsSecret{Namespace: "crossplane-system", Name: "aws-local", Key: "credentials"})
	ref := local.Spec.Credentials.SecretRef
	if local.Spec.Credentials.Source != "Secret" || ref == nil || ref.Namespace != "crossplane-system" ||
		ref.Name != "aws-local" || ref.Key != "credentials" {
		t.Errorf("credentials = %+v, want the Secret", local.Spec.Credentials)
	}
	if chain := local.Spec.AssumeRoleChain; len(chain) != 1 || *chain[0].RoleARN != tenantRole {
		t.Errorf("assumeRoleChain with a Secret = %+v, want the tenant role", chain)
	}
}

func TestAWSProviderConfig(t *testing.T) {
	pc := crossplaneaws.ProviderConfig("tenant-acme", "aws-0a1b2c3d", "acme", tenantRole, "", nil)
	if pc.Name != "aws-0a1b2c3d" || pc.Namespace != "tenant-acme" || pc.Labels[crossplane.LabelTenantID] != "acme" {
		t.Errorf("metadata = %+v", pc.ObjectMeta)
	}
	if pc.Spec.Credentials.Source != "IRSA" || pc.Spec.Credentials.SecretRef != nil {
		t.Errorf("credentials = %+v, want IRSA without a secret", pc.Spec.Credentials)
	}
	if chain := pc.Spec.AssumeRoleChain; len(chain) != 1 || *chain[0].RoleARN != tenantRole {
		t.Errorf("assumeRoleChain = %+v", chain)
	}
	if ref := crossplaneaws.ProviderConfigRef(pc.Name); ref.Kind != "ProviderConfig" || ref.Name != "aws-0a1b2c3d" {
		t.Errorf("ref = %+v", ref)
	}
}

// With a direct credentials Secret (--skip-tenant-role) the ProviderConfig
// uses the keys as they are: no role is assumed.
func TestAWSProviderConfigDirectCredentials(t *testing.T) {
	direct := crossplaneaws.ProviderConfig("tenant-acme", "aws-0a1b2c3d", "acme", tenantRole, "",
		&crossplaneaws.CredentialsSecret{Namespace: "tenant-acme", Name: "aws-0a1b2c3d-credentials", Key: "credentials", Direct: true})
	if direct.Spec.Credentials.Source != "Secret" || direct.Spec.Credentials.SecretRef == nil {
		t.Errorf("credentials = %+v, want the Secret", direct.Spec.Credentials)
	}
	if len(direct.Spec.AssumeRoleChain) != 0 {
		t.Errorf("assumeRoleChain = %+v, want none", direct.Spec.AssumeRoleChain)
	}
}

// With the account's external ID the tenant role is assumed passing it
// (sts:ExternalId); a direct Secret assumes nothing, so passes nothing.
func TestAWSProviderConfigExternalID(t *testing.T) {
	ext := strings.Repeat("e", 40)

	pc := crossplaneaws.ProviderConfig("tenant-acme", "aws-0a1b2c3d", "acme", tenantRole, ext, nil)

	if chain := pc.Spec.AssumeRoleChain; len(chain) != 1 || chain[0].ExternalID == nil || *chain[0].ExternalID != ext {
		t.Errorf("assumeRoleChain = %+v, want the tenant role with the external ID", chain)
	}
	direct := crossplaneaws.ProviderConfig("tenant-acme", "aws-0a1b2c3d", "acme", tenantRole, ext,
		&crossplaneaws.CredentialsSecret{Namespace: "tenant-acme", Name: "k", Key: "credentials", Direct: true})
	if len(direct.Spec.AssumeRoleChain) != 0 {
		t.Errorf("direct assumeRoleChain = %+v, want none", direct.Spec.AssumeRoleChain)
	}
}
