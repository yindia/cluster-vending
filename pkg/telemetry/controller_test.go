package telemetry

import (
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestControllerCountsEventsWithBoundedLabels(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	c := NewController(reg)
	c.Event("Cluster", "Warning", "TeardownTimedOut")
	c.Event("Cluster", "Warning", "TeardownTimedOut")
	c.Event("Secret", "Bogus", "cluster prod failed: boom")
	if got := value(t, reg, "events_total", map[string]string{"kind": "cluster", "type": "warning", "reason": "TeardownTimedOut"}); got != 2 {
		t.Fatalf("teardown timeouts %v", got)
	}
	if got := value(t, reg, "events_total", map[string]string{"kind": "other", "type": "unknown", "reason": "other"}); got != 1 {
		t.Fatalf("unknown event %v", got)
	}
	for i := range maxReasons + 10 {
		c.Event("Network", "Normal", fmt.Sprintf("Reason%c%c", 'A'+i/26%26, 'a'+i%26))
	}
	if n := testutil.CollectAndCount(c.events); n > maxReasons+2 {
		t.Fatalf("%d event series past the bound", n)
	}
}

func TestControllerObservesTransitionDurations(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	c := NewController(reg)
	c.ClusterProvisioned(20 * time.Minute)
	c.UpgradeFinished(time.Hour)
	c.ComponentReady("karpenter", 2*time.Minute)
	c.ComponentReady("Not A Name", time.Minute)
	c.ResultWriteFailed("deprecations")
	c.ResultWriteFailed("../etc")
	checks := []struct {
		name   string
		labels map[string]string
	}{
		{"cluster_provisioning_seconds", nil},
		{"upgrade_duration_seconds", nil},
		{"component_install_seconds", map[string]string{"component": "karpenter"}},
		{"component_install_seconds", map[string]string{"component": "other"}},
		{"results_write_failures_total", map[string]string{"kind": "deprecations"}},
		{"results_write_failures_total", map[string]string{"kind": "other"}},
	}
	for _, check := range checks {
		if got := value(t, reg, check.name, check.labels); got != 1 {
			t.Errorf("%s%v = %v", check.name, check.labels, got)
		}
	}
	if got := value(t, reg, "results_write_failures_total", map[string]string{"kind": "nodes"}); got != 0 {
		t.Fatalf("zero series %v", got)
	}
}
