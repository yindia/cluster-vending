package config_test

import (
	"os"
	"reflect"
	"testing"

	"github.com/yindia/cluster-vending/pkg/config"
)

func TestNodeGroupAllowlistDefaultsToNoRestriction(t *testing.T) {
	serve, err := config.LoadServe(nil, nil, nil)
	if err != nil || len(serve.NodeGroups.InstanceFamilies) != 0 || len(serve.NodeGroups.InstanceTypes) != 0 {
		t.Fatalf("serve = %+v (%v)", serve.NodeGroups, err)
	}
	ctl, err := config.LoadController(nil, lookupIn(management(nil)), nil)
	if err != nil || len(ctl.NodeGroups.InstanceFamilies) != 0 {
		t.Fatalf("controller = %+v (%v)", ctl.NodeGroups, err)
	}
}

func TestNodeGroupAllowlistFlagEnvAndFile(t *testing.T) {
	want := config.NodeGroupAllowlist{InstanceFamilies: []string{"m7i", "c7i"}, InstanceTypes: []string{"r7i.large"}}
	// env: spaces around items and repeats are tolerated.
	env := lookupIn(map[string]string{config.EnvNodeGroupInstanceFamilies: " m7i, c7i,m7i", config.EnvNodeGroupInstanceTypes: "r7i.large"})
	serve, err := config.LoadServe(nil, env, nil)
	if err != nil || !reflect.DeepEqual(serve.NodeGroups, want) {
		t.Fatalf("serve env = %+v (%v)", serve.NodeGroups, err)
	}
	// flag beats env.
	flags := lookupIn(map[string]string{config.FlagNodeGroupInstanceFamilies: "c7g"})
	ctl, err := config.LoadController(flags, lookupIn(management(map[string]string{config.EnvNodeGroupInstanceFamilies: "m7i"})), nil)
	if err != nil || !reflect.DeepEqual(ctl.NodeGroups.InstanceFamilies, []string{"c7g"}) {
		t.Fatalf("controller flag = %+v (%v)", ctl.NodeGroups, err)
	}
	// file.
	file := lookupIn(map[string]string{config.FlagNodeGroupInstanceFamilies: "m7i,c7i", config.FlagNodeGroupInstanceTypes: "r7i.large"})
	serve, err = config.LoadServe(nil, nil, file)
	if err != nil || !reflect.DeepEqual(serve.NodeGroups, want) {
		t.Fatalf("serve file = %+v (%v)", serve.NodeGroups, err)
	}
}

func TestNodeGroupAllowlistRejectsMalformedItems(t *testing.T) {
	for name, kv := range map[string]map[string]string{
		"family with a dot":  {config.EnvNodeGroupInstanceFamilies: "m7i.large"},
		"family uppercase":   {config.EnvNodeGroupInstanceFamilies: "M7i"},
		"empty item":         {config.EnvNodeGroupInstanceFamilies: "m7i,,c7i"},
		"type without a dot": {config.EnvNodeGroupInstanceTypes: "m7i"},
	} {
		if _, err := config.LoadServe(nil, lookupIn(kv), nil); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestNodeGroupAllowlistInConfigFile(t *testing.T) {
	path := t.TempDir() + "/c.yaml"
	if err := writeFile(path, "node-group-instance-families: m7i,c7i\n"); err != nil {
		t.Fatal(err)
	}
	file, err := config.LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	serve, err := config.LoadServe(nil, nil, file)
	if err != nil || !reflect.DeepEqual(serve.NodeGroups.InstanceFamilies, []string{"m7i", "c7i"}) {
		t.Fatalf("%+v (%v)", serve.NodeGroups, err)
	}
}

func writeFile(path, content string) error { return os.WriteFile(path, []byte(content), 0o600) }
