package cmd

import (
	"strings"
	"testing"
)

func TestTenantCommandSurface(t *testing.T) {
	command := newTenantCmd()
	for _, path := range [][]string{{"create"}, {"list"}, {"delete"}, {"freeze"}, {"freeze", "status"}, {"unfreeze"}} {
		child, _, err := command.Find(path)
		if err != nil {
			t.Fatal(err)
		}
		if child.Name() != path[len(path)-1] {
			t.Fatalf("missing command %v", path)
		}
		if child.Flags().Lookup("platform") != nil || child.Flags().Lookup("reset-exceptions") != nil {
			t.Fatal("tenant command exposes operator options")
		}
	}
	admin := newAdminCmd()
	freeze, _, err := admin.Find([]string{"freeze"})
	if err != nil {
		t.Fatal(err)
	}
	if freeze.Flags().Lookup("platform") == nil || freeze.Flags().Lookup("reset-exceptions") == nil {
		t.Fatal("operator compatibility removed")
	}
}

// The AdminService groups take no tenant: neither admin nor its release and
// channel subgroups show --tenant.
func TestAdminGroupsHaveNoTenantFlag(t *testing.T) {
	admin := newAdminCmd()
	for _, path := range [][]string{{"freeze"}, {"release", "list"}, {"release", "get"}, {"channel", "list"}, {"channel", "get"}} {
		child, _, err := admin.Find(path)
		if err != nil {
			t.Fatal(err)
		}
		if child.Flag("tenant") != nil && path[0] != "freeze" {
			t.Errorf("admin %v has --tenant", path)
		}
	}
}

// cluster nodes takes the cluster ID and has no --options.
func TestClusterNodesHelp(t *testing.T) {
	nodes, _, err := newClusterCmd().Find([]string{"nodes"})
	if err != nil {
		t.Fatal(err)
	}
	if nodes.Flags().Lookup("options") != nil || strings.Contains(nodes.Short, "--options") {
		t.Errorf("nodes still mentions --options: %q", nodes.Short)
	}
	// "nodes <id>": the ID is --id (or the argument), required when run.
	if nodes.Flags().Lookup("id") == nil || nodes.Use != "nodes" {
		t.Errorf("nodes takes the cluster ID as --id: use %q", nodes.Use)
	}
}
