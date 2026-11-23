package cluster

import (
	"context"
	"errors"
	"reflect"
	"slices"
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
	w, row := nodeGroupWorld(t, fw, &cloudv1.NodeGroupSpec{Name: "gpu"})

	res := passOf(w, row, karpenterOf(cloudv1.ComponentPhase_COMPONENT_PHASE_INSTALLING))

	if fw.opened != 0 || fw.get(t, "NodePool", "", "gpu") != nil {
		t.Errorf("the cluster was touched before Karpenter was Ready (opened %d)", fw.opened)
	}
	if g := statusOf(res, "gpu"); g == nil || g.GetPhase() != cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_PENDING || g.GetPlatformManaged() {
		t.Errorf("gpu status = %v", g)
	}
}

// Once Karpenter is READY the user groups are applied, labelled; readiness
// follows the NodePool's Ready condition. There is no platform group.
func TestUserNodeGroupsAreAppliedOnceKarpenterIsReady(t *testing.T) {
	fw := newFakeWorkload(interceptor.Funcs{})
	w, row := nodeGroupWorld(t, fw, &cloudv1.NodeGroupSpec{Name: "gpu", InstanceFamilies: []string{"g5"}})

	res := passOf(w, row, karpenterOf(cloudv1.ComponentPhase_COMPONENT_PHASE_READY))

	pool, class := fw.get(t, "NodePool", "", "gpu"), fw.get(t, "EC2NodeClass", "", "gpu")
	if pool == nil || class == nil {
		t.Fatalf("gpu: pool %v class %v", pool != nil, class != nil)
	}
	if pool.GetLabels()[crossplaneaws.LabelManagedBy] != crossplaneaws.ManagedByPlatform || class.GetLabels()[crossplaneaws.LabelManagedBy] != crossplaneaws.ManagedByPlatform {
		t.Error("gpu is not labelled managed")
	}
	if fw.get(t, "NodePool", "", "platform") != nil || fw.get(t, "EC2NodeClass", "", "platform") != nil {
		t.Error("a platform NodePool or EC2NodeClass was applied: the system node group runs the platform")
	}
	terms, _, _ := unstructured.NestedSlice(class.Object, "spec", "amiSelectorTerms")
	if got := terms[0].(map[string]any)["alias"]; got != "al2023@v20261107" {
		t.Errorf("AMI alias %v", got)
	}
	if len(res.groups) != 1 || res.groups[0].GetName() != "gpu" || res.groups[0].GetPhase() != cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_PENDING {
		t.Fatalf("groups %v", res.groups)
	}

	setReady(t, fw, "NodePool", "gpu", true)
	res = passOf(w, row, karpenterOf(cloudv1.ComponentPhase_COMPONENT_PHASE_READY))

	if g := statusOf(res, "gpu"); g.GetPhase() != cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_READY || g.GetNodes() != 3 {
		t.Errorf("gpu status = %v", g)
	}
}

// A cluster with no user node groups applies nothing of them to the workload
// cluster, and needs none of Karpenter's IAM for it: only the live reader's
// RBAC and flow schema (PROP-02 7.1), which every cluster gets.
func TestNoUserNodeGroupsApplyNothing(t *testing.T) {
	var applied []string
	fw := newFakeWorkload(interceptor.Funcs{Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
		if u, ok := obj.(interface{ GetName() string }); ok {
			applied = append(applied, u.GetName())
		}
		return c.Apply(ctx, obj, opts...)
	}})
	w, row, _ := nodeGroupWorldSrc(t, fw) // no IAM seeded: only user groups wait for it

	res := passOf(w, row, karpenterOf(cloudv1.ComponentPhase_COMPONENT_PHASE_READY))

	want := []string{crossplaneaws.LiveReaderRole, crossplaneaws.LiveReaderRole, crossplaneaws.LiveFlowSchema, crossplaneaws.LiveFlowSchema}
	if !slices.Equal(applied, want) {
		t.Errorf("applied %v, want only %v", applied, want)
	}
	if res == nil || len(res.groups) != 0 || !res.system.counted {
		t.Errorf("result = %+v", res)
	}
}

// The IAM wait gates user groups only.
func TestKarpenterIAMGatesUserGroupsOnly(t *testing.T) {
	fw := newFakeWorkload(interceptor.Funcs{})
	w, row, _ := nodeGroupWorldSrc(t, fw, &cloudv1.NodeGroupSpec{Name: "gpu"}) // no IAM seeded

	res := passOf(w, row, karpenterOf(cloudv1.ComponentPhase_COMPONENT_PHASE_READY))

	if g := statusOf(res, "gpu"); g == nil || !strings.Contains(g.GetMessage(), "waiting for Karpenter's IAM") {
		t.Errorf("gpu status = %v", g)
	}
	if fw.get(t, "NodePool", "", "gpu") != nil {
		t.Error("a group was applied before Karpenter's IAM was ready")
	}
}

// A platform object that was edited or deleted is put back; an object
// without the managed label is left alone.
func TestNodeGroupsAreRevertedAndOthersLeftAlone(t *testing.T) {
	foreign := &unstructured.Unstructured{}
	foreign.SetGroupVersionKind(crossplaneaws.KarpenterNodePoolGVK)
	foreign.SetName("hand-made")
	fw := newFakeWorkload(interceptor.Funcs{}, foreign)
	w, row := nodeGroupWorld(t, fw, &cloudv1.NodeGroupSpec{Name: "gpu"})
	karpenter := karpenterOf(cloudv1.ComponentPhase_COMPONENT_PHASE_READY)
	passOf(w, row, karpenter)
	if err := fw.c.Delete(t.Context(), fw.get(t, "NodePool", "", "gpu")); err != nil {
		t.Fatal(err)
	}

	passOf(w, row, karpenter)

	if fw.get(t, "NodePool", "", "gpu") == nil {
		t.Error("the deleted NodePool was not put back")
	}
	if fw.get(t, "NodePool", "", "hand-made") == nil {
		t.Error("an object the platform does not own was deleted")
	}
}

// A group removed from the spec is deleted (NodePool before its class),
// and reported DELETING while it is there; the others stay.
func TestRemovedNodeGroupIsDeleted(t *testing.T) {
	fw := newFakeWorkload(interceptor.Funcs{})
	w, row := nodeGroupWorld(t, fw, &cloudv1.NodeGroupSpec{Name: "gpu"}, &cloudv1.NodeGroupSpec{Name: "web"})
	karpenter := karpenterOf(cloudv1.ComponentPhase_COMPONENT_PHASE_READY)
	passOf(w, row, karpenter)
	spec, _ := SpecOf(row)
	spec.NodeGroups = spec.NodeGroups[1:]
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
	if fw.get(t, "NodePool", "", "web") == nil {
		t.Error("the other group went with it")
	}
}

// Errors reading or applying the workload cluster are the groups' status:
// the last good phase stays, nothing returns an error.
func TestNodeGroupsKeepTheLastGoodStatusOnErrors(t *testing.T) {
	fw := newFakeWorkload(interceptor.Funcs{})
	w, row := nodeGroupWorld(t, fw, &cloudv1.NodeGroupSpec{Name: "gpu"})
	karpenter := karpenterOf(cloudv1.ComponentPhase_COMPONENT_PHASE_READY)
	passOf(w, row, karpenter)
	setReady(t, fw, "NodePool", "gpu", true)
	good := passOf(w, row, karpenter)
	row.Status = mustStatus(t, &cloudv1.ClusterStatus{NodeGroups: good.groups})

	fw.err = errors.New("connection refused")
	res := passOf(w, row, karpenter)

	g := statusOf(res, "gpu")
	if g.GetPhase() != cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_READY || !strings.Contains(g.GetMessage(), "status unavailable") ||
		g.GetNodes() != 3 {
		t.Errorf("gpu status after an error = %v", g)
	}
	if !hasKarpenterNodes(withNodeGroupsRow(row, good.groups)) {
		t.Error("a recorded READY user group means Karpenter has nodes")
	}
}

func withNodeGroupsRow(row *model.Cluster, groups []*cloudv1.NodeGroupStatus) *model.Cluster {
	out := *row
	raw, _ := Encode(&cloudv1.ClusterStatus{NodeGroups: groups})
	out.Status = raw
	return &out
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
	w, row := nodeGroupWorld(t, fw, &cloudv1.NodeGroupSpec{Name: "gpu"}, &cloudv1.NodeGroupSpec{Name: "web"})

	res := passOf(w, row, karpenterOf(cloudv1.ComponentPhase_COMPONENT_PHASE_READY))

	g := statusOf(res, "gpu")
	if g.GetPhase() != cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_FAILED || len([]rune(g.GetMessage())) > nodeGroupMessageMax {
		t.Errorf("gpu status = %v", g)
	}
	if fw.get(t, "NodePool", "", "web") == nil {
		t.Error("the other group was not applied")
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
	w, row := nodeGroupWorld(t, fw, &cloudv1.NodeGroupSpec{Name: "gpu"})

	res := passOf(w, row, karpenterOf(cloudv1.ComponentPhase_COMPONENT_PHASE_READY))

	if g := statusOf(res, "gpu"); g == nil || !strings.Contains(g.GetMessage(), "CRDs") {
		t.Errorf("gpu status = %v", g)
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

// The user groups are on the status from the start: PENDING before the
// platform components are looked at, and the recorded state of a group is
// kept while Karpenter is not Ready.
func TestNodeGroupsVisibleFromTheStart(t *testing.T) {
	fw := newFakeWorkload(interceptor.Funcs{})
	w, row := nodeGroupWorld(t, fw, &cloudv1.NodeGroupSpec{Name: "gpu"})

	res := passOf(w, row, nil)

	if res == nil || len(res.groups) != 1 || res.groups[0].GetName() != "gpu" || res.groups[0].GetPlatformManaged() ||
		res.groups[0].GetPhase() != cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_PENDING ||
		res.groups[0].GetMessage() != "waiting for Karpenter to be Ready" {
		t.Fatalf("groups = %v", res)
	}
	if fw.opened != 0 {
		t.Error("the cluster was opened before Karpenter was Ready")
	}
	row.Status = mustStatus(t, &cloudv1.ClusterStatus{NodeGroups: []*cloudv1.NodeGroupStatus{
		{Name: "gpu", Phase: cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_READY, Nodes: 2}}})

	res = passOf(w, row, karpenterOf(cloudv1.ComponentPhase_COMPONENT_PHASE_INSTALLING))

	if res.groups[0].GetPhase() != cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_READY {
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
