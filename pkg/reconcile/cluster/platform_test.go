package cluster

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// fakePlatform is a catalog and one release offered by stable.
type fakePlatform struct {
	defs    []cloudv1alpha1.ComponentDefinition
	offered string
	release *cloudv1alpha1.Release
}

func (f *fakePlatform) Definitions(context.Context) ([]cloudv1alpha1.ComponentDefinition, error) {
	return f.defs, nil
}

func (f *fakePlatform) Offered(_ context.Context, channel string) (string, error) {
	if channel != cloudv1alpha1.ChannelStable {
		return "", nil
	}
	return f.offered, nil
}

func (f *fakePlatform) Release(_ context.Context, name string) (*cloudv1alpha1.Release, error) {
	if f.release == nil || f.release.Name != name {
		return nil, nil
	}
	return f.release, nil
}

func platformDef(name string, requires ...string) cloudv1alpha1.ComponentDefinition {
	return cloudv1alpha1.ComponentDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: cloudv1alpha1.ComponentDefinitionSpec{Description: name, Management: cloudv1alpha1.ComponentManagementPlatform,
			Namespace: name + "-system", Requires: requires,
			Install: &cloudv1alpha1.ComponentInstall{Helm: &cloudv1alpha1.HelmInstall{
				HelmChartSource: cloudv1alpha1.HelmChartSource{Repository: "https://charts.example.com", Chart: name}}}},
	}
}

// platformWorld is a RUNNING cluster on Kubernetes 1.37 with a catalog of
// two Platform components (mesh requires base), a tenant one, and a
// release pinning them.
func platformWorld(t *testing.T) (world, *fakePlatform) {
	t.Helper()
	tenant := platformDef("gpu")
	tenant.Spec.Management = cloudv1alpha1.ComponentManagementTenant
	src := &fakePlatform{offered: "2026-11-a",
		defs: []cloudv1alpha1.ComponentDefinition{platformDef("mesh", "base"), platformDef("base"), tenant},
		release: &cloudv1alpha1.Release{ObjectMeta: metav1.ObjectMeta{Name: "2026-11-a"},
			Spec: cloudv1alpha1.ReleaseSpec{Content: cloudv1alpha1.ReleaseContent{Kubernetes: []cloudv1alpha1.KubernetesRelease{
				{Minor: "1.37", Components: []cloudv1alpha1.ComponentPin{{Name: "base", Version: "1.2.0"}, {Name: "mesh", Version: "2.0.0"}}}}}}}}
	w := newWorld(t, interceptor.Funcs{}, nil, func(d *reconcile.Deps) { d.Platform = src })
	row := w.cluster(t)
	spec, err := SpecOf(row)
	if err != nil {
		t.Fatal(err)
	}
	spec.ControlPlane.KubernetesVersion = "1.37"
	raw, err := Encode(spec)
	if err != nil {
		t.Fatal(err)
	}
	row.Spec, row.State, row.AppliedGeneration = raw, StateRunning, row.Generation
	if err := w.store.Clusters().Update(t.Context(), row); err != nil {
		t.Fatal(err)
	}
	return w, src
}

func (w world) helmConfig(t *testing.T) {
	t.Helper()
	pc := &unstructured.Unstructured{}
	pc.SetGroupVersionKind(crossplaneaws.HelmProviderConfigGVK)
	pc.SetNamespace(worldNS)
	pc.SetName(crossplaneaws.HelmProviderConfigName(crossplaneaws.ClusterPrefix(worldTenant, w.row.ID, w.row.Name)))
	if err := w.kube.Create(t.Context(), pc); err != nil {
		t.Fatal(err)
	}
}

// release reads the Release object of a Helm release of the cluster; nil
// when there is none.
func (w world) release(t *testing.T, name string) *unstructured.Unstructured {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(crossplaneaws.HelmReleaseGVK)
	key := client.ObjectKey{Namespace: worldNS,
		Name: crossplaneaws.HelmReleaseObjectName(crossplaneaws.ClusterPrefix(worldTenant, w.row.ID, w.row.Name), name)}
	if err := w.kube.Get(t.Context(), key, u); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return nil
		}
		t.Fatal(err)
	}
	return u
}

// deployed marks a Release Ready, Synced and deployed, as provider-helm
// does once Helm installed the chart.
func (w world) deployed(t *testing.T, name string) {
	t.Helper()
	u := w.release(t, name)
	if u == nil {
		t.Fatalf("no Release %s", name)
	}
	u.Object["status"] = map[string]any{
		"atProvider": map[string]any{"state": crossplaneaws.HelmDeployed},
		"conditions": []any{
			map[string]any{"type": "Ready", "status": "True", "reason": "Available"},
			map[string]any{"type": "Synced", "status": "True", "reason": "ReconcileSuccess"},
		},
	}
	if err := w.kube.Update(t.Context(), u); err != nil {
		t.Fatal(err)
	}
}

func phases(p *platform) string {
	var out []string
	for _, c := range p.components {
		out = append(out, c.GetName()+"="+strings.TrimPrefix(c.GetPhase().String(), "COMPONENT_PHASE_"))
	}
	return strings.Join(out, ",")
}

func (w world) install(t *testing.T) *platform {
	t.Helper()
	p, err := w.svc.installPlatform(t.Context(), w.cluster(t))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// Platform components install in requires order, each once the one
// before is deployed, and only once the cluster's kubeconfig (its Helm
// ProviderConfig) exists. Tenant components are not installed.
func TestPlatformComponentsInstallInOrder(t *testing.T) {
	w, _ := platformWorld(t)

	// No kubeconfig yet: nothing is installed.
	p := w.install(t)
	if got := phases(p); got != "base=PENDING,mesh=PENDING" || w.release(t, "base") != nil {
		t.Fatalf("before the kubeconfig: %s", got)
	}
	if !strings.Contains(p.components[0].GetMessage(), "ProviderConfig") || p.components[0].GetVersion() != "1.2.0" {
		t.Errorf("base: %q, version %q", p.components[0].GetMessage(), p.components[0].GetVersion())
	}

	// The kubeconfig exists: the first chart is installed, the second waits.
	w.helmConfig(t)
	p = w.install(t)
	if got := phases(p); got != "base=INSTALLING,mesh=PENDING" {
		t.Fatalf("after the kubeconfig: %s", got)
	}
	if w.release(t, "base") == nil || w.release(t, "mesh") != nil || w.release(t, "gpu") != nil {
		t.Fatal("only base must be installed")
	}
	if p.release != "2026-11-a" {
		t.Errorf("release %q", p.release)
	}

	// base is deployed: mesh follows.
	w.deployed(t, "base")
	p = w.install(t)
	if got := phases(p); got != "base=READY,mesh=INSTALLING" || w.release(t, "mesh") == nil {
		t.Fatalf("after base: %s", got)
	}
	w.deployed(t, "mesh")
	if got := phases(w.install(t)); got != "base=READY,mesh=READY" {
		t.Fatalf("after mesh: %s", got)
	}

	// Their objects are recorded apart from the cluster's own.
	own, err := w.store.ManagedResources().ListByOwner(t.Context(), model.OwnerCluster, w.row.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range own {
		if r.Kind == "Release" {
			t.Errorf("Release %s is in the cluster's own inventory", r.Name)
		}
	}
	theirs, err := w.store.ManagedResources().ListByOwner(t.Context(), model.OwnerClusterComponents, w.row.ID)
	if err != nil || len(theirs) != 3 { // two Releases and the Usage ordering their deletion
		t.Errorf("component inventory: %d records, %v", len(theirs), err)
	}
}

func TestPlatformComponentsAreNotInstalled(t *testing.T) {
	tests := []struct {
		name string
		edit func(*model.Cluster, *fakePlatform)
		want string // phases; "" for nothing to say, "-" for none
		msg  string
	}{
		{name: "before the cluster is running", edit: func(r *model.Cluster, _ *fakePlatform) { r.State = StateUpdating }},
		{name: "on an imported cluster", edit: func(r *model.Cluster, _ *fakePlatform) { r.Imported = true }, want: "-"},
		{name: "while it is deleted", edit: func(r *model.Cluster, _ *fakePlatform) { r.State = StateDeleting }},
		{name: "the channel offers nothing", edit: func(_ *model.Cluster, f *fakePlatform) { f.offered = "" },
			want: "base=FAILED,mesh=FAILED", msg: "release channel stable offers no release yet"},
		{name: "the release is gone", edit: func(_ *model.Cluster, f *fakePlatform) { f.release = nil },
			want: "base=FAILED,mesh=FAILED", msg: "release 2026-11-a does not exist"},
		{name: "the release lacks the Kubernetes minor", edit: func(_ *model.Cluster, f *fakePlatform) {
			f.release.Spec.Content.Kubernetes[0].Minor = "1.36"
		}, want: "base=FAILED,mesh=FAILED", msg: "does not offer Kubernetes 1.37"},
		{name: "a component the release does not pin", edit: func(_ *model.Cluster, f *fakePlatform) {
			f.release.Spec.Content.Kubernetes[0].Components = f.release.Spec.Content.Kubernetes[0].Components[1:]
		}, want: "base=FAILED,mesh=FAILED", msg: "not in release 2026-11-a"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, src := platformWorld(t)
			w.helmConfig(t)
			row := w.cluster(t)
			tt.edit(row, src)

			p, err := w.svc.installPlatform(t.Context(), row)

			if err != nil {
				t.Fatal(err)
			}
			switch tt.want {
			case "":
				if p != nil {
					t.Fatalf("got %s, want nothing to report", phases(p))
				}
			case "-":
				if p == nil || len(p.components) != 0 {
					t.Fatalf("got %+v, want no components", p)
				}
			default:
				if got := phases(p); got != tt.want {
					t.Fatalf("got %s, want %s", got, tt.want)
				}
				if !strings.Contains(p.components[0].GetMessage(), tt.msg) {
					t.Errorf("message %q, want %q", p.components[0].GetMessage(), tt.msg)
				}
			}
			if tt.name != "a component the release does not pin" && w.release(t, "base") != nil {
				t.Error("a chart was installed")
			}
		})
	}
}

// The cluster keeps the release its components were installed from, also
// when its channel moves on.
func TestPlatformReleaseIsKept(t *testing.T) {
	w, src := platformWorld(t)
	w.helmConfig(t)
	row := w.cluster(t)
	st := &cloudv1.ClusterStatus{}
	withPlatform(st, w.install(t))
	raw, err := Encode(st)
	if err != nil {
		t.Fatal(err)
	}
	row.Status = raw
	src.offered = "2026-12-a"

	p, err := w.svc.installPlatform(t.Context(), row)

	if err != nil || p.release != "2026-11-a" {
		t.Fatalf("release %q, %v", p.release, err)
	}
}

func TestWithPlatformCondition(t *testing.T) {
	pc := func(name string, phase cloudv1.ComponentPhase) *cloudv1.PlatformComponent {
		return &cloudv1.PlatformComponent{Name: name, Phase: phase}
	}
	ready, installing, failed := cloudv1.ComponentPhase_COMPONENT_PHASE_READY, cloudv1.ComponentPhase_COMPONENT_PHASE_INSTALLING, cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED
	tests := []struct {
		name            string
		in              *platform
		status          cloudv1.ConditionStatus
		reason, message string
		pending         bool
	}{
		{name: "nothing found out", in: nil},
		{name: "no components", in: &platform{}},
		{name: "all installed", in: &platform{release: "r", components: []*cloudv1.PlatformComponent{pc("a", ready), pc("b", ready)}},
			status: cloudv1.ConditionStatus_CONDITION_STATUS_TRUE, reason: "Installed", message: "2 components installed"},
		{name: "installing", in: &platform{components: []*cloudv1.PlatformComponent{pc("a", ready), pc("b", installing), pc("c", failed)}},
			status: cloudv1.ConditionStatus_CONDITION_STATUS_FALSE, reason: "Installing", message: "installing b; failed: c", pending: true},
		{name: "failed", in: &platform{components: []*cloudv1.PlatformComponent{pc("a", ready), pc("c", failed)}},
			status: cloudv1.ConditionStatus_CONDITION_STATUS_FALSE, reason: "Failed", message: "failed: c"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := &cloudv1.ClusterStatus{State: cloudv1.ClusterState_CLUSTER_STATE_RUNNING}

			withPlatform(st, tt.in)

			if st.GetState() != cloudv1.ClusterState_CLUSTER_STATE_RUNNING {
				t.Error("the cluster's state must not depend on its platform components")
			}
			if tt.reason == "" {
				if len(st.GetConditions()) != 0 {
					t.Fatalf("conditions %v", st.GetConditions())
				}
				return
			}
			c := st.GetConditions()[0]
			if c.GetType() != "PlatformComponentsReady" || c.GetStatus() != tt.status || c.GetReason() != tt.reason || c.GetMessage() != tt.message {
				t.Errorf("condition %v", c)
			}
			raw, err := Encode(st)
			if err != nil {
				t.Fatal(err)
			}
			row := &model.Cluster{Resource: model.Resource{Status: raw}}
			if got := PlatformPending(row); got != tt.pending {
				t.Errorf("PlatformPending = %v", got)
			}
			if stored := storedPlatform(row); len(stored.components) != len(tt.in.components) || stored.release != tt.in.release {
				t.Errorf("storedPlatform = %+v", stored)
			}
		})
	}
}

// Deleting a cluster removes its platform components first; with the EKS
// cluster not ACTIVE the charts are let go without an uninstall.
func TestDeletePlatformComponents(t *testing.T) {
	for _, active := range []bool{true, false} {
		name := map[bool]string{true: "EKS active: uninstalled", false: "EKS not active: released"}[active]
		t.Run(name, func(t *testing.T) {
			w, _ := platformWorld(t)
			w.helmConfig(t)
			w.install(t)
			w.deployed(t, "base")
			w.install(t)
			row := w.cluster(t)
			records, err := w.store.ManagedResources().ListByOwner(t.Context(), model.OwnerCluster, row.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !active {
				records = nil // no EKS cluster recorded: not ACTIVE
			}

			var gone bool
			for i := 0; i < 5 && !gone; i++ {
				if gone, err = w.svc.deletePlatform(t.Context(), row, records); err != nil {
					t.Fatal(err)
				}
			}

			if !gone || w.release(t, "base") != nil || w.release(t, "mesh") != nil {
				t.Fatalf("gone=%v, base=%v", gone, w.release(t, "base"))
			}
			left, err := w.store.ManagedResources().ListByOwner(t.Context(), model.OwnerClusterComponents, row.ID)
			if err != nil || len(left) != 0 {
				t.Errorf("%d records left, %v", len(left), err)
			}
			// Nothing to remove: gone at once.
			if gone, err := w.svc.deletePlatform(t.Context(), row, records); err != nil || !gone {
				t.Errorf("second call: %v, %v", gone, err)
			}
		})
	}
}

// A reconcile pass records the platform components with the cluster's
// status, and a read returns them; the cluster's own state is unchanged.
func TestReconcileRecordsPlatformComponents(t *testing.T) {
	w, _ := platformWorld(t)
	w.helmConfig(t)
	before := w.cluster(t).State

	if err := w.svc.Reconcile(t.Context(), worldTenant, w.row.ID); err != nil {
		t.Fatal(err)
	}

	row := w.cluster(t)
	if row.State != before {
		t.Errorf("state %s, was %s", row.State, before)
	}
	if got := phases(storedPlatform(row)); got != "base=INSTALLING,mesh=PENDING" {
		t.Errorf("recorded %s", got)
	}
	if !PlatformPending(row) {
		t.Error("a cluster whose components are being installed must be looked at again soon")
	}
	c, err := w.svc.View(t.Context(), row)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.GetStatus().GetPlatformComponents()) != 2 || c.GetStatus().GetPlatformRelease() != "2026-11-a" {
		t.Errorf("a read returns %v from %q", c.GetStatus().GetPlatformComponents(), c.GetStatus().GetPlatformRelease())
	}
}

func TestKubernetesMinorAndChannel(t *testing.T) {
	for in, want := range map[string]string{"1.37": "1.37", "1.37.2": "1.37", "": ""} {
		if got := kubernetesMinor(in); got != want {
			t.Errorf("kubernetesMinor(%q) = %q", in, got)
		}
	}
	for ch, want := range map[cloudv1.ReleaseChannel]string{
		cloudv1.ReleaseChannel_RELEASE_CHANNEL_UNSPECIFIED: "stable", cloudv1.ReleaseChannel_RELEASE_CHANNEL_ALPHA: "alpha",
		cloudv1.ReleaseChannel_RELEASE_CHANNEL_BETA: "beta", cloudv1.ReleaseChannel_RELEASE_CHANNEL_STABLE: "stable",
	} {
		if got := channelOf(&cloudv1.ClusterSpec{ReleaseChannel: ch}); got != want {
			t.Errorf("channelOf(%v) = %q", ch, got)
		}
	}
}
