package aws_test

import (
	"testing"

	"sigs.k8s.io/yaml"

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
	if err != nil || len(configs) != 3 {
		t.Fatalf("configs = %d, %v", len(configs), err)
	}
	wantPoll := map[string]string{crossplaneaws.RuntimeConfigName: "--poll=10m0s", crossplaneaws.RuntimeConfigEC2Name: "--poll=30m0s",
		crossplaneaws.RuntimeConfigHelmName: "--poll=10m0s"}
	for _, rc := range configs {
		if rc.Namespace != "" || wantPoll[rc.Name] == "" {
			t.Errorf("metadata = %+v", rc.ObjectMeta)
		}
		// provider-helm has no AWS identity.
		if rc.Name == crossplaneaws.RuntimeConfigHelmName {
			if rc.Spec.ServiceAccountTemplate != nil {
				t.Errorf("%s: service account template %+v, want none", rc.Name, rc.Spec.ServiceAccountTemplate)
			}
		} else if got := rc.Spec.ServiceAccountTemplate.Metadata.Annotations["eks.amazonaws.com/role-arn"]; got != mgmtRole {
			t.Errorf("%s: IRSA annotation = %q", rc.Name, got)
		}
		// Two replicas, one active through leader election.
		if r := rc.Spec.DeploymentTemplate.Spec.Replicas; r == nil || *r != 2 {
			t.Errorf("%s: replicas = %v, want 2", rc.Name, r)
		}
		c := rc.Spec.DeploymentTemplate.Spec.Template.Spec.Containers[0]
		if c.Name != "package-runtime" || len(c.Args) != 3 || c.Args[0] != wantPoll[rc.Name] || c.Args[1] != "--max-reconcile-rate=10" ||
			c.Args[2] != "--leader-election" {
			t.Errorf("%s: container %s args %v", rc.Name, c.Name, c.Args)
		}
		if c.Resources.Requests.Cpu().String() != "1" || c.Resources.Requests.Memory().String() != "2Gi" ||
			c.Resources.Limits.Cpu().String() != "4" || c.Resources.Limits.Memory().String() != "4Gi" {
			t.Errorf("%s: resources = %+v", rc.Name, c.Resources)
		}
	}

	// No replicas set: Crossplane's default.
	one := rt
	one.Replicas = 0
	if configs, err := crossplaneaws.RuntimeConfigs(one); err != nil || configs[0].Spec.DeploymentTemplate.Spec.Replicas != nil {
		t.Errorf("without replicas: %v, %v", configs[0].Spec.DeploymentTemplate.Spec.Replicas, err)
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

// Without a credentials Secret the ProviderConfig is the IRSA one, exactly
// as it was before the Secret existed; with one, only the credentials
// source changes: the tenant role is assumed either way.
func TestAWSProviderConfigCredentials(t *testing.T) {
	irsa, err := yaml.Marshal(crossplaneaws.ProviderConfig("tenant-acme", "aws-0a1b2c3d", "acme", tenantRole, nil))
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

	local := crossplaneaws.ProviderConfig("tenant-acme", "aws-0a1b2c3d", "acme", tenantRole,
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
	pc := crossplaneaws.ProviderConfig("tenant-acme", "aws-0a1b2c3d", "acme", tenantRole, nil)
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
	direct := crossplaneaws.ProviderConfig("tenant-acme", "aws-0a1b2c3d", "acme", tenantRole,
		&crossplaneaws.CredentialsSecret{Namespace: "tenant-acme", Name: "aws-0a1b2c3d-credentials", Key: "credentials", Direct: true})
	if direct.Spec.Credentials.Source != "Secret" || direct.Spec.Credentials.SecretRef == nil {
		t.Errorf("credentials = %+v, want the Secret", direct.Spec.Credentials)
	}
	if len(direct.Spec.AssumeRoleChain) != 0 {
		t.Errorf("assumeRoleChain = %+v, want none", direct.Spec.AssumeRoleChain)
	}
}
