package cluster

import (
	"context"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/crossplane/managed"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// releaseDetail is what provider-helm reports of one chart's Release
// object: why a component is where it is.
type releaseDetail struct {
	// Chart is the Helm release name in the workload cluster (the Release
	// object's external name).
	Chart string
	// TargetNamespace is where the chart is installed in the workload
	// cluster (spec.forProvider.namespace).
	TargetNamespace string
	// HelmState is status.atProvider.state: deployed, failed,
	// uninstalling, pending-install, ...
	HelmState string
	// LastError is the Synced condition's message while it is False.
	LastError string
	// Failures is status.failed: failed attempts in a row.
	Failures int32
	// Since is when the Release stopped being synced, or ready: the
	// transition time of its Synced condition while that is False, else of
	// its Ready condition while that is not True. Zero when it is fine.
	Since time.Time
	// Paused: the Release carries crossplane.io/paused (an operator paused
	// the cluster, e.g. for a provider upgrade).
	Paused bool
}

// releaseDetail reads the Release object name in namespace; nil when it
// does not exist or cannot be read (the phase stands without the detail).
func (s *Reconciler) releaseDetail(ctx context.Context, namespace, name string) *releaseDetail {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(crossplaneaws.HelmReleaseGVK)
	if err := s.deps.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, u); err != nil {
		return nil
	}
	return detailOf(u)
}

// detailOf reads a Release object as provider-helm v1.5.0 writes it
// (status.atProvider.state, status.failed, status.conditions).
func detailOf(u *unstructured.Unstructured) *releaseDetail {
	d := &releaseDetail{Chart: u.GetAnnotations()[crossplaneaws.ExternalNameAnnotation], Paused: managed.IsPaused(u)}
	d.TargetNamespace, _, _ = unstructured.NestedString(u.Object, "spec", "forProvider", "namespace")
	d.HelmState, _, _ = unstructured.NestedString(u.Object, "status", "atProvider", "state")
	if failed, ok, _ := unstructured.NestedFieldNoCopy(u.Object, "status", "failed"); ok {
		switch n := failed.(type) {
		case int64:
			d.Failures = int32(min(n, int64(1<<31-1))) //nolint:gosec // bounded above
		case float64:
			d.Failures = int32(min(n, float64(1<<31-1)))
		}
	}
	conditions, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	var readySince time.Time
	for _, raw := range conditions {
		c, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		kind, _ := c["type"].(string)
		status, _ := c["status"].(string)
		at, _ := c["lastTransitionTime"].(string)
		since, _ := time.Parse(time.RFC3339, at)
		switch {
		case kind == "Synced" && status == "False":
			d.LastError, _ = c["message"].(string)
			d.Since = since
		case kind == "Ready" && status != "True":
			readySince = since
		}
	}
	if d.Since.IsZero() {
		d.Since = readySince
	}
	return d
}

// describe completes what a pass found out about the platform components
// with what the status keeps between passes and what the Release objects
// say: since when each is in its phase, how often it was recovered at
// this version, and, for a component that is not READY, the chart it is
// about with Helm's state, the provider's last error and its failures. A
// stuck release is recovered here (recoverStuck). objects names, per
// component, the Release object its phase is about.
func (s *Reconciler) describe(ctx context.Context, row *model.Cluster, namespace string, p *platform, objects map[string]string) {
	before := map[string]*cloudv1.PlatformComponent{}
	for _, c := range storedPlatform(row).components {
		before[c.GetName()] = c
	}
	at := now()
	for _, c := range p.components {
		prev := before[c.GetName()]
		if prev != nil && prev.GetVersion() == c.GetVersion() {
			c.Recoveries = prev.GetRecoveries()
		}
		if object := objects[c.GetName()]; object != "" {
			if d := s.releaseDetail(ctx, namespace, object); d != nil {
				c.Chart, c.HelmState, c.LastError, c.Failures = d.Chart, d.HelmState, d.LastError, d.Failures
				s.recover(ctx, row, namespace, object, c, d, at)
			}
		}
		if prev != nil && prev.GetPhase() == c.GetPhase() && prev.GetSince() != nil {
			c.Since = prev.GetSince()
		} else {
			c.Since = timestamppb.New(at)
		}
	}
}

// logPlatform logs what changed about the platform components in this
// pass, once: a component that entered FAILED (warn, with why), one that
// became READY or started INSTALLING (info), one that is no longer
// installed (info), and the PlatformComponentsReady condition changing.
// A pass that changed nothing logs nothing.
func (s *Reconciler) logPlatform(ctx context.Context, row *model.Cluster, before, after *platform) {
	if after == nil {
		return
	}
	log := s.logger().With("cluster_id", row.ID, "cluster", row.Name, "tenant_id", row.TenantID)
	was := map[string]*cloudv1.PlatformComponent{}
	for _, c := range before.components {
		was[c.GetName()] = c
	}
	for _, c := range after.components {
		prev, known := was[c.GetName()]
		delete(was, c.GetName())
		if known && prev.GetPhase() == c.GetPhase() {
			continue
		}
		attrs := []any{"component", c.GetName(), "version", c.GetVersion()}
		switch c.GetPhase() {
		case cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED:
			log.WarnContext(ctx, "platform component failed", append(attrs, "message", c.GetMessage(),
				"chart", c.GetChart(), "helm_state", c.GetHelmState(), "failures", c.GetFailures())...)
		case cloudv1.ComponentPhase_COMPONENT_PHASE_READY:
			log.InfoContext(ctx, "platform component ready", attrs...)
		case cloudv1.ComponentPhase_COMPONENT_PHASE_INSTALLING:
			log.InfoContext(ctx, "platform component installing", attrs...)
		default: // waiting again is not news: the status says what for
		}
	}
	for name, c := range was {
		log.InfoContext(ctx, "platform component removed", "component", name, "version", c.GetVersion())
	}
	if now, then := platformCondition(after), platformCondition(before); now != nil &&
		(then == nil || then.GetStatus() != now.GetStatus() || then.GetReason() != now.GetReason()) {
		log.InfoContext(ctx, "platform components condition changed", "status", conditionWord(now.GetStatus()),
			"reason", now.GetReason(), "message", now.GetMessage())
	}
}

// platformCondition is the PlatformComponentsReady condition p gives a
// status; nil when it gives none (no components).
func platformCondition(p *platform) *cloudv1.Condition {
	st := &cloudv1.ClusterStatus{}
	withPlatform(st, p)
	for _, c := range st.GetConditions() {
		if c.GetType() == ConditionPlatformComponents {
			return c
		}
	}
	return nil
}

func conditionWord(s cloudv1.ConditionStatus) string {
	switch s {
	case cloudv1.ConditionStatus_CONDITION_STATUS_TRUE:
		return "True"
	case cloudv1.ConditionStatus_CONDITION_STATUS_FALSE:
		return "False"
	default:
		return "Unknown"
	}
}
