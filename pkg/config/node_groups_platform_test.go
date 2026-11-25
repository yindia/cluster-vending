package config_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yindia/cluster-vending/pkg/config"
)

// PROP-03 4.4: the bootstrap group's list of instance types and the
// platform pool's section, in the one file both commands read.

func TestNodeGroupsDefaultsOfThePlatformPool(t *testing.T) {
	want := config.PlatformNodePool{
		InstanceFamilies: []string{"t3"}, InstanceSizes: []string{"large", "xlarge", "2xlarge"},
		Architectures: []string{"AMD64"}, CapacityTypes: []string{"ON_DEMAND"},
		Limits:              config.PlatformLimits{CPU: 128, Memory: "512Gi"},
		ConsolidationPolicy: "WhenEmpty", ConsolidateAfter: 5 * time.Minute,
		ExpireAfter: 336 * time.Hour, TerminationGracePeriod: time.Hour, DisruptionBudget: 1,
	}
	if got := config.DefaultNodeGroups().Platform; !reflect.DeepEqual(got, want) {
		t.Fatalf("DefaultNodeGroups().Platform = %+v", got)
	}
	serve, err := config.LoadServe(nil, nil, nil)
	if err != nil || !reflect.DeepEqual(serve.NodeGroups, config.DefaultNodeGroups()) {
		t.Fatalf("serve = %+v (%v)", serve.NodeGroups, err)
	}
	if got := config.DefaultNodeGroups().System.InstanceTypes; !reflect.DeepEqual(got, []string{"t3.medium"}) {
		t.Errorf("system instance types = %v", got)
	}
}

func TestNodeGroupsSectionOfTheBootstrapAndPlatformGroups(t *testing.T) {
	file := loadSection(t, `
nodeGroups:
  system:
    instanceTypes: [m6i.large, m5.large]
    minSize: 3
    capacityType: ON_DEMAND
    diskSizeGiB: 40
  platform:
    instanceFamilies: [m7g]
    instanceSizes: [xlarge, 2xlarge, 4xlarge]
    architectures: [ARM64]
    capacityTypes: [ON_DEMAND]
    limits:
      cpu: 64
      memory: 256Gi
    consolidationPolicy: WhenEmptyOrUnderutilized
    consolidateAfter: 10m
    expireAfter: 168h
    terminationGracePeriod: 30m
    disruptionBudget: 2
    maxPods: 110
`)
	want := config.PlatformNodePool{
		InstanceFamilies: []string{"m7g"}, InstanceSizes: []string{"xlarge", "2xlarge", "4xlarge"},
		Architectures: []string{"ARM64"}, CapacityTypes: []string{"ON_DEMAND"},
		Limits:              config.PlatformLimits{CPU: 64, Memory: "256Gi"},
		ConsolidationPolicy: "WhenEmptyOrUnderutilized", ConsolidateAfter: 10 * time.Minute,
		ExpireAfter: 168 * time.Hour, TerminationGracePeriod: 30 * time.Minute, DisruptionBudget: 2, MaxPods: 110,
	}
	serve, err := config.LoadServe(nil, nil, file)
	if err != nil {
		t.Fatal(err)
	}
	ctl, err := config.LoadController(nil, lookupIn(management(nil)), file)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(serve.NodeGroups, ctl.NodeGroups) {
		t.Fatalf("serve %+v != controller %+v", serve.NodeGroups, ctl.NodeGroups)
	}
	if got := serve.NodeGroups.Platform; !reflect.DeepEqual(got, want) {
		t.Errorf("platform = %+v", got)
	}
	sys := serve.NodeGroups.System
	if !reflect.DeepEqual(sys.InstanceTypes, []string{"m6i.large", "m5.large"}) || sys.MinSize != 3 || sys.DiskSizeGiB != 40 {
		t.Errorf("system = %+v", sys)
	}
}

// A flag beats the variable, which beats the file, for the new keys too.
func TestPlatformSettingsFlagAndEnvOverrideTheFile(t *testing.T) {
	file := loadSection(t, "nodeGroups:\n  platform:\n    instanceFamilies: [m7i]\n    disruptionBudget: 2\n")
	env := lookupIn(map[string]string{config.EnvPlatformInstanceFamilies: "m6i, c7i", config.EnvSystemInstanceTypes: "m6i.large,m5.large"})
	serve, err := config.LoadServe(nil, env, file)
	if err != nil || !reflect.DeepEqual(serve.NodeGroups.Platform.InstanceFamilies, []string{"m6i", "c7i"}) ||
		!reflect.DeepEqual(serve.NodeGroups.System.InstanceTypes, []string{"m6i.large", "m5.large"}) ||
		serve.NodeGroups.Platform.DisruptionBudget != 2 {
		t.Fatalf("env: %+v (%v)", serve.NodeGroups, err)
	}
	flags := lookupIn(map[string]string{config.FlagPlatformDisruptionBudget: "3", config.FlagPlatformLimitCPU: "32", config.FlagPlatformLimitMemory: "128Gi",
		config.FlagPlatformConsolidationPolicy: "WhenEmptyOrUnderutilized"})
	serve, err = config.LoadServe(flags, env, file)
	got := serve.NodeGroups.Platform
	if err != nil || got.DisruptionBudget != 3 || got.Limits != (config.PlatformLimits{CPU: 32, Memory: "128Gi"}) || got.ConsolidationPolicy != "WhenEmptyOrUnderutilized" {
		t.Fatalf("flags: %+v (%v)", got, err)
	}
}

// The old single instanceType key keeps working with a warning; a leftover
// maxSize is parsed, kept as the old ceiling and warned about.
func TestOldBootstrapKeysStillParseWithAWarning(t *testing.T) {
	file := loadSection(t, "nodeGroups:\n  system:\n    instanceType: m6i.large\n    minSize: 2\n    maxSize: 9\n")
	for who, load := range map[string]func() (config.NodeGroups, error){
		"serve": func() (config.NodeGroups, error) {
			c, err := config.LoadServe(nil, nil, file)
			return c.NodeGroups, err
		},
		"controller": func() (config.NodeGroups, error) {
			c, err := config.LoadController(nil, lookupIn(management(nil)), file)
			return c.NodeGroups, err
		},
	} {
		ng, err := load()
		if err != nil {
			t.Fatalf("%s: %v", who, err)
		}
		if !reflect.DeepEqual(ng.System.InstanceTypes, []string{"m6i.large"}) || ng.System.MinSize != 2 {
			t.Errorf("%s: system = %+v", who, ng.System)
		}
		joined := strings.Join(ng.Warnings, "\n")
		for _, want := range []string{"nodeGroups.system.maxSize", "nodeGroups.system.instanceType"} {
			if !strings.Contains(joined, want) {
				t.Errorf("%s: warnings %q lack %q", who, joined, want)
			}
		}
	}
	// No old key, no warning.
	serve, err := config.LoadServe(nil, nil, nil)
	if err != nil || len(serve.NodeGroups.Warnings) != 0 {
		t.Errorf("default warnings = %v (%v)", serve.NodeGroups.Warnings, err)
	}
}

func TestSystemInstanceTypeAndTypesTogetherAreRefused(t *testing.T) {
	file := loadSection(t, "nodeGroups:\n  system:\n    instanceType: t3.large\n    instanceTypes: [t3.medium]\n")
	if _, err := config.LoadServe(nil, nil, file); err == nil || !strings.Contains(err.Error(), "instanceType") {
		t.Fatalf("err = %v", err)
	}
}

// Every rule of 4.4 has a case; both commands refuse identically.
func TestPlatformAndBootstrapValidationRules(t *testing.T) {
	for name, tt := range map[string]struct {
		section string
		want    string
	}{
		"empty bootstrap list":      {"system:\n    instanceTypes: []\n", "instance type"},
		"bootstrap bad type":        {"system:\n    instanceTypes: [t3large]\n", "instance type"},
		"bootstrap mixes arch":      {"system:\n    instanceTypes: [m6i.large, m6g.large]\n", "architecture"},
		"bootstrap min below two":   {"system:\n    minSize: 1\n", "at least 2"},
		"platform no family":        {"platform:\n    instanceFamilies: []\n", "instanceFamilies"},
		"platform family shape":     {"platform:\n    instanceFamilies: [m7i.large]\n", "instance family"},
		"platform no size":          {"platform:\n    instanceSizes: []\n", "instanceSizes"},
		"platform size shape":       {"platform:\n    instanceSizes: [big]\n", "instance size"},
		"platform mixes arch":       {"platform:\n    instanceFamilies: [m7i, m7g]\n", "architecture"},
		"platform arch not one":     {"platform:\n    architectures: [AMD64, ARM64]\n", "one architecture"},
		"platform arch unknown":     {"platform:\n    architectures: [x86]\n", "AMD64, ARM64"},
		"platform family vs arch":   {"platform:\n    architectures: [ARM64]\n", "does not match"},
		"platform spot":             {"platform:\n    capacityTypes: [ON_DEMAND, SPOT]\n", "SPOT"},
		"platform capacity empty":   {"platform:\n    capacityTypes: []\n", "capacityTypes"},
		"limit cpu zero":            {"platform:\n    limits:\n      cpu: 0\n", "limits.cpu"},
		"limit cpu not a number":    {"platform:\n    limits:\n      cpu: lots\n", "limits.cpu"},
		"limit memory not quantity": {"platform:\n    limits:\n      memory: lots\n", "limits.memory"},
		"limit memory zero":         {"platform:\n    limits:\n      memory: 0Gi\n", "limits.memory"},
		"policy unknown":            {"platform:\n    consolidationPolicy: Always\n", "WhenEmpty, WhenEmptyOrUnderutilized"},
		"consolidateAfter negative": {"platform:\n    consolidateAfter: -1m\n", "consolidateAfter"},
		"consolidateAfter garbage":  {"platform:\n    consolidateAfter: soon\n", "consolidateAfter"},
		"expireAfter zero":          {"platform:\n    expireAfter: 0s\n", "expireAfter"},
		"grace period zero":         {"platform:\n    terminationGracePeriod: 0s\n", "terminationGracePeriod"},
		"budget zero":               {"platform:\n    disruptionBudget: 0\n", "disruptionBudget"},
		"budget not a number":       {"platform:\n    disruptionBudget: one\n", "disruptionBudget"},
		"maxPods above 110":         {"platform:\n    maxPods: 111\n", "maxPods"},
		"maxPods negative":          {"platform:\n    maxPods: -1\n", "maxPods"},
	} {
		t.Run(name, func(t *testing.T) {
			file := loadSection(t, "nodeGroups:\n  "+tt.section)
			_, serveErr := config.LoadServe(nil, nil, file)
			_, ctlErr := config.LoadController(nil, lookupIn(management(nil)), file)
			for who, err := range map[string]error{"serve": serveErr, "controller": ctlErr} {
				if err == nil || !strings.Contains(err.Error(), tt.want) {
					t.Errorf("%s: err = %v, want it to mention %q", who, err, tt.want)
				}
			}
			if serveErr != nil && ctlErr != nil && nodeGroupProblemLines(serveErr) != nodeGroupProblemLines(ctlErr) {
				t.Errorf("different problems:\nserve:      %v\ncontroller: %v", serveErr, ctlErr)
			}
		})
	}
}

// Every problem is listed, not only the first.
func TestPlatformValidationListsEveryProblem(t *testing.T) {
	file := loadSection(t, `
nodeGroups:
  system:
    minSize: 1
    instanceTypes: [t3large]
  platform:
    capacityTypes: [SPOT]
    limits: {cpu: 0, memory: 0}
    disruptionBudget: 0
    maxPods: 500
`)
	_, err := config.LoadServe(nil, nil, file)
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"at least 2", "instance type", "SPOT", "limits.cpu", "limits.memory", "disruptionBudget", "maxPods"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q:\n%v", want, err)
		}
	}
}

// A valid but unusual shape passes: a different pool architecture, with
// matching families, and mixed vCPU classes of one family.
func TestPlatformPoolOnArm(t *testing.T) {
	file := loadSection(t, "nodeGroups:\n  platform:\n    architectures: [ARM64]\n    instanceFamilies: [m7g, m6g]\n")
	serve, err := config.LoadServe(nil, nil, file)
	if err != nil || serve.NodeGroups.Platform.Architectures[0] != "ARM64" {
		t.Fatalf("%+v (%v)", serve.NodeGroups.Platform, err)
	}
}

func nodeGroupProblemLines(err error) string {
	var out []string
	for _, line := range strings.Split(err.Error(), "\n") {
		if strings.Contains(line, "nodeGroups") || strings.Contains(line, "node-group") || strings.Contains(line, "platform-") || strings.Contains(line, "system-") {
			out = append(out, strings.TrimSpace(line))
		}
	}
	return strings.Join(out, "\n")
}

func TestLeftoverPlatformVariablesAreNamed(t *testing.T) {
	got := config.LeftoverNodeGroupVariables(lookupIn(map[string]string{config.EnvPlatformLimitCPU: "64", config.EnvSystemInstanceTypes: "t3.large"}))
	if len(got) != 2 {
		t.Errorf("got %v, want both variables", got)
	}
}
