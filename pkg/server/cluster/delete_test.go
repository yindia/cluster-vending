package cluster

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/yindia/cluster-vending/pkg/crossplane"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

func TestDeletingMessage(t *testing.T) {
	at := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	now = func() time.Time { return at }
	t.Cleanup(func() { now = time.Now })
	const (
		deleting = cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_DELETING
		missing  = cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_MISSING
		ready    = cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY
	)
	views := []*cloudv1.ManagedResource{
		{Kind: "Addon", Name: "p-addon-coredns", State: missing},
		{Kind: "NodeGroup", Name: "p-default", State: deleting, DeletionTime: timestamppb.New(at.Add(-3 * time.Minute))},
		{Kind: "Cluster", Name: "p", State: deleting, DeletionTime: timestamppb.New(at.Add(-3 * time.Minute))},
		{Kind: "Role", Name: "p-node-role", State: deleting, Synced: cloudv1.ConditionStatus_CONDITION_STATUS_FALSE,
			Reason: "ReconcileError", Message: "DeleteConflict: must detach policies first",
			DeletionTime: timestamppb.New(at.Add(-12 * time.Minute))},
		{Kind: "Role", Name: "p-cluster-role", State: ready},
	}
	got := deletingMessage(views, 4)
	for _, want := range []string{
		"waiting for NodeGroup p-default", "4 resources left",
		"stuck: Role p-node-role (ReconcileError: DeleteConflict: must detach policies first) for 12m0s",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("message %q lacks %q", got, want)
		}
	}
	if strings.Contains(got, "Cluster p,") || strings.Contains(got, "p-cluster-role (") {
		t.Errorf("message %q names more than the first stage / stuck objects", got)
	}
}

// A forced deletion takes Helm Releases out of the way: without Delete in
// managementPolicies (no uninstall from an unreachable cluster), then gone.
func TestForceReleaseReleasesHelmReleases(t *testing.T) {
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	release := func(name, cluster string) *unstructured.Unstructured {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(helmRelease)
		u.SetNamespace("tenant-acme")
		u.SetName(name)
		u.SetLabels(map[string]string{crossplane.LabelCluster: cluster})
		return u
	}
	var patched []string
	k := fake.NewClientBuilder().WithScheme(scheme).WithObjects(release("karpenter", "c-1"), release("other", "c-2")).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
				pol, _, _ := unstructured.NestedStringSlice(obj.(*unstructured.Unstructured).Object, "spec", "managementPolicies")
				patched = append(patched, obj.GetName()+":"+strings.Join(pol, ","))
				return c.Patch(ctx, obj, p, opts...)
			},
		}).Build()
	s := NewClusterService(service.Deps{Kube: k})
	row := &model.Cluster{Base: model.Base{ID: "c-1"}, TenantID: "acme", DeleteForce: true}
	if err := s.forceRelease(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	if len(patched) != 1 || patched[0] != "karpenter:Observe,Create,Update,LateInitialize" {
		t.Errorf("patched %v, want only c-1's release, without Delete", patched)
	}
	left := &unstructured.UnstructuredList{}
	left.SetGroupVersionKind(helmRelease.GroupVersion().WithKind("ReleaseList"))
	if err := k.List(context.Background(), left); err != nil {
		t.Fatal(err)
	}
	if len(left.Items) != 1 || left.Items[0].GetName() != "other" {
		t.Errorf("releases left: %d, want only the other cluster's", len(left.Items))
	}
}
