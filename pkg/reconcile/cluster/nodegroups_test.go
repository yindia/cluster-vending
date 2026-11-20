package cluster

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// fakeWorkload is the workload cluster: a fake client, or an error.
type fakeWorkload struct {
	c   client.Client
	err error
	// opened counts how often the cluster was opened.
	opened int
}

func (f *fakeWorkload) Client(context.Context, string, string) (client.Client, error) {
	f.opened++
	return f.c, f.err
}

func newFakeWorkload(funcs interceptor.Funcs, objects ...client.Object) *fakeWorkload {
	return &fakeWorkload{c: fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(objects...).WithInterceptorFuncs(funcs).Build()}
}

func (f *fakeWorkload) get(t *testing.T, kind, group, name string) *unstructured.Unstructured {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(crossplaneaws.KarpenterNodePoolGVK)
	if kind == "EC2NodeClass" {
		u.SetGroupVersionKind(crossplaneaws.KarpenterNodeClassGVK)
	}
	if err := f.c.Get(t.Context(), client.ObjectKey{Name: name}, u); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return nil
		}
		t.Fatal(err)
	}
	return u
}

// karpenterOf is the installed platform: Karpenter in a phase.
func karpenterOf(phase cloudv1.ComponentPhase) *platform {
	return &platform{release: "2026-11-a", components: []*cloudv1.PlatformComponent{{Name: "karpenter", Phase: phase}}}
}

func nodeGroupWorld(t *testing.T, fw *fakeWorkload, groups ...*cloudv1.NodeGroupSpec) (world, *model.Cluster) {
	t.Helper()
	w, row, _ := nodeGroupWorldSrc(t, fw, groups...)
	// Karpenter's IAM chain is Ready and its policy passes the node role.
	iamFake{role: true, access: true, policy: true, attachments: 4, passRoleOn: nodeRoleARN(row)}.seed(t, w, row, "us-east-1")
	return w, row
}

// nodeGroupWorldSrc is nodeGroupWorld with the release source the cluster
// reads its release from.
func nodeGroupWorldSrc(t *testing.T, fw *fakeWorkload, groups ...*cloudv1.NodeGroupSpec) (world, *model.Cluster, *fakePlatform) {
	t.Helper()
	w, src := platformWorld(t, func(d *reconcile.Deps) { d.NodeGroupClients = fw })
	src.release.Spec.Content.Kubernetes[0].NodeAMIPin = "v20261107"
	row := w.cluster(t)
	spec, err := SpecOf(row)
	if err != nil {
		t.Fatal(err)
	}
	spec.NodeGroups = groups
	raw, err := Encode(spec)
	if err != nil {
		t.Fatal(err)
	}
	row.Spec = raw
	if err := w.store.Clusters().Update(t.Context(), row); err != nil {
		t.Fatal(err)
	}
	return w, w.cluster(t), src
}

func passOf(w world, row *model.Cluster, p *platform) *nodeGroupsResult {
	ctx := withNodeGroupsHolder(context.Background())
	w.svc.reconcileNodeGroups(ctx, row, p)
	return nodeGroupsOfPass(ctx)
}

func statusOf(r *nodeGroupsResult, name string) *cloudv1.NodeGroupStatus {
	for _, g := range r.groups {
		if g.GetName() == name {
			return g
		}
	}
	return nil
}

func setReady(t *testing.T, fw *fakeWorkload, kind, name string, ready bool) {
	t.Helper()
	u := fw.get(t, kind, "", name)
	status := "False"
	if ready {
		status = "True"
	}
	u.Object["status"] = map[string]any{"conditions": []any{
		map[string]any{"type": "ValidationSucceeded", "status": "True"},
		map[string]any{"type": "Ready", "status": status, "message": "node class not ready"}}, "resources": map[string]any{"nodes": "3"}}
	if err := fw.c.Update(t.Context(), u); err != nil {
		t.Fatal(err)
	}
}

// Nothing of the node groups is applied before Karpenter is READY.
func TestNodeGroupsWaitForKarpenter(t *testing.T) {
	fw := newFakeWorkload(interceptor.Funcs{})
	w, row := nodeGroupWorld(t, fw)

	res := passOf(w, row, karpenterOf(cloudv1.ComponentPhase_COMPONENT_PHASE_INSTALLING))

	if fw.opened != 0 || fw.get(t, "NodePool", "", "platform") != nil {
		t.Errorf("the cluster was touched before Karpenter was Ready (opened %d)", fw.opened)
	}
	if p := statusOf(res, "platform"); p == nil || p.GetPhase() != cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_PENDING || !p.GetPlatformManaged() {
		t.Errorf("platform status = %v", p)
	}
}

// Once Karpenter is READY the platform group and the spec's groups are
// applied, labelled, platform first; readiness follows the NodePool's
// Ready condition.
func TestNodeGroupsAreAppliedOnceKarpenterIsReady(t *testing.T) {
	fw := newFakeWorkload(interceptor.Funcs{})
	w, row := nodeGroupWorld(t, fw, &cloudv1.NodeGroupSpec{Name: "gpu", InstanceFamilies: []string{"g5"}})

	res := passOf(w, row, karpenterOf(cloudv1.ComponentPhase_COMPONENT_PHASE_READY))

	for _, name := range []string{"platform", "gpu"} {
		pool, class := fw.get(t, "NodePool", "", name), fw.get(t, "EC2NodeClass", "", name)
		if pool == nil || class == nil {
			t.Fatalf("%s: pool %v class %v", name, pool != nil, class != nil)
		}
		if pool.GetLabels()[crossplaneaws.LabelManagedBy] != crossplaneaws.ManagedByPlatform || class.GetLabels()[crossplaneaws.LabelManagedBy] != crossplaneaws.ManagedByPlatform {
			t.Errorf("%s is not labelled managed", name)
		}
	}
	class := fw.get(t, "EC2NodeClass", "", "platform")
	alias, _, _ := unstructured.NestedString(class.Object, "spec", "amiSelectorTerms", "0", "alias")
	terms, _, _ := unstructured.NestedSlice(class.Object, "spec", "amiSelectorTerms")
	if got := terms[0].(map[string]any)["alias"]; got != "al2023@v20261107" || alias != "" {
		t.Errorf("AMI alias %v", got)
	}
	if got, want := res.groups[0].GetName(), "platform"; got != want || len(res.groups) != 2 {
		t.Fatalf("groups %v", res.groups)
	}
	if res.groups[0].GetPhase() != cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_PENDING {
		t.Errorf("a pool without a Ready condition is %v", res.groups[0].GetPhase())
	}

	setReady(t, fw, "NodePool", "platform", true)
	res = passOf(w, row, karpenterOf(cloudv1.ComponentPhase_COMPONENT_PHASE_READY))

	if p := statusOf(res, "platform"); p.GetPhase() != cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_READY || p.GetNodes() != 3 {
		t.Errorf("platform status = %v", p)
	}
}

// A platform object that was edited or deleted is put back; an object
// without the managed label is left alone.
func TestNodeGroupsAreRevertedAndOthersLeftAlone(t *testing.T) {
	foreign := &unstructured.Unstructured{}
	foreign.SetGroupVersionKind(crossplaneaws.KarpenterNodePoolGVK)
	foreign.SetName("hand-made")
	fw := newFakeWorkload(interceptor.Funcs{}, foreign)
	w, row := nodeGroupWorld(t, fw)
	karpenter := karpenterOf(cloudv1.ComponentPhase_COMPONENT_PHASE_READY)
	passOf(w, row, karpenter)
	if err := fw.c.Delete(t.Context(), fw.get(t, "NodePool", "", "platform")); err != nil {
		t.Fatal(err)
	}

	passOf(w, row, karpenter)

	if fw.get(t, "NodePool", "", "platform") == nil {
		t.Error("the deleted platform NodePool was not put back")
	}
	if fw.get(t, "NodePool", "", "hand-made") == nil {
		t.Error("an object the platform does not own was deleted")
	}
}

// A group removed from the spec is deleted (NodePool before its class),
// and reported DELETING while it is there.
func TestRemovedNodeGroupIsDeleted(t *testing.T) {
	fw := newFakeWorkload(interceptor.Funcs{})
	w, row := nodeGroupWorld(t, fw, &cloudv1.NodeGroupSpec{Name: "gpu"})
	karpenter := karpenterOf(cloudv1.ComponentPhase_COMPONENT_PHASE_READY)
	passOf(w, row, karpenter)
	spec, _ := SpecOf(row)
	spec.NodeGroups = nil
	raw, _ := Encode(spec)
	row.Spec = raw

	res := passOf(w, row, karpenter)

	if fw.get(t, "NodePool", "", "gpu") != nil {
		t.Error("the removed group's NodePool is still there")
	}
	if g := statusOf(res, "gpu"); g.GetPhase() != cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_DELETING {
		t.Errorf("removed group status = %v", g)
	}
	passOf(w, row, karpenter)
	if fw.get(t, "EC2NodeClass", "", "gpu") != nil {
		t.Error("the removed group's EC2NodeClass is still there")
	}
	if platform := fw.get(t, "NodePool", "", "platform"); platform == nil {
		t.Error("the platform group went with it")
	}
}

// Errors reading or applying the workload cluster are the groups' status:
// the last good phase stays, nothing returns an error.
func TestNodeGroupsKeepTheLastGoodStatusOnErrors(t *testing.T) {
	fw := newFakeWorkload(interceptor.Funcs{})
	w, row := nodeGroupWorld(t, fw)
	karpenter := karpenterOf(cloudv1.ComponentPhase_COMPONENT_PHASE_READY)
	passOf(w, row, karpenter)
	setReady(t, fw, "NodePool", "platform", true)
	good := passOf(w, row, karpenter)
	row.Status = mustStatus(t, &cloudv1.ClusterStatus{NodeGroups: good.groups})

	fw.err = errors.New("connection refused")
	res := passOf(w, row, karpenter)

	p := statusOf(res, "platform")
	if p.GetPhase() != cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_READY || !strings.Contains(p.GetMessage(), "status unavailable") ||
		p.GetNodes() != 3 {
		t.Errorf("platform status after an error = %v", p)
	}
	if !platformNodeGroupReady(row) {
		t.Error("the recorded platform group is READY")
	}
}

// A failed apply is that group's FAILED status; others go on, and the
// message is short.
func TestNodeGroupApplyFailureIsStatus(t *testing.T) {
	fw := newFakeWorkload(interceptor.Funcs{Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
		if u, ok := obj.(interface{ GetName() string }); ok && u.GetName() == "gpu" {
			return errors.New(strings.Repeat("x", 1000))
		}
		return c.Apply(ctx, obj, opts...)
	}})
	w, row := nodeGroupWorld(t, fw, &cloudv1.NodeGroupSpec{Name: "gpu"})

	res := passOf(w, row, karpenterOf(cloudv1.ComponentPhase_COMPONENT_PHASE_READY))

	g := statusOf(res, "gpu")
	if g.GetPhase() != cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_FAILED || len([]rune(g.GetMessage())) > nodeGroupMessageMax {
		t.Errorf("gpu status = %v", g)
	}
	if fw.get(t, "NodePool", "", "platform") == nil {
		t.Error("the platform group was not applied")
	}
}

// Missing Karpenter CRDs are PENDING, not an error.
func TestNodeGroupsToleratesMissingCRDs(t *testing.T) {
	fw := newFakeWorkload(interceptor.Funcs{
		Apply: func(context.Context, client.WithWatch, runtime.ApplyConfiguration, ...client.ApplyOption) error {
			return &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "karpenter.sh", Kind: "NodePool"}}
		},
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "karpenter.sh", Kind: "NodePool"}}
		},
	})
	w, row := nodeGroupWorld(t, fw)

	res := passOf(w, row, karpenterOf(cloudv1.ComponentPhase_COMPONENT_PHASE_READY))

	if p := statusOf(res, "platform"); p == nil || !strings.Contains(p.GetMessage(), "CRDs") {
		t.Errorf("platform status = %v", p)
	}
}

// Nothing is applied for an imported cluster, nor without the node group
// client.
func TestNodeGroupsSkipped(t *testing.T) {
	fw := newFakeWorkload(interceptor.Funcs{})
	w, row := nodeGroupWorld(t, fw)
	row.Imported, row.State = true, StateImported

	if res := passOf(w, row, karpenterOf(cloudv1.ComponentPhase_COMPONENT_PHASE_READY)); res != nil || fw.opened != 0 {
		t.Errorf("an observed cluster got node groups: %v", res)
	}
}

// The AMI pin follows the running minor until the upgrade's control plane
// is done.
func TestNodeAMIMinor(t *testing.T) {
	up := func(step cloudv1.ClusterUpgradePhase) *cloudv1.ClusterStatus {
		return &cloudv1.ClusterStatus{CurrentKubernetesVersion: "1.36.4",
			Upgrade: &cloudv1.ClusterUpgrade{FromVersion: "1.36.4", ToVersion: "1.37", Phase: step}}
	}
	tests := []struct {
		name   string
		stored *cloudv1.ClusterStatus
		want   string
	}{
		{"no upgrade", &cloudv1.ClusterStatus{CurrentKubernetesVersion: "1.37.1"}, "1.37"},
		{"control plane upgrading", up(stepControlPlane), "1.36"},
		{"node groups step", up(stepNodeGroups), "1.37"},
		{"asked for, not started", &cloudv1.ClusterStatus{CurrentKubernetesVersion: "1.36.4"}, "1.36"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := nodeAMIMinor("1.37", tt.stored); got != tt.want {
				t.Errorf("minor %s, want %s", got, tt.want)
			}
		})
	}
}

// The node groups step is settled only once Karpenter's nodes run the
// target, every claim has a node, and the read was complete.
func TestKarpenterNodesSettleTheNodeGroupsStep(t *testing.T) {
	tests := []struct {
		name string
		u    upgrade
		want bool
	}{
		{"no karpenter nodes", upgrade{target: "1.37"}, true},
		{"not read", upgrade{target: "1.37", karpenterNodes: true}, false},
		{"incomplete read", upgrade{target: "1.37", karpenterNodes: true, kubelets: &kubeletView{minors: []string{"1.37"}}}, false},
		{"old nodes", upgrade{target: "1.37", karpenterNodes: true, kubelets: &kubeletView{known: true, minors: []string{"1.36", "1.37"}}}, false},
		{"claim launching", upgrade{target: "1.37", karpenterNodes: true, kubelets: &kubeletView{known: true, minors: []string{"1.37"}, launching: 1}}, false},
		{"all on target", upgrade{target: "1.37", karpenterNodes: true, kubelets: &kubeletView{known: true, minors: []string{"1.37"}}}, true},
		{"no nodes", upgrade{target: "1.37", karpenterNodes: true, kubelets: &kubeletView{known: true}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.u.karpenterSettled(); got != tt.want {
				t.Errorf("settled = %v, want %v", got, tt.want)
			}
		})
	}
}

// Every platform component above sequence 0 waits for the platform node
// group, which Karpenter's cluster does not; once something else started,
// a wavering node group status never holds it back.
func TestPlatformComponentsWaitForThePlatformNodeGroup(t *testing.T) {
	karp := platformDef("karpenter")
	zero := int32(0)
	karp.Spec.Sequence = &zero
	w, src := platformWorld(t)
	src.defs = append(src.defs, karp)
	src.release.Spec.Content.Kubernetes[0].Components = append(src.release.Spec.Content.Kubernetes[0].Components,
		cloudv1alpha1.ComponentPin{Name: "karpenter", Version: "1.0.0"})
	w.helmConfig(t)

	p := w.install(t)

	if got := phases(p); !strings.Contains(got, "karpenter=INSTALLING") || !strings.Contains(got, "base=PENDING") || !strings.Contains(got, "mesh=PENDING") {
		t.Fatalf("components = %s", got)
	}
	for _, c := range p.components {
		if c.GetName() == "base" && !strings.Contains(c.GetMessage(), "waiting for the platform node group to be Ready") {
			t.Errorf("base message %q", c.GetMessage())
		}
	}
	if w.release(t, "base") != nil {
		t.Error("base was installed before the platform node group was Ready")
	}
	w.deployed(t, "karpenter")
	row := w.cluster(t)
	row.Status = mustStatus(t, &cloudv1.ClusterStatus{NodeGroups: []*cloudv1.NodeGroupStatus{
		{Name: "platform", Phase: cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_READY}}})
	if err := w.store.Clusters().Update(t.Context(), row); err != nil {
		t.Fatal(err)
	}

	w.install(t)

	if w.release(t, "base") == nil {
		t.Error("base is not installed once the platform node group is Ready")
	}
}

// A taken-over cluster gets Karpenter's setup (its queue and node role):
// the chart's values point at them. One only observed does not.
func TestRenderPlanKarpenterForTakeover(t *testing.T) {
	w, _ := platformWorld(t)
	for name, tt := range map[string]struct {
		edit func(*model.Cluster)
		want bool
	}{
		"taken over":               {func(r *model.Cluster) { r.Imported = true }, true},
		"imported":                 {func(r *model.Cluster) { r.Imported, r.State = true, StateImported }, false},
		"taken over, platform off": {func(r *model.Cluster) { r.Imported = true; r.Adoption, _ = WithoutPlatformComponents(r.Adoption) }, false},
	} {
		row := w.cluster(t)
		row.Adoption = []byte(`{"clusterRoleName":"tf-cluster"}`)
		tt.edit(row)
		spec, _ := SpecOf(row)
		target, err := w.svc.Target(t.Context(), row.TenantID, spec)
		if err != nil {
			t.Fatal(err)
		}
		if got := w.svc.RenderPlan(spec, row, target).Karpenter; got != tt.want {
			t.Errorf("%s: Karpenter = %v, want %v", name, got, tt.want)
		}
	}
}

func mustStatus(t *testing.T, st *cloudv1.ClusterStatus) model.JSON {
	t.Helper()
	raw, err := Encode(st)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// The platform group is on the status from the start: PENDING before the
// platform components are looked at, with the user groups, and the
// recorded state of a group is kept while Karpenter is not Ready.
func TestNodeGroupsVisibleFromTheStart(t *testing.T) {
	fw := newFakeWorkload(interceptor.Funcs{})
	w, row := nodeGroupWorld(t, fw, &cloudv1.NodeGroupSpec{Name: "gpu"})

	res := passOf(w, row, nil)

	if res == nil || len(res.groups) != 2 || res.groups[0].GetName() != "platform" || !res.groups[0].GetPlatformManaged() ||
		res.groups[0].GetPhase() != cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_PENDING ||
		res.groups[0].GetMessage() != "waiting for Karpenter to be Ready" {
		t.Fatalf("groups = %v", res)
	}
	if fw.opened != 0 {
		t.Error("the cluster was opened before Karpenter was Ready")
	}
	row.Status = mustStatus(t, &cloudv1.ClusterStatus{NodeGroups: []*cloudv1.NodeGroupStatus{
		{Name: "platform", PlatformManaged: true, Phase: cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_READY, Nodes: 2}}})

	res = passOf(w, row, karpenterOf(cloudv1.ComponentPhase_COMPONENT_PHASE_INSTALLING))

	if res.groups[0].GetPhase() != cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_READY || res.groups[1].GetPhase() != cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_PENDING {
		t.Errorf("groups = %v", res.groups)
	}
}

// What is applied is what crossplaneaws.KarpenterNodeGroupObjects renders
// (the API shows the same objects).
func TestAppliedObjectsAreTheRenderedOnes(t *testing.T) {
	fw := newFakeWorkload(interceptor.Funcs{})
	group := &cloudv1.NodeGroupSpec{Name: "gpu", InstanceFamilies: []string{"g5"}, Labels: map[string]string{"a": "b"}}
	w, row := nodeGroupWorld(t, fw, group)
	passOf(w, row, karpenterOf(cloudv1.ComponentPhase_COMPONENT_PHASE_READY))
	want, err := crossplaneaws.KarpenterNodeGroupObjects(crossplaneaws.KarpenterNodeGroupsInput{ClusterName: row.Name,
		ClusterPrefix: crossplaneaws.ClusterPrefix(row.TenantID, row.ID, row.Name), AMIPin: "v20261107", Discovery: true,
		NodeGroups: []*cloudv1.NodeGroupSpec{group}})
	if err != nil {
		t.Fatal(err)
	}

	for _, o := range want {
		got := fw.get(t, o.GetKind(), "", o.GetName())
		if got == nil || !reflect.DeepEqual(got.Object["spec"], o.Object["spec"]) || !reflect.DeepEqual(got.GetLabels(), o.GetLabels()) {
			t.Errorf("%s/%s applied differently from what is rendered: %v", o.GetKind(), o.GetName(), got)
		}
	}
}
