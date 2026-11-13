package cluster

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

const (
	// stuckAfter is how long a Helm release must have been in a stuck
	// state before it is recovered: an install, upgrade or uninstall that
	// is really running is given this long.
	stuckAfter = 10 * time.Minute
	// maxRecoveries is how many times a component's stuck release is
	// recovered at one version. After that it stays FAILED: what makes it
	// stick needs a person.
	maxRecoveries = 3
)

// Helm's release states an operation leaves behind when it is interrupted
// or its hooks fail (helm.sh/helm/v4 pkg/release/common/status.go). A
// release whose newest revision is in one of them cannot be installed or
// upgraded again: upgrade answers "has no deployed releases"
// (uninstalling) or "another operation (install/upgrade/rollback) is in
// progress" (pending-*) for ever (pkg/action/upgrade.go).
var stuckStates = map[string]bool{
	"uninstalling":     true,
	"pending-install":  true,
	"pending-upgrade":  true,
	"pending-rollback": true,
}

// stuckErrors are what Helm answers for such a release.
var stuckErrors = []string{"has no deployed releases", "another operation (install/upgrade/rollback) is in progress"}

// stuckBy reports whether Helm's state or its error is that of a stuck
// release, whatever how long.
func stuckBy(helmState, lastError string) bool {
	if stuckStates[helmState] {
		return true
	}
	for _, e := range stuckErrors {
		if strings.Contains(lastError, e) {
			return true
		}
	}
	return false
}

// PlatformStuck reports whether the row's recorded status has a platform
// component whose Helm release is stuck and may still be recovered: the
// cluster is looked at again soon although it is RUNNING, because the
// recovery waits for stuckAfter and nothing else brings a pass.
func PlatformStuck(row *model.Cluster) bool {
	for _, c := range storedStatus(row).GetPlatformComponents() {
		if c.GetRecoveries() < maxRecoveries && stuckBy(c.GetHelmState(), c.GetLastError()) {
			return true
		}
	}
	return false
}

// stuck reports whether a Release is stuck: Helm's state is one an
// interrupted operation leaves, or Helm refuses the release with the
// error of such a state, and it has been like that for stuckAfter.
func stuck(d *releaseDetail, at time.Time) bool {
	if d == nil || d.Since.IsZero() || at.Sub(d.Since) < stuckAfter {
		return false
	}
	return stuckBy(d.HelmState, d.LastError)
}

// recordToRemove picks the Helm record whose removal lets the release be
// installed again: the newest revision, and only when it is in a stuck
// state. Removing it makes the revision before it the newest again (a
// deployed one after a stuck upgrade or rollback, none after a stuck
// first install or uninstall, so the chart is installed anew). A deployed,
// superseded or failed revision is never removed, and neither is a
// revision Helm wrote less than stuckAfter ago: that is an operation
// still running (provider-helm retrying an install), not a stuck one.
func recordToRemove(records []reconcile.HelmRecord, at time.Time) (reconcile.HelmRecord, bool) {
	var newest reconcile.HelmRecord
	for _, r := range records {
		if r.Revision > newest.Revision {
			newest = r
		}
	}
	if newest.Name == "" || !stuckStates[newest.Status] || at.Sub(newest.Changed) < stuckAfter {
		return reconcile.HelmRecord{}, false
	}
	return newest, true
}

// recoverStuck recovers a platform component's chart whose Helm release
// is stuck (stuck): it removes the Helm record of the stuck revision in
// the workload cluster (recordToRemove), with the kubeconfig the platform
// holds for the cluster, so provider-helm's next attempt can install or
// upgrade again. Nothing else in the workload cluster is touched.
//
// At most maxRecoveries times per component and version: after that the
// component is FAILED and says so. Without the setting, or when the
// workload cluster cannot be reached, nothing is removed and the
// component's message says why.
func (s *Reconciler) recoverStuck(ctx context.Context, row *model.Cluster, namespace string, c *cloudv1.PlatformComponent, d *releaseDetail, at time.Time) {
	if !stuck(d, at) {
		return
	}
	what := fmt.Sprintf("chart %s is stuck (Helm state %q since %s)", d.Chart, d.HelmState, d.Since.UTC().Format(time.RFC3339))
	fail := func(why string) {
		c.Phase, c.Message = cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED, what+": "+why
	}
	switch {
	case c.GetRecoveries() >= maxRecoveries:
		fail(fmt.Sprintf("it was recovered %d times and got stuck again; check the chart's hook Jobs and whether the default node pool has room, "+
			"then remove its Helm record (Secret sh.helm.release.v1.%s.v<revision> in namespace %s of the cluster)",
			c.GetRecoveries(), d.Chart, d.TargetNamespace))
		return
	case !s.deps.RecoverStuckReleases || s.deps.Workloads == nil:
		fail("recovering stuck releases is off (--recover-stuck-releases)")
		return
	}
	storage, err := s.deps.Workloads.HelmStorage(ctx, namespace, storedStatus(row).GetKubeconfigSecret())
	if err != nil {
		fail("the cluster cannot be reached to recover it: " + err.Error())
		return
	}
	records, err := storage.Records(ctx, d.TargetNamespace, d.Chart)
	if err != nil {
		fail("the cluster cannot be reached to recover it: " + err.Error())
		return
	}
	record, ok := recordToRemove(records, at)
	if !ok {
		return // Helm's newest revision is not a stuck one (or not for long enough): nothing safe to remove
	}
	if err := storage.Remove(ctx, d.TargetNamespace, record.Name); err != nil {
		fail("its Helm record could not be removed: " + err.Error())
		return
	}
	c.Recoveries = c.GetRecoveries() + 1
	c.Phase = cloudv1.ComponentPhase_COMPONENT_PHASE_INSTALLING
	c.Message = fmt.Sprintf("%s: recovered (%d of %d), removed Helm revision %d (%s); the chart is installed again",
		what, c.GetRecoveries(), maxRecoveries, record.Revision, record.Status)
	s.logger().WarnContext(ctx, "platform component recovered: removed the Helm record of a stuck revision",
		"cluster_id", row.ID, "cluster", row.Name, "tenant_id", row.TenantID, "component", c.GetName(), "version", c.GetVersion(),
		"chart", d.Chart, "helm_state", d.HelmState, "revision", record.Revision, "record", record.Name,
		"recoveries", c.GetRecoveries())
}
