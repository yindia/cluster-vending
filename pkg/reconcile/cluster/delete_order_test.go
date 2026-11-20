package cluster

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	awsevents "github.com/upbound/provider-aws/v2/apis/namespaced/cloudwatchevents/v1beta1"
	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	awsiam "github.com/upbound/provider-aws/v2/apis/namespaced/iam/v1beta1"
	awssqs "github.com/upbound/provider-aws/v2/apis/namespaced/sqs/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/crossplane"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/crossplane/managed"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// The ranks of a deletion, in the order it must delete them: an object is
// deleted only once every object of a lower rank is gone.
const (
	rankLoadBalancer = iota + 1
	rankTenantDash   // tenant "dash" requires "gpu"
	rankTenantGPU    // "gpu" requires the platform's "base"
	rankMesh         // platform, sequence 100, requires base
	rankBase         // platform, sequence 100
	rankEarly        // platform, sequence 10
	rankPool         // user pools, and the old platform pool of a cluster built before the system group
	rankNodeClaim
	rankNodeClass
	rankKarpenterChart // platform, sequence 0
	rankKarpenterAWS   // queue, rules, node role, access entry
	rankAddons
	rankRest
)

// delOrder is a cluster being deleted in a fake management cluster whose
// provider holds every object until the test lets it go (a finalizer), and
// a fake workload cluster; every deletion is checked against the order.
type delOrder struct {
	world
	t      *testing.T
	prefix string
	td     *fakeTeardowns
	src    *fakePlatform
	// registry: the objects of the deletion by rank.
	objects []delObject
	// holds: object name -> provider passes an object being deleted stays.
	holds map[string]int
	// log: what was deleted, in order, as "rank kind name".
	log      []string
	deleting map[string]int
	// relaxed: the order is not checked (a forced deletion, a timeout).
	relaxed bool
	// webhook, when set, is asked about every deletion before it happens
	// (the Usage protection webhook): an error refuses it, and the refusal
	// is kept in refused.
	webhook func(ctx context.Context, c client.Reader, obj client.Object) error
	refused []string
}

type delObject struct {
	rank     int
	gvk      schema.GroupVersionKind
	name     string
	workload string // the WorkloadObject.Ref() of an object of the workload cluster
}

func (o delObject) String() string {
	if o.workload != "" {
		return o.workload
	}
	return o.gvk.Kind + " " + o.name
}

func (d *delOrder) rankOf(kind, name string) int {
	for _, o := range d.objects {
		if o.workload == "" && o.gvk.Kind == kind && o.name == name {
			return o.rank
		}
	}
	return 0
}

// exists reports whether the object is still in its cluster (an object the
// provider is deleting still is).
func (d *delOrder) exists(ctx context.Context, c client.Reader, o delObject) bool {
	if o.workload != "" {
		for _, w := range d.td.objs {
			if w.Ref() == o.workload {
				return true
			}
		}
		return false
	}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(o.gvk)
	err := c.Get(ctx, client.ObjectKey{Namespace: worldNS, Name: o.name}, u)
	return err == nil
}

// deleted is told of every deletion: it must not skip ahead.
func (d *delOrder) deleted(ctx context.Context, c client.Reader, o delObject) {
	d.t.Helper()
	for _, earlier := range d.objects {
		if !d.relaxed && earlier.rank < o.rank && d.exists(ctx, c, earlier) {
			d.t.Errorf("deleted %s (rank %d) while %s (rank %d) still exists", o, o.rank, earlier, earlier.rank)
		}
	}
	d.log = append(d.log, fmt.Sprintf("%02d %s", o.rank, o))
}

func gvkOf(obj client.Object) schema.GroupVersionKind {
	gvk := obj.GetObjectKind().GroupVersionKind()
	if gvk.Kind == "" {
		gvk.Kind = reflect.TypeOf(obj).Elem().Name()
	}
	return gvk
}

const holdFinalizer = "test.cluster-vending.io/provider"

// newDelOrder is a RUNNING-turned-DELETING cluster with: a load balancer, a
// user node pool and the platform pool, node claims and a node class in the
// workload cluster; tenant components dash (requires gpu) and gpu (requires
// base); platform components karpenter (sequence 0), early (10), base and
// mesh (100, mesh requires base); the add-ons, the Karpenter queue, rule,
// node role and access entry and the default node group, all recorded.
func newDelOrder(t *testing.T) *delOrder {
	t.Helper()
	d := &delOrder{t: t, holds: map[string]int{}, deleting: map[string]int{}}
	d.td = &fakeTeardowns{}
	d.td.onDelete = func(o reconcile.WorkloadObject) {
		for _, obj := range d.objects {
			if obj.workload == o.Ref() {
				d.deleted(t.Context(), d.kube, obj)
			}
		}
	}
	funcs := interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
		if d.webhook != nil {
			if err := d.webhook(ctx, c, obj); err != nil {
				d.refused = append(d.refused, gvkOf(obj).Kind+" "+obj.GetName())
				return err
			}
		}
		if r := d.rankOf(gvkOf(obj).Kind, obj.GetName()); r > 0 {
			d.deleted(ctx, c, delObject{rank: r, gvk: gvkOf(obj), name: obj.GetName()})
		}
		return c.Delete(ctx, obj, opts...)
	}}
	mesh, base, early, karp := platformDef("mesh", "base"), platformDef("base"), platformDef("early"), platformDef("karpenter")
	early.Spec.Sequence, karp.Spec.Sequence = ptr.To(int32(10)), ptr.To(int32(0))
	gpu, dash := platformDef("gpu", "base"), platformDef("dash", "gpu")
	gpu.Spec.Management, dash.Spec.Management = cloudv1alpha1.ComponentManagementTenant, cloudv1alpha1.ComponentManagementTenant
	d.src = &fakePlatform{offered: "2026-11-a", defs: []cloudv1alpha1.ComponentDefinition{mesh, base, early, karp, gpu, dash}}
	d.world = newWorld(t, funcs, nil, func(deps *reconcile.Deps) { deps.Platform, deps.Teardowns = d.src, d.td })
	d.prefix = crossplaneaws.ClusterPrefix(worldTenant, d.row.ID, d.row.Name)

	d.workload()
	d.tenantComponent("dash", rankTenantDash)
	d.tenantComponent("gpu", rankTenantGPU)
	d.platformComponent("mesh", rankMesh)
	d.platformComponent("base", rankBase)
	d.platformComponent("early", rankEarly)
	d.platformComponent("karpenter", rankKarpenterChart)
	p := d.prefix
	d.clusterObject(rankKarpenterAWS, &awssqs.Queue{ObjectMeta: metav1.ObjectMeta{Name: p + "-karpenter"}})
	d.clusterObject(rankKarpenterAWS, &awsiam.Role{ObjectMeta: metav1.ObjectMeta{Name: p + "-karpenter-node"}})
	d.clusterObject(rankKarpenterAWS, &awseks.AccessEntry{ObjectMeta: metav1.ObjectMeta{Name: p + "-karpenter-access"}})
	d.clusterObject(rankKarpenterAWS, &awsevents.Rule{ObjectMeta: metav1.ObjectMeta{Name: p + "-karp-spot"}})
	d.clusterObject(rankAddons, &awseks.Addon{ObjectMeta: metav1.ObjectMeta{Name: p + "-addon-vpc-cni"}})
	d.clusterObject(rankAddons, &awseks.Addon{ObjectMeta: metav1.ObjectMeta{Name: p + "-addon-aws-ebs-csi-driver"}})
	d.clusterObject(rankAddons, &awsiam.Role{ObjectMeta: metav1.ObjectMeta{Name: p + "-ebs-csi-role"}})
	d.clusterObject(rankAddons, &awseks.PodIdentityAssociation{ObjectMeta: metav1.ObjectMeta{Name: p + "-ebs-csi-identity"}})
	d.clusterObject(rankRest, &awseks.NodeGroup{ObjectMeta: metav1.ObjectMeta{Name: crossplaneaws.DefaultNodeGroupName(p)}})
	d.clusterObject(rankRest, &awsiam.Role{ObjectMeta: metav1.ObjectMeta{Name: p + "-cluster-role"}})
	// The EKS cluster and its ClusterAuth (the kubeconfig) of newWorld,
	// held like the rest.
	d.holdExisting(&awseks.Cluster{}, p)
	d.holdExisting(&awseks.ClusterAuth{}, p+"-auth")
	d.record(model.OwnerCluster, d.row.ID, awseks.CRDGroupVersion.WithKind(awseks.ClusterAuth_Kind), p+"-auth")
	d.deleteCluster()
	return d
}

// holdExisting makes an object of newWorld one the provider holds.
func (d *delOrder) holdExisting(o client.Object, name string) {
	d.t.Helper()
	require.NoError(d.t, d.kube.Get(d.t.Context(), client.ObjectKey{Namespace: worldNS, Name: name}, o))
	patch := client.MergeFrom(o.DeepCopyObject().(client.Object))
	o.SetFinalizers([]string{holdFinalizer})
	require.NoError(d.t, d.kube.Patch(d.t.Context(), o, patch))
	gvk, err := d.kubeGVK(o)
	require.NoError(d.t, err)
	d.objects = append(d.objects, delObject{rank: rankRest, gvk: gvk, name: name})
}

// deleteCluster is the cluster being deleted.
func (d *delOrder) deleteCluster() {
	d.t.Helper()
	row := d.cluster(d.t)
	raw, err := Encode(&cloudv1.ClusterStatus{KubeconfigSecret: worldNS + "/" + kubeconfigSecret(d.prefix+"-auth")})
	require.NoError(d.t, err)
	row.Status, row.State = raw, StateDeleting
	require.NoError(d.t, d.store.Clusters().Update(d.t.Context(), row))
}

func (d *delOrder) workload() {
	d.td.objs = []reconcile.WorkloadObject{
		obj(reconcile.KindService, "istio-system", "gateway", false),
		obj(reconcile.KindNodePool, "", "gpu", false), obj(reconcile.KindNodePool, "", "platform", false),
		obj(reconcile.KindNodeClaim, "", "gpu-x1", false), obj(reconcile.KindNodeClaim, "", "platform-y1", false),
		obj(reconcile.KindEC2NodeClass, "", "gpu", false), obj(reconcile.KindEC2NodeClass, "", "platform", false),
	}
	ranks := map[string]int{reconcile.KindService: rankLoadBalancer, reconcile.KindNodeClaim: rankNodeClaim, reconcile.KindEC2NodeClass: rankNodeClass}
	for _, o := range d.td.objs {
		rank := ranks[o.Kind]
		if o.Kind == reconcile.KindNodePool {
			rank = rankPool
		}
		d.objects = append(d.objects, delObject{rank: rank, workload: o.Ref()})
	}
}

func (d *delOrder) hold(o client.Object) {
	d.t.Helper()
	o.SetNamespace(worldNS)
	o.SetFinalizers([]string{holdFinalizer})
	o.SetLabels(crossplane.ClusterLabels(worldTenant, d.row.ID))
}

// clusterObject creates an object of the cluster and records it.
func (d *delOrder) clusterObject(rank int, o client.Object) {
	d.t.Helper()
	d.hold(o)
	require.NoError(d.t, d.kube.Create(d.t.Context(), o))
	gvk, err := d.kubeGVK(o)
	require.NoError(d.t, err)
	d.record(model.OwnerCluster, d.row.ID, gvk, o.GetName())
	d.objects = append(d.objects, delObject{rank: rank, gvk: gvk, name: o.GetName()})
}

func (d *delOrder) kubeGVK(o client.Object) (schema.GroupVersionKind, error) {
	gvks, _, err := d.kube.Scheme().ObjectKinds(o)
	if err != nil || len(gvks) == 0 {
		return schema.GroupVersionKind{}, fmt.Errorf("%T: %w", o, err)
	}
	return gvks[0], nil
}

func (d *delOrder) record(owner model.OwnerKind, id string, gvk schema.GroupVersionKind, name string) {
	d.t.Helper()
	account, err := d.store.Tenants().GetCloudAccount(d.t.Context(), worldTenant, model.ProviderAWS, "444455556666")
	require.NoError(d.t, err)
	require.NoError(d.t, d.store.ManagedResources().Put(d.t.Context(), []model.ManagedResource{{TenantID: worldTenant,
		CloudAccountID: account.ID, OwnerKind: owner, OwnerID: id, APIVersion: gvk.GroupVersion().String(), Kind: gvk.Kind,
		Namespace: worldNS, Name: name}}))
}

// release creates the Helm Release of a component, with the labels the
// platform gives it.
func (d *delOrder) helmRelease(component, chart string, labels map[string]string) *unstructured.Unstructured {
	d.t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(crossplaneaws.HelmReleaseGVK)
	u.SetNamespace(worldNS)
	u.SetName(crossplaneaws.HelmReleaseObjectName(d.prefix, chart))
	u.SetLabels(labels)
	u.SetFinalizers([]string{holdFinalizer})
	u.SetAnnotations(map[string]string{crossplaneaws.ExternalNameAnnotation: chart})
	u.Object["spec"] = map[string]any{"forProvider": map[string]any{"namespace": component + "-system"}}
	require.NoError(d.t, d.kube.Create(d.t.Context(), u))
	return u
}

func (d *delOrder) platformComponent(name string, rank int) {
	d.t.Helper()
	u := d.helmRelease(name, name, crossplane.ComponentLabels(worldTenant, d.row.ID, name))
	d.record(model.OwnerClusterComponents, d.row.ID, crossplaneaws.HelmReleaseGVK, u.GetName())
	d.objects = append(d.objects, delObject{rank: rank, gvk: crossplaneaws.HelmReleaseGVK, name: u.GetName()})
}

func (d *delOrder) tenantComponent(name string, rank int) {
	d.t.Helper()
	c := &model.Component{TenantID: worldTenant, ClusterID: d.row.ID, Component: name, State: "COMPONENT_PHASE_PENDING"}
	require.NoError(d.t, d.store.Components().Create(d.t.Context(), c))
	u := d.helmRelease(name, name, crossplane.ComponentLabels(worldTenant, d.row.ID, name))
	d.record(model.OwnerComponent, c.ID, crossplaneaws.HelmReleaseGVK, u.GetName())
	d.objects = append(d.objects, delObject{rank: rank, gvk: crossplaneaws.HelmReleaseGVK, name: u.GetName()})
}

// provider is the controllers of the management cluster: an object being
// deleted is let go once it has been seen deleting for its hold passes.
func (d *delOrder) provider() {
	d.t.Helper()
	for _, o := range d.objects {
		if o.workload != "" {
			continue
		}
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(o.gvk)
		if err := d.kube.Get(d.t.Context(), client.ObjectKey{Namespace: worldNS, Name: o.name}, u); err != nil || u.GetDeletionTimestamp() == nil {
			continue
		}
		if u.GetAnnotations()[managed.PausedAnnotation] == "true" {
			continue // a paused object is not reconciled: its deletion waits
		}
		d.deleting[o.name]++
		if d.deleting[o.name] > d.holds[o.name] {
			u.SetFinalizers(nil)
			require.NoError(d.t, d.kube.Update(d.t.Context(), u))
		}
	}
}

// pass is one pass of the controller, then the providers; it returns the
// status message the cluster shows.
func (d *delOrder) pass() string {
	d.t.Helper()
	require.NoError(d.t, d.svc.Reconcile(d.t.Context(), worldTenant, d.row.ID))
	msg := ""
	if row, err := d.store.Clusters().Get(d.t.Context(), worldTenant, d.row.ID); err == nil {
		msg = storedStatus(row).GetMessage()
	}
	d.provider()
	return msg
}

func (d *delOrder) gone() bool {
	_, err := d.store.Clusters().Get(d.t.Context(), worldTenant, d.row.ID)
	return err != nil
}

// The whole deletion, stage by stage, the reverse of the creation: nothing
// of a later stage is deleted while anything of an earlier one still
// exists, whatever the providers' pace; the cluster says where it waits.
func TestDeletionRunsInTheReverseOfCreation(t *testing.T) {
	d := newDelOrder(t)
	// Slow providers: charts that take passes to uninstall, and a pool that
	// does not go at once.
	for _, name := range []string{"mesh", "base", "early", "karpenter", "dash"} {
		d.holds[crossplaneaws.HelmReleaseObjectName(d.prefix, name)] = 2
	}
	d.holds[d.prefix+"-addon-vpc-cni"] = 2
	var messages []string
	for i := 0; i < 80 && !d.gone(); i++ {
		messages = append(messages, d.pass())
	}
	require.True(t, d.gone(), "the cluster is deleted; log %v", d.log)

	// Every rank was deleted, in rank order.
	ranks := map[int]bool{}
	last := 0
	for _, entry := range d.log {
		var r int
		_, err := fmt.Sscanf(entry, "%d", &r)
		require.NoError(t, err)
		require.GreaterOrEqual(t, r, last, "deleted out of order: %v", d.log)
		last = r
		ranks[r] = true
	}
	for r := rankLoadBalancer; r <= rankRest; r++ {
		require.True(t, ranks[r], "rank %d was never deleted: %v", r, d.log)
	}
	// The status named each stage while it waited.
	joined := strings.Join(messages, "\n")
	for stage := 1; stage <= 6; stage++ {
		require.Contains(t, joined, fmt.Sprintf("stage %d of 7, %s", stage, stageNames[stage]), "no message for stage %d", stage)
	}
	require.Contains(t, joined, "tenant components: removing dash; gpu wait for the components that require them to be gone")
	require.Contains(t, joined, "platform components: uninstalling sequence 100")
	require.Contains(t, joined, "platform components: uninstalling sequence 10")
	require.Contains(t, joined, "waiting for Release "+crossplaneaws.HelmReleaseObjectName(d.prefix, "mesh")+" to be uninstalled")
	require.Contains(t, joined, "node pools")
	require.Contains(t, joined, "instance profiles")
	require.Contains(t, joined, "base waits for the components that require it", "mesh requires base: base goes after it")
	left, err := d.store.ManagedResources().ListByOwner(t.Context(), model.OwnerClusterComponents, d.row.ID)
	require.NoError(t, err)
	require.Empty(t, left)
	_, err = d.store.Clusters().Get(t.Context(), worldTenant, d.row.ID)
	require.ErrorIs(t, err, repository.ErrNotFound)
}
