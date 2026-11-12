package cluster

import (
	"context"
	"fmt"
	"slices"

	awsec2 "github.com/upbound/provider-aws/v2/apis/namespaced/ec2/v1beta1"
	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

const kindNodeGroup = "NodeGroup"

// nodeGroupRecords are the cluster's NodeGroup records, oldest first: more
// than one while a replaced node group is retired.
func nodeGroupRecords(records []model.ManagedResource) []model.ManagedResource {
	var out []model.ManagedResource
	for _, r := range records {
		if r.Kind == kindNodeGroup {
			out = append(out, r)
		}
	}
	return out
}

// nodeGroupName picks the default node group for plan. EKS cannot change
// a node group's instance types, capacity type, AMI type, disk size or
// launch template, so when they differ from the current node group's a new
// one is created (ReplacementNodeGroupName) and the others are retired
// once it is ready. A recorded node group that already has the plan's
// settings is the one: reverting a change before its replacement is ready
// goes back to the node group that is still there instead of creating a
// third. It returns the name and the node groups to keep until then.
func (s *ClusterService) nodeGroupName(ctx context.Context, row *model.Cluster, plan crossplaneaws.ClusterPlan) (string, []client.Object, error) {
	records, err := s.deps.Store.ManagedResources().ListByOwner(ctx, model.OwnerCluster, row.ID)
	if err != nil {
		return "", nil, fmt.Errorf("list cluster resources: %w", err)
	}
	groups := nodeGroupRecords(records)
	if len(groups) == 0 {
		return crossplaneaws.DefaultNodeGroupName(plan.Prefix), nil, nil
	}
	name, newestGone := "", false
	for i := len(groups) - 1; i >= 0 && name == ""; i-- {
		live := &awseks.NodeGroup{}
		err := s.deps.Get(ctx, client.ObjectKey{Namespace: groups[i].Namespace, Name: groups[i].Name}, live)
		switch {
		case apierrors.IsNotFound(err):
			newestGone = newestGone || i == len(groups)-1
		case err != nil:
			return "", nil, fmt.Errorf("read node group %s: %w", groups[i].Name, err)
		case crossplaneaws.SameNodeGroup(live, plan.NodePool):
			name = groups[i].Name
		}
	}
	switch {
	case name != "":
	case newestGone:
		// Gone: render it again under its name with the new settings.
		name = groups[len(groups)-1].Name
	default:
		name = crossplaneaws.ReplacementNodeGroupName(plan.Prefix, plan.NodePool)
	}
	var keep []client.Object
	for _, g := range groups {
		if g.Name != name {
			keep = append(keep, &awseks.NodeGroup{ObjectMeta: metav1.ObjectMeta{Namespace: g.Namespace, Name: g.Name}})
			// With its rendered launch template, if it has one: the node
			// group runs on it until it is retired.
			lt := crossplaneaws.LaunchTemplateName(g.Name)
			if slices.ContainsFunc(records, func(r model.ManagedResource) bool { return r.Kind == "LaunchTemplate" && r.Name == lt }) {
				keep = append(keep, &awsec2.LaunchTemplate{ObjectMeta: metav1.ObjectMeta{Namespace: g.Namespace, Name: lt}})
			}
		}
	}
	return name, keep, nil
}

// retireLaunchTemplate deletes and forgets the rendered launch template of
// a node group that is gone.
func (s *ClusterService) retireLaunchTemplate(ctx context.Context, records []model.ManagedResource, group model.ManagedResource) error {
	for _, r := range records {
		if r.Kind != "LaunchTemplate" || r.Name != crossplaneaws.LaunchTemplateName(group.Name) {
			continue
		}
		if err := s.deps.DeleteRecorded(ctx, []model.ManagedResource{r}); err != nil {
			return err
		}
		if err := s.deps.Store.ManagedResources().Delete(ctx, r.ID); err != nil {
			return err
		}
	}
	return nil
}

// currentNodeGroup is the node group the cluster's spec renders now (see
// nodeGroupName): the one the others are retired for.
func (s *ClusterService) currentNodeGroup(ctx context.Context, row *model.Cluster) (string, error) {
	spec, err := specOf(row)
	if err != nil {
		return "", err
	}
	t, err := s.target(ctx, row.TenantID, spec)
	if err != nil {
		return "", err
	}
	name, _, err := s.nodeGroupName(ctx, row, s.renderPlan(spec, row, t))
	return name, err
}

// retireNodeGroups deletes the node groups that current superseded once
// current is READY, and forgets them when they are gone. current is the
// node group the spec renders, not the one recorded last: after a change
// is reverted it is the older of the two.
func (s *ClusterService) retireNodeGroups(ctx context.Context, records []model.ManagedResource, views []*cloudv1.ManagedResource, current string) error {
	groups := nodeGroupRecords(records)
	if len(groups) < 2 {
		return nil
	}
	state := make(map[string]cloudv1.ManagedResourceState, len(views))
	for _, v := range views {
		state[v.GetKind()+"/"+v.GetName()] = v.GetState()
	}
	if state[kindNodeGroup+"/"+current] != cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY {
		return nil
	}
	for _, old := range groups {
		if old.Name == current {
			continue
		}
		if state[kindNodeGroup+"/"+old.Name] == cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_MISSING {
			if err := s.retireLaunchTemplate(ctx, records, old); err != nil {
				return err
			}
			if err := s.deps.Store.ManagedResources().Delete(ctx, old.ID); err != nil {
				return err
			}
			continue
		}
		if err := s.deps.DeleteRecorded(ctx, []model.ManagedResource{old}); err != nil {
			return err
		}
		s.logger().InfoContext(ctx, "retiring replaced node group", "cluster_id", old.OwnerID, "node_group", old.Name, "replacement", current)
	}
	return nil
}
