package cluster

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect/v2"
	awsec2 "github.com/upbound/provider-aws/v2/apis/namespaced/ec2/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/crossplane"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/kube"
)

// objectNames are the Subnet object names of ids, in order.
func objectNames(subnets []privateSubnet, ids []string) []string {
	byID := make(map[string]string, len(subnets))
	for _, s := range subnets {
		byID[s.id] = s.name
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, byID[id])
	}
	return out
}

// subnetManager is the field manager of a cluster's tags on the network's
// subnets. The network's own applies (another manager) leave them alone,
// and untagSubnets removes exactly them.
func subnetManager(clusterID string) string { return kube.FieldOwner + "/cluster-" + clusterID }

// tagSubnets adds the cluster's tags to the network subnets it runs in:
// kubernetes.io/cluster/<name>=shared always, and karpenter.sh/discovery=
// <name> unless another cluster of the network already has it (the tag
// holds one value: that cluster's Karpenter selects the subnets by
// kubernetes.io/cluster/<name> instead).
func (s *ClusterService) tagSubnets(ctx context.Context, row *model.Cluster, t target) error {
	for _, name := range t.subnetObjects {
		subnet := &awsec2.Subnet{}
		if err := s.deps.Kube.Get(ctx, client.ObjectKey{Namespace: t.account.Namespace, Name: name}, subnet); err != nil {
			s.logger().ErrorContext(ctx, "read network subnet", "subnet", name, "error", err)
			return connect.NewError(connect.CodeUnavailable, "cannot read the network's subnet "+name)
		}
		tags := map[string]any{crossplaneaws.TagKubernetesClusterPrefix + row.Name: "shared"}
		if v := subnet.Spec.ForProvider.Tags[crossplaneaws.TagKarpenterDiscovery]; v == nil || *v == row.Name {
			tags[crossplaneaws.TagKarpenterDiscovery] = row.Name
		} else {
			s.logger().WarnContext(ctx, "subnet already has a karpenter discovery tag; select it by kubernetes.io/cluster/<name>",
				"cluster_id", row.ID, "subnet", name, "karpenter.sh/discovery", *v)
		}
		u := subnetPatch(t.account.Namespace, name)
		if err := unstructured.SetNestedMap(u.Object, tags, "spec", "forProvider", "tags"); err != nil {
			return err
		}
		if err := kube.ApplyFields(ctx, s.deps.Kube, u, subnetManager(row.ID)); err != nil {
			s.logger().ErrorContext(ctx, "tag network subnet", "cluster_id", row.ID, "subnet", name, "error", err)
			return connect.NewError(connect.CodeInternal, "could not tag the network's subnet "+name+" for the cluster")
		}
	}
	s.logger().InfoContext(ctx, "tagged network subnets", "cluster_id", row.ID, "subnets", t.subnetObjects)
	return nil
}

// untagSubnets removes the cluster's tags from every subnet of the
// namespace that has them: applying no fields as the cluster's manager
// drops what it owned.
func (s *ClusterService) untagSubnets(ctx context.Context, row *model.Cluster, namespace string) error {
	subnets := &awsec2.SubnetList{}
	err := s.deps.Kube.List(ctx, subnets, client.InNamespace(namespace),
		client.MatchingLabels{crossplane.LabelNetwork: row.NetworkID})
	if err != nil {
		return fmt.Errorf("list network subnets: %w", err)
	}
	manager := subnetManager(row.ID)
	var errs []error
	for i := range subnets.Items {
		sub := &subnets.Items[i]
		if !managedBy(sub.GetManagedFields(), manager) || sub.GetDeletionTimestamp() != nil {
			continue
		}
		err := kube.ApplyFields(ctx, s.deps.Kube, subnetPatch(namespace, sub.Name), manager)
		if err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func managedBy(fields []metav1.ManagedFieldsEntry, manager string) bool {
	for _, f := range fields {
		if f.Manager == manager {
			return true
		}
	}
	return false
}

func subnetPatch(namespace, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(awsec2.CRDGroupVersion.String())
	u.SetKind(awsec2.Subnet_Kind)
	u.SetNamespace(namespace)
	u.SetName(name)
	return u
}
