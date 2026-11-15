package convert

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// Release move phases as the kind spells them.
const (
	ReleaseMoveIdle       = "Idle"
	ReleaseMovePending    = "Pending"
	ReleaseMoveComponents = "Components"
	ReleaseMoveHalted     = "Halted"
)

// ReleaseMoveToAPI is the contract's release move as the kind reports
// it; nil when unset.
func ReleaseMoveToAPI(p *cloudv1.ClusterReleaseMove) *cloudv1alpha1.ClusterReleaseMove {
	if p == nil {
		return nil
	}
	out := &cloudv1alpha1.ClusterReleaseMove{Current: p.GetCurrent(), Target: p.GetTarget(),
		Phase: camel(p.GetPhase(), "CLUSTER_RELEASE_MOVE_PHASE_"), Component: p.GetComponent(),
		Step: p.GetStep(), Steps: p.GetSteps(), Message: p.GetMessage()}
	if t := p.GetStartTime(); t != nil {
		out.StartTime = &metav1.Time{Time: t.AsTime()}
	}
	if t := p.GetComponentStartTime(); t != nil {
		out.ComponentStartTime = &metav1.Time{Time: t.AsTime()}
	}
	return out
}
