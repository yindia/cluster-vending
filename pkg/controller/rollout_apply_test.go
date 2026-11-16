package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/database/storetest"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/kube"
	platform "github.com/yindia/cluster-vending/pkg/reconcile"
	reconcilecluster "github.com/yindia/cluster-vending/pkg/reconcile/cluster"
)

const upgradeTenant = "acme"

// upgradeWorld is a management cluster with the stable channel offering
// 1.36 and, through the store, a tier 2 automatic cluster on 1.35 per
// name, each Ready with a clean scan against 1.36.
type upgradeWorld struct {
	k     client.WithWatch
	store repository.Store
	ids   map[string]string
}

func newUpgradeWorld(t *testing.T, policy cloudv1alpha1.RolloutPolicy, names ...string) upgradeWorld {
	t.Helper()
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	rel := &cloudv1alpha1.Release{ObjectMeta: metav1.ObjectMeta{Name: "shipped-00000001"},
		Spec: cloudv1alpha1.ReleaseSpec{Content: cloudv1alpha1.ReleaseContent{Kubernetes: []cloudv1alpha1.KubernetesRelease{
			{Minor: "1.38", Channel: "alpha"}, {Minor: "1.37", Channel: "beta"}, {Minor: "1.36", Channel: "stable"}, {Minor: "1.35"},
		}}}}
	stable := &cloudv1alpha1.ReleaseChannel{ObjectMeta: metav1.ObjectMeta{Name: "stable"},
		Spec:   cloudv1alpha1.ReleaseChannelSpec{Promotion: cloudv1alpha1.PromotionAutomatic, Policy: policy},
		Status: cloudv1alpha1.ReleaseChannelStatus{Release: rel.Name, KubernetesMinor: "1.36"}}
	k := fake.NewClientBuilder().WithScheme(scheme).WithObjects(rel, stable).
		WithStatusSubresource(&cloudv1alpha1.CloudAccount{}, &cloudv1alpha1.Network{}, &cloudv1alpha1.Cluster{},
			&cloudv1alpha1.Component{}, &cloudv1alpha1.Release{}, &cloudv1alpha1.ReleaseChannel{}, &cloudv1alpha1.ReleaseHold{}).Build()
	w := upgradeWorld{k: k, store: cloudstore.New(k), ids: map[string]string{}}
	storetest.MustTenant(t, w.store, upgradeTenant)
	network := storetest.MustNetwork(t, w.store, upgradeTenant, "net")
	networkObject := &cloudv1alpha1.Network{}
	if err := k.Get(t.Context(), client.ObjectKey{Namespace: "tenant-" + upgradeTenant, Name: "net"}, networkObject); err != nil {
		t.Fatal(err)
	}
	networkObject.Spec.Tier = cloudv1alpha1.ClusterTier2
	if err := k.Update(t.Context(), networkObject); err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		spec, err := protojson.Marshal(&cloudv1.ClusterSpec{Region: "us-east-1",
			Network:        &cloudv1.ClusterNetwork{NetworkId: network.ID},
			ControlPlane:   &cloudv1.ControlPlaneSpec{KubernetesVersion: "1.35"},
			ReleaseChannel: cloudv1.ReleaseChannel_RELEASE_CHANNEL_STABLE,
			UpgradeMode:    cloudv1.UpgradeMode_UPGRADE_MODE_AUTOMATIC,
			Provider:       &cloudv1.ClusterSpec_Aws{Aws: &cloudv1.AwsClusterSpec{}}})
		if err != nil {
			t.Fatal(err)
		}
		row := &model.Cluster{TenantID: upgradeTenant, Name: name, NetworkID: network.ID, Provider: "aws", Region: "us-east-1",
			Resource:           model.Resource{State: reconcilecluster.StateRunning, Spec: spec, Generation: 1, AppliedGeneration: 1},
			ResolvedGeneration: 1}
		if err := w.store.Clusters().Create(t.Context(), row); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		w.ids[name] = row.ID
		w.setStatus(t, name, func(st *cloudv1alpha1.ClusterStatus) {
			ready := metav1.NewTime(rolloutNow.Add(-48 * time.Hour))
			scanned := metav1.NewTime(rolloutNow.Add(-time.Hour))
			st.Phase, st.CurrentKubernetesVersion = cloudv1alpha1.ClusterPhaseRunning, "1.35"
			st.Conditions = []metav1.Condition{
				{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Ready", LastTransitionTime: ready},
				{Type: cloudv1alpha1.ConditionPlatformComponentsReady, Status: metav1.ConditionTrue, Reason: "Ready", LastTransitionTime: ready}}
			st.Deprecations = &cloudv1alpha1.ClusterDeprecations{TargetVersion: "1.36", ScannedAt: &scanned}
		})
	}
	return w
}

func (w upgradeWorld) object(t *testing.T, name string) *cloudv1alpha1.Cluster {
	t.Helper()
	c := &cloudv1alpha1.Cluster{}
	if err := w.k.Get(t.Context(), client.ObjectKey{Namespace: kube.TenantNamespace(upgradeTenant), Name: name}, c); err != nil {
		t.Fatal(err)
	}
	return c
}

func (w upgradeWorld) setStatus(t *testing.T, name string, set func(*cloudv1alpha1.ClusterStatus)) {
	t.Helper()
	c := w.object(t, name)
	set(&c.Status)
	if err := w.k.Status().Update(t.Context(), c); err != nil {
		t.Fatal(err)
	}
}

// version is the Kubernetes version the cluster's stored spec asks for.
func (w upgradeWorld) version(t *testing.T, name string) string {
	t.Helper()
	row, err := w.store.Clusters().Get(t.Context(), upgradeTenant, w.ids[name])
	if err != nil {
		t.Fatal(err)
	}
	spec, err := reconcilecluster.SpecOf(row)
	if err != nil {
		t.Fatal(err)
	}
	return spec.GetControlPlane().GetKubernetesVersion()
}

type upgradeEvent struct{ kind, name, reason, message string }

type upgradeEvents struct{ seen []upgradeEvent }

func (e *upgradeEvents) Event(_ context.Context, obj platform.Object, _, reason, message string) {
	e.seen = append(e.seen, upgradeEvent{obj.Kind, obj.Name, reason, message})
}

func (e *upgradeEvents) count(reason string) int {
	n := 0
	for _, ev := range e.seen {
		if ev.reason == reason {
			n++
		}
	}
	return n
}

// pass runs one rollout pass at now; plan is the client the pass plans
// with (the manager's cache), the store writes through w's.
func (w upgradeWorld) pass(t *testing.T, automatic bool, plan client.Client, now time.Time) *upgradeEvents {
	t.Helper()
	events := &upgradeEvents{}
	r := &RolloutReconciler{c: plan, store: w.store, automatic: automatic, now: func() time.Time { return now }, events: events}
	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKey{Name: "stable"}}); err != nil {
		t.Fatal(err)
	}
	return events
}

var twoAtOnce = cloudv1alpha1.RolloutPolicy{MaxInProgress: 2, MaxFailed: 1, Soak: metav1.Duration{Duration: time.Hour}}

func TestAutomaticUpgradeRechecksNetworkTier(t *testing.T) {
	w := newUpgradeWorld(t, twoAtOnce, "a")
	network := &cloudv1alpha1.Network{}
	key := client.ObjectKey{Namespace: kube.TenantNamespace(upgradeTenant), Name: "net"}
	if err := w.k.Get(t.Context(), key, network); err != nil {
		t.Fatal(err)
	}
	network.Spec.Tier = cloudv1alpha1.ClusterTier0
	if err := w.k.Update(t.Context(), network); err != nil {
		t.Fatal(err)
	}
	stale := interceptor.NewClient(w.k, interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		if err := c.List(ctx, list, opts...); err != nil {
			return err
		}
		if networks, ok := list.(*cloudv1alpha1.NetworkList); ok {
			for i := range networks.Items {
				networks.Items[i].Spec.Tier = cloudv1alpha1.ClusterTier2
			}
		}
		return nil
	}})
	events := w.pass(t, true, stale, rolloutNow)
	if w.version(t, "a") != "1.35" || events.count(platform.ReasonAutomaticUpgradeStarted) != 0 {
		t.Fatal("a stale network tier admitted an automatic upgrade")
	}
	// A fresh plan can use the new tier and proceed.
	events = w.pass(t, true, w.k, rolloutNow)
	if w.version(t, "a") != "1.36" || events.count(platform.ReasonAutomaticUpgradeStarted) == 0 {
		t.Fatal("a fresh plan did not use the new network tier")
	}
}

// With automatic upgrades on, the plan's clusters get their next minor
// once, through the store, with a record and an event; the plan's limit
// holds on the next pass, before any cluster reports an upgrade.
func TestAutomaticUpgradeStartsThePlansHops(t *testing.T) {
	w := newUpgradeWorld(t, twoAtOnce, "a", "b", "c")

	events := w.pass(t, true, w.k, rolloutNow)

	for _, name := range []string{"a", "b"} {
		if v := w.version(t, name); v != "1.36" {
			t.Errorf("%s asks for %s, want 1.36", name, v)
		}
		rec := w.object(t, name).Status.Rollout
		if rec == nil || rec.Result != cloudv1alpha1.ClusterRolloutInProgress || rec.FromVersion != "1.35" || rec.ToVersion != "1.36" ||
			rec.Channel != "stable" || rec.Tier != 2 || !rec.StartTime.Time.Equal(rolloutNow) {
			t.Errorf("%s record = %+v", name, rec)
		}
	}
	if v := w.version(t, "c"); v != "1.35" {
		t.Errorf("c asks for %s: more than maxInProgress started", v)
	}
	if rec := w.object(t, "c").Status.Rollout; rec == nil || rec.Result != cloudv1alpha1.ClusterRolloutPending ||
		!strings.Contains(rec.Message, "waiting its turn in tier 2") {
		t.Errorf("c record = %+v", rec)
	}
	if n := events.count(platform.ReasonAutomaticUpgradeStarted); n != 4 { // each cluster's, and the channel's
		t.Errorf("AutomaticUpgradeStarted events = %d, want 4: %+v", n, events.seen)
	}
	for _, ev := range events.seen {
		if ev.reason == platform.ReasonAutomaticUpgradeStarted && ev.kind == platform.KindCluster &&
			ev.message != "automatic upgrade 1.35 → 1.36 (channel stable, tier 2)" {
			t.Errorf("event message = %q", ev.message)
		}
	}

	again := w.pass(t, true, w.k, rolloutNow.Add(time.Minute))

	if v := w.version(t, "c"); v != "1.35" || again.count(platform.ReasonAutomaticUpgradeStarted) != 0 {
		t.Errorf("second pass: c asks for %s, %d started; want nothing more while two are in progress", v,
			again.count(platform.ReasonAutomaticUpgradeStarted))
	}
}

// Off, a pass changes no cluster's version and records once where each
// one is; a pass that finds the same writes nothing.
func TestAutomaticUpgradesOffOnlyRecordThePlan(t *testing.T) {
	w := newUpgradeWorld(t, twoAtOnce, "a")

	events := w.pass(t, false, w.k, rolloutNow)

	if v := w.version(t, "a"); v != "1.35" || events.count(platform.ReasonAutomaticUpgradeStarted) != 0 {
		t.Fatalf("a asks for %s (%d started); want no change", v, events.count(platform.ReasonAutomaticUpgradeStarted))
	}
	obj := w.object(t, "a")
	if rec := obj.Status.Rollout; rec == nil || rec.Result != cloudv1alpha1.ClusterRolloutPending ||
		rec.Message != "automatic upgrades are off (would start)" {
		t.Fatalf("record = %+v", rec)
	}

	w.pass(t, false, w.k, rolloutNow.Add(time.Minute))

	if again := w.object(t, "a"); again.ResourceVersion != obj.ResourceVersion {
		t.Errorf("an unchanged plan was written again (resourceVersion %s → %s)", obj.ResourceVersion, again.ResourceVersion)
	}
}

// The cluster is read again right before its version is written: one its
// owner switched to manual meanwhile is not upgraded.
func TestAutomaticUpgradeRechecksTheClusterFirst(t *testing.T) {
	w := newUpgradeWorld(t, twoAtOnce, "a")
	row, err := w.store.Clusters().Get(t.Context(), upgradeTenant, w.ids["a"])
	if err != nil {
		t.Fatal(err)
	}
	spec, err := reconcilecluster.SpecOf(row)
	if err != nil {
		t.Fatal(err)
	}
	spec.UpgradeMode = cloudv1.UpgradeMode_UPGRADE_MODE_MANUAL
	if row.Spec, err = reconcilecluster.Encode(spec); err != nil {
		t.Fatal(err)
	}
	if err := w.store.Clusters().Update(t.Context(), row); err != nil {
		t.Fatal(err)
	}
	// The cache the plan reads has not seen the change yet.
	stale := interceptor.NewClient(w.k, interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		if err := c.List(ctx, list, opts...); err != nil {
			return err
		}
		if clusters, ok := list.(*cloudv1alpha1.ClusterList); ok {
			for i := range clusters.Items {
				clusters.Items[i].Spec.UpgradeMode = cloudv1alpha1.UpgradeModeAutomatic
			}
		}
		return nil
	}})

	events := w.pass(t, true, stale, rolloutNow)

	if v := w.version(t, "a"); v != "1.35" || events.count(platform.ReasonAutomaticUpgradeStarted) != 0 {
		t.Errorf("a manual cluster was upgraded: asks for %s, %d started", v, events.count(platform.ReasonAutomaticUpgradeStarted))
	}
}

// A version write that loses to another change is skipped for this pass:
// no record, no event; the next pass plans again.
func TestAutomaticUpgradeSkipsOnConflict(t *testing.T) {
	w := newUpgradeWorld(t, twoAtOnce, "a")
	conflicting := interceptor.NewClient(w.k, interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		if _, ok := obj.(*cloudv1alpha1.Cluster); ok {
			return apierrors.NewConflict(schema.GroupResource{Resource: "clusters"}, obj.GetName(), nil)
		}
		return c.Update(ctx, obj, opts...)
	}})
	w.store = cloudstore.New(conflicting)

	events := w.pass(t, true, w.k, rolloutNow)

	if v := w.version(t, "a"); v != "1.35" || events.count(platform.ReasonAutomaticUpgradeStarted) != 0 {
		t.Errorf("after a conflict: asks for %s, %d started", v, events.count(platform.ReasonAutomaticUpgradeStarted))
	}
	if rec := w.object(t, "a").Status.Rollout; rec != nil && rec.Result == cloudv1alpha1.ClusterRolloutInProgress {
		t.Errorf("record = %+v, want no upgrade in progress", rec)
	}
}

// The upgrade the rollout started ends on the cluster's own report: it
// runs the new minor (succeeded), or it does not in time (timed out, a
// failure the rollout counts).
func TestAutomaticUpgradeFinishes(t *testing.T) {
	w := newUpgradeWorld(t, twoAtOnce, "fast", "slow")
	w.pass(t, true, w.k, rolloutNow)
	w.setStatus(t, "fast", func(st *cloudv1alpha1.ClusterStatus) { st.CurrentKubernetesVersion = "1.36" })
	w.setStatus(t, "slow", func(st *cloudv1alpha1.ClusterStatus) {
		st.Upgrade = &cloudv1alpha1.ClusterUpgrade{FromVersion: "1.35", ToVersion: "1.36", Phase: "ControlPlane"}
	})

	events := w.pass(t, true, w.k, rolloutNow.Add(config90m+time.Minute))

	if rec := w.object(t, "fast").Status.Rollout; rec == nil || rec.Result != cloudv1alpha1.ClusterRolloutSucceeded || rec.FinishTime == nil {
		t.Errorf("fast = %+v, want succeeded", rec)
	}
	if rec := w.object(t, "slow").Status.Rollout; rec == nil || rec.Result != cloudv1alpha1.ClusterRolloutTimedOut ||
		!strings.Contains(rec.Message, "no progress for 1h30m0s") {
		t.Errorf("slow = %+v, want timed out", rec)
	}
	if events.count(platform.ReasonAutomaticUpgradeSucceeded) != 1 || events.count(platform.ReasonAutomaticUpgradeFailed) != 1 {
		t.Errorf("events = %+v", events.seen)
	}
}

const config90m = 90 * time.Minute
