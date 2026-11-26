package telemetry

import (
	"encoding/json"
	"regexp"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/helmrel"
)

// Label bounds of the fleet series whose values come from object data
// rather than a fixed vocabulary. A value past the bound is "other".
const (
	maxMinors         = 12 // Kubernetes minors live at once in a fleet
	maxManagedKinds   = 64 // Crossplane managed kinds of the AWS providers
	maxGates          = 24 // rollout gate codes
	labelOther        = "other"
	helmStateDeployed = "deployed"
)

var (
	minorPattern = regexp.MustCompile(`^v?([0-9]{1,2})\.([0-9]{1,3})`)
	// kindPattern and gatePattern are the shapes a Kubernetes kind and a
	// rollout gate code have; anything else is not used as a label value.
	kindPattern = regexp.MustCompile(`^[A-Z][A-Za-z0-9]{0,62}$`)
	gatePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,47}$`)
)

// defineState declares the series computed from owners' status beyond the
// lifecycle counts of fleet.go.
func (f *Fleet) defineState() {
	f.define("fleet_clusters_by_version", "Clusters by running Kubernetes minor and effective release channel.", "minor", "channel")
	f.define("fleet_clusters_extended_support", "Clusters whose Kubernetes minor is on no release channel (extended support), by minor.", "minor")
	f.define("fleet_oldest_deleting_seconds", "Age since the deletion request of the oldest owner still deleting; zero when none.", "kind")
	f.define("fleet_deletions", "Owners being deleted, by deletion stage (teardown: workload load balancers and nodes; resources: cloud resources).", "kind", "stage")
	f.define("fleet_component_phases", "Component instances by catalog component, scope and phase; components outside the catalog are \"other\".", "scope", "component", "phase")
	f.define("fleet_component_oldest_not_ready_seconds", "Longest time a component instance has not been Ready, by scope and catalog component.", "scope", "component")
	f.define("fleet_helm_releases", "Platform component Helm Releases by platform state: deployed, backoff (paused by the platform's backoff), gave_up, stuck (pending or uninstalling), failed, other.", "state")
	f.define("fleet_upgrade_halted", "Kubernetes upgrades halted (add-ons did not settle after the recoveries), by channel.", "channel")
	f.define("fleet_managed_resources_not_ready", "Crossplane managed resources of networks and clusters that are not READY, by owner kind, resource kind and state.", "owner_kind", "kind", "state")
	f.define("fleet_scan_oldest_attempt_seconds", "Longest time since a running cluster was last scanned or sampled, by scan kind.", "kind")
	f.define("rollout_clusters", "Clusters of a channel's rollout by tier and rollout state, per rollout (kubernetes, release, code).", "channel", "rollout", "tier", "state")
	f.define("rollout_halted", "Whether a channel's rollout is halted (more failures in a tier than its policy allows).", "channel", "rollout")
	f.define("rollout_current_tier", "One for the tier a channel's rollout is at.", "channel", "rollout", "tier")
	f.define("rollout_blocked_clusters", "Clusters a channel's Kubernetes rollout passes over, by first gate code (from the listed ones, at most 20 per channel).", "channel", "gate")
}

// bounded returns value while fewer than limit distinct values were seen
// for dimension in this snapshot, and "other" past it.
func (b snapshotBuilder) bounded(dimension, value string, limit int) string {
	seen := b.seen[dimension]
	if seen == nil {
		seen = map[string]bool{}
		b.seen[dimension] = seen
	}
	if seen[value] {
		return value
	}
	if len(seen) >= limit {
		return labelOther
	}
	seen[value] = true
	return value
}

func (b snapshotBuilder) minor(version string) string {
	m := minorPattern.FindStringSubmatch(version)
	if m == nil {
		return "unknown"
	}
	return b.bounded("minor", m[1]+"."+m[2], maxMinors)
}

func tierLabel(tier cloud.ClusterTier) string {
	return label(string(tier), "Tier0", "Tier1", "Tier2")
}

// stateZeros are the stable zero series of the state metrics.
func (b snapshotBuilder) stateZeros() {
	for _, kind := range []string{"cloudaccount", "network", "cluster", "component"} {
		b.oldest("fleet_oldest_deleting_seconds", 0, kind)
	}
	for _, s := range []string{"teardown", "resources"} {
		b.add("fleet_deletions", 0, "cluster", s)
	}
	for _, kind := range []string{"cloudaccount", "network", "component"} {
		b.add("fleet_deletions", 0, kind, "resources")
	}
	for _, s := range []string{helmStateDeployed, "backoff", "gave_up", "stuck", "failed", labelOther} {
		b.add("fleet_helm_releases", 0, s)
	}
	for _, k := range []string{"deprecations", "recommendations", "policy"} {
		b.oldest("fleet_scan_oldest_attempt_seconds", 0, k)
	}
	for _, ch := range []string{"alpha", "beta", "stable", "none"} {
		b.add("fleet_upgrade_halted", 0, ch)
	}
	for _, ch := range []string{"alpha", "beta", "stable"} {
		for _, r := range []string{"kubernetes", "release", "code"} {
			b.add("rollout_halted", 0, ch, r)
		}
	}
}

// deleting records an owner being deleted at stage.
func (b snapshotBuilder) deleting(kind, stage string, object metav1.Object, now time.Time) {
	if object.GetDeletionTimestamp() == nil {
		return
	}
	b.add("fleet_deletions", 1, kind, stage)
	b.oldest("fleet_oldest_deleting_seconds", age(now, object.GetDeletionTimestamp()), kind)
}

// contract is the part of a contractStatus (protojson) the fleet reads.
type contract struct {
	ManagedResources []struct {
		Kind  string `json:"kind"`
		State string `json:"state"`
	} `json:"managedResources"`
	Teardown *struct {
		Result string `json:"result"`
	} `json:"teardown"`
}

func readContract(raw *runtime.RawExtension) contract {
	var c contract
	if raw != nil && len(raw.Raw) > 0 {
		_ = json.Unmarshal(raw.Raw, &c) // a status this build cannot read counts nothing
	}
	return c
}

// managed counts an owner's managed resources that are not READY.
func (b snapshotBuilder) managed(ownerKind string, c contract) {
	for _, r := range c.ManagedResources {
		state := strings.TrimPrefix(r.State, "MANAGED_RESOURCE_STATE_")
		if state == "READY" {
			continue
		}
		kind := labelOther
		if kindPattern.MatchString(r.Kind) {
			kind = b.bounded("managed_kind", r.Kind, maxManagedKinds)
		}
		b.add("fleet_managed_resources_not_ready", 1, ownerKind, kind,
			label(state, "Creating", "Waiting", "Failed", "Deleting", "Missing", "Updating"))
	}
}

// clusterState adds a cluster's version, deletion, upgrade, scan and
// platform component series.
func (b snapshotBuilder) clusterState(c *cloud.Cluster, ch string, catalog map[string]bool, now time.Time) {
	version := c.Status.CurrentKubernetesVersion
	if version == "" && c.Spec.ControlPlane != nil {
		version = c.Spec.ControlPlane.KubernetesVersion
	}
	minor := b.minor(version)
	b.add("fleet_clusters_by_version", 1, minor, ch)
	if k := c.Status.Kubernetes; k != nil && k.Ending {
		b.add("fleet_clusters_extended_support", 1, minor)
	}
	ct := readContract(c.Status.ContractStatus)
	b.managed("cluster", ct)
	stage := "resources"
	if ct.Teardown != nil && (ct.Teardown.Result == "" || ct.Teardown.Result == "CLUSTER_TEARDOWN_RESULT_UNSPECIFIED") {
		stage = "teardown"
	}
	b.deleting("cluster", stage, c, now)
	b.add("fleet_upgrade_halted", 0, ch)
	if u := c.Status.Upgrade; u != nil && u.Halted {
		b.add("fleet_upgrade_halted", 1, ch)
	}
	if c.Status.Phase == cloud.ClusterPhaseRunning {
		b.scans(c, now)
	}
	for _, pc := range c.Status.PlatformComponents {
		name := catalogName(catalog, pc.Name)
		p := phase(pc.Phase)
		b.add("fleet_component_phases", 1, "platform", name, p)
		b.oldest("fleet_component_oldest_not_ready_seconds", 0, "platform", name)
		if p != "ready" {
			since := pc.Since
			if since == nil {
				created := c.GetCreationTimestamp()
				since = &created
			}
			b.oldest("fleet_component_oldest_not_ready_seconds", age(now, since), "platform", name)
		}
	}
}

// scans records the age of a running cluster's last scan attempt per kind;
// a cluster never scanned counts from its creation.
func (b snapshotBuilder) scans(c *cloud.Cluster, now time.Time) {
	attempts := map[string]*metav1.Time{}
	if d := c.Status.Deprecations; d != nil {
		attempts["deprecations"] = d.AttemptedAt
	}
	if r := c.Status.Recommendations; r != nil {
		attempts["recommendations"] = r.AttemptedAt
	}
	if v := c.Status.Violations; v != nil {
		attempts["policy"] = v.PolicyCollectedAt
	}
	for _, ref := range c.Status.Results {
		switch ref.Kind {
		case "deprecations", "recommendations":
			attempts[ref.Kind] = latest(attempts[ref.Kind], ref.AttemptedAt)
		case "violations":
			attempts["policy"] = latest(attempts["policy"], ref.AttemptedAt)
		}
	}
	created := c.GetCreationTimestamp()
	for _, kind := range []string{"deprecations", "recommendations", "policy"} {
		at := attempts[kind]
		if at == nil {
			at = &created
		}
		b.oldest("fleet_scan_oldest_attempt_seconds", age(now, at), kind)
	}
}

func latest(a, b *metav1.Time) *metav1.Time {
	if a == nil || (b != nil && b.After(a.Time)) {
		return b
	}
	return a
}

// catalogName is the component's catalog name, or "other" for one the
// catalog does not define.
func catalogName(catalog map[string]bool, name string) string {
	if catalog[name] {
		return name
	}
	return labelOther
}

// tenantComponent adds a tenant Component's catalog, deletion and
// not-ready series.
func (b snapshotBuilder) tenantComponent(c *cloud.Component, catalog map[string]bool, now time.Time) {
	name := catalogName(catalog, c.Spec.Component)
	p := phase(c.Status.Phase)
	b.add("fleet_component_phases", 1, "tenant", name, p)
	b.deleting("component", "resources", c, now)
	b.oldest("fleet_component_oldest_not_ready_seconds", 0, "tenant", name)
	if p == "ready" {
		return
	}
	since := c.GetCreationTimestamp()
	for _, cond := range c.Status.Conditions {
		if cond.Type == "Ready" && cond.Status != metav1.ConditionTrue && !cond.LastTransitionTime.IsZero() {
			since = cond.LastTransitionTime
		}
	}
	b.oldest("fleet_component_oldest_not_ready_seconds", age(now, &since), "tenant", name)
}

// helmRelease classifies one provider-helm Release by what the platform
// did to it, then by Helm's own state.
func (b snapshotBuilder) helmRelease(r *unstructured.Unstructured, now time.Time) {
	annotations := r.GetAnnotations()
	state, _, _ := unstructured.NestedString(r.Object, "status", "atProvider", "state")
	until, err := time.Parse(time.RFC3339, annotations[helmrel.BackoffAnnotation])
	switch {
	case annotations[helmrel.GaveUpAnnotation] == "true":
		b.add("fleet_helm_releases", 1, "gave_up")
	case err == nil && until.After(now):
		b.add("fleet_helm_releases", 1, "backoff")
	case strings.HasPrefix(state, "pending-") || state == "uninstalling":
		b.add("fleet_helm_releases", 1, "stuck")
	case state == "failed":
		b.add("fleet_helm_releases", 1, "failed")
	case state == helmStateDeployed:
		b.add("fleet_helm_releases", 1, helmStateDeployed)
	default:
		b.add("fleet_helm_releases", 1, labelOther)
	}
}

// channel adds the rollouts of one release channel.
func (b snapshotBuilder) channel(c *cloud.ReleaseChannel) {
	ch := label(c.Name, "Alpha", "Beta", "Stable")
	if r := c.Status.Rollout; r != nil {
		b.rollout(ch, "kubernetes", r.Halted, c.Status.RolloutTier, r.Members)
		for _, blocked := range r.Blocked {
			gate := labelOther
			if gatePattern.MatchString(blocked.Gate) {
				gate = b.bounded("gate", blocked.Gate, maxGates)
			}
			b.add("rollout_blocked_clusters", 1, ch, gate)
		}
	}
	if m := c.Status.ReleaseMoves; m != nil {
		b.rollout(ch, "release", m.Halted, m.Tier, m.Members)
	}
	if m := c.Status.CodeChanges; m != nil {
		b.rollout(ch, "code", m.Halted, m.Tier, m.Members)
	}
}

func (b snapshotBuilder) rollout(ch, rollout string, halted bool, tier cloud.ClusterTier, members []cloud.RolloutTierCount) {
	if halted {
		b.add("rollout_halted", 1, ch, rollout)
	}
	if tier != "" {
		b.add("rollout_current_tier", 1, ch, rollout, tierLabel(tier))
	}
	for _, m := range members {
		t := tierLabel(m.Tier)
		for state, n := range map[string]int32{"pending": m.Pending, "in_progress": m.InProgress, "succeeded": m.Succeeded, "failed": m.Failed, "blocked": m.Blocked} {
			b.add("rollout_clusters", float64(n), ch, rollout, t, state)
		}
	}
}
