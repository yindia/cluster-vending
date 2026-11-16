package admin

import (
	"context"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"connectrpc.com/connect/v2"
	awsec2 "github.com/upbound/provider-aws/v2/apis/namespaced/ec2/v1beta1"
	"google.golang.org/protobuf/types/known/timestamppb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/auth"
	"github.com/yindia/cluster-vending/pkg/crossplane"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/crossplane/managed"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

const freezeCluster = "0190a000-0000-7000-8000-000000000001"
const freezeOtherCluster = "0190a000-0000-7000-8000-000000000002"
const freezeNetwork = "0190a000-0000-7000-8000-000000000003"

func freezeService(t *testing.T) (*Service, client.Client) {
	t.Helper()
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	labels := func(key, id string) map[string]string {
		return map[string]string{cloudstore.LabelTenant: "acme", cloudstore.LabelID: id, key: id}
	}
	network := &cloudv1alpha1.Network{ObjectMeta: metav1.ObjectMeta{Name: "net", Namespace: "tenant-acme", Labels: labels(crossplane.LabelNetwork, freezeNetwork)}}
	cluster := &cloudv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "one", Namespace: "tenant-acme", Labels: labels(crossplane.LabelCluster, freezeCluster)},
		Spec: cloudv1alpha1.ClusterSpec{Network: cloudv1alpha1.ClusterNetwork{NetworkRef: cloudv1alpha1.LocalReference{Name: "net"}}}}
	other := cluster.DeepCopy()
	other.Name = "two"
	other.Labels = labels(crossplane.LabelCluster, freezeOtherCluster)
	vpc := &awsec2.VPC{ObjectMeta: metav1.ObjectMeta{Name: "vpc", Namespace: "tenant-acme", Labels: map[string]string{crossplane.LabelTenantID: "acme", crossplane.LabelNetwork: freezeNetwork}}}
	helm := func(name, id string) *unstructured.Unstructured {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(crossplaneaws.HelmReleaseGVK)
		obj.SetName(name)
		obj.SetNamespace("tenant-acme")
		obj.SetLabels(map[string]string{crossplane.LabelTenantID: "acme", crossplane.LabelCluster: id})
		return obj
	}
	k := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant-acme"}}, network, cluster, other, vpc, helm("one-helm", freezeCluster), helm("two-helm", freezeOtherCluster)).WithStatusSubresource(&cloudv1alpha1.ReleaseHold{}).Build()
	return NewService(reconcile.Deps{Kube: k}), k
}

func TestPlatformFreezeAndSelectiveUnfreeze(t *testing.T) {
	s, k := freezeService(t)
	ctx := auth.WithIdentity(t.Context(), &auth.Identity{Username: "alice"})
	result, err := s.Freeze(ctx, &cloudv1.FreezeRequest{Reason: "deploy"})
	if err != nil || result.GetClusters() != 2 || result.GetNetworks() != 1 || result.GetResources() != 0 || result.GetPhase() != "Freezing" {
		t.Fatalf("freeze: %v, %v", result, err)
	}
	confirmFreeze(t, s)
	hold := &cloudv1alpha1.ReleaseHold{}
	if err := k.Get(ctx, client.ObjectKey{Name: reconcile.FreezeName}, hold); err != nil {
		t.Fatal(err)
	}
	if hold.Spec.By != "alice" || !hold.Spec.Platform {
		t.Fatal(hold.Spec)
	}
	repeated, err := s.Freeze(ctx, &cloudv1.FreezeRequest{Reason: "deploy"})
	if err != nil || repeated.GetResources() != 0 {
		t.Fatalf("repeat: %v, %v", repeated, err)
	}
	resumed, err := s.Unfreeze(ctx, &cloudv1.UnfreezeRequest{ClusterId: freezeCluster})
	if err != nil || resumed.GetResources() != 0 {
		t.Fatalf("let through: %v, %v", resumed, err)
	}
	if err := k.Get(ctx, client.ObjectKey{Name: reconcile.FreezeName}, hold); err != nil {
		t.Fatal(err)
	}
	if len(hold.Spec.Rules) != 1 || hold.Spec.Rules[0].ClusterID != freezeCluster || hold.Spec.Rules[0].Frozen {
		t.Fatal(hold.Spec.Rules)
	}
	if slices.Contains(hold.Spec.Except, freezeOtherCluster) {
		t.Fatal("other cluster let through")
	}
	if _, err := s.Freeze(ctx, &cloudv1.FreezeRequest{Reason: "still deploying"}); err != nil {
		t.Fatal(err)
	}
	f, err := s.deps.Freeze(ctx, freezeCluster)
	if err != nil || !f.LetThrough {
		t.Fatalf("exception lost: %v, %v", f, err)
	}
	resumed, err = s.Unfreeze(ctx, &cloudv1.UnfreezeRequest{})
	if err != nil || resumed.GetResources() != 0 {
		t.Fatalf("unfreeze: %v, %v", resumed, err)
	}
	f, err = s.deps.Freeze(ctx)
	if err != nil || f != nil {
		t.Fatalf("freeze remains: %v, %v", f, err)
	}
}

func TestFreezeExpiryAndManualPause(t *testing.T) {
	s, k := freezeService(t)
	ctx := t.Context()
	manual := &awsec2.VPC{ObjectMeta: metav1.ObjectMeta{Name: "manual", Namespace: "tenant-acme",
		Labels:      map[string]string{crossplane.LabelTenantID: "acme", crossplane.LabelNetwork: freezeNetwork},
		Annotations: map[string]string{managed.PausedAnnotation: "true"}}}
	if err := k.Create(ctx, manual); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(time.Hour)
	if _, err := s.Freeze(ctx, &cloudv1.FreezeRequest{Until: timestamppb.New(until)}); err != nil {
		t.Fatal(err)
	}
	hold := &cloudv1alpha1.ReleaseHold{}
	if err := k.Get(ctx, client.ObjectKey{Name: reconcile.FreezeName}, hold); err != nil {
		t.Fatal(err)
	}
	past := metav1.NewTime(time.Now().Add(-time.Second))
	hold.Spec.Until = &past
	if err := k.Update(ctx, hold); err != nil {
		t.Fatal(err)
	}
	frozen, err := s.deps.SyncFreeze(ctx, reconcile.PauseScope{TenantID: "acme", NetworkID: freezeNetwork})
	if err != nil || frozen != nil {
		t.Fatalf("expired freeze: %v, %v", frozen, err)
	}
	if err := k.Get(ctx, client.ObjectKeyFromObject(manual), manual); err != nil {
		t.Fatal(err)
	}
	if !managed.IsPaused(manual) {
		t.Fatal("manual pause removed")
	}
}

func TestFreezeValidation(t *testing.T) {
	s, _ := freezeService(t)
	if _, err := s.Freeze(t.Context(), &cloudv1.FreezeRequest{Until: timestamppb.New(time.Now().Add(-time.Second))}); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatal(err)
	}
	if _, err := s.Unfreeze(t.Context(), &cloudv1.UnfreezeRequest{ClusterId: freezeCluster}); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatal(err)
	}
	if _, err := s.Freeze(t.Context(), &cloudv1.FreezeRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Unfreeze(t.Context(), &cloudv1.UnfreezeRequest{ClusterId: freezeCluster, NetworkId: freezeNetwork}); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatal(err)
	}
	if _, err := s.Unfreeze(t.Context(), &cloudv1.UnfreezeRequest{NetworkId: "0190a000-0000-7000-8000-000000000099"}); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatal(err)
	}
}

func TestNetworkUnfreezeResumesItsClusters(t *testing.T) {
	s, k := freezeService(t)
	if _, err := s.Freeze(t.Context(), &cloudv1.FreezeRequest{}); err != nil {
		t.Fatal(err)
	}
	result, err := s.Unfreeze(t.Context(), &cloudv1.UnfreezeRequest{NetworkId: freezeNetwork})
	if err != nil || result.GetResources() != 0 {
		t.Fatalf("unfreeze network: %v, %v", result, err)
	}
	confirmFreeze(t, s)
	for _, name := range []string{"one-helm", "two-helm"} {
		release := &unstructured.Unstructured{}
		release.SetGroupVersionKind(crossplaneaws.HelmReleaseGVK)
		if err := k.Get(t.Context(), client.ObjectKey{Namespace: "tenant-acme", Name: name}, release); err != nil {
			t.Fatal(err)
		}
		if managed.IsPaused(release) {
			t.Fatalf("%s stayed paused after network-wide unfreeze", name)
		}
	}
}
func confirmFreeze(t *testing.T, s *Service) {
	t.Helper()
	if _, err := reconcile.ConfirmFreeze(t.Context(), s.deps, time.Now()); err != nil {
		t.Fatal(err)
	}
}
func TestUnfreezeCannotDeleteARenewedFreeze(t *testing.T) {
	s, k := freezeService(t)
	if _, err := s.Freeze(t.Context(), &cloudv1.FreezeRequest{Reason: "old"}); err != nil {
		t.Fatal(err)
	}
	s.deps.Kube = interceptor.NewClient(k.(client.WithWatch), interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			hold := &cloudv1alpha1.ReleaseHold{}
			if err := c.Get(ctx, client.ObjectKey{Name: reconcile.FreezeName}, hold); err != nil {
				return err
			}
			hold.Spec.Reason, hold.Spec.Revision = "renewed", "new"
			if err := c.Update(ctx, hold); err != nil {
				return err
			}
			return c.Delete(ctx, obj, opts...)
		}})
	if _, err := s.Unfreeze(t.Context(), &cloudv1.UnfreezeRequest{}); err == nil {
		t.Fatal("stale delete succeeded")
	}
	hold := &cloudv1alpha1.ReleaseHold{}
	if err := k.Get(t.Context(), client.ObjectKey{Name: reconcile.FreezeName}, hold); err != nil {
		t.Fatal(err)
	}
	if hold.Spec.Reason != "renewed" {
		t.Fatal(hold.Spec)
	}
}

func TestResetExceptionsAndStatus(t *testing.T) {
	s, _ := freezeService(t)
	if _, err := s.Freeze(t.Context(), &cloudv1.FreezeRequest{}); err != nil {
		t.Fatal(err)
	}
	confirmFreeze(t, s)
	if _, err := s.Unfreeze(t.Context(), &cloudv1.UnfreezeRequest{ClusterId: freezeCluster}); err != nil {
		t.Fatal(err)
	}
	confirmFreeze(t, s)
	status, err := s.GetFreeze(t.Context(), &cloudv1.GetFreezeRequest{})
	if err != nil || status.GetPhase() != "Frozen" || len(status.GetExceptions()) != 1 || status.GetPausedResources() != 2 {
		t.Fatalf("status: %v, %v", status, err)
	}
	result, err := s.Freeze(t.Context(), &cloudv1.FreezeRequest{ResetExceptions: true})
	if err != nil || result.GetPhase() != "Freezing" {
		t.Fatalf("reset: %v, %v", result, err)
	}
	confirmFreeze(t, s)
	status, err = s.GetFreeze(t.Context(), &cloudv1.GetFreezeRequest{})
	if err != nil || status.GetPhase() != "Frozen" || len(status.GetExceptions()) != 0 || status.GetPausedResources() != 3 {
		t.Fatalf("reset status: %v, %v", status, err)
	}
	result, err = s.Freeze(t.Context(), &cloudv1.FreezeRequest{})
	if err != nil || result.GetPausedResources() != 3 || result.GetResources() != 0 {
		t.Fatalf("repeat counts: %v, %v", result, err)
	}
}
func TestScopedFreezeHierarchy(t *testing.T) {
	service, kubeClient := freezeService(t)
	ctx := t.Context()
	if _, err := service.Freeze(ctx, &cloudv1.FreezeRequest{NetworkId: freezeNetwork, Reason: "network maintenance"}); err != nil {
		t.Fatal(err)
	}
	confirmFreeze(t, service)
	assertState := func(clusters, networks, paused int32) {
		t.Helper()
		state, err := service.GetFreeze(ctx, &cloudv1.GetFreezeRequest{})
		if err != nil || state.Clusters != clusters || state.Networks != networks || state.PausedResources != paused {
			t.Fatalf("status: %v, %v", state, err)
		}
	}
	assertState(2, 1, 3)
	if _, err := service.Unfreeze(ctx, &cloudv1.UnfreezeRequest{ClusterId: freezeCluster}); err != nil {
		t.Fatal(err)
	}
	confirmFreeze(t, service)
	assertState(1, 1, 2)
	vpc := &awsec2.VPC{}
	if err := kubeClient.Get(ctx, client.ObjectKey{Namespace: "tenant-acme", Name: "vpc"}, vpc); err != nil {
		t.Fatal(err)
	}
	if !managed.IsPaused(vpc) {
		t.Fatal("cluster exception resumed its shared network")
	}
	if _, err := service.Unfreeze(ctx, &cloudv1.UnfreezeRequest{NetworkId: freezeNetwork}); err != nil {
		t.Fatal(err)
	}
	confirmFreeze(t, service)
	assertState(0, 0, 0)
	if _, err := service.Freeze(ctx, &cloudv1.FreezeRequest{ClusterId: freezeCluster, Reason: "cluster maintenance"}); err != nil {
		t.Fatal(err)
	}
	confirmFreeze(t, service)
	assertState(1, 0, 1)
	if _, err := service.Unfreeze(ctx, &cloudv1.UnfreezeRequest{TenantId: "acme"}); err != nil {
		t.Fatal(err)
	}
	confirmFreeze(t, service)
	assertState(0, 0, 0)
	state, err := service.GetFreeze(ctx, &cloudv1.GetFreezeRequest{})
	if err != nil || state.Phase != "Unfrozen" {
		t.Fatalf("tenant-wide unfreeze: %v, %v", state, err)
	}
}

func TestScopedUnfreezeConfirmationLeavesPlatformFrozen(t *testing.T) {
	s, k := freezeService(t)
	ctx := t.Context()
	if _, err := s.Freeze(ctx, &cloudv1.FreezeRequest{Reason: "maintenance"}); err != nil {
		t.Fatal(err)
	}
	confirmFreeze(t, s)
	result, err := s.Unfreeze(ctx, &cloudv1.UnfreezeRequest{ClusterId: freezeCluster})
	if err != nil || result.GetRevision() == "" {
		t.Fatalf("unfreeze: %v %v", result, err)
	}
	req := &cloudv1.GetFreezeRequest{ClusterId: freezeCluster}
	pending, err := s.GetFreeze(ctx, req)
	if err != nil || pending.GetPhase() != "Unfreezing" {
		t.Fatalf("before confirmation: %v %v", pending, err)
	}
	confirmFreeze(t, s)
	done, err := s.GetFreeze(ctx, req)
	if err != nil || done.GetPhase() != "Unfrozen" || done.GetRevision() != result.GetRevision() {
		t.Fatalf("after confirmation: %v %v", done, err)
	}
	global, err := s.GetFreeze(ctx, &cloudv1.GetFreezeRequest{})
	if err != nil || global.GetPhase() != "Frozen" || !global.GetPlatformFrozen() {
		t.Fatalf("platform freeze lost: %v %v", global, err)
	}
	vpc := &awsec2.VPC{}
	if err := k.Get(ctx, client.ObjectKey{Namespace: "tenant-acme", Name: "vpc"}, vpc); err != nil || !managed.IsPaused(vpc) {
		t.Fatalf("shared network was resumed: %v", err)
	}
}

func TestScopedFreezeStatusChecksLivePauses(t *testing.T) {
	s, _ := freezeService(t)
	if _, err := s.Freeze(t.Context(), &cloudv1.FreezeRequest{Reason: "maintenance"}); err != nil {
		t.Fatal(err)
	}
	confirmFreeze(t, s)
	if _, err := reconcile.SetProvidersPaused(t.Context(), s.deps, reconcile.PauseScope{TenantID: "acme", ClusterID: freezeCluster}, false); err != nil {
		t.Fatal(err)
	}
	status, err := s.GetFreeze(t.Context(), &cloudv1.GetFreezeRequest{ClusterId: freezeCluster})
	if err != nil || status.GetPhase() != "Freezing" {
		t.Fatalf("stale confirmation accepted after pause removal: %v %v", status, err)
	}
}
