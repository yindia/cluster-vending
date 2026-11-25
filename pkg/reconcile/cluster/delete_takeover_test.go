package cluster

import (
	"context"
	"strings"
	"testing"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	"github.com/stretchr/testify/require"
	awsiam "github.com/upbound/provider-aws/v2/apis/namespaced/iam/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corecluster "github.com/yindia/cluster-vending/pkg/cluster"
	"github.com/yindia/cluster-vending/pkg/crossplane"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/kube"
)

// Deleting a taken-over imported cluster through the reconciler leaves the
// IAM roles it adopted and their policy attachments in the cloud: the
// rendered attachments on adopted roles and the carried-over extra
// attachments (also one an older import stored without the retain mark)
// are deleted as Kubernetes objects still managed without Delete. What the
// platform created for the cluster is switched to full management first,
// so its cloud resource goes with it.
func TestDeletingATakenOverClusterKeepsItsAdoptedRoles(t *testing.T) {
	deleted := map[string]string{} // object name -> its managementPolicies when deleted
	w := newWorld(t, interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			gvk, err := apiutil.GVKForObject(obj, c.Scheme())
			require.NoError(t, err)
			u := &unstructured.Unstructured{}
			u.SetGroupVersionKind(gvk)
			if c.Get(ctx, client.ObjectKeyFromObject(obj), u) == nil {
				pol, _, _ := unstructured.NestedStringSlice(u.Object, "spec", "managementPolicies")
				deleted[obj.GetName()] = strings.Join(pol, ",")
			}
			return c.Delete(ctx, obj, opts...)
		},
	}, nil)
	ctx := t.Context()
	prefix := w.prefix()
	scheme, err := kube.Scheme()
	require.NoError(t, err)

	// What the import kept: two extra policies on the adopted cluster role,
	// one stored by an import from before carried-over attachments were
	// marked retained (no annotation), one marked.
	extra := func(suffix, policy string, retained bool) *awsiam.RolePolicyAttachment {
		a := &awsiam.RolePolicyAttachment{ObjectMeta: metav1.ObjectMeta{Namespace: worldNS, Name: prefix + "-cluster-role-x-" + suffix,
			Labels: crossplane.ClusterLabels(worldTenant, w.row.ID),
			Annotations: map[string]string{crossplane.CarriedOverAnnotation: "true",
				crossplaneaws.ExternalNameAnnotation: "tf-cluster/" + policy}}}
		if retained {
			a.Annotations[crossplaneaws.RetainAnnotation] = "true"
		}
		a.Spec.ManagementPolicies = crossplaneaws.PolicyObserveLateInit
		a.Spec.ForProvider.PolicyArn = ptr.To(policy)
		a.Spec.ForProvider.RoleRef = &xpv2.NamespacedReference{Name: prefix + "-cluster-role"}
		return a
	}
	oldExtra := extra("0ld00000", "arn:aws:iam::aws:policy/AmazonEKSVPCResourceController", false)
	newExtra := extra("ne000000", "arn:aws:iam::aws:policy/CloudWatchAgentServerPolicy", true)
	plan := corecluster.ImportPlan{Objects: []client.Object{oldExtra, newExtra}}
	adoption, err := WithImportObjects(model.JSON(`{"clusterRoleName":"tf-cluster","nodeRoleName":"tf-node"}`), plan, scheme)
	require.NoError(t, err)
	row := w.cluster(t)
	row.Imported, row.Adoption = true, adoption // taken over: no longer CLUSTER_STATE_IMPORTED
	require.NoError(t, w.store.Clusters().Update(ctx, row))
	require.True(t, w.cluster(t).Imported, "the row keeps that it was imported")

	require.NoError(t, w.svc.Reconcile(ctx, worldTenant, w.row.ID)) // the takeover's apply
	for _, name := range []string{oldExtra.Name, newExtra.Name} {
		require.NoError(t, w.kube.Get(ctx, client.ObjectKey{Namespace: worldNS, Name: name}, &awsiam.RolePolicyAttachment{}),
			"carried-over %s applied", name)
	}

	row = w.cluster(t)
	row.State = StateDeleting
	require.NoError(t, w.store.Clusters().Update(ctx, row))
	for i := 0; i < 40; i++ {
		if _, err := w.store.Clusters().Get(ctx, worldTenant, w.row.ID); err != nil {
			break
		}
		require.NoError(t, w.svc.Reconcile(ctx, worldTenant, w.row.ID))
	}
	_, err = w.store.Clusters().Get(ctx, worldTenant, w.row.ID)
	require.Error(t, err, "the cluster is deleted; deleted objects %v", deleted)

	kept := []string{
		prefix + "-cluster-role", prefix + "-cluster-policy",
		prefix + "-node-role", prefix + "-node-worker", prefix + "-node-cni", prefix + "-node-ecr",
		oldExtra.Name, newExtra.Name,
	}
	for _, name := range kept {
		pol, ok := deleted[name]
		require.True(t, ok, "%s was never deleted as an object; deleted %v", name, deleted)
		require.NotContains(t, pol, "*", "%s went with full management: its cloud resource is deleted", name)
		require.NotContains(t, pol, "Delete", "%s", name)
		require.NotEmpty(t, pol, "%s", name)
	}
	// What the platform created for the cluster goes from the cloud too.
	created := 0
	for name, pol := range deleted {
		if pol == "*" {
			created++
			require.NotContains(t, kept, name)
		}
	}
	require.Positive(t, created, "nothing the platform created was deleted in the cloud: %v", deleted)
}
