package config_test

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/yindia/cluster-vending/pkg/config"
)

// writeFile writes a config file in a temporary directory.
func writeSharedFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "platform.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const sharedNodeGroups = `
aws-management-role-arn: arn:aws:iam::111122223333:role/r
nodeGroups:
  system:
    instanceType: m7i.large
    minSize: 3
    maxSize: 9
    capacityType: SPOT
    amiType: BOTTLEROCKET_x86_64
    diskSizeGiB: 80
`

// One file read by both commands gives the same node group layout.
func TestServeAndControllerReadTheSameNodeGroupsFromOneFile(t *testing.T) {
	path := writeSharedFile(t, sharedNodeGroups)
	file, err := config.FileFrom(nil, lookupIn(map[string]string{config.EnvConfig: path}))
	if err != nil || file == nil {
		t.Fatalf("FileFrom: %v", err)
	}
	serve, err := config.LoadServe(nil, lookupIn(nil), file)
	if err != nil {
		t.Fatal(err)
	}
	controller, err := config.LoadController(nil, lookupIn(nil), file)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(serve.NodeGroups, controller.NodeGroups) {
		t.Errorf("serve %+v != controller %+v", serve.NodeGroups, controller.NodeGroups)
	}
	if got := serve.NodeGroups.System; got.MinSize != 3 || !reflect.DeepEqual(got.InstanceTypes, []string{"m7i.large"}) {
		t.Errorf("system = %+v", got)
	}
}

// An invalid file is refused by both commands with the same problems.
func TestServeAndControllerRejectAnInvalidFileIdentically(t *testing.T) {
	cases := map[string]string{
		"values":            "nodeGroups:\n  system:\n    minSize: 1\n    maxSize: 0\n    capacityType: LUXURY\n    amiType: CUSTOM\n",
		"unknown key":       "nodeGroups:\n  system:\n    colour: red\n",
		"not a mapping":     "nodeGroups: [a]\n",
		"list for a scalar": "nodeGroups:\n  system:\n    minSize: [2]\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeSharedFile(t, body+"aws-management-role-arn: arn:aws:iam::111122223333:role/r\n")
			file, fileErr := config.LoadFile(path)
			if fileErr != nil {
				// Rejected while reading: both commands share that step.
				return
			}
			_, serveErr := config.LoadServe(nil, lookupIn(nil), file)
			_, controllerErr := config.LoadController(nil, lookupIn(nil), file)
			if serveErr == nil || controllerErr == nil {
				t.Fatalf("serve %v, controller %v: both must reject", serveErr, controllerErr)
			}
			if nodeGroupProblems(serveErr) != nodeGroupProblems(controllerErr) {
				t.Errorf("different problems:\nserve:      %v\ncontroller: %v", serveErr, controllerErr)
			}
		})
	}
}

// nodeGroupProblems are the lines of an error that concern node groups
// (the other settings differ per command).
func nodeGroupProblems(err error) string {
	var out []string
	for _, line := range strings.Split(err.Error(), "\n") {
		if strings.Contains(line, "node-group") || strings.Contains(line, "node group") {
			out = append(out, strings.TrimSpace(line))
		}
	}
	return strings.Join(out, "\n")
}

// A variable that still sets a node group key is named in a warning; the
// file's keys and unrelated variables are not.
func TestLeftoverNodeGroupVariables(t *testing.T) {
	env := lookupIn(map[string]string{
		config.EnvSystemNodeGroupMaxSize:    "8",
		config.EnvNodeGroupInstanceFamilies: "t3",
		config.EnvHTTPAddr:                  ":8080",
	})
	got := config.LeftoverNodeGroupVariables(env)
	want := []string{config.EnvSystemNodeGroupMaxSize, config.EnvNodeGroupInstanceFamilies}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if left := config.LeftoverNodeGroupVariables(lookupIn(nil)); len(left) != 0 {
		t.Errorf("nothing set, got %v", left)
	}
	if left := config.LeftoverNodeGroupVariables(nil); len(left) != 0 {
		t.Errorf("nil lookup, got %v", left)
	}
}

// A leftover variable still wins over the file: the warning is about that.
func TestLeftoverVariableStillOverridesTheFile(t *testing.T) {
	file, err := config.LoadFile(writeSharedFile(t, sharedNodeGroups))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadController(nil, lookupIn(map[string]string{config.EnvSystemNodeGroupMinSize: "12"}), file)
	if err != nil || cfg.NodeGroups.System.MinSize != 12 {
		t.Errorf("size = %d (%v)", cfg.NodeGroups.System.MinSize, err)
	}
}

// The three config files that carry the nodeGroups section (the deployed
// one, the local run's and the auth overlay's) have the same section, which
// loads as the defaults without a warning.
func TestShippedConfigFilesHaveOneNodeGroupsSection(t *testing.T) {
	var sections []string
	for _, path := range []string{"../../config/node-groups/platform.yaml", "../../hack/config.local.yaml", "../../config/server/auth/platform.yaml"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		sections = append(sections, nodeGroupsSectionOf(string(raw)))
		file, err := config.LoadFile(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		controller, err := config.LoadController(nil, lookupIn(management(nil)), file)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if !reflect.DeepEqual(controller.NodeGroups, config.DefaultNodeGroups()) {
			t.Errorf("%s: nodeGroups = %+v (warnings %v), want the defaults without a warning", path, controller.NodeGroups, controller.NodeGroups.Warnings)
		}
	}
	for i, s := range sections {
		if s == "" || s != sections[0] {
			t.Errorf("file %d's nodeGroups section differs from the first:\n%s", i, s)
		}
	}
}

// nodeGroupsSectionOf is the text of a file's nodeGroups section: its line
// and the indented or comment lines after it.
func nodeGroupsSectionOf(body string) string {
	var out []string
	in := false
	for _, line := range strings.Split(body, "\n") {
		switch {
		case line == "nodeGroups:":
			in = true
		case in && line != "" && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "#"):
			in = false
		}
		if in {
			out = append(out, line)
		}
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}
