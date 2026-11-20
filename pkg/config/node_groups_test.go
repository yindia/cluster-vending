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

func TestNodeGroupsDefaultsAreT3AndTwoToSixNodes(t *testing.T) {
	want := config.NodeGroups{
		System: config.SystemNodeGroup{InstanceType: "t3.medium", MinSize: 2, MaxSize: 6, CapacityType: "ON_DEMAND"},
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
    minSize: 3
    maxSize: 8
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
		System: config.SystemNodeGroup{InstanceType: "t3.xlarge", MinSize: 3, MaxSize: 8, CapacityType: "SPOT", DiskSizeGiB: 50},
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
	file := loadSection(t, "nodeGroups:\n  system:\n    minSize: 4\n    maxSize: 9\n  karpenter:\n    instanceFamilies: [t3]\n")
	env := lookupIn(map[string]string{config.EnvSystemNodeGroupMinSize: "5", config.EnvNodeGroupInstanceFamilies: " c7i, m7i,c7i"})
	serve, err := config.LoadServe(nil, env, file)
	if err != nil || serve.NodeGroups.System.MinSize != 5 || serve.NodeGroups.System.MaxSize != 9 || !reflect.DeepEqual(serve.NodeGroups.Karpenter.InstanceFamilies, []string{"c7i", "m7i"}) {
		t.Fatalf("env: %+v (%v)", serve.NodeGroups, err)
	}
	flags := lookupIn(map[string]string{config.FlagSystemNodeGroupMinSize: "3", config.FlagSystemNodeGroupMaxSize: "4"})
	serve, err = config.LoadServe(flags, env, file)
	if err != nil || serve.NodeGroups.System.MinSize != 3 || serve.NodeGroups.System.MaxSize != 4 {
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
		"min below two":         {map[string]string{config.EnvSystemNodeGroupMinSize: "1"}, "at least 2"},
		"min not a number":      {map[string]string{config.EnvSystemNodeGroupMinSize: "two"}, "not a number"},
		"max not a number":      {map[string]string{config.EnvSystemNodeGroupMaxSize: "many"}, "not a number"},
		"max below min":         {map[string]string{config.EnvSystemNodeGroupMinSize: "4", config.EnvSystemNodeGroupMaxSize: "3"}, "at least the minimum"},
		"max below default min": {map[string]string{config.EnvSystemNodeGroupMaxSize: "1"}, "at least the minimum"},
		"size below two":        {map[string]string{config.EnvSystemNodeGroupSize: "1"}, "at least 2"},
		"size not a number":     {map[string]string{config.EnvSystemNodeGroupSize: "two"}, "not a number"},
		"size and min differ":   {map[string]string{config.EnvSystemNodeGroupSize: "3", config.EnvSystemNodeGroupMinSize: "4"}, "different"},
		"empty instance type":   {map[string]string{config.EnvSystemNodeGroupInstanceType: " "}, "instance type"},
		"type without a dot":    {map[string]string{config.EnvSystemNodeGroupInstanceType: "t3large"}, "instance type"},
		"bad capacity type":     {map[string]string{config.EnvSystemNodeGroupCapacityType: "reserved"}, "ON_DEMAND, SPOT"},
		"negative disk":         {map[string]string{config.EnvSystemNodeGroupDiskSizeGiB: "-1"}, "0 to"},
		"family with a dot":     {map[string]string{config.EnvNodeGroupInstanceFamilies: "m7i.large"}, "instance family"},
		"family uppercase":      {map[string]string{config.EnvNodeGroupInstanceFamilies: "M7i"}, "instance family"},
		"empty item":            {map[string]string{config.EnvNodeGroupInstanceFamilies: "m7i,,c7i"}, "instance family"},
		"type without dot":      {map[string]string{config.EnvNodeGroupInstanceTypes: "m7i"}, "instance type"},
		"bad architecture":      {map[string]string{config.EnvNodeGroupDefaultArchitectures: "x86"}, "AMD64, ARM64"},
		"bad default capacity":  {map[string]string{config.EnvNodeGroupDefaultCapacityTypes: "ON_DEMAND,free"}, "ON_DEMAND, SPOT"},
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

// size is the old fixed-size setting: it still works, as the minimum (and so
// the size at creation). The maximum is the default one unless it is lower
// than the minimum or set.
func TestNodeGroupsSizeIsAnAliasOfTheMinimum(t *testing.T) {
	for name, tt := range map[string]struct {
		file     string
		env      map[string]string
		min, max int
	}{
		"section":           {file: "nodeGroups:\n  system:\n    size: 3\n", min: 3, max: 6},
		"large size":        {file: "nodeGroups:\n  system:\n    size: 8\n", min: 8, max: 8},
		"size and max":      {file: "nodeGroups:\n  system:\n    size: 3\n    maxSize: 5\n", min: 3, max: 5},
		"env":               {env: map[string]string{config.EnvSystemNodeGroupSize: "4"}, min: 4, max: 6},
		"same size and min": {env: map[string]string{config.EnvSystemNodeGroupSize: "4", config.EnvSystemNodeGroupMinSize: "4"}, min: 4, max: 6},
		"min only":          {env: map[string]string{config.EnvSystemNodeGroupMinSize: "7"}, min: 7, max: 7},
	} {
		var file config.LookupFunc
		if tt.file != "" {
			file = loadSection(t, tt.file)
		}
		serve, err := config.LoadServe(nil, lookupIn(tt.env), file)
		if err != nil || serve.NodeGroups.System.MinSize != tt.min || serve.NodeGroups.System.MaxSize != tt.max {
			t.Errorf("%s: %+v (%v), want %d..%d", name, serve.NodeGroups.System, err, tt.min, tt.max)
		}
	}
}
