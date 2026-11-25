package cluster

import (
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// The node groups follow the NodeProvisioner role, not a name: with a
// catalog whose provisioner is "nodes", a Ready "karpenter" releases
// nothing, and "nodes" being Ready (with its own controller policy passing
// the node role) applies the platform pool.
func TestNodeGroupsFollowTheNodeProvisionerRole(t *testing.T) {
	fw := newFakeWorkload(interceptor.Funcs{})
	w, row, src := nodeGroupWorldSrc(t, fw)
	withPlatformPool(w)
	nodes := platformDef("nodes")
	nodes.Spec.Role = cloudv1alpha1.ComponentRoleNodeProvisioner
	src.defs = append(src.defs, nodes, platformDef("karpenter"))
	iamFake{role: true, access: true, policy: true, attachments: 4, passRoleOn: nodeRoleARN(row), provisioner: "nodes"}.seed(t, w, row, "us-east-1")
	installed := func(phase cloudv1.ComponentPhase) *platform {
		return &platform{release: "2026-11-a", components: []*cloudv1.PlatformComponent{
			{Name: "karpenter", Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_READY}, {Name: "nodes", Phase: phase}}}
	}

	res := passOf(w, row, installed(cloudv1.ComponentPhase_COMPONENT_PHASE_INSTALLING))
	if fw.opened != 0 || fw.get(t, "NodePool", "", platformPool) != nil {
		t.Fatalf("a Ready karpenter that is not the NodeProvisioner released the node groups (opened %d)", fw.opened)
	}
	if g := res.platformPool; g == nil || g.GetPhase() != cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_PENDING {
		t.Errorf("platform pool status = %v, want PENDING", g)
	}

	passOf(w, row, installed(cloudv1.ComponentPhase_COMPONENT_PHASE_READY))
	if fw.get(t, "NodePool", "", platformPool) == nil || fw.get(t, "EC2NodeClass", "", platformPool) == nil {
		t.Error("the NodeProvisioner is Ready and its IAM granted, but the platform pool was not applied")
	}
	if objs, err := w.svc.NodeGroupObjects(t.Context(), row); err != nil || len(objs) == 0 {
		t.Errorf("NodeGroupObjects = %d objects, %v; want the platform pool's", len(objs), err)
	}
}
