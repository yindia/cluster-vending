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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/yindia/cluster-vending/pkg/crossplane"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/crossplane/managed"
)

// The sweep of what a deletion left deletes a protected object no record
// names as the deletion does: switched to full management first, so its
// cloud resource goes too. A retained or Observe-only (imported) one is
// deleted as it is: its cloud resource stays.
func TestSweepAllowsTheDeletionOfProtectedLeftovers(t *testing.T) {
	deleted := map[string]string{} // name -> managementPolicies at its deletion
	w := newWorld(t, interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if strings.HasPrefix(obj.GetName(), "stray-") {
				gvk, err := apiutil.GVKForObject(obj, c.Scheme())
				require.NoError(t, err)
				u := &unstructured.Unstructured{}
				u.SetGroupVersionKind(gvk)
				require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(obj), u))
				pol, _, _ := unstructured.NestedStringSlice(u.Object, "spec", "managementPolicies")
				deleted[obj.GetName()] = strings.Join(pol, ",")
			}
			return c.Delete(ctx, obj, opts...)
		},
	}, nil)
	role := func(name string, policies xpv2.ManagementPolicies, annotations map[string]string) *awsiam.Role {
		r := &awsiam.Role{ObjectMeta: metav1.ObjectMeta{Namespace: worldNS, Name: name,
			Labels: map[string]string{crossplane.LabelCluster: w.row.ID}, Annotations: annotations}}
		r.Spec.ManagementPolicies = policies
		return r
	}
	noDelete := strings.Join([]string{"Observe", "Create", "Update", "LateInitialize"}, ",")
	for _, r := range []*awsiam.Role{
		role("stray-protected", crossplaneaws.PolicyNoDelete, nil),
		role("stray-retained", crossplaneaws.PolicyNoDelete, map[string]string{managed.RetainAnnotation: "true"}),
		role("stray-imported", xpv2.ManagementPolicies{xpv2.ManagementActionObserve}, nil),
	} {
		require.NoError(t, w.kube.Create(t.Context(), r))
	}

	_, err := w.svc.deleteUnrecorded(t.Context(), w.cluster(t), worldNS)

	require.NoError(t, err)
	require.Equal(t, map[string]string{"stray-protected": "*", "stray-retained": noDelete, "stray-imported": "Observe"}, deleted)
}
