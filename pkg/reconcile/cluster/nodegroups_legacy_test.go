package cluster

import (
	"context"
	"testing"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	"github.com/stretchr/testify/require"
	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// legacyWorld is an existing cluster built before the system group took
// over: the workload cluster has the platform's NodePool and EC2NodeClass
// "platform" (managed label) with one node, and the cluster's default
// managed node group is at the platform's size, reported by the provider.
type legacyWorld struct {
	world
	row    *model.Cluster
	fw     *fakeWorkload
	system string
	// applied counts the Applies that reached the platform objects.
	applied *int
}

func newLegacyWorld(t *testing.T) *legacyWorld {
	t.Helper()
	applied := 0
	fw := newFakeWorkload(interceptor.Funcs{Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
		if u, ok := obj.(interface{ GetName() string }); ok && u.GetName() == "platform" {
			applied++
		}
		return c.Apply(ctx, obj, opts...)
	}})
	w, row := nodeGroupWorld(t, fw)
	lw := &legacyWorld{world: w, row: row, fw: fw, applied: &applied}
	for _, kind := range []string{"NodePool", "EC2NodeClass"} {
		u := &unstructured.Unstructured{}
		if kind == "NodePool" {
			u.SetGroupVersionKind(crossplaneaws.KarpenterNodePoolGVK)
		} else {
			u.SetGroupVersionKind(crossplaneaws.KarpenterNodeClassGVK)
		}
		u.SetName("platform")
		u.SetLabels(map[string]string{crossplaneaws.LabelManagedBy: crossplaneaws.ManagedByPlatform})
		u.Object["spec"] = map[string]any{"marker": "as the old build applied it"}
		require.NoError(t, fw.c.Create(t.Context(), u))
	}
	require.NoError(t, fw.c.Create(t.Context(), karpenterNode("old-1", "platform", true)))
	lw.systemNodeGroup(t, true, 2, 2)
	return lw
}

// systemNodeGroup creates (or replaces) the default managed node group as
// the provider reports it: rendered as the plan renders it, so it is the
// cluster's current group; labelled as asked; at the given desired size;
// with ready of its nodes Ready.
func (lw *legacyWorld) systemNodeGroup(t *testing.T, labelled bool, desired, ready int) {
	t.Helper()
	spec, err := SpecOf(lw.row)
	require.NoError(t, err)
	target, err := lw.svc.Target(t.Context(), lw.row.TenantID, spec)
	require.NoError(t, err)
	plan := lw.svc.RenderPlan(spec, lw.row, target)
	var ng *awseks.NodeGroup
	for _, o := range crossplaneaws.ClusterResources(plan) {
		if g, ok := o.(*awseks.NodeGroup); ok {
			ng = g
		}
	}
	require.NotNil(t, ng)
	lw.system = ng.Name
	ng = &awseks.NodeGroup{ObjectMeta: metav1.ObjectMeta{Namespace: worldNS, Name: ng.Name}, Spec: ng.Spec}
	ng.Status.AtProvider.ID = ptr.To("prod:" + ng.Name)
	ng.Status.AtProvider.Status = ptr.To("ACTIVE")
	ng.Status.AtProvider.ScalingConfig = &awseks.ScalingConfigObservation{DesiredSize: ptr.To(float64(desired))}
	ng.SetConditions(xpv2.Available(), xpv2.ReconcileSuccess())
	setMNGPlacement(ng, labelled)
	old := &awseks.NodeGroup{}
	if err := lw.kube.Get(t.Context(), client.ObjectKey{Namespace: worldNS, Name: ng.Name}, old); err == nil {
		require.NoError(t, lw.kube.Delete(t.Context(), old))
	}
	require.NoError(t, lw.kube.Create(t.Context(), ng))
	if records, _ := lw.store.ManagedResources().ListByOwner(t.Context(), model.OwnerCluster, lw.row.ID); len(nodeGroupRecords(records)) == 0 {
		require.NoError(t, lw.store.ManagedResources().Put(t.Context(), []model.ManagedResource{{TenantID: worldTenant, CloudAccountID: records[0].CloudAccountID,
			OwnerKind: model.OwnerCluster, OwnerID: lw.row.ID, APIVersion: "eks.aws.m.upbound.io/v1beta1", Kind: KindNodeGroup, Namespace: worldNS, Name: ng.Name}}))
	}
	for i := 0; i < 3; i++ {
		name := "sys-" + string(rune('a'+i))
		n := managedNode(name, ng.Name)
		if i >= ready {
			n.Status.Conditions[0].Status = "False"
		}
		if err := lw.fw.c.Delete(t.Context(), n); err != nil {
			_ = err // not there yet
		}
		if i < max(desired, ready) {
			require.NoError(t, lw.fw.c.Create(t.Context(), n))
		}
	}
}

func (lw *legacyWorld) platformComponents(unsettled ...string) *platform {
	p := &platform{release: "2026-11-a"}
	for _, name := range []string{"karpenter", "base", "mesh"} {
		c := &cloudv1.PlatformComponent{Name: name, Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_READY, Unchanged: true}
		for _, u := range unsettled {
			if u == name {
				c.Unchanged = false
			}
		}
		p.components = append(p.components, c)
	}
	return p
}

func (lw *legacyWorld) pass(t *testing.T, p *platform) *nodeGroupsResult {
	t.Helper()
	return passOf(lw.world, lw.cluster(t), p)
}

func (lw *legacyWorld) legacyObject(t *testing.T, kind string) *unstructured.Unstructured {
	return lw.fw.get(t, kind, "", "platform")
}

// The old platform pool stays exactly as it is (not re-applied, not
// deleted) while any component may still need it, and the system group
// says what is left.
func TestOldPlatformPoolIsKeptUntilComponentsSettleOnTheSystemGroup(t *testing.T) {
	lw := newLegacyWorld(t)
	before := lw.legacyObject(t, "NodePool").GetResourceVersion()

	res := lw.pass(t, lw.platformComponents("base"))

	require.NotNil(t, lw.legacyObject(t, "NodePool"), "the pool was deleted while base is still being moved")
	require.NotNil(t, lw.legacyObject(t, "EC2NodeClass"))
	require.Equal(t, before, lw.legacyObject(t, "NodePool").GetResourceVersion(), "an old object is not changed")
	require.Zero(t, *lw.applied, "an old object is not applied again")
	require.Contains(t, res.system.move, "moving components off the old platform pool: 1 left")
	require.Contains(t, res.system.move, "base")
	require.True(t, res.system.counted)
	require.Equal(t, int32(2), res.system.nodes, "the system group's nodes are the ones of its managed group")
}

func TestOldPlatformPoolIsKeptWhileATenantComponentIsNotSettled(t *testing.T) {
	lw := newLegacyWorld(t)
	c := &model.Component{TenantID: worldTenant, ClusterID: lw.row.ID, Component: "gpu", State: "COMPONENT_PHASE_PENDING"}
	require.NoError(t, lw.store.Components().Create(t.Context(), c))
	settle := func(ready bool) {
		phase := cloudv1.ComponentPhase_COMPONENT_PHASE_INSTALLING
		if ready {
			phase = cloudv1.ComponentPhase_COMPONENT_PHASE_READY
		}
		raw, err := Encode(&cloudv1.ComponentStatus{Phase: phase})
		require.NoError(t, err)
		cur, err := lw.store.Components().Get(t.Context(), worldTenant, c.ID)
		require.NoError(t, err)
		require.NoError(t, lw.store.Components().SetStatus(t.Context(), worldTenant, c.ID, cur.ETag, raw))
	}
	settle(false)

	res := lw.pass(t, lw.platformComponents())

	require.NotNil(t, lw.legacyObject(t, "NodePool"))
	require.Contains(t, res.system.move, "1 left (gpu)")

	settle(true)
	lw.pass(t, lw.platformComponents())
	require.Nil(t, lw.legacyObject(t, "NodePool"), "settled: the pool goes")
}

// The system group must be at least at its minimum size, Ready and carrying the system
// placement before the old pool goes.
func TestOldPlatformPoolIsKeptUntilTheSystemGroupIsReady(t *testing.T) {
	for name, tt := range map[string]struct {
		labelled       bool
		desired, ready int
		want           string
	}{
		"not labelled yet":        {false, 2, 2, "its system label is not applied yet"},
		"not at its minimum yet":  {true, 1, 2, "not at its minimum size of 2 yet"},
		"a node is not Ready yet": {true, 2, 1, "1 of 2 nodes are Ready"},
	} {
		t.Run(name, func(t *testing.T) {
			lw := newLegacyWorld(t)
			lw.systemNodeGroup(t, tt.labelled, tt.desired, tt.ready)

			res := lw.pass(t, lw.platformComponents())

			require.NotNil(t, lw.legacyObject(t, "NodePool"))
			require.Contains(t, res.system.move, "waiting for the system node group")
			require.Contains(t, res.system.move, tt.want)
		})
	}
}

// The system group is autoscaled: more nodes than its minimum (the
// autoscaler added one) is not a reason to keep the old pool.
func TestOldPlatformPoolGoesWhenTheAutoscalerHasAddedNodes(t *testing.T) {
	lw := newLegacyWorld(t)
	lw.systemNodeGroup(t, true, 3, 3) // the render's minimum is 2

	lw.pass(t, lw.platformComponents())

	require.Nil(t, lw.legacyObject(t, "NodePool"), "settled: the pool goes")
}

// Settled: the pool is deleted; its node class only once its pool and its
// nodes are gone, bounded reads each pass.
func TestOldPlatformPoolThenItsNodeClassAreDeletedOnceSettled(t *testing.T) {
	lw := newLegacyWorld(t)
	karpenter := lw.platformComponents()

	res := lw.pass(t, karpenter)

	require.Nil(t, lw.legacyObject(t, "NodePool"), "settled: the pool is deleted")
	require.NotNil(t, lw.legacyObject(t, "EC2NodeClass"), "the class waits for the pool")
	require.Contains(t, res.system.move, "removing the old platform pool")

	// The pool is gone but its node is still there: the class waits.
	res = lw.pass(t, karpenter)
	require.NotNil(t, lw.legacyObject(t, "EC2NodeClass"), "the class waits for the pool's nodes")
	require.Contains(t, res.system.move, "1 left")

	require.NoError(t, lw.fw.c.Delete(t.Context(), karpenterNode("old-1", "platform", true)))
	res = lw.pass(t, karpenter)
	require.Nil(t, lw.legacyObject(t, "EC2NodeClass"), "nodes gone: the class is deleted")
	require.Zero(t, *lw.applied, "nothing re-applied the platform objects")
	for _, g := range res.groups {
		require.NotEqual(t, "platform", g.GetName(), "no platform row")
	}
	// Nothing is left to say.
	res = lw.pass(t, karpenter)
	require.Empty(t, res.system.move)
}

// Only objects with the platform label are removed.
func TestOldPlatformPoolWithoutTheManagedLabelIsLeftAlone(t *testing.T) {
	lw := newLegacyWorld(t)
	pool := lw.legacyObject(t, "NodePool")
	pool.SetLabels(nil)
	require.NoError(t, lw.fw.c.Update(t.Context(), pool))

	lw.pass(t, lw.platformComponents())

	require.NotNil(t, lw.legacyObject(t, "NodePool"), "a pool the platform does not own is never deleted")
}

func podOn(name, node string, owner string) *corev1.Pod {
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "istio-system", Name: name}, Spec: corev1.PodSpec{NodeName: node},
		Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	if owner != "" {
		p.OwnerReferences = []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: owner, Name: owner + "-x", UID: "u"}}
	}
	return p
}

// A pod of any component that still runs on the old pool's nodes keeps the
// pool, whatever the components' statuses say; DaemonSet pods, static pods
// and pods of other nodes do not count.
func TestOldPlatformPoolIsKeptWhilePodsStillRunOnItsNodes(t *testing.T) {
	lw := newLegacyWorld(t)
	for _, p := range []*corev1.Pod{podOn("agent", "old-1", "DaemonSet"), podOn("elsewhere", "sys-a", "ReplicaSet")} {
		require.NoError(t, lw.fw.c.Create(t.Context(), p))
	}
	mirror := podOn("static", "old-1", "")
	mirror.Annotations = map[string]string{"kubernetes.io/config.mirror": "x"}
	done := podOn("job", "old-1", "Job")
	done.Status.Phase = corev1.PodSucceeded
	require.NoError(t, lw.fw.c.Create(t.Context(), mirror))
	require.NoError(t, lw.fw.c.Create(t.Context(), done))
	stuck := podOn("gateway", "old-1", "ReplicaSet")
	require.NoError(t, lw.fw.c.Create(t.Context(), stuck))

	res := lw.pass(t, lw.platformComponents())

	require.NotNil(t, lw.legacyObject(t, "NodePool"), "a deployment's pod is still on the old pool")
	require.Contains(t, res.system.move, "1 left (1 pods on its nodes)")

	require.NoError(t, lw.fw.c.Delete(t.Context(), stuck))
	lw.pass(t, lw.platformComponents())
	require.Nil(t, lw.legacyObject(t, "NodePool"), "only DaemonSet, static and finished pods are left: the pool goes")
}
