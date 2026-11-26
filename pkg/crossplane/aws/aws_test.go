package aws_test

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
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
	wantRate := map[string]string{crossplaneaws.RuntimeConfigName: "--max-reconcile-rate=10", crossplaneaws.RuntimeConfigEC2Name: "--max-reconcile-rate=10",
		crossplaneaws.RuntimeConfigHelmName: "--max-reconcile-rate=50"}
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
		if c.Name != "package-runtime" || len(c.Args) != 3 || c.Args[0] != wantPoll[rc.Name] || c.Args[1] != wantRate[rc.Name] ||
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

func TestAWSRuntimeConfigsIndependentHelmPoll(t *testing.T) {
	for _, test := range []struct {
		name     string
		helmPoll time.Duration
		want     string
	}{
		{"override", 20 * time.Second, "--poll=20s"},
		{"zero-is-the-default", 0, "--poll=10m0s"},
		{"small-positive", time.Nanosecond, "--poll=1ns"},
	} {
		t.Run(test.name, func(t *testing.T) {
			rt := crossplaneaws.DefaultProviderRuntime()
			rt.Poll, rt.EC2Poll, rt.HelmPoll = 7*time.Minute, 19*time.Minute, test.helmPoll
			configs, err := crossplaneaws.RuntimeConfigs(rt)
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]string{
				crossplaneaws.RuntimeConfigName:     "--poll=7m0s",
				crossplaneaws.RuntimeConfigEC2Name:  "--poll=19m0s",
				crossplaneaws.RuntimeConfigHelmName: test.want,
			}
			for _, rc := range configs {
				if got := rc.Spec.DeploymentTemplate.Spec.Template.Spec.Containers[0].Args[0]; got != want[rc.Name] {
					t.Errorf("%s poll = %q; want %q", rc.Name, got, want[rc.Name])
				}
			}
			if rt.HelmPoll != test.helmPoll {
				t.Fatal("renderer mutated caller runtime settings")
			}
		})
	}
	for _, interval := range []time.Duration{-time.Nanosecond, -time.Minute} {
		rt := crossplaneaws.DefaultProviderRuntime()
		rt.HelmPoll = interval
		if configs, err := crossplaneaws.RuntimeConfigs(rt); err == nil || configs != nil {
			t.Fatalf("negative Helm poll %s produced runtime configs: %v, %v", interval, configs, err)
		}
	}
}

func TestHelmPollDefaultPreservesExistingInstallation(t *testing.T) {
	ctx := context.Background()
	old := crossplaneaws.DefaultProviderRuntime()
	old.HelmPoll = 20 * time.Second // tuned by someone: never moved
	configs, err := crossplaneaws.RuntimeConfigs(old)
	if err != nil {
		t.Fatal(err)
	}
	helmConfig := configs[2]
	if helmConfig.Name != crossplaneaws.RuntimeConfigHelmName {
		t.Fatal("expected Helm runtime config")
	}
	// Older/operator-managed Providers may use Crossplane's default runtime.
	provider := crossplane.HelmProvider()
	provider.Spec.RuntimeConfigReference = nil
	k := fakeKube(t, helmConfig, provider)
	if _, err := crossplaneaws.InstallProviders(ctx, k, crossplaneaws.DefaultProviderRuntime()); err != nil {
		t.Fatal(err)
	}
	gotConfig := helmConfig.DeepCopy()
	if err := k.Get(ctx, client.ObjectKeyFromObject(helmConfig), gotConfig); err != nil {
		t.Fatal(err)
	}
	gotProvider := provider.DeepCopy()
	if err := k.Get(ctx, client.ObjectKeyFromObject(provider), gotProvider); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotConfig.Spec, helmConfig.Spec) || !reflect.DeepEqual(gotProvider.Spec, provider.Spec) {
		t.Fatal("new Helm polling default overwrote existing runtime configuration or Provider reference")
	}
}

// An existing provider-helm runtime config still at the 15s poll of earlier
// builds is moved to DefaultHelmPoll by --install-providers: only that
// argument changes; the rest of the config stays as it is.
func TestInstallProvidersMovesTheLegacyHelmPoll(t *testing.T) {
	ctx := context.Background()
	legacy := crossplaneaws.DefaultProviderRuntime()
	legacy.HelmPoll = 15 * time.Second
	configs, err := crossplaneaws.RuntimeConfigs(legacy)
	if err != nil {
		t.Fatal(err)
	}
	helmConfig := configs[2]
	ctr := &helmConfig.Spec.DeploymentTemplate.Spec.Template.Spec.Containers[0]
	ctr.Args = append(ctr.Args, "--debug")
	k := fakeKube(t, helmConfig)

	res, err := crossplaneaws.InstallProviders(ctx, k, crossplaneaws.DefaultProviderRuntime())
	if err != nil {
		t.Fatal(err)
	}

	got := helmConfig.DeepCopy()
	if err := k.Get(ctx, client.ObjectKeyFromObject(helmConfig), got); err != nil {
		t.Fatal(err)
	}
	want := []string{"--poll=" + crossplaneaws.DefaultHelmPoll.String(), "--max-reconcile-rate=50", "--leader-election", "--debug"}
	if args := got.Spec.DeploymentTemplate.Spec.Template.Spec.Containers[0].Args; !reflect.DeepEqual(args, want) {
		t.Errorf("args = %v, want %v", args, want)
	}
	if !strings.Contains(strings.Join(res.Updated, ","), crossplaneaws.RuntimeConfigHelmName) {
		t.Errorf("updated = %v, want the Helm runtime config", res.Updated)
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

// provider-helm polls only to catch drift (DefaultHelmPoll, 10m): spec
// changes reach it through its watch and the platform judges a rollout
// itself, so nothing waits for the poll. It keeps its own reconcile rate,
// apart from the AWS providers'.
func TestHelmProviderPollsOnlyForDriftWithItsOwnReconcileRate(t *testing.T) {
	configs, err := crossplaneaws.RuntimeConfigs(crossplaneaws.DefaultProviderRuntime())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		crossplaneaws.RuntimeConfigName:     {"--poll=10m0s", "--max-reconcile-rate=10"},
		crossplaneaws.RuntimeConfigEC2Name:  {"--poll=30m0s", "--max-reconcile-rate=10"},
		crossplaneaws.RuntimeConfigHelmName: {"--poll=10m0s", "--max-reconcile-rate=50"},
	}
	for _, rc := range configs {
		args := rc.Spec.DeploymentTemplate.Spec.Template.Spec.Containers[0].Args
		if !reflect.DeepEqual(args[:2], want[rc.Name]) {
			t.Errorf("%s args = %v, want %v first", rc.Name, args, want[rc.Name])
		}
	}
}
