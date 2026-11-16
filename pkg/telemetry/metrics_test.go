package telemetry

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
	v1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

func value(t *testing.T, reg prometheus.Gatherer, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != "cluster_vending_"+name {
			continue
		}
		for _, metric := range family.Metric {
			match := true
			for key, want := range labels {
				found := false
				for _, pair := range metric.Label {
					if pair.GetName() == key && pair.GetValue() == want {
						found = true
					}
				}
				match = match && found
			}
			if match {
				if metric.Counter != nil {
					return metric.Counter.GetValue()
				}
				if metric.Histogram != nil {
					return float64(metric.Histogram.GetSampleCount())
				}
				return metric.GetGauge().GetValue()
			}
		}
	}
	t.Fatalf("missing %s %v", name, labels)
	return 0
}

func TestRPCMeasuresStreamingLifetimeAndNormalizedErrors(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	r := NewRPC(reg)
	method := v1.File_cloud_v1_cluster_service_proto.Services().ByName("ClusterService").Methods().ByName("GetCluster")
	if method == nil {
		t.Fatal("missing RPC schema")
	}
	started, finish, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	handler := r.Interceptor(func(context.Context, connect.Spec, connect.ServerStream) error {
		close(started)
		<-finish
		return connect.NewError(connect.CodeCanceled, "client disconnected")
	})
	go func() {
		done <- handler(t.Context(), connect.Spec{Schema: method, StreamType: connect.StreamTypeServer}, nil)
	}()
	<-started
	labels := map[string]string{"rpc_service": "cloud.v1.ClusterService", "rpc_method": "GetCluster", "rpc_type": "server"}
	if got := value(t, reg, "rpc_in_flight", labels); got != 1 {
		t.Fatalf("in flight %v", got)
	}
	close(finish)
	if connect.CodeOf(<-done) != connect.CodeCanceled {
		t.Fatal("handler error changed")
	}
	if got := value(t, reg, "rpc_in_flight", labels); got != 0 {
		t.Fatalf("in flight after close %v", got)
	}
	labels["code"] = "canceled"
	if got := value(t, reg, "rpc_requests_total", labels); got != 1 {
		t.Fatalf("completed %v", got)
	}
	delete(labels, "code")
	if got := value(t, reg, "rpc_duration_seconds", labels); got != 1 {
		t.Fatalf("histogram observations %v", got)
	}
	for _, path := range []string{"/attacker/one", "/attacker/two"} {
		if err := r.Interceptor(func(context.Context, connect.Spec, connect.ServerStream) error { return nil })(t.Context(), connect.Spec{Procedure: path}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if got := value(t, reg, "rpc_requests_total", map[string]string{"rpc_service": "unknown", "rpc_method": "unknown", "code": "ok"}); got != 2 {
		t.Fatal(got)
	}
}

func TestFleetSnapshotIsAtomicBoundedAndDoesNotReadOnScrape(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := cloud.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1800000000, 0)
	created, phaseStarted := metav1.NewTime(now.Add(-time.Hour)), metav1.NewTime(now.Add(-10*time.Minute))
	network := &cloud.Network{ObjectMeta: metav1.ObjectMeta{Name: "private-network", Namespace: "private-tenant"}, Spec: cloud.NetworkSpec{Tier: cloud.ClusterTier2}}
	cluster := &cloud.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "secret-cluster", Namespace: network.Namespace, Generation: 2, CreationTimestamp: created}, Spec: cloud.ClusterSpec{UpgradeMode: cloud.UpgradeModeAutomatic, Network: cloud.ClusterNetwork{NetworkRef: cloud.LocalReference{Name: network.Name}}}, Status: cloud.ClusterStatus{Tier: cloud.ClusterTier0, Phase: cloud.ClusterPhaseProvisioning, ObservedGeneration: 1,
		Inventory: []cloud.InventoryEntry{{Kind: "Cluster", Name: "sensitive-name"}}, ComponentInventory: []cloud.InventoryEntry{{Kind: "Release"}},
		Conditions:         []metav1.Condition{{Type: "Frozen", Status: metav1.ConditionTrue}},
		Upgrade:            &cloud.ClusterUpgrade{Phase: cloud.ClusterUpgradeAddons, PhaseStartTime: &phaseStarted, AddonsBehind: []string{"coredns"}},
		PlatformComponents: []cloud.PlatformComponentStatus{{Name: "arbitrary-component", Phase: "attacker-input"}},
	}}
	hold := &cloud.ReleaseHold{ObjectMeta: metav1.ObjectMeta{Name: "platform-freeze", Generation: 2}, Spec: cloud.ReleaseHoldSpec{Platform: true, Revision: "new"}, Status: cloud.ReleaseHoldStatus{ObservedGeneration: 1, FreezeRevision: "old", FreezePhase: "Frozen", Resources: 10, PausedResources: 8}}
	var fail atomic.Bool
	var reads atomic.Int64
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(network, cluster, hold).WithInterceptorFuncs(interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		reads.Add(1)
		if fail.Load() {
			return errors.New("API unavailable")
		}
		return c.List(ctx, list, opts...)
	}}).Build()
	f := NewFleet(c)
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(f)
	if !f.NeedLeaderElection() {
		t.Fatal("standbys would publish duplicate fleets")
	}
	if err := f.Refresh(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	before := reads.Load()
	checks := []struct {
		name   string
		labels map[string]string
		want   float64
	}{
		{"fleet_cluster_configuration", map[string]string{"channel": "stable", "tier": "tier2", "mode": "automatic"}, 1},
		{"fleet_oldest_provisioning_seconds", map[string]string{"kind": "cluster"}, 3600},
		{"fleet_generation_pending", map[string]string{"kind": "cluster"}, 1},
		{"fleet_frozen_resources", map[string]string{"kind": "cluster"}, 1},
		{"fleet_inventory_resources", map[string]string{"owner_kind": "platform_component"}, 1},
		{"fleet_upgrade_oldest_phase_seconds", map[string]string{"phase": "addons"}, 600},
		{"fleet_addons_behind", nil, 1},
		{"fleet_components", map[string]string{"scope": "platform", "phase": "unknown"}, 1},
		{"freeze_phase", map[string]string{"phase": "pending"}, 1},
	}
	for _, check := range checks {
		if got := value(t, reg, check.name, check.labels); got != check.want {
			t.Errorf("%s = %v, want %v", check.name, got, check.want)
		}
	}
	if reads.Load() != before {
		t.Fatal("scrape performed Kubernetes reads")
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		for _, metric := range family.Metric {
			for _, pair := range metric.Label {
				for _, forbidden := range []string{"secret", "private", "attacker", "sensitive", "arbitrary"} {
					if strings.Contains(pair.GetValue(), forbidden) {
						t.Fatalf("unbounded label: %v", pair)
					}
				}
			}
		}
	}
	fail.Store(true)
	if err := f.Refresh(t.Context(), now.Add(time.Minute)); err == nil {
		t.Fatal("refresh did not fail")
	}
	if value(t, reg, "fleet_metrics_refresh_success", nil) != 0 || value(t, reg, "fleet_metrics_refresh_errors_total", nil) != 1 || value(t, reg, "fleet_metrics_last_success_timestamp_seconds", nil) != float64(now.Unix()) {
		t.Fatal("stale snapshot not marked")
	}
	if value(t, reg, "fleet_resources", map[string]string{"kind": "cluster", "phase": "provisioning"}) != 1 {
		t.Fatal("failed refresh destroyed previous snapshot")
	}
	fail.Store(false)
	if err := c.Delete(t.Context(), cluster); err != nil {
		t.Fatal(err)
	}
	if err := f.Refresh(t.Context(), now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if value(t, reg, "fleet_resources", map[string]string{"kind": "cluster", "phase": "provisioning"}) != 0 || value(t, reg, "fleet_inventory_resources", map[string]string{"owner_kind": "platform_component"}) != 0 {
		t.Fatal("removed owner left stale metrics")
	}
	var wg sync.WaitGroup
	wg.Go(func() {
		for range 10 {
			if _, err := reg.Gather(); err != nil {
				t.Error(err)
			}
		}
	})
	for range 10 {
		if err := f.Refresh(t.Context(), now); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
}
