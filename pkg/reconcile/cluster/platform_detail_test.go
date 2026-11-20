package cluster

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/crossplane/managed"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/helmrel"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// stuckAt is the clock of the tests below.
var stuckAt = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

func atStuckTime(t *testing.T) {
	t.Helper()
	now = func() time.Time { return stuckAt }
	t.Cleanup(func() { now = time.Now })
}

// failing marks a Release as provider-helm does when Helm fails: not
// synced since `since`, with Helm's state, the error and the failures.
func (w world) failing(t *testing.T, name, helmState, message string, since time.Time, failures int64) {
	t.Helper()
	u := w.release(t, name)
	if u == nil {
		t.Fatalf("no Release %s", name)
	}
	u.Object["status"] = map[string]any{
		"atProvider": map[string]any{"state": helmState},
		"failed":     failures,
		"conditions": []any{
			map[string]any{"type": "Ready", "status": "False", "reason": "Unavailable", "lastTransitionTime": since.Format(time.RFC3339)},
			map[string]any{"type": "Synced", "status": "False", "reason": "ReconcileError", "message": message,
				"lastTransitionTime": since.Format(time.RFC3339)},
		},
	}
	if err := w.kube.Update(t.Context(), u); err != nil {
		t.Fatal(err)
	}
}

// with records the given components as the cluster's stored status.
func (w world) withStored(t *testing.T, components ...*cloudv1.PlatformComponent) *model.Cluster {
	t.Helper()
	row := w.cluster(t)
	st := &cloudv1.ClusterStatus{}
	withPlatform(st, &platform{release: "2026-11-a", components: components})
	raw, err := Encode(st)
	if err != nil {
		t.Fatal(err)
	}
	row.Status = raw
	return row
}

// A failed component says which chart failed, Helm's state, the
// provider's whole error, how often it failed and since when.
func TestPlatformComponentSaysWhyItFailed(t *testing.T) {
	atStuckTime(t)
	w, _ := platformWorld(t)
	w.helmConfig(t)
	w.install(t)
	long := "create failed: failed to install release: " + strings.Repeat("x", 3000)
	w.failing(t, "base", "failed", long, stuckAt.Add(-time.Minute), 4)

	p := w.install(t)

	base := p.components[0]
	if base.GetPhase() != cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED || base.GetChart() != "base" ||
		base.GetHelmState() != "failed" || base.GetLastError() != long || base.GetFailures() != 4 ||
		!base.GetSince().AsTime().Equal(stuckAt) {
		t.Fatalf("base = %+v", base)
	}
	if !strings.Contains(base.GetMessage(), long) {
		t.Errorf("the message is cut: %d bytes", len(base.GetMessage()))
	}
	if mesh := p.components[1]; mesh.GetChart() != "" || mesh.GetHelmState() != "" {
		t.Errorf("mesh was never started: %+v", mesh)
	}
}

// since is when the phase was entered: kept while the phase stays, new
// when it changes.
func TestPlatformComponentSince(t *testing.T) {
	atStuckTime(t)
	earlier := timestamppb.New(stuckAt.Add(-time.Hour))
	tests := []struct {
		name   string
		stored cloudv1.ComponentPhase
		want   time.Time
	}{
		{"the phase stays", cloudv1.ComponentPhase_COMPONENT_PHASE_INSTALLING, earlier.AsTime()},
		{"the phase changed", cloudv1.ComponentPhase_COMPONENT_PHASE_PENDING, stuckAt},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, _ := platformWorld(t)
			w.helmConfig(t)
			row := w.withStored(t, &cloudv1.PlatformComponent{Name: "base", Version: "1.2.0", Phase: tt.stored, Since: earlier})

			p, err := w.svc.installPlatform(t.Context(), row)

			if err != nil || !p.components[0].GetSince().AsTime().Equal(tt.want) {
				t.Fatalf("base since %v (%v), want %v", p.components[0].GetSince().AsTime(), err, tt.want)
			}
		})
	}
}

// A phase change is logged once; a pass that changed nothing logs nothing.
func TestPlatformChangesAreLoggedOnce(t *testing.T) {
	pc := func(name string, phase cloudv1.ComponentPhase, message string) *cloudv1.PlatformComponent {
		return &cloudv1.PlatformComponent{Name: name, Version: "1.0.0", Phase: phase, Message: message}
	}
	const (
		pending    = cloudv1.ComponentPhase_COMPONENT_PHASE_PENDING
		installing = cloudv1.ComponentPhase_COMPONENT_PHASE_INSTALLING
		ready      = cloudv1.ComponentPhase_COMPONENT_PHASE_READY
		failed     = cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED
	)
	tests := []struct {
		name          string
		before, after []*cloudv1.PlatformComponent
		want          []string // each must be in one log line
		lines         int
	}{
		{"nothing changed", []*cloudv1.PlatformComponent{pc("base", ready, "")}, []*cloudv1.PlatformComponent{pc("base", ready, "")}, nil, 0},
		{"a new message in the same phase", []*cloudv1.PlatformComponent{pc("base", failed, "a")}, []*cloudv1.PlatformComponent{pc("base", failed, "b")}, nil, 0},
		{"failed", []*cloudv1.PlatformComponent{pc("base", installing, "")}, []*cloudv1.PlatformComponent{pc("base", failed, "chart base: boom")},
			[]string{`level=WARN msg="platform component failed"`, "component=base", "version=1.0.0", `message="chart base: boom"`,
				"cluster_id=", "cluster=prod", "tenant_id=acme", "reason=Failed"}, 2},
		{"installing, then ready", []*cloudv1.PlatformComponent{pc("base", pending, ""), pc("mesh", installing, "")},
			[]*cloudv1.PlatformComponent{pc("base", installing, ""), pc("mesh", ready, "")},
			[]string{`level=INFO msg="platform component installing"`, `level=INFO msg="platform component ready"`}, 2},
		{"first seen pending", nil, []*cloudv1.PlatformComponent{pc("base", pending, "waiting")},
			[]string{`msg="platform components condition changed"`, "status=False", "reason=Installing"}, 1},
		{"removed", []*cloudv1.PlatformComponent{pc("base", ready, ""), pc("old", ready, "")}, []*cloudv1.PlatformComponent{pc("base", ready, "")},
			[]string{`msg="platform component removed"`, "component=old"}, 1},
		{"all ready", []*cloudv1.PlatformComponent{pc("base", installing, "")}, []*cloudv1.PlatformComponent{pc("base", ready, "")},
			[]string{`msg="platform component ready"`, "status=True", "reason=Installed"}, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			s := &Reconciler{deps: reconcile.Deps{Logger: slog.New(slog.NewTextHandler(&out, nil))}}
			row := &model.Cluster{TenantID: "acme", Name: "prod"}
			row.ID = "01a1d2f6-12a9-7505-b7b2-8b8acfcab306"

			s.logPlatform(t.Context(), row, &platform{components: tt.before}, &platform{components: tt.after})

			if got := strings.Count(out.String(), "\n"); got != tt.lines {
				t.Fatalf("%d log lines, want %d:\n%s", got, tt.lines, out.String())
			}
			for _, want := range tt.want {
				if !strings.Contains(out.String(), want) {
					t.Errorf("log lacks %s:\n%s", want, out.String())
				}
			}
		})
	}
}

// A pass that could not look (nil) logs nothing.
func TestPlatformNothingFoundOutLogsNothing(t *testing.T) {
	var out bytes.Buffer
	s := &Reconciler{deps: reconcile.Deps{Logger: slog.New(slog.NewTextHandler(&out, nil))}}

	s.logPlatform(t.Context(), &model.Cluster{}, &platform{}, nil)

	if out.Len() != 0 {
		t.Fatalf("logged %s", out.String())
	}
}

func TestStuck(t *testing.T) {
	old, fresh := stuckAt.Add(-11*time.Minute), stuckAt.Add(-9*time.Minute)
	tests := []struct {
		name string
		d    *releaseDetail
		want bool
	}{
		{"no detail", nil, false},
		{"uninstalling for long", &releaseDetail{HelmState: "uninstalling", Since: old}, true},
		{"pending-install for long", &releaseDetail{HelmState: "pending-install", Since: old}, true},
		{"pending-upgrade for long", &releaseDetail{HelmState: "pending-upgrade", Since: old}, true},
		{"pending-rollback for long", &releaseDetail{HelmState: "pending-rollback", Since: old}, true},
		{"uninstalling, not long enough", &releaseDetail{HelmState: "uninstalling", Since: fresh}, false},
		{"no deployed releases", &releaseDetail{HelmState: "failed", Since: old,
			LastError: `update failed: failed to upgrade release: "kyverno" has no deployed releases`}, true},
		{"another operation in progress", &releaseDetail{Since: old,
			LastError: "another operation (install/upgrade/rollback) is in progress"}, true},
		{"failed for another reason", &releaseDetail{HelmState: "failed", Since: old, LastError: "failed to pull chart"}, false},
		{"deployed", &releaseDetail{HelmState: "deployed"}, false},
		{"stuck state, since unknown", &releaseDetail{HelmState: "uninstalling"}, false},
		// A paused Release (a provider upgrade in progress) is the operator's:
		// nothing is removed in the workload cluster while it is paused.
		{"uninstalling for long, but paused", &releaseDetail{HelmState: "uninstalling", Since: old, Paused: true}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := stuck(tt.d, stuckAt); got != tt.want {
				t.Fatalf("stuck = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRecordToRemove(t *testing.T) {
	old, fresh := stuckAt.Add(-11*time.Minute), stuckAt.Add(-time.Minute)
	rec := func(revision int, status string, changed time.Time) reconcile.HelmRecord {
		return reconcile.HelmRecord{Name: "sh.helm.release.v1.kyverno.v" + string(rune('0'+revision)), Revision: revision, Status: status, Changed: changed}
	}
	tests := []struct {
		name    string
		records []reconcile.HelmRecord
		want    string // name of the record to remove, "" for none
	}{
		{"no records", nil, ""},
		{"a first install stuck uninstalling", []reconcile.HelmRecord{rec(1, "uninstalling", old)}, "sh.helm.release.v1.kyverno.v1"},
		{"a first install stuck pending", []reconcile.HelmRecord{rec(1, "pending-install", old)}, "sh.helm.release.v1.kyverno.v1"},
		{"an upgrade stuck pending: only the pending revision", []reconcile.HelmRecord{rec(1, "superseded", old), rec(2, "deployed", old), rec(3, "pending-upgrade", old)},
			"sh.helm.release.v1.kyverno.v3"},
		{"a rollback stuck pending", []reconcile.HelmRecord{rec(2, "deployed", old), rec(3, "pending-rollback", old)}, "sh.helm.release.v1.kyverno.v3"},
		{"deployed is never removed", []reconcile.HelmRecord{rec(1, "deployed", old)}, ""},
		{"superseded is never removed", []reconcile.HelmRecord{rec(1, "superseded", old)}, ""},
		{"failed is never removed", []reconcile.HelmRecord{rec(1, "failed", old)}, ""},
		{"an older stuck revision under a deployed one stays", []reconcile.HelmRecord{rec(1, "pending-install", old), rec(2, "deployed", old)}, ""},
		{"an operation still running is left alone", []reconcile.HelmRecord{rec(1, "pending-install", fresh)}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := recordToRemove(tt.records, stuckAt)

			if got.Name != tt.want || ok != (tt.want != "") {
				t.Fatalf("recordToRemove = %q, %v; want %q", got.Name, ok, tt.want)
			}
		})
	}
}

// fakeWorkloads is a workload cluster's Helm storage.
type fakeWorkloads struct {
	err       error // of opening the storage
	records   []reconcile.HelmRecord
	removed   []string
	namespace string // the management namespace the kubeconfig was asked in
	looked    string // "<workload namespace>/<release>" the records were listed for
}

func (f *fakeWorkloads) HelmStorage(_ context.Context, namespace, _ string) (reconcile.HelmStorage, error) {
	f.namespace = namespace
	if f.err != nil {
		return nil, f.err
	}
	return f, nil
}

func (f *fakeWorkloads) Records(_ context.Context, namespace, release string) ([]reconcile.HelmRecord, error) {
	f.looked = namespace + "/" + release
	return f.records, nil
}

func (f *fakeWorkloads) Remove(_ context.Context, _, name string) error {
	f.removed = append(f.removed, name)
	return nil
}

// A component whose Helm release is stuck is recovered: the stuck
// revision's record is removed in the workload cluster, and nothing else.
func TestStuckReleaseIsRecovered(t *testing.T) {
	old := stuckAt.Add(-13 * time.Minute) // a Release of this build waits 10m: stuck after 12m
	const noDeployed = `update failed: failed to upgrade release: "base" has no deployed releases`
	stuckRecord := reconcile.HelmRecord{Name: "sh.helm.release.v1.base.v1", Revision: 1, Status: "uninstalling", Changed: old}
	tests := []struct {
		name        string
		since       time.Time
		recoveries  int32
		off         bool
		workloads   *fakeWorkloads
		wantRemoved []string
		wantPhase   cloudv1.ComponentPhase
		wantCount   int32
		wantMessage string
	}{
		{name: "stuck uninstalling", since: old, workloads: &fakeWorkloads{records: []reconcile.HelmRecord{stuckRecord}},
			wantRemoved: []string{"sh.helm.release.v1.base.v1"}, wantPhase: cloudv1.ComponentPhase_COMPONENT_PHASE_INSTALLING,
			wantCount: 1, wantMessage: "recovered (1 of 3), removed Helm revision 1 (uninstalling)"},
		{name: "not stuck for long enough", since: stuckAt.Add(-time.Minute), workloads: &fakeWorkloads{records: []reconcile.HelmRecord{stuckRecord}},
			wantPhase: cloudv1.ComponentPhase_COMPONENT_PHASE_INSTALLING, wantMessage: "has no deployed releases"}, // a stuck state is a transient error until it is recovered
		{name: "the newest revision is deployed", since: old,
			workloads: &fakeWorkloads{records: []reconcile.HelmRecord{{Name: "sh.helm.release.v1.base.v1", Revision: 1, Status: "deployed", Changed: old}}},
			wantPhase: cloudv1.ComponentPhase_COMPONENT_PHASE_INSTALLING, wantMessage: "has no deployed releases"}, // a stuck state is a transient error until it is recovered
		{name: "recovered three times already", since: old, recoveries: 3, workloads: &fakeWorkloads{records: []reconcile.HelmRecord{stuckRecord}},
			wantPhase: cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED, wantCount: 3, wantMessage: "it was recovered 3 times and got stuck again"},
		{name: "the setting is off", since: old, off: true, workloads: &fakeWorkloads{records: []reconcile.HelmRecord{stuckRecord}},
			wantPhase: cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED, wantMessage: "recovering stuck releases is off"},
		{name: "the cluster cannot be reached", since: old, workloads: &fakeWorkloads{err: errors.New("secret prod-kubeconfig not found")},
			wantPhase: cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED, wantMessage: "the cluster cannot be reached to recover it: secret prod-kubeconfig not found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			atStuckTime(t)
			var log bytes.Buffer
			w, _ := platformWorld(t, func(d *reconcile.Deps) {
				d.Workloads, d.RecoverStuckReleases = tt.workloads, !tt.off
				d.Logger = slog.New(slog.NewTextHandler(&log, nil))
			})
			w.helmConfig(t)
			w.install(t)
			w.failing(t, "base", "uninstalling", noDeployed, tt.since, 7)
			row := w.withStored(t, &cloudv1.PlatformComponent{Name: "base", Version: "1.2.0",
				Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED, Recoveries: tt.recoveries})

			p, err := w.svc.installPlatform(t.Context(), row)

			if err != nil {
				t.Fatal(err)
			}
			base := p.components[0]
			if strings.Join(tt.workloads.removed, ",") != strings.Join(tt.wantRemoved, ",") {
				t.Errorf("removed %v, want %v", tt.workloads.removed, tt.wantRemoved)
			}
			if base.GetPhase() != tt.wantPhase || base.GetRecoveries() != tt.wantCount || !strings.Contains(base.GetMessage(), tt.wantMessage) {
				t.Errorf("base = %s, %d recoveries, %q; want %s, %d, a message with %q", base.GetPhase(), base.GetRecoveries(),
					base.GetMessage(), tt.wantPhase, tt.wantCount, tt.wantMessage)
			}
			recovered := strings.Contains(log.String(), "platform component recovered")
			if recovered != (len(tt.wantRemoved) > 0) {
				t.Errorf("recovery logged = %v:\n%s", recovered, log.String())
			}
			if len(tt.wantRemoved) > 0 && (tt.workloads.namespace != worldNS || tt.workloads.looked != "base-system/base") {
				t.Errorf("looked in %q for %q", tt.workloads.namespace, tt.workloads.looked)
			}
		})
	}
}

// The recoveries are counted per version: a new version starts again.
func TestRecoveriesStartAgainWithANewVersion(t *testing.T) {
	atStuckTime(t)
	w, _ := platformWorld(t)
	w.helmConfig(t)
	row := w.withStored(t, &cloudv1.PlatformComponent{Name: "base", Version: "1.1.0", Recoveries: 3})

	p, err := w.svc.installPlatform(t.Context(), row)

	if err != nil || p.components[0].GetRecoveries() != 0 {
		t.Fatalf("recoveries = %d, %v; want 0 for 1.2.0", p.components[0].GetRecoveries(), err)
	}
}

// A cluster with a stuck component is looked at again soon, although it
// is RUNNING: the recovery waits for stuckAfter, and no event comes.
func TestPlatformStuck(t *testing.T) {
	tests := []struct {
		name string
		c    *cloudv1.PlatformComponent
		want bool
	}{
		{"ready", &cloudv1.PlatformComponent{Name: "base", Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_READY, HelmState: "deployed"}, false},
		{"failed for another reason", &cloudv1.PlatformComponent{Name: "base", HelmState: "failed", LastError: "failed to pull chart"}, false},
		{"uninstalling", &cloudv1.PlatformComponent{Name: "base", HelmState: "uninstalling"}, true},
		{"pending-upgrade", &cloudv1.PlatformComponent{Name: "base", HelmState: "pending-upgrade"}, true},
		{"no deployed releases", &cloudv1.PlatformComponent{Name: "base", LastError: `"base" has no deployed releases`}, true},
		{"given up", &cloudv1.PlatformComponent{Name: "base", HelmState: "uninstalling", Recoveries: 3}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := &cloudv1.ClusterStatus{PlatformComponents: []*cloudv1.PlatformComponent{tt.c}}
			raw, err := Encode(st)
			if err != nil {
				t.Fatal(err)
			}
			row := &model.Cluster{}
			row.Status = raw

			if got := PlatformStuck(row); got != tt.want {
				t.Fatalf("PlatformStuck = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDetailOfReadsThePause(t *testing.T) {
	w, _ := platformWorld(t)
	w.helmConfig(t)
	w.install(t)
	u := w.release(t, "base")
	u.SetAnnotations(map[string]string{managed.PausedAnnotation: "true"})

	d := detailOf(u)

	if d == nil || !d.Paused || d.PausedByUs {
		t.Fatalf("detail = %+v, want paused by someone else", d)
	}
	// With the platform's marker the pause is the platform's own backoff.
	until := stuckAt.Add(time.Minute)
	u.SetAnnotations(map[string]string{managed.PausedAnnotation: "true", helmrel.BackoffAnnotation: until.Format(time.RFC3339)})
	d = detailOf(u)
	if d.Paused || !d.PausedByUs || !d.BackoffUntil.Equal(until) {
		t.Fatalf("detail = %+v, want paused by us until %v", d, until)
	}
}

func TestDetailOfReadsWhatProviderHelmWrites(t *testing.T) {
	w, _ := platformWorld(t)
	w.helmConfig(t)
	w.install(t)
	since := stuckAt.Add(-time.Hour)
	w.failing(t, "base", "pending-upgrade", "boom", since, 2)

	d := detailOf(w.release(t, "base"))

	want := releaseDetail{Chart: "base", TargetNamespace: "base-system", HelmState: "pending-upgrade", LastError: "boom", Failures: 2, Since: since,
		Digest: w.release(t, "base").GetAnnotations()[helmrel.DigestAnnotation], Timeout: crossplaneaws.DefaultHelmTimeout}
	if d == nil || *d != want {
		t.Fatalf("detail = %+v, want %+v", d, want)
	}
	if got := crossplaneaws.HelmReleaseGVK.Kind; got != "Release" {
		t.Fatalf("kind %s", got)
	}
}
