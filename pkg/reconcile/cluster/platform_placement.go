package cluster

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/component"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
)

// livePlacement resolves ${system.*} (and its alias ${platform.*}) from live
// state, never from what the platform intends to roll out
// (docs/design/karpenter-node-groups.md, "Migration of existing clusters"):
// it is what the default managed node group carries now: its labels and
// taints as the provider reports them (status.atProvider), so an existing
// cluster keeps its old placement until the node group really has the
// system label and taint. Where the node group is not reported yet: a
// cluster whose components were started keeps what its stored default node
// pool says (the placement before this build); a new one gets the
// platform's fixed system placement.
//
// nil for a cluster whose placement is its own (an imported one). An error
// when the node group cannot be read: the placement is then not known.
func (s *Reconciler) livePlacement(ctx context.Context, cluster *cloudv1alpha1.Cluster, namespace, nodeGroup string, installed bool) (*component.Placement, error) {
	if cluster.Spec.ImportFrom != nil {
		return nil, nil
	}
	system, err := s.systemPlacement(ctx, cluster, namespace, nodeGroup, installed)
	if err != nil {
		return nil, err
	}
	return &system, nil
}

func (s *Reconciler) systemPlacement(ctx context.Context, cluster *cloudv1alpha1.Cluster, namespace, nodeGroup string, installed bool) (component.Placement, error) {
	if nodeGroup != "" {
		ng := &awseks.NodeGroup{}
		if err := s.getIgnoreMissing(ctx, client.ObjectKey{Namespace: namespace, Name: nodeGroup}, ng); err != nil {
			return component.Placement{}, fmt.Errorf("read node group %s: %w", nodeGroup, err)
		}
		if ptr.Deref(ng.Status.AtProvider.ID, "") != "" {
			return observedPlacement(ng), nil
		}
	}
	if installed {
		return storedPlacement(cluster.Spec.DefaultNodePool), nil
	}
	return component.Placement{Labels: crossplaneaws.PoolLabels(crossplaneaws.PoolSystem),
		Taints: []crossplaneaws.TaintPlan{crossplaneaws.PoolTaint(crossplaneaws.PoolSystem)}}, nil
}

// observedPlacement is the labels and taints the provider reports for the
// node group.
func observedPlacement(ng *awseks.NodeGroup) component.Placement {
	p := component.Placement{Labels: map[string]string{}}
	for k, v := range ng.Status.AtProvider.Labels {
		p.Labels[k] = ptr.Deref(v, "")
	}
	for _, t := range ng.Status.AtProvider.Taint {
		p.Taints = append(p.Taints, crossplaneaws.TaintPlan{Key: ptr.Deref(t.Key, ""), Value: ptr.Deref(t.Value, ""), Effect: ptr.Deref(t.Effect, "")})
	}
	slices.SortFunc(p.Taints, func(a, b crossplaneaws.TaintPlan) int {
		return strings.Compare(a.Key+"="+a.Value+":"+a.Effect, b.Key+"="+b.Value+":"+b.Effect)
	})
	return p
}

// storedPlacement is the default node pool of the stored spec: the labels
// and taints the cluster had before the platform fixed them.
func storedPlacement(pool *cloudv1alpha1.NodePoolSpec) component.Placement {
	p := component.Placement{Labels: map[string]string{}}
	if pool == nil {
		return p
	}
	maps.Copy(p.Labels, pool.Labels)
	for _, t := range pool.Taints {
		p.Taints = append(p.Taints, crossplaneaws.TaintPlan{Key: t.Key, Value: t.Value, Effect: string(t.Effect)})
	}
	return p
}
