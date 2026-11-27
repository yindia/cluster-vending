package config_test

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/yindia/cluster-vending/pkg/config"
)

func loadSection(t *testing.T, yaml string) config.LookupFunc {
	t.Helper()
	path := t.TempDir() + "/c.yaml"
	if err := writeFile(path, yaml); err != nil {
		t.Fatal(err)
	}
	file, err := config.LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return file
}

func TestNodeGroupsDefaultsAreT3AndThreeNodes(t *testing.T) {
	want := config.NodeGroups{
		Platform: config.DefaultNodeGroups().Platform,
		System: config.SystemNodeGroup{InstanceTypes: []string{"t3.medium"}, MinSize: 3, CapacityType: "ON_DEMAND",
			AMIType: "AL2023_x86_64_STANDARD"},
	}
	if got := config.DefaultNodeGroups(); !reflect.DeepEqual(got, want) {
		t.Fatalf("DefaultNodeGroups = %+v", got)
	}
	serve, err := config.LoadServe(nil, nil, nil)
	if err != nil || !reflect.DeepEqual(serve.NodeGroups, want) {
		t.Fatalf("serve = %+v (%v)", serve.NodeGroups, err)
	}
	ctl, err := config.LoadController(nil, lookupIn(management(nil)), nil)
	if err != nil || !reflect.DeepEqual(ctl.NodeGroups, want) {
		t.Fatalf("controller = %+v (%v)", ctl.NodeGroups, err)
	}
}

// The YAML section, with real lists, is the primary interface; the server
// and the controller read the same shape into the same struct.
func TestNodeGroupsSectionWithLists(t *testing.T) {
	file := loadSection(t, `
nodeGroups:
  system:
    instanceTypes: [t3.xlarge]
    minSize: 3
    maxSize: 8
    capacityType: SPOT
    amiType: BOTTLEROCKET_x86_64
    diskSizeGiB: 50
`)
	want := config.NodeGroups{
		Platform: config.DefaultNodeGroups().Platform,
		System: config.SystemNodeGroup{InstanceTypes: []string{"t3.xlarge"}, MinSize: 3, CapacityType: "SPOT",
			AMIType: "BOTTLEROCKET_x86_64", DiskSizeGiB: 50},
		Warnings: serveWarnings(t, file),
	}
	serve, err := config.LoadServe(nil, nil, file)
	if err != nil || !reflect.DeepEqual(serve.NodeGroups, want) {
		t.Fatalf("serve = %+v (%v)", serve.NodeGroups, err)
	}
	ctl, err := config.LoadController(nil, lookupIn(management(nil)), file)
	if err != nil || !reflect.DeepEqual(ctl.NodeGroups, want) {
		t.Fatalf("controller = %+v (%v)", ctl.NodeGroups, err)
	}
}

// A flag beats the environment, which beats the section.
func TestNodeGroupsFlagAndEnvOverrideTheSection(t *testing.T) {
	file := loadSection(t, "nodeGroups:\n  system:\n    minSize: 4\n    amiType: AL2023_x86_64_STANDARD\n")
	env := lookupIn(map[string]string{config.EnvSystemNodeGroupMinSize: "5", config.EnvSystemNodeGroupAMIType: "BOTTLEROCKET_x86_64"})
	serve, err := config.LoadServe(nil, env, file)
	if err != nil || serve.NodeGroups.System.MinSize != 5 || serve.NodeGroups.System.AMIType != "BOTTLEROCKET_x86_64" {
		t.Fatalf("env: %+v (%v)", serve.NodeGroups, err)
	}
	flags := lookupIn(map[string]string{config.FlagSystemNodeGroupMinSize: "3", config.FlagSystemNodeGroupMaxSize: "4"})
	serve, err = config.LoadServe(flags, env, file)
	if err != nil || serve.NodeGroups.System.MinSize != 3 {
		t.Fatalf("flag: %+v (%v)", serve.NodeGroups, err)
	}
}

// The system group's AMI type is one of the four Linux ones Karpenter and
// the EKS add-ons run on, of the instance types' architecture.
func TestSystemNodeGroupAMIType(t *testing.T) {
	for _, tt := range []struct {
		ami, types string
		ok         bool
	}{
		{"AL2023_x86_64_STANDARD", "t3.medium", true},
		{"BOTTLEROCKET_x86_64", "m7i.large", true},
		{"AL2023_ARM_64_STANDARD", "m7g.large", true},
		{"BOTTLEROCKET_ARM_64", "t4g.medium,m7g.large", true},
		{"AL2023_ARM_64_STANDARD", "t3.medium", false},
		{"BOTTLEROCKET_x86_64", "m7g.large", false},
		{"AL2023_x86_64_NVIDIA", "g5.xlarge", false},
		{"WINDOWS_CORE_2022_x86_64", "t3.medium", false},
		{"CUSTOM", "t3.medium", false},
		{"al2023_x86_64_standard", "t3.medium", false},
	} {
		env := map[string]string{config.EnvSystemNodeGroupAMIType: tt.ami, config.EnvSystemInstanceTypes: tt.types}
		serve, serveErr := config.LoadServe(nil, lookupIn(env), nil)
		_, ctlErr := config.LoadController(nil, lookupIn(management(env)), nil)
		if tt.ok {
			if serveErr != nil || ctlErr != nil || serve.NodeGroups.System.AMIType != tt.ami {
				t.Errorf("%s on %s: %+v (%v, %v)", tt.ami, tt.types, serve.NodeGroups.System, serveErr, ctlErr)
			}
			continue
		}
		for who, err := range map[string]error{"serve": serveErr, "controller": ctlErr} {
			if err == nil || !strings.Contains(err.Error(), "ami-type") {
				t.Errorf("%s on %s (%s): err = %v, want a refusal of the AMI type", tt.ami, tt.types, who, err)
			}
		}
	}
}

// The tenant node group settings of an older config (the karpenter section,
// its flat keys and variables) still load, change nothing and earn one
// warning.
func TestOldKarpenterSettingsAreIgnoredWithAWarning(t *testing.T) {
	for name, tt := range map[string]struct {
		file string
		env  map[string]string
	}{
		"section": {file: "nodeGroups:\n  karpenter:\n    instanceFamilies: [t3, t3a]\n    instanceTypes: [m7i.large]\n" +
			"    defaults:\n      architectures: [AMD64, ARM64]\n      capacityTypes: [ON_DEMAND, SPOT]\n"},
		"flat keys": {file: "node-group-instance-families: m7i,c7i\nnode-group-instance-types: r7i.large\n"},
		"variables": {env: map[string]string{config.EnvNodeGroupInstanceFamilies: "c7i", config.EnvNodeGroupInstanceTypes: "m7i.large",
			config.EnvNodeGroupDefaultArchitectures: "ARM64", config.EnvNodeGroupDefaultCapacityTypes: "SPOT"}},
	} {
		var file config.LookupFunc
		if tt.file != "" {
			file = loadSection(t, tt.file)
		}
		want := config.DefaultNodeGroups()
		serve, err := config.LoadServe(nil, lookupIn(tt.env), file)
		if err != nil || len(serve.NodeGroups.Warnings) != 1 || !strings.Contains(serve.NodeGroups.Warnings[0], "nodeGroups.karpenter is ignored") {
			t.Fatalf("%s: warnings %v (%v)", name, serve.NodeGroups.Warnings, err)
		}
		serve.NodeGroups.Warnings = nil
		if !reflect.DeepEqual(serve.NodeGroups, want) {
			t.Errorf("%s: %+v, want the defaults", name, serve.NodeGroups)
		}
		ctl, err := config.LoadController(nil, lookupIn(management(tt.env)), file)
		if err != nil || len(ctl.NodeGroups.Warnings) != 1 {
			t.Errorf("%s: controller warnings %v (%v)", name, ctl.NodeGroups.Warnings, err)
		}
	}
}

func TestNodeGroupsValidationErrors(t *testing.T) {
	for name, tt := range map[string]struct {
		env  map[string]string
		want string
	}{
		"min below two":       {map[string]string{config.EnvSystemNodeGroupMinSize: "1"}, "at least 2"},
		"min not a number":    {map[string]string{config.EnvSystemNodeGroupMinSize: "two"}, "not a number"},
		"size below two":      {map[string]string{config.EnvSystemNodeGroupSize: "1"}, "at least 2"},
		"size not a number":   {map[string]string{config.EnvSystemNodeGroupSize: "two"}, "not a number"},
		"size and min differ": {map[string]string{config.EnvSystemNodeGroupSize: "3", config.EnvSystemNodeGroupMinSize: "4"}, "different"},
		"empty instance type": {map[string]string{config.EnvSystemInstanceTypes: " "}, "at least one instance type"},
		"type without a dot":  {map[string]string{config.EnvSystemInstanceTypes: "t3large"}, "instance type"},
		"bad capacity type":   {map[string]string{config.EnvSystemNodeGroupCapacityType: "reserved"}, "ON_DEMAND, SPOT"},
		"negative disk":       {map[string]string{config.EnvSystemNodeGroupDiskSizeGiB: "-1"}, "0 to"},
	} {
		_, serveErr := config.LoadServe(nil, lookupIn(tt.env), nil)
		_, ctlErr := config.LoadController(nil, lookupIn(management(tt.env)), nil)
		for who, err := range map[string]error{"serve": serveErr, "controller": ctlErr} {
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("%s (%s): err = %v, want it to mention %q", name, who, err, tt.want)
			}
		}
	}
}

func TestNodeGroupsSectionErrors(t *testing.T) {
	for name, yaml := range map[string]string{
		"unknown key":       "nodeGroups:\n  system:\n    colour: red\n",
		"scalar section":    "nodeGroups: x\n",
		"list for a scalar": "nodeGroups:\n  system:\n    size: [2]\n",
		"null value":        "nodeGroups:\n  system:\n    size:\n",
		"nested list item":  "nodeGroups:\n  system:\n    instanceTypes: [[t3.medium]]\n",
		"set twice":         "nodeGroups:\n  system:\n    size: 3\nsystem-node-group-size: 4\n",
	} {
		path := t.TempDir() + "/c.yaml"
		if err := writeFile(path, yaml); err != nil {
			t.Fatal(err)
		}
		if _, err := config.LoadFile(path); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func writeFile(path, content string) error { return os.WriteFile(path, []byte(content), 0o600) }

// size is the old fixed-size setting: it still works, as minSize, the
// static size. maxSize is ignored, whatever its value.
func TestNodeGroupsSizeIsAnAliasOfTheMinimum(t *testing.T) {
	for name, tt := range map[string]struct {
		file string
		env  map[string]string
		min  int
	}{
		"section":           {file: "nodeGroups:\n  system:\n    size: 3\n", min: 3},
		"large size":        {file: "nodeGroups:\n  system:\n    size: 8\n", min: 8},
		"size and max":      {file: "nodeGroups:\n  system:\n    size: 3\n    maxSize: 5\n", min: 3},
		"max below min":     {file: "nodeGroups:\n  system:\n    minSize: 4\n    maxSize: 1\n", min: 4},
		"max not a number":  {env: map[string]string{config.EnvSystemNodeGroupMaxSize: "many"}, min: 3},
		"env":               {env: map[string]string{config.EnvSystemNodeGroupSize: "4"}, min: 4},
		"same size and min": {env: map[string]string{config.EnvSystemNodeGroupSize: "4", config.EnvSystemNodeGroupMinSize: "4"}, min: 4},
		"min only":          {env: map[string]string{config.EnvSystemNodeGroupMinSize: "7"}, min: 7},
	} {
		var file config.LookupFunc
		if tt.file != "" {
			file = loadSection(t, tt.file)
		}
		serve, err := config.LoadServe(nil, lookupIn(tt.env), file)
		if err != nil || serve.NodeGroups.System.MinSize != tt.min {
			t.Errorf("%s: %+v (%v), want size %d", name, serve.NodeGroups.System, err, tt.min)
		}
	}
}

// serveWarnings are the warnings the file earns: the test names the keys
// that cause them, not their text.
func serveWarnings(t *testing.T, file config.LookupFunc) []string {
	t.Helper()
	serve, err := config.LoadServe(nil, nil, file)
	if err != nil || len(serve.NodeGroups.Warnings) != 1 || !strings.Contains(serve.NodeGroups.Warnings[0], "nodeGroups.system.maxSize") {
		t.Fatalf("warnings = %v (%v)", serve.NodeGroups.Warnings, err)
	}
	return serve.NodeGroups.Warnings
}
