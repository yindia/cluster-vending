package aws_test

import (
	"testing"

	"github.com/yindia/cluster-vending/pkg/crossplane"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
)

const (
	mgmtRole   = "arn:aws:iam::111122223333:role/crossplane-provider-aws"
	tenantRole = "arn:aws:iam::444455556666:role/cluster-vending-provisioner"
)

func TestAWSRuntimeConfig(t *testing.T) {
	rc := crossplaneaws.RuntimeConfig(mgmtRole)
	if rc.Name != crossplaneaws.RuntimeConfigName || rc.Namespace != "" {
		t.Errorf("metadata = %+v", rc.ObjectMeta)
	}
	if got := rc.Spec.ServiceAccountTemplate.Metadata.Annotations["eks.amazonaws.com/role-arn"]; got != mgmtRole {
		t.Errorf("IRSA annotation = %q", got)
	}
}

func TestAWSProviders(t *testing.T) {
	ps := crossplaneaws.Providers()
	if len(ps) != len(crossplaneaws.ProviderPackages) {
		t.Fatalf("got %d providers", len(ps))
	}
	ec2 := ps[0]
	if ec2.Name != "provider-aws-ec2" || ec2.Spec.Package != "xpkg.upbound.io/upbound/provider-aws-ec2:"+crossplaneaws.ProviderVersion {
		t.Errorf("provider = %s %s", ec2.Name, ec2.Spec.Package)
	}
	if ref := ec2.Spec.RuntimeConfigReference; ref == nil || ref.Name != crossplaneaws.RuntimeConfigName {
		t.Errorf("runtimeConfigRef = %+v", ref)
	}
}

func TestAWSProviderConfig(t *testing.T) {
	pc := crossplaneaws.ProviderConfig("tenant-acme", "aws-0a1b2c3d", "acme", tenantRole)
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
