package cluster

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	awsiam "github.com/upbound/provider-aws/v2/apis/namespaced/iam/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// passWithNotes is passOf that collects the pass's notes (its events).
func passWithNotes(w world, row *model.Cluster, p *platform) (*nodeGroupsResult, []reconcile.Note) {
	ctx := reconcile.WithNotes(withNodeGroupsHolder(context.Background()))
	w.svc.reconcileNodeGroups(ctx, row, p)
	return nodeGroupsOfPass(ctx), reconcile.Notes(ctx)
}

// A cluster built before the platform owned its node groups still has
// tenant pools the platform applied (labelled managed by it). Tenants own
// their pools now: the platform hands them over (drops its label) instead
// of deleting them, so their nodes and pods stay. Once, quietly, with one
// event naming them; nothing is drained, nothing reported DELETING. The
// platform pool and pools the tenant made stay as they are.
func TestOldTenantPoolsAreHandedOverNotDeleted(t *testing.T) {
	foreign := &unstructured.Unstructured{}
	foreign.SetGroupVersionKind(crossplaneaws.KarpenterNodePoolGVK)
	foreign.SetName("hand-made")
	objects := slices.Concat(tenantGroup("gpu"), tenantGroup("batch"))
	fw := newFakeWorkload(interceptor.Funcs{}, append(objects, foreign, karpenterNode("gpu-1", "gpu", true))...)
	w, row := nodeGroupWorld(t, fw)
	withPlatformPool(w)
	karpenter := karpenterOf(cloudv1.ComponentPhase_COMPONENT_PHASE_READY)

	res, notes := passWithNotes(w, row, karpenter)

	for _, name := range []string{"gpu", "batch"} {
		for _, kind := range []string{"NodePool", "EC2NodeClass"} {
			u := fw.get(t, kind, "", name)
			if u == nil {
				t.Fatalf("%s %s was deleted: a tenant's pool is handed over, never deleted", kind, name)
			}
			if _, ok := u.GetLabels()[crossplaneaws.LabelManagedBy]; ok {
				t.Errorf("%s %s still carries the platform's label %v", kind, name, u.GetLabels())
			}
		}
		if g := statusOf(res, name); g != nil {
			t.Errorf("the handed-over pool %s is reported as %v", name, g)
		}
	}
	var handed []reconcile.Note
	for _, n := range notes {
		if n.Reason == reconcile.ReasonNodePoolsHandedOver {
			handed = append(handed, n)
		}
	}
	if len(handed) != 1 || !strings.Contains(handed[0].Message, "batch, gpu") || handed[0].Type != reconcile.EventNormal {
		t.Fatalf("notes = %+v, want one Normal NodePoolsHandedOver naming batch, gpu", notes)
	}
	if fw.get(t, "NodePool", "", "hand-made") == nil || fw.get(t, "NodePool", "", platformPool) == nil {
		t.Error("an object the platform does not own, or the platform pool, was deleted")
	}
	if l := fw.get(t, "NodePool", "", platformPool).GetLabels(); l[crossplaneaws.LabelManagedBy] != crossplaneaws.ManagedByPlatform {
		t.Errorf("the platform pool lost its label: %v", l)
	}

	_, notes = passWithNotes(w, row, karpenter)
	for _, n := range notes {
		if n.Reason == reconcile.ReasonNodePoolsHandedOver {
			t.Errorf("handed over again: %+v", n)
		}
	}
}

// A handover that cannot be written is tried again by the next pass, and
// the event names only the pools handed over.
func TestATenantPoolHandoverThatFailsIsTriedAgain(t *testing.T) {
	fail := true
	fw := newFakeWorkload(interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
		if fail && obj.GetName() == "gpu" {
			return context.DeadlineExceeded
		}
		return c.Patch(ctx, obj, patch, opts...)
	}}, tenantGroup("gpu")...)
	w, row := nodeGroupWorld(t, fw)
	withPlatformPool(w)
	karpenter := karpenterOf(cloudv1.ComponentPhase_COMPONENT_PHASE_READY)

	if _, notes := passWithNotes(w, row, karpenter); slices.ContainsFunc(notes, func(n reconcile.Note) bool { return n.Reason == reconcile.ReasonNodePoolsHandedOver }) {
		t.Fatalf("notes = %+v: nothing was handed over", notes)
	}
	fail = false
	if _, notes := passWithNotes(w, row, karpenter); !slices.ContainsFunc(notes, func(n reconcile.Note) bool {
		return n.Reason == reconcile.ReasonNodePoolsHandedOver && strings.Contains(n.Message, "gpu")
	}) {
		t.Fatalf("notes = %+v, want gpu handed over", notes)
	}
	if _, ok := fw.get(t, "NodePool", "", "gpu").GetLabels()[crossplaneaws.LabelManagedBy]; ok {
		t.Error("gpu still carries the platform's label")
	}
}

// The upgrade's node groups step waits for the platform's pools only (the
// platform pool, and the old "platform" while it exists): a tenant pool's
// nodes on an older kubelet, or its NodeClaims still launching, do not
// hold it. They are reported as a warning, the tenant's to roll.
func TestTenantPoolsDoNotGateTheNodeGroupsStep(t *testing.T) {
	old := func(name, pool string) client.Object {
		n := karpenterNode(name, pool, true)
		n.Status.NodeInfo.KubeletVersion = "v1.36.4-eks"
		return n
	}
	claim := nodeClaim("gpu-launching", "")
	claim.SetLabels(map[string]string{crossplaneaws.KarpenterNodePoolLabel: "gpu"})
	fw := newFakeWorkload(interceptor.Funcs{}, karpenterNode("p-1", platformPool, true), old("gpu-1", "gpu"), old("gpu-2", "gpu"),
		old("batch-1", "batch"), claim)
	w, _ := nodeGroupWorld(t, fw)

	_, view := w.svc.readNodes(t.Context(), fw.c)

	u := upgrade{target: "1.37", karpenterNodes: true, kubelets: &view}
	if !u.karpenterSettled() {
		t.Fatalf("view %+v: the step waits on tenant pools", view)
	}
	if got := view.tenantBehind("1.37"); !slices.Equal(got, []string{"batch (1.36)", "gpu (1.36)"}) {
		t.Errorf("tenant pools behind = %v", got)
	}

	// The platform's pools still gate: the old "platform" pool on 1.36.
	fw = newFakeWorkload(interceptor.Funcs{}, karpenterNode("p-1", platformPool, true), old("legacy-1", legacyPlatformPool))
	_, view = w.svc.readNodes(t.Context(), fw.c)
	if u := (upgrade{target: "1.37", karpenterNodes: true, kubelets: &view}); u.karpenterSettled() {
		t.Error("the old platform pool's 1.36 node does not hold the step")
	}
	launching := nodeClaim("p-launching", "")
	launching.SetLabels(map[string]string{crossplaneaws.KarpenterNodePoolLabel: platformPool})
	fw = newFakeWorkload(interceptor.Funcs{}, karpenterNode("p-1", platformPool, true), launching)
	_, view = w.svc.readNodes(t.Context(), fw.c)
	if u := (upgrade{target: "1.37", karpenterNodes: true, kubelets: &view}); u.karpenterSettled() {
		t.Error("the platform pool's launching NodeClaim does not hold the step")
	}
}

// The upgrade's status carries the tenant pools behind as a warning,
// whatever step it is at.
func TestUpgradeStatusWarnsOfTenantPoolsBehind(t *testing.T) {
	yes := cloudv1.ConditionStatus_CONDITION_STATUS_TRUE
	row := &model.Cluster{Resource: model.Resource{Base: model.Base{ID: "c-1", UpdatedAt: time.Now()},
		State: StateUpdating, Generation: 4, AppliedGeneration: 4}}
	view := kubeletView{known: true, minors: []string{"1.37"}, tenant: map[string][]string{"gpu": {"1.36"}, "web": {"1.37"}}}
	u := upgrade{target: "1.37", controlPlane: "1.37", nodes: []string{"1.37"}, nodeGroups: map[string]string{"prod-default": "1.37"},
		nodeSpecs: map[string]string{"prod-default": "1.37"}, karpenterNodes: true, kubelets: &view}
	views := []*cloudv1.ManagedResource{
		upgradeView("Cluster", "prod", readyState, yes, ""),
		named(upgradeView(KindNodeGroup, "prod-default", readyState, yes, ""), "prod-eks-default"),
		named(upgradeView("Addon", "prod-addon-coredns", updatingState, yes, ""), "prod-eks:coredns"),
	}

	got := u.status(row, views, &cloudv1.ClusterUpgrade{FromVersion: "1.36", ToVersion: "1.37", Phase: stepNodes, Applied: true}, componentsState{})

	if got.GetPhase() == stepNodes {
		t.Errorf("phase = %s: the tenant pool holds the node groups step", got.GetPhase())
	}
	if !slices.Equal(got.GetTenantPoolsBehind(), []string{"gpu (1.36)"}) {
		t.Errorf("tenant pools behind = %v, want [gpu (1.36)]", got.GetTenantPoolsBehind())
	}
}

// The cluster's status names Karpenter's node role and its ARN once the
// role is Ready: what a tenant's own EC2NodeClass puts in spec.role.
func TestStatusNamesKarpentersNodeRole(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{}, nil)
	prefix := crossplaneaws.ClusterPrefix(worldTenant, w.row.ID, w.row.Name)
	name := crossplaneaws.KarpenterNodeRoleName(prefix)
	arn := "arn:aws:iam::444455556666:role/" + name
	role := &awsiam.Role{ObjectMeta: metav1.ObjectMeta{Namespace: worldNS, Name: name}}
	role.Status.AtProvider.Arn = &arn
	if err := w.kube.Create(t.Context(), role); err != nil {
		t.Fatal(err)
	}
	records := []model.ManagedResource{{Kind: "Role", Namespace: worldNS, Name: name}}
	st := &cloudv1.ClusterStatus{ManagedResources: []*cloudv1.ManagedResource{{Kind: "Role", Name: name,
		State: cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY}}}

	if err := w.svc.observeEKS(t.Context(), records, st, name); err != nil {
		t.Fatal(err)
	}

	if aws := st.GetAws(); aws.GetKarpenterNodeRole() != name || aws.GetKarpenterNodeRoleArn() != arn {
		t.Errorf("aws status = %v, want node role %s (%s)", aws, name, arn)
	}

	st.ManagedResources[0].State = cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_CREATING
	if err := w.svc.observeEKS(t.Context(), records, st, name); err != nil {
		t.Fatal(err)
	}
	if aws := st.GetAws(); aws.GetKarpenterNodeRole() != "" {
		t.Errorf("aws status = %v: no role before it is Ready", aws)
	}
}
