package cluster

import (
	"strings"
	"testing"
	"time"

	protectionv1beta1 "github.com/crossplane/crossplane/apis/v2/protection/v1beta1"
	"github.com/stretchr/testify/require"
	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	awsiam "github.com/upbound/provider-aws/v2/apis/namespaced/iam/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/crossplane/managed"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/helmrel"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

func (d *delOrder) releaseOf(chart string) *unstructured.Unstructured {
	d.t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(crossplaneaws.HelmReleaseGVK)
	require.NoError(d.t, d.kube.Get(d.t.Context(), client.ObjectKey{Namespace: worldNS,
		Name: crossplaneaws.HelmReleaseObjectName(d.prefix, chart)}, u))
	return u
}

func (d *delOrder) annotate(chart string, annotations map[string]string) {
	d.t.Helper()
	u := d.releaseOf(chart)
	u.SetAnnotations(annotations)
	require.NoError(d.t, d.kube.Update(d.t.Context(), u))
}

func (d *delOrder) run(max int) []string {
	d.t.Helper()
	var messages []string
	for i := 0; i < max && !d.gone(); i++ {
		messages = append(messages, d.pass())
	}
	return messages
}

// A Release the Helm reliability machinery paused (a backoff after failed
// attempts, even one it gave up on) is still uninstalled: the deletion lifts
// the platform's own pause, and nothing else holds it.
func TestDeletionLiftsTheHelmBackoffPause(t *testing.T) {
	d := newDelOrder(t)
	d.annotate("mesh", map[string]string{crossplaneaws.ExternalNameAnnotation: "mesh", managed.PausedAnnotation: "true",
		helmrel.BackoffAnnotation: time.Now().Add(30 * time.Minute).UTC().Format(time.RFC3339), helmrel.GaveUpAnnotation: "true"})

	d.run(80)

	require.True(t, d.gone(), "the deletion finished although mesh was in its backoff; log %v", d.log)
}

// A Release someone else paused (an operator, a freeze) is not uninstalled:
// the deletion says so and waits, and nothing of a lower sequence is touched;
// it goes on once the pause is lifted.
func TestDeletionBlocksOnAnOperatorPause(t *testing.T) {
	d := newDelOrder(t)
	d.annotate("mesh", map[string]string{crossplaneaws.ExternalNameAnnotation: "mesh", managed.PausedAnnotation: "true"})

	messages := d.run(30)

	require.False(t, d.gone(), "a Release paused by an operator holds the deletion")
	last := messages[len(messages)-1]
	require.Contains(t, last, "stage 3 of 7")
	require.Contains(t, last, "Release "+crossplaneaws.HelmReleaseObjectName(d.prefix, "mesh")+" is paused (crossplane.io/paused) by an operator or a freeze")
	require.Contains(t, last, "unpause it to go on")
	require.Nil(t, d.releaseOf("base").GetDeletionTimestamp(), "base, which mesh requires, is not uninstalled while mesh is held")
	require.Nil(t, d.releaseOf("karpenter").GetDeletionTimestamp())
	require.Equal(t, []string{"01 Service istio-system/gateway", "02 Release " + crossplaneaws.HelmReleaseObjectName(d.prefix, "dash"),
		"03 Release " + crossplaneaws.HelmReleaseObjectName(d.prefix, "gpu"), "04 Release " + crossplaneaws.HelmReleaseObjectName(d.prefix, "mesh")}[:3],
		d.log[:3])

	d.annotate("mesh", map[string]string{crossplaneaws.ExternalNameAnnotation: "mesh"})
	d.run(80)
	require.True(t, d.gone(), "the deletion goes on once the pause is lifted; log %v", d.log)
}

// A Release stuck uninstalling is recovered with the recovery path of a
// stuck install, and counted.
func TestDeletionRecoversAStuckUninstall(t *testing.T) {
	atStuckTime(t)
	d := newDelOrder(t)
	old := stuckAt.Add(-20 * time.Minute)
	helm := &fakeHelm{fakeWorkloads: fakeWorkloads{records: []reconcile.HelmRecord{
		record(1, "deployed", old), record(2, "uninstalling", old)}}}
	d.svc.deps.Workloads, d.svc.deps.RecoverStuckReleases = helm, true
	u := d.releaseOf("mesh")
	u.Object["status"] = map[string]any{
		"atProvider": map[string]any{"state": "uninstalling"},
		"conditions": []any{
			map[string]any{"type": "Ready", "status": "False", "reason": "Deleting", "lastTransitionTime": old.Format(time.RFC3339)},
			map[string]any{"type": "Synced", "status": "False", "reason": "ReconcileError", "message": "uninstall: release is in progress",
				"lastTransitionTime": old.Format(time.RFC3339)},
		}}
	require.NoError(t, d.kube.Update(t.Context(), u))

	messages := d.run(80)

	require.True(t, d.gone(), "log %v", d.log)
	require.Contains(t, strings.Join(helm.ops, ";"), "rollback mesh-system/mesh 2->1")
	require.Contains(t, strings.Join(messages, "\n"), "recovered (1 of 3)")
}

// A node pool Karpenter does not remove holds the deletion for the teardown
// timeout only; then it goes on, with a warning naming what is left.
func TestDeletionNodeGroupWaitEnds(t *testing.T) {
	clock := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	now = func() time.Time { return clock }
	t.Cleanup(func() { now = time.Now })
	d := newDelOrder(t)
	d.relaxed = true
	inner := d.td.onDelete
	d.td.onDelete = func(o reconcile.WorkloadObject) {
		inner(o)
		d.td.stuck = d.td.stuck || o.Kind == reconcile.KindNodePool // Karpenter never finishes a pool
	}
	var last string
	for i := 0; i < 40 && !strings.Contains(last, "stage 4 of 7, node groups: deleting the node pools"); i++ {
		last = d.pass()
	}
	require.Contains(t, last, "stage 4 of 7, node groups: deleting the node pools")
	require.Nil(t, d.releaseOf("karpenter").GetDeletionTimestamp(), "Karpenter stays while it has nodes to terminate")
	for range 3 {
		require.Contains(t, d.pass(), "stage 4 of 7, node groups", "it waits within the timeout")
	}

	clock = clock.Add(16 * time.Minute) // past the 15-minute default
	msg := d.pass()

	require.NotContains(t, msg, "stage 4 of 7", "the deletion went on")
	row := d.cluster(t)
	var timedOut bool
	for _, e := range storedStatus(row).GetRecentEvents() {
		timedOut = timedOut || e.GetReason() == reconcile.ReasonTeardownTimedOut && strings.Contains(e.GetMessage(), "NodePool gpu")
	}
	require.True(t, timedOut, "a warning names what is left: %v", storedStatus(row).GetRecentEvents())
	d.run(80)
	require.True(t, d.gone())
}

// A forced deletion does not wait for the workload cluster: no load
// balancers or nodes are asked for, the Releases are let go.
func TestForcedDeletionSkipsTheWorkloadStages(t *testing.T) {
	d := newDelOrder(t)
	d.relaxed = true
	row := d.cluster(t)
	row.DeleteForce = true
	require.NoError(t, d.store.Clusters().Update(t.Context(), row))

	d.run(80)

	require.True(t, d.gone(), "log %v", d.log)
	require.Empty(t, d.td.deleted, "nothing is deleted in a cluster that may not be there")
}

// A cluster without a kubeconfig or platform components (an imported one
// that was not taken over) goes straight to its own objects.
func TestDeletionWithoutWorkloadOrPlatform(t *testing.T) {
	f := &fakeTeardowns{objs: []reconcile.WorkloadObject{obj(reconcile.KindService, "web", "shop", false)}}
	w := newWorld(t, interceptor.Funcs{}, nil, func(deps *reconcile.Deps) { deps.Teardowns = f })
	row := w.cluster(t)
	row.State = StateDeleting
	require.NoError(t, w.store.Clusters().Update(t.Context(), row))

	for i := 0; i < 10 && func() bool { _, err := w.store.Clusters().Get(t.Context(), worldTenant, w.row.ID); return err == nil }(); i++ {
		require.NoError(t, w.svc.Reconcile(t.Context(), worldTenant, w.row.ID))
	}

	_, err := w.store.Clusters().Get(t.Context(), worldTenant, w.row.ID)
	require.Error(t, err, "the cluster is gone")
	require.Empty(t, f.deleted, "its workload cluster is not touched")
}

// Karpenter's node role is kept by a Usage until the EKS cluster is gone;
// deleting Karpenter before the cluster releases it, and no other Usage.
func TestDeletionReleasesTheNodeRoleUsage(t *testing.T) {
	d := newDelOrder(t)
	usage := func(name, role string) {
		u := &protectionv1beta1.Usage{ObjectMeta: metav1.ObjectMeta{Namespace: worldNS, Name: name,
			Labels: map[string]string{"cluster-vending.io/cluster": d.row.ID}}}
		u.Spec.Of = protectionv1beta1.NamespacedResource{APIVersion: awsiam.CRDGroupVersion.String(), Kind: awsiam.Role_Kind,
			ResourceRef: &protectionv1beta1.NamespacedResourceRef{Name: role}}
		u.Spec.By = &protectionv1beta1.Resource{APIVersion: awseks.CRDGroupVersion.String(), Kind: awseks.Cluster_Kind,
			ResourceRef: &protectionv1beta1.ResourceRef{Name: d.prefix}}
		require.NoError(t, d.kube.Create(t.Context(), u))
		d.record(model.OwnerCluster, d.row.ID, protectionv1beta1.SchemeGroupVersion.WithKind(protectionv1beta1.UsageKind), name)
	}
	usage("node-role", d.prefix+"-karpenter-node")
	usage("cluster-role", d.prefix+"-cluster-role")
	exists := func(name string) bool {
		return d.kube.Get(t.Context(), client.ObjectKey{Namespace: worldNS, Name: name}, &protectionv1beta1.Usage{}) == nil
	}
	role := func() bool { // the node role is being deleted or gone
		r := &awsiam.Role{}
		err := d.kube.Get(t.Context(), client.ObjectKey{Namespace: worldNS, Name: d.prefix + "-karpenter-node"}, r)
		return err != nil || r.DeletionTimestamp != nil
	}

	for i := 0; i < 80 && !d.gone(); i++ {
		require.NoError(t, d.svc.Reconcile(t.Context(), worldTenant, d.row.ID))
		if role() {
			require.False(t, exists("node-role"), "the Usage that keeps the node role until the cluster is gone is released first")
		}
		d.provider()
	}
	require.True(t, d.gone())
}
