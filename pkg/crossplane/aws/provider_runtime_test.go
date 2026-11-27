package aws_test

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/yindia/cluster-vending/pkg/crossplane"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
)

// Each provider has its own runtime config: one Deployment each, tuned on
// its own.
func TestEveryProviderHasItsOwnRuntimeConfig(t *testing.T) {
	rt := crossplaneaws.DefaultProviderRuntime()
	rt.ManagementRoleARN = mgmtRole
	configs, err := crossplaneaws.RuntimeConfigs(rt)
	if err != nil || len(configs) != len(crossplaneaws.ProviderPackages)+1 {
		t.Fatalf("%d configs, %v", len(configs), err)
	}
	for i, p := range crossplaneaws.Providers() {
		if ref := p.Spec.RuntimeConfigReference; ref == nil || ref.Name != configs[i].Name {
			t.Errorf("%s refers to %+v, want %s", p.Name, ref, configs[i].Name)
		}
	}
	if configs[0].Name != crossplaneaws.RuntimeConfigEC2Name || configs[len(configs)-1].Name != crossplaneaws.RuntimeConfigHelmName ||
		crossplaneaws.HelmProvider().Spec.RuntimeConfigReference.Name != crossplaneaws.RuntimeConfigHelmName {
		t.Errorf("names: first %s, last %s", configs[0].Name, configs[len(configs)-1].Name)
	}
	for _, rc := range configs {
		irsa := rc.Spec.ServiceAccountTemplate
		if rc.Name == crossplaneaws.RuntimeConfigHelmName {
			if irsa != nil {
				t.Errorf("%s: provider-helm has no AWS identity, got %+v", rc.Name, irsa)
			}
		} else if irsa == nil || irsa.Metadata.Annotations["eks.amazonaws.com/role-arn"] != mgmtRole {
			t.Errorf("%s: IRSA annotation %+v", rc.Name, irsa)
		}
		ctr := rc.Spec.DeploymentTemplate.Spec.Template.Spec.Containers[0]
		if ctr.Name != "package-runtime" || len(ctr.Args) != 3 || ctr.Args[2] != "--leader-election" {
			t.Errorf("%s: container %s args %v", rc.Name, ctr.Name, ctr.Args)
		}
	}
}

// The defaults: one replica each, the AWS providers at 10 reconciles a
// second and provider-helm at 50, EC2 polling every 30m, requests by size.
func TestProviderRuntimeDefaults(t *testing.T) {
	configs, err := crossplaneaws.RuntimeConfigs(crossplaneaws.DefaultProviderRuntime())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]struct {
		args          []string
		cpu, memory   string
		limitCPU, mem string
	}{
		"cluster-vending-aws-ec2":              {[]string{"--poll=30m0s", "--max-reconcile-rate=10"}, "500m", "1Gi", "2", "2Gi"},
		"cluster-vending-aws-eks":              {[]string{"--poll=10m0s", "--max-reconcile-rate=10"}, "500m", "1Gi", "2", "2Gi"},
		"cluster-vending-aws-iam":              {[]string{"--poll=10m0s", "--max-reconcile-rate=10"}, "200m", "512Mi", "1", "1Gi"},
		"cluster-vending-aws-kms":              {[]string{"--poll=10m0s", "--max-reconcile-rate=10"}, "50m", "256Mi", "500m", "768Mi"},
		"cluster-vending-aws-sqs":              {[]string{"--poll=10m0s", "--max-reconcile-rate=10"}, "50m", "256Mi", "500m", "768Mi"},
		"cluster-vending-aws-cloudwatchevents": {[]string{"--poll=10m0s", "--max-reconcile-rate=10"}, "50m", "256Mi", "500m", "768Mi"},
		"cluster-vending-aws-cloudwatchlogs":   {[]string{"--poll=10m0s", "--max-reconcile-rate=10"}, "50m", "256Mi", "500m", "768Mi"},
		"cluster-vending-aws-s3":               {[]string{"--poll=10m0s", "--max-reconcile-rate=10"}, "50m", "256Mi", "500m", "768Mi"},
		"cluster-vending-helm":                 {[]string{"--poll=10m0s", "--max-reconcile-rate=50"}, "100m", "256Mi", "1", "1Gi"},
	}
	for _, rc := range configs {
		w, ok := want[rc.Name]
		ctr := rc.Spec.DeploymentTemplate.Spec.Template.Spec.Containers[0]
		r := ctr.Resources
		if !ok || !reflect.DeepEqual(ctr.Args[:2], w.args) || r.Requests.Cpu().String() != w.cpu || r.Requests.Memory().String() != w.memory ||
			r.Limits.Cpu().String() != w.limitCPU || r.Limits.Memory().String() != w.mem {
			t.Errorf("%s: args %v, resources %+v; want %+v", rc.Name, ctr.Args, r, w)
		}
		if replicas := rc.Spec.DeploymentTemplate.Spec.Replicas; replicas == nil || *replicas != 1 {
			t.Errorf("%s: replicas %v, want 1", rc.Name, replicas)
		}
	}
	if crossplaneaws.DefaultProviderRuntime().For("provider-aws-ec2").Poll != crossplaneaws.DefaultEC2Poll {
		t.Error("the EC2 poll is not DefaultEC2Poll (the network's observation bound is derived from it)")
	}
}

// The platform config's providers section goes over the defaults: its
// default for every provider, its per-provider values over that.
func TestProviderRuntimeOverrides(t *testing.T) {
	base := crossplaneaws.DefaultProviderRuntime()
	configured := crossplaneaws.ProviderRuntime{
		Default:   crossplaneaws.ProviderTuning{Replicas: 2, MemoryLimit: "3Gi"},
		Providers: map[string]crossplaneaws.ProviderTuning{"provider-aws-eks": {Poll: 5 * time.Minute, CPURequest: "1"}, "provider-aws-iam": {MaxReconcileRate: 20}},
	}

	rt := base.Over(configured)

	for _, tc := range []struct {
		provider string
		want     crossplaneaws.ProviderTuning
	}{
		{"provider-aws-eks", crossplaneaws.ProviderTuning{Replicas: 2, Poll: 5 * time.Minute, MaxReconcileRate: 10, CPURequest: "1", MemoryRequest: "1Gi", CPULimit: "2", MemoryLimit: "3Gi"}},
		{"provider-aws-iam", crossplaneaws.ProviderTuning{Replicas: 2, Poll: 10 * time.Minute, MaxReconcileRate: 20, CPURequest: "200m", MemoryRequest: "512Mi", CPULimit: "1", MemoryLimit: "3Gi"}},
		{"provider-aws-kms", crossplaneaws.ProviderTuning{Replicas: 2, Poll: 10 * time.Minute, MaxReconcileRate: 10, CPURequest: "50m", MemoryRequest: "256Mi", CPULimit: "500m", MemoryLimit: "3Gi"}},
		{crossplane.HelmProviderName, crossplaneaws.ProviderTuning{Replicas: 2, Poll: crossplaneaws.DefaultHelmPoll, MaxReconcileRate: 50, CPURequest: "100m", MemoryRequest: "256Mi", CPULimit: "1", MemoryLimit: "3Gi"}},
	} {
		if got := rt.For(tc.provider); got != tc.want {
			t.Errorf("%s = %+v, want %+v", tc.provider, got, tc.want)
		}
	}
	if !reflect.DeepEqual(base, crossplaneaws.DefaultProviderRuntime()) {
		t.Error("Over changed the defaults it was called on")
	}
}

func TestProviderRuntimeRefusesBadTuning(t *testing.T) {
	for name, rt := range map[string]crossplaneaws.ProviderRuntime{
		"an unknown provider": {Default: crossplaneaws.DefaultProviderRuntime().Default,
			Providers: map[string]crossplaneaws.ProviderTuning{"provider-aws-glacier": {Replicas: 1}}},
		"a bad quantity": crossplaneaws.DefaultProviderRuntime().Over(crossplaneaws.ProviderRuntime{
			Providers: map[string]crossplaneaws.ProviderTuning{"provider-aws-s3": {MemoryLimit: "lots"}}}),
		"no poll": {Default: crossplaneaws.ProviderTuning{MaxReconcileRate: 1}},
		// The network's observation bound is DefaultEC2Poll plus a margin.
		"an EC2 poll past the observation bound": crossplaneaws.DefaultProviderRuntime().Over(crossplaneaws.ProviderRuntime{
			Providers: map[string]crossplaneaws.ProviderTuning{"provider-aws-ec2": {Poll: time.Hour}}}),
	} {
		if _, err := crossplaneaws.RuntimeConfigs(rt); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if !slices.Contains(crossplaneaws.RuntimeProviders(), crossplane.HelmProviderName) {
		t.Error("provider-helm is not tunable")
	}
}
