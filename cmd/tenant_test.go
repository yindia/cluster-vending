package cmd

import "testing"

func TestTenantCommandSurface(t *testing.T) {
	command := newTenantCmd()
	for _, path := range [][]string{{"create"}, {"list"}, {"freeze"}, {"freeze", "status"}, {"unfreeze"}} {
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
