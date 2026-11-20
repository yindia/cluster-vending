package cluster

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// The platform pool (docs/proposals/PROP-03-component-placement.md, phase 2):
// the EC2NodeClass and NodePool crossplaneaws.PoolPlatform, built from
// nodeGroups.platform and applied like a user group once Karpenter is Ready.
// No component is placed on it yet. It is Ready when its objects are Ready,
// not when it has nodes: pods create the nodes, and a wait for nodes would
// bring back the deadlock of PROP-03 2.3.

// platformPoolOn: the config has a platform pool (the loaded config always
// does; a Deps without one renders none).
func (s *Reconciler) platformPoolOn() bool {
	return len(s.deps.NodeGroups.Platform.InstanceFamilies) > 0
}

// platformPoolInput is nodeGroups.platform as the render takes it; nil when
// there is none.
func (s *Reconciler) platformPoolInput() *crossplaneaws.PlatformPoolInput {
	if !s.platformPoolOn() {
		return nil
	}
	p := s.deps.NodeGroups.Platform
	arch := ""
	if len(p.Architectures) > 0 {
		arch = p.Architectures[0]
	}
	return &crossplaneaws.PlatformPoolInput{
		Families: p.InstanceFamilies, Sizes: p.InstanceSizes, Architecture: arch, CapacityTypes: p.CapacityTypes,
		LimitCPU: p.Limits.CPU, LimitMemory: p.Limits.Memory,
		ConsolidationPolicy: p.ConsolidationPolicy, ConsolidateAfter: p.ConsolidateAfter,
		ExpireAfter: p.ExpireAfter, TerminationGracePeriod: p.TerminationGracePeriod,
		DisruptionBudget: p.DisruptionBudget, MaxPods: p.MaxPods,
	}
}

// recordedPool is the pool's status with the phase and node count last
// recorded (PENDING and none when there is no record), and message.
func recordedPool(stored []*cloudv1.NodeGroupStatus, message string) *cloudv1.NodeGroupStatus {
	phase, nodes := cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_PENDING, int32(0)
	for _, o := range stored {
		if o.GetName() == crossplaneaws.PoolPlatform && o.GetPlatformManaged() {
			if o.GetPhase() != cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_UNSPECIFIED {
				phase = o.GetPhase()
			}
			nodes = o.GetNodes()
		}
	}
	return groupStatus(crossplaneaws.PoolPlatform, true, phase, message, nodes)
}

// withPoolUnavailable adds the pool's "status unavailable" to a result: the
// last good phase stays, the reason is the message.
func (s *Reconciler) withPoolUnavailable(res *nodeGroupsResult, stored []*cloudv1.NodeGroupStatus, why string) *nodeGroupsResult {
	if s.platformPoolOn() {
		res.platformPool = recordedPool(stored, "status unavailable: "+why)
	}
	return res
}

// readPlatformPool is the pool's status: READY when its NodePool and its
// EC2NodeClass report Ready (however many nodes it has), else PENDING with
// what the object says, FAILED when it could not be applied.
func (s *Reconciler) readPlatformPool(ctx context.Context, c client.Client, stored []*cloudv1.NodeGroupStatus, applyFailure string, nodes nodeCounts) *cloudv1.NodeGroupStatus {
	name := crossplaneaws.PoolPlatform
	status := func(phase cloudv1.NodeGroupPhase, message string) *cloudv1.NodeGroupStatus {
		return groupStatus(name, true, phase, message, nodes.count[name])
	}
	if applyFailure != "" {
		return status(cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_FAILED, applyFailure)
	}
	for _, o := range []struct {
		kind string
		gvk  func() *unstructured.Unstructured
	}{{"NodePool", newNodePool}, {"EC2NodeClass", newNodeClass}} {
		obj := o.gvk()
		err := call(ctx, func(ctx context.Context) error { return c.Get(ctx, client.ObjectKey{Name: name}, obj) })
		switch {
		case err == nil:
		case meta.IsNoMatchError(err) || apierrors.IsNotFound(err):
			return status(cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_PENDING, "waiting for Karpenter to report the platform pool")
		default:
			return recordedPool(stored, "status unavailable: "+err.Error())
		}
		if ready, message := readyCondition(obj); !ready {
			if message == "" {
				message = "waiting for Karpenter to report the platform pool's " + o.kind
			}
			return status(cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_PENDING, message)
		}
	}
	return status(cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_READY, "")
}

func newNodePool() *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(crossplaneaws.KarpenterNodePoolGVK)
	return u
}

func newNodeClass() *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(crossplaneaws.KarpenterNodeClassGVK)
	return u
}
