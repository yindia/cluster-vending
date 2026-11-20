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

func TestNodeGroupsDefaultsAreT3AndTwoNodes(t *testing.T) {
	want := config.NodeGroups{
		System: config.SystemNodeGroup{InstanceType: "t3.medium", Size: 2, CapacityType: "ON_DEMAND"},
		Karpenter: config.KarpenterNodeGroups{InstanceFamilies: []string{"t3"},
			Defaults: config.NodeGroupDefaults{Architectures: []string{"AMD64"}, CapacityTypes: []string{"ON_DEMAND"}}},
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
    instanceType: t3.xlarge
    size: 4
    capacityType: SPOT
    diskSizeGiB: 50
  karpenter:
    instanceFamilies: [t3, t3a]
    instanceTypes:
      - m7i.large
    defaults:
      architectures: [AMD64, ARM64]
      capacityTypes: [ON_DEMAND, SPOT]
`)
	want := config.NodeGroups{
		System: config.SystemNodeGroup{InstanceType: "t3.xlarge", Size: 4, CapacityType: "SPOT", DiskSizeGiB: 50},
		Karpenter: config.KarpenterNodeGroups{InstanceFamilies: []string{"t3", "t3a"}, InstanceTypes: []string{"m7i.large"},
			Defaults: config.NodeGroupDefaults{Architectures: []string{"AMD64", "ARM64"}, CapacityTypes: []string{"ON_DEMAND", "SPOT"}}},
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
	file := loadSection(t, "nodeGroups:\n  system:\n    size: 4\n  karpenter:\n    instanceFamilies: [t3]\n")
	env := lookupIn(map[string]string{config.EnvSystemNodeGroupSize: "5", config.EnvNodeGroupInstanceFamilies: " c7i, m7i,c7i"})
	serve, err := config.LoadServe(nil, env, file)
	if err != nil || serve.NodeGroups.System.Size != 5 || !reflect.DeepEqual(serve.NodeGroups.Karpenter.InstanceFamilies, []string{"c7i", "m7i"}) {
		t.Fatalf("env: %+v (%v)", serve.NodeGroups, err)
	}
	flags := lookupIn(map[string]string{config.FlagSystemNodeGroupSize: "3"})
	serve, err = config.LoadServe(flags, env, file)
	if err != nil || serve.NodeGroups.System.Size != 3 {
		t.Fatalf("flag: %+v (%v)", serve.NodeGroups, err)
	}
}

// The setting names that shipped before the section still work.
func TestNodeGroupsOldFlatKeysStillWork(t *testing.T) {
	file := loadSection(t, "node-group-instance-families: m7i,c7i\nnode-group-instance-types: r7i.large\n")
	serve, err := config.LoadServe(nil, nil, file)
	want := config.KarpenterNodeGroups{InstanceFamilies: []string{"m7i", "c7i"}, InstanceTypes: []string{"r7i.large"},
		Defaults: config.DefaultNodeGroups().Karpenter.Defaults}
	if err != nil || !reflect.DeepEqual(serve.NodeGroups.Karpenter, want) {
		t.Fatalf("%+v (%v)", serve.NodeGroups.Karpenter, err)
	}
}

// An empty list (or "none") lifts the restriction instead of taking the
// default.
func TestNodeGroupsEmptyListLiftsTheRestriction(t *testing.T) {
	serve, err := config.LoadServe(nil, nil, loadSection(t, "nodeGroups:\n  karpenter:\n    instanceFamilies: []\n"))
	if err != nil || serve.NodeGroups.Karpenter.Restricted() {
		t.Fatalf("%+v (%v)", serve.NodeGroups.Karpenter, err)
	}
	serve, err = config.LoadServe(nil, lookupIn(map[string]string{config.EnvNodeGroupInstanceFamilies: "none"}), nil)
	if err != nil || serve.NodeGroups.Karpenter.Restricted() {
		t.Fatalf("env none: %+v (%v)", serve.NodeGroups.Karpenter, err)
	}
}

func TestNodeGroupsValidationErrors(t *testing.T) {
	for name, tt := range map[string]struct {
		env  map[string]string
		want string
	}{
		"size below two":       {map[string]string{config.EnvSystemNodeGroupSize: "1"}, "at least 2"},
		"size not a number":    {map[string]string{config.EnvSystemNodeGroupSize: "two"}, "not a number"},
		"empty instance type":  {map[string]string{config.EnvSystemNodeGroupInstanceType: " "}, "instance type"},
		"type without a dot":   {map[string]string{config.EnvSystemNodeGroupInstanceType: "t3large"}, "instance type"},
		"bad capacity type":    {map[string]string{config.EnvSystemNodeGroupCapacityType: "reserved"}, "ON_DEMAND, SPOT"},
		"negative disk":        {map[string]string{config.EnvSystemNodeGroupDiskSizeGiB: "-1"}, "0 to"},
		"family with a dot":    {map[string]string{config.EnvNodeGroupInstanceFamilies: "m7i.large"}, "instance family"},
		"family uppercase":     {map[string]string{config.EnvNodeGroupInstanceFamilies: "M7i"}, "instance family"},
		"empty item":           {map[string]string{config.EnvNodeGroupInstanceFamilies: "m7i,,c7i"}, "instance family"},
		"type without dot":     {map[string]string{config.EnvNodeGroupInstanceTypes: "m7i"}, "instance type"},
		"bad architecture":     {map[string]string{config.EnvNodeGroupDefaultArchitectures: "x86"}, "AMD64, ARM64"},
		"bad default capacity": {map[string]string{config.EnvNodeGroupDefaultCapacityTypes: "ON_DEMAND,free"}, "ON_DEMAND, SPOT"},
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
		"nested list item":  "nodeGroups:\n  karpenter:\n    instanceFamilies: [[t3]]\n",
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
