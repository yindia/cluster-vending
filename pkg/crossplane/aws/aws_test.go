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

func TestAWSRuntimeConfigs(t *testing.T) {
	rt := crossplaneaws.DefaultProviderRuntime()
	rt.ManagementRoleARN = mgmtRole
	configs, err := crossplaneaws.RuntimeConfigs(rt)
	if err != nil || len(configs) != 2 {
		t.Fatalf("configs = %d, %v", len(configs), err)
	}
	wantPoll := map[string]string{crossplaneaws.RuntimeConfigName: "--poll=10m0s", crossplaneaws.RuntimeConfigEC2Name: "--poll=30m0s"}
	for _, rc := range configs {
		if rc.Namespace != "" || wantPoll[rc.Name] == "" {
			t.Errorf("metadata = %+v", rc.ObjectMeta)
		}
		if got := rc.Spec.ServiceAccountTemplate.Metadata.Annotations["eks.amazonaws.com/role-arn"]; got != mgmtRole {
			t.Errorf("%s: IRSA annotation = %q", rc.Name, got)
		}
		c := rc.Spec.DeploymentTemplate.Spec.Template.Spec.Containers[0]
		if c.Name != "package-runtime" || len(c.Args) != 2 || c.Args[0] != wantPoll[rc.Name] || c.Args[1] != "--max-reconcile-rate=10" {
			t.Errorf("%s: container %s args %v", rc.Name, c.Name, c.Args)
		}
		if c.Resources.Requests.Memory().String() != "512Mi" || c.Resources.Limits.Memory().String() != "2Gi" ||
			!c.Resources.Limits.Cpu().IsZero() {
			t.Errorf("%s: resources = %+v", rc.Name, c.Resources)
		}
	}

	// No management role (local): no IRSA annotation; a bad quantity fails.
	rt.ManagementRoleARN = ""
	configs, err = crossplaneaws.RuntimeConfigs(rt)
	if err != nil || configs[0].Spec.ServiceAccountTemplate != nil {
		t.Errorf("without a role: service account template %+v, %v", configs[0].Spec.ServiceAccountTemplate, err)
	}
	rt.MemoryLimit = "lots"
	if _, err := crossplaneaws.RuntimeConfigs(rt); err == nil {
		t.Error("a bad quantity must fail")
	}
}

func TestAWSProviders(t *testing.T) {
	ps := crossplaneaws.Providers()
	if len(ps) != len(crossplaneaws.ProviderPackages) {
		t.Fatalf("got %d providers", len(ps))
	}
	ec2 := ps[0]
	if ec2.Name != "provider-aws-ec2" || ec2.Spec.Package != "xpkg.crossplane.io/crossplane-contrib/provider-aws-ec2:"+crossplaneaws.ProviderVersion {
		t.Errorf("provider = %s %s", ec2.Name, ec2.Spec.Package)
	}
	if ref := ec2.Spec.RuntimeConfigReference; ref == nil || ref.Name != crossplaneaws.RuntimeConfigEC2Name {
		t.Errorf("ec2 runtimeConfigRef = %+v, want its own (poll interval)", ref)
	}
	if ref := ps[1].Spec.RuntimeConfigReference; ref == nil || ref.Name != crossplaneaws.RuntimeConfigName {
		t.Errorf("%s runtimeConfigRef = %+v", ps[1].Name, ref)
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
