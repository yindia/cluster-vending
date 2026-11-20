//go:build e2e

package e2e

import "testing"

// TestE2E runs the scenarios in order against the sandbox account. They
// share what they create (the account, the network, the cluster), so they
// are subtests of one test: `-run 'TestE2E/03_cluster_create'` runs one,
// which skips when what it needs was not created before it.
//
// Every scenario that creates something registers its deletion on this test,
// so it runs when all scenarios are done (last created, first deleted) and
// verifies that the deletion completed. The leak check is registered first
// and so runs last.
func TestE2E(t *testing.T) {
	e := newEnv(t)
	e.parent = t
	t.Logf("run %s: server %s, tenant %s, account %s, region %s, role %s, management context %q",
		e.runID, e.cfg.Server, e.cfg.Tenant, e.cfg.AccountID, e.cfg.Region, e.cfg.Role, e.cfg.KubeContext)

	t.Cleanup(func() { e.leakCheck(t) })

	for _, s := range []struct {
		name string
		run  func(*testing.T)
	}{
		{"01_account_onboarding", e.scenarioAccount},
		{"02_network_create", e.scenarioNetworkCreate},
		{"03_cluster_create", e.scenarioClusterCreate},
		{"04_cluster_update", e.scenarioClusterUpdate},
		{"05_cluster_upgrade", e.scenarioClusterUpgrade},
		{"06_scanners", e.scenarioScanners},
		{"07_cluster_delete", e.scenarioClusterDelete},
		{"08_import_takeover", e.scenarioImport},
		{"09_network_delete", e.scenarioNetworkDelete},
	} {
		t.Run(s.name, s.run)
	}
}
