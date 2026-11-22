package tenant

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	awsec2 "github.com/upbound/provider-aws/v2/apis/namespaced/ec2/v1beta1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/auth"
	"github.com/yindia/cluster-vending/pkg/crossplane"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	v1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/reconcile"
	"github.com/yindia/cluster-vending/pkg/server/admin"
)

const (
	dashboardNetwork        = "0190a000-0000-7000-8000-000000000001"
	dashboardCluster        = "0190a000-0000-7000-8000-000000000002"
	dashboardForeignNetwork = "0190a000-0000-7000-8000-000000000003"
	dashboardForeignCluster = "0190a000-0000-7000-8000-000000000004"
)

func dashboardIdentity(t *testing.T) context.Context {
	t.Helper()
	return auth.WithIdentity(t.Context(), &auth.Identity{Subject: "tenant-user"})
}

func dashboardWorld(t *testing.T, extra ...client.Object) (*TenantService, client.Client) {
	t.Helper()
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	objects := []client.Object{}
	for _, tenant := range []string{"acme", "foreign-tenant"} {
		network, cluster, name := dashboardNetwork, dashboardCluster, "own-cluster"
		if tenant != "acme" {
			network, cluster, name = dashboardForeignNetwork, dashboardForeignCluster, "foreign-private-cluster"
		}
		labels := map[string]string{cloudstore.LabelTenant: tenant, cloudstore.LabelID: network, crossplane.LabelNetwork: network}
		objects = append(objects,
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant-" + tenant, Labels: map[string]string{cloudstore.LabelTenant: tenant}}},
			&cloud.Network{ObjectMeta: metav1.ObjectMeta{Name: "net", Namespace: "tenant-" + tenant, Labels: labels}, Spec: cloud.NetworkSpec{Tier: cloud.ClusterTier2}},
			&cloud.Cluster{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "tenant-" + tenant, Labels: map[string]string{cloudstore.LabelTenant: tenant, cloudstore.LabelID: cluster, cloudstore.LabelNetworkID: network, crossplane.LabelCluster: cluster}},
				Spec:   cloud.ClusterSpec{ReleaseChannel: cloud.ReleaseChannelStable, UpgradeMode: cloud.UpgradeModeAutomatic, Network: cloud.ClusterNetwork{NetworkRef: cloud.LocalReference{Name: "net"}}, ControlPlane: &cloud.ControlPlaneSpec{KubernetesVersion: "1.34"}},
				Status: cloud.ClusterStatus{CurrentKubernetesVersion: "1.34", Phase: cloud.ClusterPhaseRunning}},
			&awsec2.VPC{ObjectMeta: metav1.ObjectMeta{Name: "vpc", Namespace: "tenant-" + tenant, Labels: map[string]string{crossplane.LabelTenantID: tenant, crossplane.LabelNetwork: network}}},
		)
		helm := &unstructured.Unstructured{}
		helm.SetGroupVersionKind(crossplaneaws.HelmReleaseGVK)
		helm.SetName("chart")
		helm.SetNamespace("tenant-" + tenant)
		helm.SetLabels(map[string]string{crossplane.LabelTenantID: tenant, crossplane.LabelCluster: cluster})
		objects = append(objects, helm)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(append(objects, extra...)...).WithStatusSubresource(&cloud.ReleaseHold{}).Build()
	return NewTenantService(reconcile.Deps{Kube: c, Store: cloudstore.New(c), ManagedResourceKinds: []schema.GroupVersionKind{{Group: "ec2.aws.m.upbound.io", Version: "v1beta1", Kind: "VPC"}, crossplaneaws.HelmReleaseGVK}}), c
}

func dashboardHold(t *testing.T, c client.Client) *cloud.ReleaseHold {
	t.Helper()
	hold := &cloud.ReleaseHold{}
	if err := c.Get(t.Context(), client.ObjectKey{Name: reconcile.FreezeName}, hold); err != nil {
		t.Fatal(err)
	}
	return hold
}

func TestDashboardTenantFreezeLifecycleAndIsolation(t *testing.T) {
	s, c := dashboardWorld(t)
	ctx := dashboardIdentity(t)
	foreign := admin.NewService(s.deps)
	if _, err := foreign.Freeze(t.Context(), &v1.FreezeRequest{Target: &v1.FreezeTarget{Scope: &v1.FreezeTarget_TenantId{TenantId: "foreign-tenant"}}, Reason: "foreign-secret"}); err != nil {
		t.Fatal(err)
	}
	result, err := s.Freeze(ctx, &v1.TenantFreezeRequest{TenantId: "acme", Reason: "own maintenance"})
	if err != nil {
		t.Fatal(err)
	}
	if result.GetStatus().GetTarget().GetTenantId() != "acme" || result.Clusters != 1 || result.Networks != 1 || result.TotalResources != 2 {
		t.Fatalf("own-only freeze: %v", result)
	}
	if dashboardHold(t, c).Spec.Scoped != true {
		t.Fatal("tenant freeze became global")
	}
	updated, err := s.Freeze(ctx, &v1.TenantFreezeRequest{TenantId: "acme", Reason: "updated own reason", Until: timestamppb.New(time.Now().Add(time.Hour))})
	if err != nil || updated.GetStatus().GetFreeze().GetReason() != "updated own reason" {
		t.Fatalf("update: %v %v", updated, err)
	}
	view, err := s.GetFreeze(dashboardIdentity(t), &v1.TenantGetFreezeRequest{TenantId: "acme"})
	if err != nil || view.Clusters != 1 || view.Networks != 1 || view.TotalResources != 2 || len(view.Scopes) != 0 || len(view.Exceptions) != 0 {
		t.Fatalf("tenant read: %v %v", view, err)
	}
	encoded, _ := protojson.Marshal(view)
	if strings.Contains(string(encoded), "foreign") {
		t.Fatalf("foreign status leaked: %s", encoded)
	}
	if _, err := s.Unfreeze(ctx, &v1.TenantUnfreezeRequest{TenantId: "acme"}); err != nil {
		t.Fatal(err)
	}
	hold := dashboardHold(t, c)
	if !reconcile.FreezeForTarget(hold, time.Now(), reconcile.FreezeTarget{TenantID: "foreign-tenant", NetworkID: dashboardForeignNetwork, ClusterID: dashboardForeignCluster}).Holds() {
		t.Fatal("foreign tenant's hold was removed")
	}
	if reconcile.FreezeForTarget(hold, time.Now(), reconcile.FreezeTarget{TenantID: "acme", NetworkID: dashboardNetwork, ClusterID: dashboardCluster}).Holds() {
		t.Fatal("own scope not resumed")
	}
}

func TestDashboardOwnedResourceScopes(t *testing.T) {
	for name, target := range map[string]*v1.FreezeTarget{
		"network": {Scope: &v1.FreezeTarget_NetworkId{NetworkId: dashboardNetwork}},
		"cluster": {Scope: &v1.FreezeTarget_ClusterId{ClusterId: dashboardCluster}},
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := dashboardWorld(t)
			ctx := dashboardIdentity(t)
			out, err := s.Freeze(ctx, &v1.TenantFreezeRequest{TenantId: "acme", Target: target})
			if err != nil || !proto.Equal(out.GetStatus().GetTarget(), target) {
				t.Fatalf("freeze: %v %v", out, err)
			}
			if _, err := s.GetFreeze(ctx, &v1.TenantGetFreezeRequest{TenantId: "acme", Target: target}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Unfreeze(ctx, &v1.TenantUnfreezeRequest{TenantId: "acme", Target: target}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDashboardRejectsForeignAndPlatformTargetsWithoutMutation(t *testing.T) {
	s, c := dashboardWorld(t)
	ctx := dashboardIdentity(t)
	if _, err := admin.NewService(s.deps).Freeze(t.Context(), &v1.FreezeRequest{Target: &v1.FreezeTarget{Scope: &v1.FreezeTarget_TenantId{TenantId: "foreign-tenant"}}, Reason: "foreign-secret"}); err != nil {
		t.Fatal(err)
	}
	before := dashboardHold(t, c).DeepCopy()
	for name, target := range map[string]*v1.FreezeTarget{
		"platform":        {Scope: &v1.FreezeTarget_Platform{Platform: true}},
		"platform false":  {Scope: &v1.FreezeTarget_Platform{Platform: false}},
		"empty":           {},
		"foreign tenant":  {Scope: &v1.FreezeTarget_TenantId{TenantId: "foreign-tenant"}},
		"foreign network": {Scope: &v1.FreezeTarget_NetworkId{NetworkId: dashboardForeignNetwork}},
		"foreign cluster": {Scope: &v1.FreezeTarget_ClusterId{ClusterId: dashboardForeignCluster}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := s.Freeze(ctx, &v1.TenantFreezeRequest{TenantId: "acme", Target: target}); err == nil {
				t.Fatal("freeze accepted")
			}
			if _, err := s.Unfreeze(ctx, &v1.TenantUnfreezeRequest{TenantId: "acme", Target: target}); err == nil {
				t.Fatal("unfreeze accepted")
			}
			if _, err := s.GetFreeze(ctx, &v1.TenantGetFreezeRequest{TenantId: "acme", Target: target}); err == nil {
				t.Fatal("read accepted")
			}
			if !protoSpecEqual(before, dashboardHold(t, c)) {
				t.Fatal("rejected target mutated policy")
			}
		})
	}
}

func protoSpecEqual(a, b *cloud.ReleaseHold) bool {
	return a.ResourceVersion == b.ResourceVersion && reflect.DeepEqual(a.Spec, b.Spec)
}

func TestDashboardPlatformFreezeCannotBeBypassed(t *testing.T) {
	s, c := dashboardWorld(t)
	if _, err := admin.NewService(s.deps).Freeze(t.Context(), &v1.FreezeRequest{Reason: "foreign-private platform maintenance"}); err != nil {
		t.Fatal(err)
	}
	before := dashboardHold(t, c).DeepCopy()
	for _, target := range []*v1.FreezeTarget{nil, {Scope: &v1.FreezeTarget_NetworkId{NetworkId: dashboardNetwork}}, {Scope: &v1.FreezeTarget_ClusterId{ClusterId: dashboardCluster}}} {
		if _, err := s.Unfreeze(dashboardIdentity(t), &v1.TenantUnfreezeRequest{TenantId: "acme", Target: target}); connect.CodeOf(err) != connect.CodeFailedPrecondition {
			t.Fatalf("platform bypass: %v", err)
		}
	}
	if !protoSpecEqual(before, dashboardHold(t, c)) {
		t.Fatal("platform hold changed")
	}
	view, err := s.GetFreeze(dashboardIdentity(t), &v1.TenantGetFreezeRequest{TenantId: "acme"})
	if err != nil || !view.GetPlatformFrozen() || view.GetMessage() != platformFreezeReadOnly {
		t.Fatalf("readonly platform view: %v %v", view, err)
	}
	encoded, _ := protojson.Marshal(view)
	if strings.Contains(string(encoded), "foreign") || strings.Contains(string(encoded), "anonymous") {
		t.Fatalf("global metadata leaked: %s", encoded)
	}
}

func TestDashboardConcurrentPlatformFreezeCannotBeBypassed(t *testing.T) {
	s, c := dashboardWorld(t)
	ctx := dashboardIdentity(t)
	if _, err := s.Freeze(ctx, &v1.TenantFreezeRequest{TenantId: "acme"}); err != nil {
		t.Fatal(err)
	}
	reads := 0
	s.deps.KubeReader = interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, underlying client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if _, ok := obj.(*cloud.ReleaseHold); ok && key.Name == reconcile.FreezeName {
			reads++
			if reads == 2 {
				hold := dashboardHold(t, c)
				hold.Spec.Scoped, hold.Spec.Reason, hold.Spec.Revision = false, "foreign-race-secret", "global-race"
				if err := c.Update(ctx, hold); err != nil {
					return err
				}
			}
		}
		return underlying.Get(ctx, key, obj, opts...)
	}})
	_, err := s.Unfreeze(ctx, &v1.TenantUnfreezeRequest{TenantId: "acme"})
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("race guard: %v", err)
	}
	hold := dashboardHold(t, c)
	if !reconcile.GlobalFreezeActive(hold, time.Now()) || hold.Spec.Revision != "global-race" {
		t.Fatal("concurrent global hold overwritten")
	}
	for _, rule := range hold.Spec.Rules {
		if !rule.Frozen {
			t.Fatal("allow rule added beneath global hold")
		}
	}
}

func TestDashboardRejectsUnknownLegacyMutationFields(t *testing.T) {
	s, c := dashboardWorld(t)
	req := &v1.TenantFreezeRequest{TenantId: "acme"}
	req.ProtoReflect().SetUnknown([]byte{0x30, 0x01}) // former reset_exceptions = true
	if _, err := s.Freeze(dashboardIdentity(t), req); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatal(err)
	}
	if err := c.Get(t.Context(), client.ObjectKey{Name: reconcile.FreezeName}, &cloud.ReleaseHold{}); err == nil {
		t.Fatal("legacy field wrote policy")
	}
}
