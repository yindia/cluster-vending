package cluster

import (
	"context"
	"encoding/json"
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

func nodeGroupWorld(t *testing.T, fw *fakeWorkload) (world, *model.Cluster) {
	t.Helper()
	w, row, _ := nodeGroupWorldSrc(t, fw)
	// Karpenter's IAM chain is Ready and its policy passes the node role.
	iamFake{role: true, access: true, policy: true, attachments: 4, passRoleOn: nodeRoleARN(row)}.seed(t, w, row, "us-east-1")
	return w, row
}

// nodeGroupWorldSrc is nodeGroupWorld with the release source the cluster
// reads its release from.
func nodeGroupWorldSrc(t *testing.T, fw *fakeWorkload) (world, *model.Cluster, *fakePlatform) {
	t.Helper()
	w, src := platformWorld(t, func(d *reconcile.Deps) { d.NodeGroupClients = fw })
	src.release.Spec.Content.Kubernetes[0].NodeAMIPin = "v20261107"
	return w, w.cluster(t), src
}

func passOf(w world, row *model.Cluster, p *platform) *nodeGroupsResult {
	ctx := withNodeGroupsHolder(context.Background())
	w.svc.reconcileNodeGroups(ctx, row, p)
	return nodeGroupsOfPass(ctx)
}

// statusOf is a group's status in a pass: the platform pool, or a removed
// tenant group.
func statusOf(r *nodeGroupsResult, name string) *cloudv1.NodeGroupStatus {
	if p := r.platformPool; p != nil && p.GetName() == name {
		return p
	}
	for _, g := range r.removed {
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

// tenantGroup is a NodePool and its EC2NodeClass an earlier build applied
// for a tenant node group: labelled managed by the platform.
func tenantGroup(name string) []client.Object {
	var out []client.Object
	for _, gvk := range []schema.GroupVersionKind{crossplaneaws.KarpenterNodePoolGVK, crossplaneaws.KarpenterNodeClassGVK} {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(gvk)
		u.SetName(name)
		u.SetLabels(map[string]string{crossplaneaws.LabelManagedBy: crossplaneaws.ManagedByPlatform})
		out = append(out, u)
	}
	return out
}

// A stored spec of an earlier build still carries the removed node group
// fields: they are ignored on read and never rendered. The cluster gets the
// platform's system group and only the platform pool.
func TestStoredNodeGroupFieldsAreIgnored(t *testing.T) {
	fw := newFakeWorkload(interceptor.Funcs{})
	w, row := nodeGroupWorld(t, fw)
	withPlatformPool(w)
	row.Spec = withLegacyNodeGroupFields(t, row.Spec)

	spec, err := SpecOf(row)
	if err != nil {
		t.Fatalf("a stored spec with the removed fields: %v", err)
	}
	target, err := w.svc.Target(t.Context(), row.TenantID, spec)
	if err != nil {
		t.Fatal(err)
	}
	plan := w.svc.RenderPlan(spec, row, target)
	if want := crossplaneaws.SystemNodePool(SystemNodeGroup(w.svc.deps.NodeGroups.System)); !reflect.DeepEqual(plan.NodePool, want) {
		t.Errorf("node pool = %+v\nwant the platform's %+v", plan.NodePool, want)
	}
	passOf(w, row, karpenterOf(cloudv1.ComponentPhase_COMPONENT_PHASE_READY))
	if fw.get(t, "NodePool", "", "gpu") != nil || fw.get(t, "NodePool", "", platformPool) == nil {
		t.Error("want the platform pool only")
	}
}

// withLegacyNodeGroupFields is a stored spec with the node group fields an
// earlier build stored: a default node pool, its AWS settings, a tenant group.
func withLegacyNodeGroupFields(t *testing.T, stored model.JSON) model.JSON {
	t.Helper()
	var spec map[string]any
	if err := json.Unmarshal(stored, &spec); err != nil {
		t.Fatal(err)
	}
	spec["defaultNodePool"] = map[string]any{"instanceTypes": []any{"c7i.4xlarge"}, "capacityType": "CAPACITY_TYPE_SPOT",
		"scalingConfig": map[string]any{"minSize": 1, "maxSize": 9}}
	spec["nodeGroups"] = []any{map[string]any{"name": "gpu", "instanceFamilies": []any{"g5"}}}
	spec["aws"].(map[string]any)["defaultNodePool"] = map[string]any{"amiType": "AWS_AMI_TYPE_CUSTOM",
		"launchTemplate": map[string]any{"imageId": "ami-0123456789abcdef0", "userData": "Zm9v"}}
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	return model.JSON(raw)
}

// Without a platform pool and no tenant group, nothing of Karpenter is
// applied and none of its IAM is needed: only the live reader's RBAC and
// flow schema (PROP-02 7.1), which every cluster gets.
func TestNoPlatformPoolAppliesNothingOfKarpenter(t *testing.T) {
	var applied []string
	fw := newFakeWorkload(interceptor.Funcs{Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
		if u, ok := obj.(interface{ GetName() string }); ok {
			applied = append(applied, u.GetName())
		}
		return c.Apply(ctx, obj, opts...)
	}})
	w, row, _ := nodeGroupWorldSrc(t, fw) // no IAM seeded

	res := passOf(w, row, karpenterOf(cloudv1.ComponentPhase_COMPONENT_PHASE_READY))

	want := []string{crossplaneaws.LiveReaderRole, crossplaneaws.LiveReaderRole, crossplaneaws.LiveFlowSchema, crossplaneaws.LiveFlowSchema}
	if !slices.Equal(applied, want) {
		t.Errorf("applied %v, want only %v", applied, want)
	}
	if res == nil || len(res.removed) != 0 || res.platformPool != nil || !res.system.counted {
		t.Errorf("result = %+v", res)
	}
}

// Errors reading or applying the workload cluster are the status: the last
// good phase of the platform pool and of a removed group stays, nothing
// returns an error. A READY platform pool means Karpenter has nodes.
func TestNodeGroupsKeepTheLastGoodStatusOnErrors(t *testing.T) {
	fw := newFakeWorkload(interceptor.Funcs{})
	w, row := nodeGroupWorld(t, fw)
	withPlatformPool(w)
	karpenter := karpenterOf(cloudv1.ComponentPhase_COMPONENT_PHASE_READY)
	passOf(w, row, karpenter)
	setReady(t, fw, "NodePool", platformPool, true)
	setReady(t, fw, "EC2NodeClass", platformPool, true)
	good := passOf(w, row, karpenter)
	removed := groupStatus("gpu", false, cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_DELETING, "recorded by an earlier build", 2)
	recorded := []*cloudv1.NodeGroupStatus{good.platformPool, removed}
	row.Status = mustStatus(t, &cloudv1.ClusterStatus{NodeGroups: recorded})

	fw.err = errors.New("connection refused")
	res := passOf(w, row, karpenter)

	if g := res.platformPool; g.GetPhase() != cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_READY || !strings.Contains(g.GetMessage(), "status unavailable") {
		t.Errorf("platform pool after an error = %v", g)
	}
	if g := statusOf(res, "gpu"); g.GetPhase() != cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_DELETING || g.GetNodes() != 2 {
		t.Errorf("removed group after an error = %v", g)
	}
	if !hasKarpenterNodes(withNodeGroupsRow(row, recorded)) {
		t.Error("a recorded READY platform pool means Karpenter has nodes")
	}
	if hasKarpenterNodes(withNodeGroupsRow(row, []*cloudv1.NodeGroupStatus{groupStatus(systemNodeGroup, true,
		cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_READY, "", 2)})) {
		t.Error("the system group is no Karpenter group")
	}
}

func withNodeGroupsRow(row *model.Cluster, groups []*cloudv1.NodeGroupStatus) *model.Cluster {
	out := *row
	raw, _ := Encode(&cloudv1.ClusterStatus{NodeGroups: groups})
	out.Status = raw
	return &out
}

// Missing Karpenter CRDs are the platform pool's PENDING, not an error.
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
	withPlatformPool(w)

	res := passOf(w, row, karpenterOf(cloudv1.ComponentPhase_COMPONENT_PHASE_READY))

	if g := res.platformPool; g == nil || !strings.Contains(g.GetMessage(), "CRDs") {
		t.Errorf("platform pool status = %v", g)
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

// A removed group is on the status from the start: its recorded state is
// kept while Karpenter is not Ready, and nothing is opened.
func TestRemovedGroupsStayVisibleBeforeKarpenterIsReady(t *testing.T) {
	fw := newFakeWorkload(interceptor.Funcs{})
	w, row := nodeGroupWorld(t, fw)
	row.Status = mustStatus(t, &cloudv1.ClusterStatus{NodeGroups: []*cloudv1.NodeGroupStatus{
		{Name: "gpu", Phase: cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_DELETING, Nodes: 2}}})

	for _, p := range []*platform{nil, karpenterOf(cloudv1.ComponentPhase_COMPONENT_PHASE_INSTALLING)} {
		res := passOf(w, row, p)
		if g := statusOf(res, "gpu"); g.GetPhase() != cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_DELETING || g.GetNodes() != 2 {
			t.Errorf("removed group = %v", g)
		}
	}
	if fw.opened != 0 {
		t.Error("the cluster was opened before Karpenter was Ready")
	}
}

// What is applied is what crossplaneaws.KarpenterNodeGroupObjects renders
// (the API shows the same objects).
func TestAppliedObjectsAreTheRenderedOnes(t *testing.T) {
	fw := newFakeWorkload(interceptor.Funcs{})
	w, row := nodeGroupWorld(t, fw)
	withPlatformPool(w)
	passOf(w, row, karpenterOf(cloudv1.ComponentPhase_COMPONENT_PHASE_READY))
	want, err := crossplaneaws.KarpenterNodeGroupObjects(crossplaneaws.KarpenterNodeGroupsInput{ClusterName: row.Name,
		ClusterPrefix: crossplaneaws.ClusterPrefix(row.TenantID, row.ID, row.Name), AMIPin: "v20261107", Discovery: true,
		PlatformPool: w.svc.platformPoolInput(), Tags: crossplaneaws.KarpenterNodeTags(row.TenantID, row.ID, row.Name, row.Labels)})
	if err != nil || len(want) != 2 {
		t.Fatalf("%d objects, %v", len(want), err)
	}

	for _, o := range want {
		got := fw.get(t, o.GetKind(), "", o.GetName())
		if got == nil || !reflect.DeepEqual(got.Object["spec"], o.Object["spec"]) || !reflect.DeepEqual(got.GetLabels(), o.GetLabels()) {
			t.Errorf("%s/%s applied differently from what is rendered: %v", o.GetKind(), o.GetName(), got)
		}
	}
}
