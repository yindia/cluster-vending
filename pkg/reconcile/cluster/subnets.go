package cluster

import (
	"context"
	"errors"
	"fmt"

	awsec2 "github.com/upbound/provider-aws/v2/apis/namespaced/ec2/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/crossplane"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/fault"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// subnetManager is the field manager of a cluster's tags on the network's
// subnets. The network's own applies (another manager) leave them alone,
// and untagSubnets removes exactly them.
func subnetManager(clusterID string) string { return kube.FieldOwner + "/cluster-" + clusterID }

// TagSubnets adds the cluster's tags to the network subnets it runs in
// (clusterSubnetTags): kubernetes.io/cluster/<name>=shared, unless the
// subnet came with that tag at another value (an imported network), and
// karpenter.sh/discovery=<name> unless another cluster of the network
// already has it (the tag holds one value: that cluster's Karpenter
// selects the subnets by kubernetes.io/cluster/<name> instead).
func (s *Reconciler) TagSubnets(ctx context.Context, row *model.Cluster, t Target) error {
	frozen, err := s.deps.FreezeFor(ctx, reconcile.FreezeTarget{TenantID: row.TenantID, NetworkID: row.NetworkID})
	if err != nil {
		return err
	}
	for _, name := range t.SubnetObjects {
		subnet := &awsec2.Subnet{}
		if err := s.deps.Get(ctx, client.ObjectKey{Namespace: t.Account.Namespace, Name: name}, subnet); err != nil {
			s.logger().ErrorContext(ctx, "read network subnet", "subnet", name, "error", err)
			return fault.New(fault.Unavailable, "cannot read the network's subnet "+name)
		}
		tags := clusterSubnetTags(subnet.Spec.ForProvider.Tags, row.Name)
		if frozen.Holds() {
			for key, value := range tags {
				if existing := subnet.Spec.ForProvider.Tags[key]; existing == nil || *existing != value {
					return fault.New(fault.Unavailable, "network "+row.NetworkID+" is frozen; required subnet tags wait for network unfreeze")
				}
			}
			continue
		}
		if v := subnet.Spec.ForProvider.Tags[crossplaneaws.TagKarpenterDiscovery]; v != nil && *v != row.Name {
			s.logger().WarnContext(ctx, "subnet already has a karpenter discovery tag; select it by kubernetes.io/cluster/<name>",
				"cluster_id", row.ID, "subnet", name, "karpenter.sh/discovery", *v)
		}
		u := subnetPatch(t.Account.Namespace, name)
		if err := unstructured.SetNestedMap(u.Object, tags, "spec", "forProvider", "tags"); err != nil {
			return err
		}
		err := kube.ApplyFields(ctx, s.deps.Kube, u, subnetManager(row.ID))
		if apierrors.IsConflict(err) && tags[crossplaneaws.TagKarpenterDiscovery] != nil {
			// Another cluster of the network tagged the subnet since it was read:
			// its karpenter.sh/discovery tag is not ours to fight over (see
			// clusterSubnetTags); this cluster's Karpenter selects by
			// kubernetes.io/cluster/<name>.
			s.logger().WarnContext(ctx, "subnet got a karpenter discovery tag meanwhile; skipping it for the cluster",
				"cluster_id", row.ID, "subnet", name)
			delete(tags, crossplaneaws.TagKarpenterDiscovery)
			if err := unstructured.SetNestedMap(u.Object, tags, "spec", "forProvider", "tags"); err != nil {
				return err
			}
			err = kube.ApplyFields(ctx, s.deps.Kube, u, subnetManager(row.ID))
		}
		if err != nil {
			s.logger().ErrorContext(ctx, "tag network subnet", "cluster_id", row.ID, "subnet", name, "error", err)
			return fault.New(fault.Internal, "could not tag the network's subnet "+name+" for the cluster")
		}
	}
	s.logger().InfoContext(ctx, "tagged network subnets", "cluster_id", row.ID, "subnets", t.SubnetObjects)
	return nil
}

// clusterSubnetTags are the tags a cluster applies to a subnet that has
// existing as its tags: kubernetes.io/cluster/<name>=shared and
// karpenter.sh/discovery=<name>, each only where the subnet does not
// already have the key with another value. Such a tag is somebody else's
// (the subnets of an imported network often carry
// kubernetes.io/cluster/<name>=owned; another cluster's Karpenter tag):
// applying the cluster's value next to it is a field conflict, which
// would fail every apply of the cluster. The key is there, which is what
// the tag is for.
func clusterSubnetTags(existing map[string]*string, cluster string) map[string]any {
	tags := map[string]any{}
	for key, value := range map[string]string{
		crossplaneaws.TagKubernetesClusterPrefix + cluster: "shared",
		crossplaneaws.TagKarpenterDiscovery:                cluster,
	} {
		if v := existing[key]; v == nil || *v == value {
			tags[key] = value
		}
	}
	return tags
}

// untagSubnets removes the cluster's tags from every subnet of the
// namespace that has them: applying no fields as the cluster's manager
// drops what it owned.
func (s *Reconciler) untagSubnets(ctx context.Context, row *model.Cluster, namespace string) error {
	frozen, err := s.deps.FreezeFor(ctx, reconcile.FreezeTarget{TenantID: row.TenantID, NetworkID: row.NetworkID})
	if err != nil {
		return err
	}
	// From the API server: the cache strips the managedFields this reads.
	subnets := &awsec2.SubnetList{}
	err = s.deps.Reader().List(ctx, subnets, client.InNamespace(namespace),
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
		if frozen.Holds() {
			return fault.New(fault.Unavailable, "network "+row.NetworkID+" is frozen; subnet tag cleanup waits for network unfreeze")
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
