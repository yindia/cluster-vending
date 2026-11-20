package management

import (
	"context"
	"errors"
	"io"
	"slices"
	"testing"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chartutil"
	kubefake "helm.sh/helm/v3/pkg/kube/fake"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage"
	"helm.sh/helm/v3/pkg/storage/driver"
	helmtime "helm.sh/helm/v3/pkg/time"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// memoryHelm is Helm with its records in memory: the SDK's own
// rollback and uninstall run, against no cluster.
func memoryHelm(t *testing.T, revisions map[int]release.Status) (helmOps, *action.Configuration) {
	t.Helper()
	mem := driver.NewMemory()
	mem.SetNamespace("kyverno")
	cfg := &action.Configuration{Releases: storage.Init(mem), KubeClient: &kubefake.PrintingKubeClient{Out: io.Discard},
		Capabilities: chartutil.DefaultCapabilities, Log: func(string, ...any) {}}
	for rev, status := range revisions {
		err := cfg.Releases.Create(&release.Release{Name: "kyverno", Namespace: "kyverno", Version: rev, Info: &release.Info{Status: status, FirstDeployed: helmtime.Now()},
			Chart: &chart.Chart{Metadata: &chart.Metadata{Name: "kyverno", Version: "3.10.0", APIVersion: "v2"}}})
		if err != nil {
			t.Fatal(err)
		}
	}
	return helmOps{configFor: func(string) (*action.Configuration, error) { return cfg, nil }}, cfg
}

func statuses(t *testing.T, cfg *action.Configuration) map[int]release.Status {
	t.Helper()
	history, err := cfg.Releases.History("kyverno")
	if err != nil && !errors.Is(err, driver.ErrReleaseNotFound) {
		t.Fatal(err)
	}
	out := map[int]release.Status{}
	for _, r := range history {
		out[r.Version] = r.Info.Status
	}
	return out
}

// Kyverno on the live cluster: revision 3 left in pending-rollback. It is
// marked failed and the release goes back to revision 2, as a new
// revision, deployed; nothing is left pending.
func TestRollBackFromAStuckRevision(t *testing.T) {
	ops, cfg := memoryHelm(t, map[int]release.Status{1: release.StatusSuperseded, 2: release.StatusDeployed, 3: release.StatusPendingRollback})

	if err := ops.RollBack(t.Context(), "kyverno", "kyverno", 3, 2); err != nil {
		t.Fatal(err)
	}

	got := statuses(t, cfg)
	if got[3] != release.StatusFailed {
		t.Errorf("the stuck revision is %s, want failed", got[3])
	}
	if got[4] != release.StatusDeployed {
		t.Errorf("revisions %v: want a new deployed revision 4", got)
	}
	for rev, status := range got {
		if status.IsPending() {
			t.Errorf("revision %d is still %s", rev, status)
		}
	}
}

// A first install that never finished is uninstalled, history included, so
// the next attempt installs cleanly.
func TestUninstallAFirstInstallThatNeverFinished(t *testing.T) {
	ops, cfg := memoryHelm(t, map[int]release.Status{1: release.StatusPendingInstall})

	if err := ops.Uninstall(t.Context(), "kyverno", "kyverno"); err != nil {
		t.Fatal(err)
	}

	if got := statuses(t, cfg); len(got) != 0 {
		t.Errorf("records left: %v", got)
	}
}

// A release that is already gone is not an error: the next attempt is
// what was wanted.
func TestUninstallOfAGoneReleaseIsFine(t *testing.T) {
	ops, _ := memoryHelm(t, nil)

	if err := ops.Uninstall(t.Context(), "kyverno", "kyverno"); err != nil {
		t.Fatal(err)
	}
}

func TestRollBackOfAMissingRevisionFails(t *testing.T) {
	ops, _ := memoryHelm(t, map[int]release.Status{1: release.StatusDeployed})

	if err := ops.RollBack(t.Context(), "kyverno", "kyverno", 9, 1); err == nil {
		t.Fatal("rolling back a stuck revision that does not exist must fail, not guess")
	}
}

func job(name string, annotations map[string]string, labels map[string]string, failed, succeeded int32, conditions ...batchv1.JobConditionType) *batchv1.Job {
	j := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "gpu-operator", Name: name, Annotations: annotations, Labels: labels},
		Status: batchv1.JobStatus{Failed: failed, Succeeded: succeeded}}
	for _, c := range conditions {
		j.Status.Conditions = append(j.Status.Conditions, batchv1.JobCondition{Type: c, Status: corev1.ConditionTrue})
	}
	return j
}

// The failed hook Jobs of the release, and only those: a Job Helm made as a
// hook, of this release, that failed.
func TestFailedHookJob(t *testing.T) {
	hook := map[string]string{"helm.sh/hook": "pre-upgrade", "meta.helm.sh/release-name": "gpu-operator"}
	for name, tc := range map[string]struct {
		job  *batchv1.Job
		want bool
	}{
		"failed pre-upgrade hook":      {job("gpu-operator-upgrade-crd", hook, nil, 1, 0, batchv1.JobFailed), true},
		"failed hook of the name only": {job("gpu-operator-upgrade-crd", map[string]string{"helm.sh/hook": "pre-upgrade"}, nil, 1, 0, batchv1.JobFailed), true},
		"matched by instance label":    {job("upgrade-crd", map[string]string{"helm.sh/hook": "pre-upgrade"}, map[string]string{"app.kubernetes.io/instance": "gpu-operator"}, 1, 0, batchv1.JobFailed), true},
		"still running":                {job("gpu-operator-upgrade-crd", hook, nil, 0, 0), false},
		"a retry in progress":          {job("gpu-operator-upgrade-crd", hook, nil, 2, 0), false},
		"succeeded":                    {job("gpu-operator-upgrade-crd", hook, nil, 0, 1, batchv1.JobComplete), false},
		"not a hook":                   {job("gpu-operator-batch", nil, nil, 1, 0, batchv1.JobFailed), false},
		"another release's hook":       {job("other-upgrade", map[string]string{"helm.sh/hook": "pre-upgrade", "meta.helm.sh/release-name": "other"}, nil, 1, 0, batchv1.JobFailed), false},
		"another release, same prefix": {job("gpu-operator-extra-hook", map[string]string{"helm.sh/hook": "pre-upgrade", "meta.helm.sh/release-name": "gpu-operator-extra"}, nil, 1, 0, batchv1.JobFailed), false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := failedHookJob(tc.job, "gpu-operator"); got != tc.want {
				t.Errorf("failedHookJob = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDeleteFailedHookJobsDeletesOnlyThose(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = batchv1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	hook := map[string]string{"helm.sh/hook": "pre-upgrade", "meta.helm.sh/release-name": "gpu-operator"}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		job("gpu-operator-upgrade-crd", hook, nil, 1, 0, batchv1.JobFailed),
		job("gpu-operator-running", hook, nil, 0, 0),
		job("workload", nil, nil, 1, 0, batchv1.JobFailed)).Build()
	ops := helmOps{helmStorage: helmStorage{c: c}}

	names, err := ops.DeleteFailedHookJobs(t.Context(), "gpu-operator", "gpu-operator")

	if err != nil || !slices.Equal(names, []string{"gpu-operator-upgrade-crd"}) {
		t.Fatalf("deleted %v (%v)", names, err)
	}
	left := &batchv1.JobList{}
	if err := c.List(t.Context(), left, client.InNamespace("gpu-operator")); err != nil || len(left.Items) != 2 {
		t.Errorf("left %d (%v), want the running hook and the workload's Job", len(left.Items), err)
	}
}

// The workload cluster's Helm is what the reconciler asks for.
var (
	_ reconcile.HelmOperator = helmOps{}
	_ context.Context        = context.Background()
)
