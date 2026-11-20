//go:build e2e

package e2e

import (
	"fmt"
	"sort"
	"time"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// karpenter is the platform component everything else waits for.
const karpenter = "karpenter"

// installOrder watches a new cluster come up, one read at a time, and
// records whether the install order of docs/architecture.md, Platform
// components, held: the EKS cluster, node group and add-ons first (they are
// what RUNNING means), then Karpenter, then the other platform components,
// none of which starts before Karpenter is READY.
type installOrder struct {
	reads        int
	firstRunning int // read number; 0 until seen
	addonsAtRun  []string
	addonKinds   int
	readyAt      map[string]int // component: the read it was first READY
	started      map[string]bool
	violations   []string
}

func newInstallOrder() *installOrder {
	return &installOrder{readyAt: map[string]int{}, started: map[string]bool{}}
}

func isStarted(p cloudv1.ComponentPhase) bool {
	return p != cloudv1.ComponentPhase_COMPONENT_PHASE_UNSPECIFIED && p != cloudv1.ComponentPhase_COMPONENT_PHASE_PENDING
}

func (o *installOrder) observe(c *cloudv1.Cluster) {
	o.reads++
	st := c.GetStatus()
	if o.firstRunning == 0 && st.GetState() == cloudv1.ClusterState_CLUSTER_STATE_RUNNING {
		o.firstRunning = o.reads
		addons := managedOfKind(st.GetManagedResources(), "Addon")
		o.addonKinds = len(addons)
		o.addonsAtRun = notReady(addons)
	}
	phases := map[string]cloudv1.ComponentPhase{}
	for _, p := range st.GetPlatformComponents() {
		phases[p.GetName()] = p.GetPhase()
		if p.GetPhase() == cloudv1.ComponentPhase_COMPONENT_PHASE_READY {
			if _, ok := o.readyAt[p.GetName()]; !ok {
				o.readyAt[p.GetName()] = o.reads
			}
		}
	}
	for name, phase := range phases {
		if !isStarted(phase) || o.started[name] {
			continue
		}
		o.started[name] = true
		switch {
		case o.firstRunning == 0:
			o.violate("%s started (%s) before the cluster was RUNNING", name, phase)
		case name != karpenter && phases[karpenter] != cloudv1.ComponentPhase_COMPONENT_PHASE_READY:
			o.violate("%s started (%s) before %s was READY (%s)", name, phase, karpenter, phases[karpenter])
		}
	}
}

func (o *installOrder) violate(format string, args ...any) {
	o.violations = append(o.violations, fmt.Sprintf("read %d: ", o.reads)+fmt.Sprintf(format, args...))
}

// verify returns what contradicts the order, judged on the final status
// too: READY since (the time each component entered READY) is the server's
// own record and does not depend on how often the harness polled.
func (o *installOrder) verify(c *cloudv1.Cluster) []string {
	out := append([]string(nil), o.violations...)
	if o.firstRunning == 0 {
		out = append(out, "the cluster was never seen RUNNING")
	}
	if o.addonKinds > 0 && len(o.addonsAtRun) > 0 {
		out = append(out, fmt.Sprintf("RUNNING with EKS add-ons not ready: %v", o.addonsAtRun))
	}
	var kar *cloudv1.PlatformComponent
	comps := c.GetStatus().GetPlatformComponents()
	for _, p := range comps {
		if p.GetName() == karpenter {
			kar = p
		}
	}
	if kar == nil {
		return append(out, "no platform component karpenter")
	}
	names := make([]string, 0, len(o.readyAt))
	for n := range o.readyAt {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if n != karpenter && o.readyAt[n] < o.readyAt[karpenter] {
			out = append(out, fmt.Sprintf("%s was READY (read %d) before %s (read %d)", n, o.readyAt[n], karpenter, o.readyAt[karpenter]))
		}
	}
	for _, p := range comps {
		if p.GetName() != karpenter && p.GetSince().IsValid() && kar.GetSince().IsValid() &&
			p.GetPhase() == cloudv1.ComponentPhase_COMPONENT_PHASE_READY && p.GetSince().AsTime().Before(kar.GetSince().AsTime().Add(-time.Second)) {
			out = append(out, fmt.Sprintf("%s has been READY since %s, before %s since %s", p.GetName(),
				p.GetSince().AsTime().Format(time.RFC3339), karpenter, kar.GetSince().AsTime().Format(time.RFC3339)))
		}
	}
	return out
}
