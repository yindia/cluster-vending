package cli

import (
	"fmt"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// releaseLine is cluster get's Release: the release the components run,
// and where a move to another one is.
func releaseLine(st *cloudv1.ClusterStatus) string {
	m := st.GetReleaseMove()
	current := st.GetPlatformRelease()
	switch m.GetPhase() {
	case cloudv1.ClusterReleaseMovePhase_CLUSTER_RELEASE_MOVE_PHASE_COMPONENTS:
		return fmt.Sprintf("%s → %s: %s (%d of %d), since %s", m.GetCurrent(), m.GetTarget(), m.GetComponent(),
			m.GetStep(), m.GetSteps(), age(m.GetComponentStartTime()))
	case cloudv1.ClusterReleaseMovePhase_CLUSTER_RELEASE_MOVE_PHASE_HALTED:
		return fmt.Sprintf("%s → %s: halted at %s (%d of %d): %s", m.GetCurrent(), m.GetTarget(), m.GetComponent(),
			m.GetStep(), m.GetSteps(), oneLine(m.GetMessage()))
	case cloudv1.ClusterReleaseMovePhase_CLUSTER_RELEASE_MOVE_PHASE_PENDING:
		return fmt.Sprintf("%s (%s available: %s)", current, m.GetTarget(), oneLine(m.GetMessage()))
	case cloudv1.ClusterReleaseMovePhase_CLUSTER_RELEASE_MOVE_PHASE_IDLE,
		cloudv1.ClusterReleaseMovePhase_CLUSTER_RELEASE_MOVE_PHASE_UNSPECIFIED:
	}
	return current
}
