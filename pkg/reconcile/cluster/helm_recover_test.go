package cluster

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// fakeHelm is a workload cluster's Helm: its records and the operations
// done with Helm itself.
type fakeHelm struct {
	fakeWorkloads
	hooks   []string
	hookErr error
	ops     []string
	opErr   error
}

func (f *fakeHelm) HelmStorage(ctx context.Context, namespace, secret string) (reconcile.HelmStorage, error) {
	if _, err := f.fakeWorkloads.HelmStorage(ctx, namespace, secret); err != nil {
		return nil, err
	}
	return f, nil
}

func (f *fakeHelm) RollBack(_ context.Context, namespace, release string, stuck, to int) error {
	f.ops = append(f.ops, "rollback "+namespace+"/"+release+" "+string(rune('0'+stuck))+"->"+string(rune('0'+to)))
	return f.opErr
}

func (f *fakeHelm) Uninstall(_ context.Context, namespace, release string) error {
	f.ops = append(f.ops, "uninstall "+namespace+"/"+release)
	return f.opErr
}

func (f *fakeHelm) DeleteFailedHookJobs(_ context.Context, namespace, release string) ([]string, error) {
	f.ops = append(f.ops, "hooks "+namespace+"/"+release)
	return f.hooks, f.hookErr
}

func record(rev int, status string, changed time.Time) reconcile.HelmRecord {
	return reconcile.HelmRecord{Name: "sh.helm.release.v1.base.v" + string(rune('0'+rev)), Revision: rev, Status: status, Changed: changed}
}

func TestLastDeployed(t *testing.T) {
	at := stuckAt
	for name, tc := range map[string]struct {
		records []reconcile.HelmRecord
		stuck   int
		want    int
	}{
		"the deployed one before an upgrade":       {[]reconcile.HelmRecord{record(1, "superseded", at), record(2, "deployed", at), record(3, "pending-upgrade", at)}, 3, 2},
		"superseded counts: the upgrade had begun": {[]reconcile.HelmRecord{record(1, "superseded", at), record(2, "superseded", at), record(3, "pending-upgrade", at)}, 3, 2},
		"failed revisions are skipped":             {[]reconcile.HelmRecord{record(1, "deployed", at), record(2, "failed", at), record(3, "pending-rollback", at)}, 3, 1},
		"a first install never deployed":           {[]reconcile.HelmRecord{record(1, "pending-install", at)}, 1, 0},
		"only failed before it":                    {[]reconcile.HelmRecord{record(1, "failed", at), record(2, "pending-upgrade", at)}, 2, 0},
		"never a newer one":                        {[]reconcile.HelmRecord{record(1, "deployed", at), record(2, "pending-upgrade", at), record(4, "deployed", at)}, 2, 1},
	} {
		t.Run(name, func(t *testing.T) {
			if got := lastDeployed(tc.records, tc.stuck); got != tc.want {
				t.Errorf("lastDeployed = %d, want %d", got, tc.want)
			}
		})
	}
}

// stuckWorld is a base Release Helm reports in state with the error, since
// `since`, with the Helm's records.
func stuckWorld(t *testing.T, helm *fakeHelm, state, message string, since time.Time, recoveries int32) (world, *platform) {
	t.Helper()
	atStuckTime(t)
	noJitter(t)
	w, _ := platformWorld(t, func(d *reconcile.Deps) { d.Workloads, d.RecoverStuckReleases = helm, true })
	w.helmConfig(t)
	w.install(t)
	w.failing(t, "base", state, message, since, 1)
	row := w.withStored(t, &cloudv1.PlatformComponent{Name: "base", Version: "1.2.0",
		Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED, Recoveries: recoveries})
	p, err := w.svc.installPlatform(reconcile.WithNotes(t.Context()), row)
	if err != nil {
		t.Fatal(err)
	}
	return w, p
}

// Kyverno on the live cluster: pending-rollback, "another operation is in
// progress". Rolled back to the last deployed revision, with Helm.
func TestStuckPendingIsRolledBackToTheLastDeployed(t *testing.T) {
	old := stuckAt.Add(-13 * time.Minute)
	helm := &fakeHelm{fakeWorkloads: fakeWorkloads{records: []reconcile.HelmRecord{
		record(1, "superseded", old), record(2, "deployed", old), record(3, "pending-rollback", old)}}}

	_, p := stuckWorld(t, helm, "pending-rollback", kyvernoLock, old, 0)

	if got := strings.Join(helm.ops, ";"); got != "hooks base-system/base;rollback base-system/base 3->2" {
		t.Fatalf("ops %q", got)
	}
	if len(helm.removed) != 0 {
		t.Errorf("records removed %v: with Helm the record is not touched", helm.removed)
	}
	base := componentOf(p, "base")
	if base.GetRecoveries() != 1 || base.GetPhase() != cloudv1.ComponentPhase_COMPONENT_PHASE_INSTALLING ||
		!strings.Contains(base.GetLastRecovery(), "rolled back to revision 2") || base.GetLastRecoveryAt().AsTime() != stuckAt {
		t.Errorf("base = %s %d %q %v", base.GetPhase(), base.GetRecoveries(), base.GetLastRecovery(), base.GetLastRecoveryAt())
	}
}

// A first install that never finished has nothing to go back to: it is
// uninstalled so the next attempt installs cleanly.
func TestStuckFirstInstallIsUninstalled(t *testing.T) {
	old := stuckAt.Add(-13 * time.Minute)
	helm := &fakeHelm{fakeWorkloads: fakeWorkloads{records: []reconcile.HelmRecord{record(1, "pending-install", old)}}}

	_, p := stuckWorld(t, helm, "pending-install", "create failed: another operation (install/upgrade/rollback) is in progress", old, 0)

	if got := strings.Join(helm.ops, ";"); got != "hooks base-system/base;uninstall base-system/base" {
		t.Fatalf("ops %q", got)
	}
	if base := componentOf(p, "base"); base.GetRecoveries() != 1 || !strings.Contains(base.GetLastRecovery(), "uninstalled") {
		t.Errorf("base = %d %q", base.GetRecoveries(), base.GetLastRecovery())
	}
}

// A failed hook Job left behind is deleted before the retry and named in
// what the recovery did.
func TestRecoveryDeletesTheFailedHookJob(t *testing.T) {
	old := stuckAt.Add(-13 * time.Minute)
	helm := &fakeHelm{hooks: []string{"gpu-operator-upgrade-crd"}, fakeWorkloads: fakeWorkloads{records: []reconcile.HelmRecord{
		record(1, "deployed", old), record(2, "pending-upgrade", old)}}}

	_, p := stuckWorld(t, helm, "pending-upgrade", gpuErr, old, 0)

	if base := componentOf(p, "base"); !strings.Contains(base.GetLastRecovery(), "deleted failed hook Job gpu-operator-upgrade-crd") {
		t.Errorf("last recovery %q", base.GetLastRecovery())
	}
}

// Younger than the Release's timeout plus two minutes is an operation
// that may still be running: left alone.
func TestRecoveryWaitsForTheTimeoutPlusTwoMinutes(t *testing.T) {
	young := stuckAt.Add(-11 * time.Minute) // the Release waits 10m
	helm := &fakeHelm{fakeWorkloads: fakeWorkloads{records: []reconcile.HelmRecord{
		record(1, "deployed", young), record(2, "pending-upgrade", young)}}}

	_, p := stuckWorld(t, helm, "pending-upgrade", kyvernoLock, young, 0)

	if len(helm.ops) != 0 || componentOf(p, "base").GetRecoveries() != 0 {
		t.Errorf("ops %v recoveries %d", helm.ops, componentOf(p, "base").GetRecoveries())
	}
}

// The recovery that fails says so on the component, counts nothing and
// does not touch the workload cluster further.
func TestRecoveryThatFailsIsReported(t *testing.T) {
	old := stuckAt.Add(-13 * time.Minute)
	helm := &fakeHelm{opErr: errors.New("rollback: timed out waiting for the condition"), fakeWorkloads: fakeWorkloads{records: []reconcile.HelmRecord{
		record(1, "deployed", old), record(2, "pending-upgrade", old)}}}

	_, p := stuckWorld(t, helm, "pending-upgrade", kyvernoLock, old, 0)

	base := componentOf(p, "base")
	if base.GetRecoveries() != 0 || base.GetPhase() != cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED ||
		!strings.Contains(base.GetMessage(), "the rollback to revision 1 failed: rollback: timed out") {
		t.Errorf("base = %s %d %q", base.GetPhase(), base.GetRecoveries(), base.GetMessage())
	}
}

// A hook-failed retry deletes the failed hook Job before the pause ends.
func TestHookJobIsDeletedBeforeTheRetry(t *testing.T) {
	atStuckTime(t)
	noJitter(t)
	helm := &fakeHelm{hooks: []string{"gpu-operator-upgrade-crd"}}
	w, _ := platformWorld(t, func(d *reconcile.Deps) { d.Workloads, d.RecoverStuckReleases = helm, true })
	w.helmConfig(t)
	w.install(t)
	w.failing(t, "base", "failed", gpuErr, stuckAt.Add(-time.Second), 1)
	first, _ := w.helmPass(t, w.cluster(t))
	now = func() time.Time { return stuckAt.Add(2 * time.Minute) }

	w.helmPass(t, w.again(t, first))

	if got := strings.Join(helm.ops, ";"); !strings.Contains(got, "hooks base-system/base") {
		t.Errorf("ops %q: the failed hook Job must go before the retry", got)
	}
}

// Transient: a failure the workload API caused is not something to clean up
// after.
func TestTransientFailureDeletesNoHookJob(t *testing.T) {
	atStuckTime(t)
	noJitter(t)
	helm := &fakeHelm{}
	w, _ := platformWorld(t, func(d *reconcile.Deps) { d.Workloads, d.RecoverStuckReleases = helm, true })
	w.helmConfig(t)
	w.install(t)
	w.failing(t, "base", "deployed", capsuleErr, stuckAt.Add(-time.Second), 1)
	first, _ := w.helmPass(t, w.cluster(t))
	now = func() time.Time { return stuckAt.Add(2 * time.Minute) }

	w.helmPass(t, w.again(t, first))

	if len(helm.ops) != 0 {
		t.Errorf("ops %v", helm.ops)
	}
}
