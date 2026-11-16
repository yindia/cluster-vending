package telemetry

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
)

const FleetInterval = 30 * time.Second

// Fleet publishes an atomic, periodically refreshed snapshot of cached CRs.
// Collect never talks to Kubernetes or cloud APIs. Only the leader refreshes it.
type Fleet struct {
	reader                                         client.Reader
	desc                                           map[string]*prometheus.Desc
	mu                                             sync.RWMutex
	snapshot                                       []prometheus.Metric
	active, success, lastSuccess, errors, duration float64
}

func NewFleet(reader client.Reader) *Fleet {
	f := &Fleet{reader: reader, desc: map[string]*prometheus.Desc{}}
	f.define("fleet_resources", "Owners by observed lifecycle phase (not individual cloud resources).", "kind", "phase")
	f.define("fleet_cluster_configuration", "Clusters by effective channel, inherited network tier and upgrade mode.", "channel", "tier", "mode")
	f.define("fleet_inventory_resources", "Recorded inventory references; includes Usages and references to resources not yet observed.", "owner_kind")
	f.define("fleet_conditions", "Owners by Ready or Synced condition status; absent conditions are unknown.", "kind", "condition", "status")
	f.define("fleet_generation_pending", "Owners whose status has not observed the latest Kubernetes generation.", "kind")
	f.define("fleet_oldest_provisioning_seconds", "Age since creation of the oldest owner still in Provisioning; zero when none.", "kind")
	f.define("fleet_frozen_resources", "Owners whose observed Frozen condition is true.", "kind")
	f.define("fleet_upgrade_clusters", "Clusters by current Kubernetes upgrade phase.", "channel", "tier", "phase")
	f.define("fleet_upgrade_oldest_phase_seconds", "Longest elapsed time in a current Kubernetes upgrade phase.", "phase")
	f.define("fleet_addons_behind", "Number of add-ons reported behind during Kubernetes upgrades.")
	f.define("fleet_components", "Component instances by scope and observed phase; platform components are counted separately from tenant Components.", "scope", "phase")
	f.define("fleet_release_moves", "Clusters by platform component release move phase.", "phase")
	f.define("fleet_code_changes", "Owners by renderer code change phase.", "kind", "phase")
	f.define("freeze_policy_present", "Whether the canonical platform freeze policy exists (including scoped policies).")
	f.define("freeze_phase", "Canonical freeze confirmation phase; not a count of affected resources.", "phase")
	f.define("freeze_resources", "Managed resources in the last canonical freeze confirmation snapshot.", "state")
	f.define("fleet_metrics_active", "One while this process is the elected fleet metrics publisher; zero on standby.")
	f.define("fleet_metrics_refresh_success", "Whether the latest fleet snapshot refresh succeeded.")
	f.define("fleet_metrics_last_success_timestamp_seconds", "Unix time of the last successful fleet snapshot refresh; zero before the first.")
	f.define("fleet_metrics_refresh_errors_total", "Failed fleet snapshot refresh attempts; previous snapshot is retained.")
	f.define("fleet_metrics_refresh_duration_seconds", "Duration of the latest fleet snapshot refresh attempt.")
	return f
}

func (f *Fleet) define(name, help string, labels ...string) {
	f.desc[name] = prometheus.NewDesc("cluster_vending_"+name, help, labels, nil)
}

func (f *Fleet) Describe(out chan<- *prometheus.Desc) {
	for _, desc := range f.desc {
		out <- desc
	}
}

func (f *Fleet) Collect(out chan<- prometheus.Metric) {
	f.mu.RLock()
	snapshot := f.snapshot
	active, success, last, failures, duration := f.active, f.success, f.lastSuccess, f.errors, f.duration
	f.mu.RUnlock()
	for _, metric := range snapshot {
		out <- metric
	}
	for name, value := range map[string]float64{"fleet_metrics_active": active, "fleet_metrics_refresh_success": success,
		"fleet_metrics_last_success_timestamp_seconds": last, "fleet_metrics_refresh_duration_seconds": duration} {
		out <- prometheus.MustNewConstMetric(f.desc[name], prometheus.GaugeValue, value)
	}
	out <- prometheus.MustNewConstMetric(f.desc["fleet_metrics_refresh_errors_total"], prometheus.CounterValue, failures)
}

func (f *Fleet) NeedLeaderElection() bool { return true }

func (f *Fleet) Start(ctx context.Context) error {
	f.mu.Lock()
	f.active = 1
	f.mu.Unlock()
	defer func() { f.mu.Lock(); f.active = 0; f.mu.Unlock() }()
	refresh := func() {
		bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		_ = f.Refresh(bounded, time.Now()) // scrape health metrics report failures
	}
	refresh()
	ticker := time.NewTicker(FleetInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			refresh()
		}
	}
}

type series struct{ name, labels string }

type snapshotBuilder struct{ values map[series]float64 }

func (b snapshotBuilder) add(name string, value float64, labels ...string) {
	b.values[series{name, strings.Join(labels, "\x00")}] += value
}

func (b snapshotBuilder) oldest(name string, value float64, labels ...string) {
	key := series{name, strings.Join(labels, "\x00")}
	b.values[key] = max(b.values[key], value)
}

// label only accepts a finite vocabulary, including for schemaless status fields.
func label(value string, choices ...string) string {
	for _, choice := range choices {
		if strings.EqualFold(value, choice) {
			return strings.ToLower(choice)
		}
	}
	return "unknown"
}

func phase(value string) string {
	return label(value, "Bootstrapping", "Provisioning", "Pending", "Installing", "Running", "Ready", "Updating", "Imported", "Blocked", "Failed", "Degraded", "Deleting")
}

func channel(c cloud.Cluster) string {
	if c.Spec.ReleaseChannel == "" {
		if c.Spec.UpgradeMode == cloud.UpgradeModeAutomatic {
			return "stable"
		}
		return "none"
	}
	return label(string(c.Spec.ReleaseChannel), "Alpha", "Beta", "Stable")
}

func age(now time.Time, since *metav1.Time) float64 {
	if since == nil || since.IsZero() {
		return 0
	}
	return max(0, now.Sub(since.Time).Seconds())
}

func (b snapshotBuilder) owner(kind, state string, object metav1.Object, observed int64, conditions []metav1.Condition, inventory int, now time.Time) {
	state = phase(state)
	b.add("fleet_resources", 1, kind, state)
	b.add("fleet_inventory_resources", float64(inventory), kind)
	b.add("fleet_generation_pending", 0, kind)
	if observed < object.GetGeneration() {
		b.add("fleet_generation_pending", 1, kind)
	}
	created := object.GetCreationTimestamp()
	b.oldest("fleet_oldest_provisioning_seconds", 0, kind)
	if state == "provisioning" || state == "bootstrapping" {
		b.oldest("fleet_oldest_provisioning_seconds", age(now, &created), kind)
	}
	b.add("fleet_frozen_resources", 0, kind)
	for _, name := range []string{"Ready", "Synced"} {
		status := "unknown"
		for _, condition := range conditions {
			if condition.Type == name {
				status = label(string(condition.Status), "True", "False", "Unknown")
			}
		}
		b.add("fleet_conditions", 1, kind, strings.ToLower(name), status)
	}
	for _, condition := range conditions {
		if condition.Type == "Frozen" && condition.Status == metav1.ConditionTrue {
			b.add("fleet_frozen_resources", 1, kind)
			break
		}
	}
}

func (b snapshotBuilder) code(kind string, code *cloud.CodeChange) {
	if code != nil {
		b.add("fleet_code_changes", 1, kind, label(code.Phase, "Pending", "Applying", "Done", "Halted"))
	}
}

// Refresh replaces all fleet series together. Failed reads preserve the previous
// snapshot, while refresh health and last-success time make staleness explicit.
func (f *Fleet) Refresh(ctx context.Context, now time.Time) (err error) {
	start := time.Now()
	defer func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.duration = time.Since(start).Seconds()
		if err != nil {
			f.success = 0
			f.errors++
		}
	}()
	accounts, networks, clusters, components, holds := &cloud.CloudAccountList{}, &cloud.NetworkList{}, &cloud.ClusterList{}, &cloud.ComponentList{}, &cloud.ReleaseHoldList{}
	for _, list := range []client.ObjectList{accounts, networks, clusters, components, holds} {
		if err := f.reader.List(ctx, list); err != nil {
			return fmt.Errorf("fleet metrics: %w", err)
		}
	}
	b := snapshotBuilder{values: map[series]float64{}}
	// Stable zero series make empty fleets and removed owners observable.
	for _, kind := range []string{"cloudaccount", "network", "cluster", "component"} {
		for _, state := range []string{"unknown", "bootstrapping", "provisioning", "pending", "installing", "running", "ready", "updating", "imported", "blocked", "failed", "degraded", "deleting"} {
			b.add("fleet_resources", 0, kind, state)
		}
		for _, name := range []string{"ready", "synced"} {
			for _, status := range []string{"true", "false", "unknown"} {
				b.add("fleet_conditions", 0, kind, name, status)
			}
		}
		for _, metric := range []string{"fleet_inventory_resources", "fleet_generation_pending", "fleet_oldest_provisioning_seconds", "fleet_frozen_resources"} {
			b.add(metric, 0, kind)
		}
	}
	b.add("fleet_inventory_resources", 0, "platform_component")
	b.add("fleet_addons_behind", 0)
	b.add("freeze_policy_present", 0)
	for _, p := range []string{"pending", "freezing", "frozen", "unfrozen", "unknown"} {
		b.add("freeze_phase", 0, p)
	}
	for _, p := range []string{"idle", "pending", "components", "halted", "unknown"} {
		b.add("fleet_release_moves", 0, p)
	}
	for _, kind := range []string{"network", "cluster"} {
		for _, p := range []string{"pending", "applying", "done", "halted", "unknown"} {
			b.add("fleet_code_changes", 0, kind, p)
		}
	}
	for _, scope := range []string{"platform", "tenant"} {
		for _, p := range []string{"pending", "installing", "ready", "updating", "degraded", "failed", "deleting", "unknown"} {
			b.add("fleet_components", 0, scope, p)
		}
	}
	for _, p := range []string{"controlplane", "nodesandaddons", "nodegroups", "addons", "platformcomponents", "tenantcomponents", "unknown"} {
		b.oldest("fleet_upgrade_oldest_phase_seconds", 0, p)
	}
	for _, state := range []string{"matched", "paused"} {
		b.add("freeze_resources", 0, state)
	}
	for _, account := range accounts.Items {
		b.owner("cloudaccount", string(account.Status.Phase), &account, account.Status.ObservedGeneration, account.Status.Conditions, len(account.Status.Inventory), now)
	}
	tiers := map[client.ObjectKey]cloud.ClusterTier{}
	for _, network := range networks.Items {
		tiers[client.ObjectKeyFromObject(&network)] = network.Spec.Tier
		b.owner("network", string(network.Status.Phase), &network, network.Status.ObservedGeneration, network.Status.Conditions, len(network.Status.Inventory), now)
		b.code("network", network.Status.CodeChange)
	}
	for _, cluster := range clusters.Items {
		b.owner("cluster", string(cluster.Status.Phase), &cluster, cluster.Status.ObservedGeneration, cluster.Status.Conditions, len(cluster.Status.Inventory), now)
		tier := "unknown"
		if inherited, found := tiers[client.ObjectKey{Namespace: cluster.Namespace, Name: cluster.Spec.Network.NetworkRef.Name}]; found {
			if inherited == "" {
				inherited = cloud.ClusterTier0
			}
			tier = label(string(inherited), "Tier0", "Tier1", "Tier2")
		}
		mode := cluster.Spec.UpgradeMode
		if mode == "" {
			mode = cloud.UpgradeModeManual
		}
		ch := channel(cluster)
		b.add("fleet_cluster_configuration", 1, ch, tier, label(string(mode), "Manual", "Automatic"))
		b.add("fleet_inventory_resources", float64(len(cluster.Status.ComponentInventory)), "platform_component")
		if upgrade := cluster.Status.Upgrade; upgrade != nil {
			p := label(string(upgrade.Phase), "ControlPlane", "NodesAndAddons", "NodeGroups", "Addons", "PlatformComponents", "TenantComponents")
			b.add("fleet_upgrade_clusters", 1, ch, tier, p)
			b.oldest("fleet_upgrade_oldest_phase_seconds", age(now, upgrade.PhaseStartTime), p)
			b.add("fleet_addons_behind", float64(len(upgrade.AddonsBehind)))
		}
		for _, component := range cluster.Status.PlatformComponents {
			b.add("fleet_components", 1, "platform", phase(component.Phase))
		}
		if move := cluster.Status.ReleaseMove; move != nil {
			b.add("fleet_release_moves", 1, label(move.Phase, "Idle", "Pending", "Components", "Halted"))
		}
		b.code("cluster", cluster.Status.CodeChange)
	}
	for _, component := range components.Items {
		b.owner("component", component.Status.Phase, &component, component.Status.ObservedGeneration, component.Status.Conditions, len(component.Status.Inventory), now)
		b.add("fleet_components", 1, "tenant", phase(component.Status.Phase))
	}
	for _, hold := range holds.Items {
		if hold.Name != "platform-freeze" || !hold.Spec.Platform {
			continue
		}
		b.add("freeze_policy_present", 1)
		p := "pending"
		if hold.Status.ObservedGeneration == hold.Generation && hold.Status.FreezeRevision == hold.Spec.Revision {
			p = label(hold.Status.FreezePhase, "Freezing", "Frozen", "Unfrozen")
		}
		b.add("freeze_phase", 1, p)
		b.add("freeze_resources", float64(hold.Status.Resources), "matched")
		b.add("freeze_resources", float64(hold.Status.PausedResources), "paused")
	}
	metrics := make([]prometheus.Metric, 0, len(b.values))
	for key, value := range b.values {
		var labels []string
		if key.labels != "" {
			labels = strings.Split(key.labels, "\x00")
		}
		metrics = append(metrics, prometheus.MustNewConstMetric(f.desc[key.name], prometheus.GaugeValue, value, labels...))
	}
	f.mu.Lock()
	f.snapshot, f.success, f.lastSuccess = metrics, 1, float64(now.Unix())
	f.mu.Unlock()
	return nil
}
