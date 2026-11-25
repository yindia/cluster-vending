package cluster

import (
	"reflect"
	"strings"
	"testing"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// placementCase is an existing cluster (created before the Karpenter
// build): Karpenter and one more component run, and the default managed
// node group carries what it carries now.
type placementCase struct {
	// mngNew: the live node group has the system label and taint.
	mngNew bool
}

func placementDefs(src *fakePlatform) {
	karp := platformDef("karpenter")
	zero := int32(0)
	karp.Spec.Sequence = &zero
	karp.Spec.PlatformValues = &runtime.RawExtension{Raw: []byte(`{"nodeSelector":"${system.nodeSelector}","tolerations":"${system.tolerations}"}`)}
	workload := platformDef("workload")
	workload.Spec.PlatformValues = &runtime.RawExtension{Raw: []byte(`{"nodeSelector":"${platform.nodeSelector}","tolerations":"${platform.tolerations}"}`)}
	// Only these two platform components (and the tenant's definition).
	src.defs = []cloudv1alpha1.ComponentDefinition{karp, workload, src.defs[2]}
	k := &src.release.Spec.Content.Kubernetes[0]
	k.Components = []cloudv1alpha1.ComponentPin{{Name: "karpenter", Version: "1.0.0"}, {Name: "workload", Version: "1.0.0"}}
}

// placedNodeGroup creates the default managed node group object as the
// provider reports it, and records it as the cluster's.
func placedNodeGroup(t *testing.T, w world, newPlacement bool) string {
	t.Helper()
	prefix := crossplaneaws.ClusterPrefix(worldTenant, w.row.ID, w.row.Name)
	name := prefix + "-default"
	ng := &awseks.NodeGroup{ObjectMeta: metav1.ObjectMeta{Namespace: worldNS, Name: name}}
	ng.Status.AtProvider.ID = ptr.To("prod:" + name)
	ng.SetConditions(xpv2.Available(), xpv2.ReconcileSuccess())
	setMNGPlacement(ng, newPlacement)
	if err := w.kube.Create(t.Context(), ng); err != nil {
		t.Fatal(err)
	}
	records, err := w.store.ManagedResources().ListByOwner(t.Context(), model.OwnerCluster, w.row.ID)
	if err != nil || len(records) == 0 {
		t.Fatalf("records %v %v", records, err)
	}
	if err := w.store.ManagedResources().Put(t.Context(), []model.ManagedResource{{TenantID: worldTenant, CloudAccountID: records[0].CloudAccountID,
		OwnerKind: model.OwnerCluster, OwnerID: w.row.ID, APIVersion: "eks.aws.m.upbound.io/v1beta1", Kind: KindNodeGroup,
		Namespace: worldNS, Name: name}}); err != nil {
		t.Fatal(err)
	}
	return name
}

func setMNGPlacement(ng *awseks.NodeGroup, newPlacement bool) {
	ng.Status.AtProvider.Labels, ng.Status.AtProvider.Taint = nil, nil
	if newPlacement {
		ng.Status.AtProvider.Labels = map[string]*string{crossplaneaws.PoolLabelKey: ptr.To(crossplaneaws.PoolSystem)}
		ng.Status.AtProvider.Taint = []awseks.TaintObservation{{Key: ptr.To(crossplaneaws.PoolLabelKey), Value: ptr.To(crossplaneaws.PoolSystem), Effect: ptr.To("NO_SCHEDULE")}}
	}
}

// existingPlacementWorld is the cluster as the live one was after the
// upgrade: its components were started (recorded), its node group is as
// c says.
func existingPlacementWorld(t *testing.T, c placementCase) (world, string) {
	t.Helper()
	w, src := platformWorld(t)
	placementDefs(src)
	w.helmConfig(t)
	ng := placedNodeGroup(t, w, c.mngNew)
	recordStarted(t, w)
	return w, ng
}

// recordStarted records that the platform components were started.
func recordStarted(t *testing.T, w world) {
	t.Helper()
	row := w.cluster(t)
	row.Status = mustStatus(t, &cloudv1.ClusterStatus{
		PlatformComponents: []*cloudv1.PlatformComponent{
			{Name: "karpenter", Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_READY},
			{Name: "workload", Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_READY}}})
	if err := w.store.Clusters().Update(t.Context(), row); err != nil {
		t.Fatal(err)
	}
}

// placementOfRelease are the nodeSelector and tolerations the Release's
// values carry.
func placementOfRelease(t *testing.T, u *unstructured.Unstructured) (map[string]any, []any) {
	t.Helper()
	if u == nil {
		t.Fatal("the Release was not written")
	}
	values, _, _ := unstructured.NestedMap(u.Object, "spec", "forProvider", "values")
	selector, _ := values["nodeSelector"].(map[string]any)
	tolerations, _ := values["tolerations"].([]any)
	return selector, tolerations
}

var (
	noSelector     = map[string]any{}
	noTolerations  = []any{}
	systemSelector = map[string]any{"node.cluster-vending.io/pool": "system"}
	systemTaints   = []any{map[string]any{"key": "node.cluster-vending.io/pool", "operator": "Equal", "value": "system", "effect": "NoSchedule"}}
)

// installBoth passes until karpenter, then workload, are written.
func installBoth(t *testing.T, w world) {
	t.Helper()
	w.install(t)
	if w.release(t, "karpenter") != nil {
		w.deployed(t, "karpenter")
	}
	w.install(t)
}

// Without the platform pool in the config, every component, Karpenter's
// and the platform's alike (${platform.*} falls back to the system group),
// follows the live node group: its old placement
// until it really carries the system label and taint, then the system one.
func TestPlacementFollowsTheLiveNodeGroup(t *testing.T) {
	for _, tt := range []struct {
		name string
		c    placementCase
		sel  map[string]any
		tol  []any
	}{
		{"existing cluster, node group without the new label: old placement everywhere",
			placementCase{}, noSelector, noTolerations},
		{"node group carries the new label: system placement everywhere",
			placementCase{mngNew: true}, systemSelector, systemTaints},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w, _ := existingPlacementWorld(t, tt.c)

			installBoth(t, w)

			for _, name := range []string{"karpenter", "workload"} {
				sel, tol := placementOfRelease(t, w.release(t, name))
				if !reflect.DeepEqual(sel, tt.sel) || !reflect.DeepEqual(tol, tt.tol) {
					t.Errorf("%s placement = %v %v, want %v %v", name, sel, tol, tt.sel, tt.tol)
				}
			}
		})
	}
}

// A component that is installed keeps its placement until the target is
// live, then moves with one Helm upgrade.
func TestInstalledComponentsMoveWhenTheNodeGroupGetsTheLabel(t *testing.T) {
	w, ngName := existingPlacementWorld(t, placementCase{})
	installBoth(t, w)
	w.deployed(t, "workload")
	before := w.version(t, "karpenter")
	if sel, _ := placementOfRelease(t, w.release(t, "karpenter")); !reflect.DeepEqual(sel, noSelector) {
		t.Fatalf("old placement expected, got %v", sel)
	}
	// The rollout's label is applied in the account but not reported yet:
	// still the old placement.
	w.install(t)
	if w.version(t, "karpenter") != before {
		t.Fatal("Karpenter was rewritten before the node group carried the new label")
	}

	ng := &awseks.NodeGroup{}
	if err := w.kube.Get(t.Context(), client.ObjectKey{Namespace: worldNS, Name: ngName}, ng); err != nil {
		t.Fatal(err)
	}
	setMNGPlacement(ng, true)
	if err := w.kube.Update(t.Context(), ng); err != nil {
		t.Fatal(err)
	}
	w.install(t)

	if w.version(t, "karpenter") == before {
		t.Fatal("Karpenter did not move once the node group carried the new label")
	}
	if sel, tol := placementOfRelease(t, w.release(t, "karpenter")); !reflect.DeepEqual(sel, systemSelector) || !reflect.DeepEqual(tol, systemTaints) {
		t.Errorf("placement after the label = %v %v", sel, tol)
	}
}

// A new cluster's components are not held for a node group: Karpenter is
// first (sequence 0), the others follow it by sequence alone, on the fixed
// system placement.
func TestNewClusterComponentsFollowSequenceNotANodeGroup(t *testing.T) {
	w, src := platformWorld(t)
	placementDefs(src)
	w.helmConfig(t)

	p := w.install(t)

	if w.release(t, "karpenter") == nil || w.release(t, "workload") != nil {
		t.Fatalf("first pass: karpenter %v workload %v", w.release(t, "karpenter") != nil, w.release(t, "workload") != nil)
	}
	if msg := componentOf(p, "workload").GetMessage(); strings.Contains(msg, "node group") {
		t.Errorf("workload message %q: it waits for karpenter by sequence, not for a node group", msg)
	}
	w.deployed(t, "karpenter")

	w.install(t)

	sel, tol := placementOfRelease(t, w.release(t, "workload"))
	if !reflect.DeepEqual(sel, systemSelector) || !reflect.DeepEqual(tol, systemTaints) {
		t.Errorf("workload placement = %v %v", sel, tol)
	}
}
