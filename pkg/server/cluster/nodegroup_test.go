package cluster

import (
	"context"
	"testing"

	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	"go.uber.org/mock/gomock"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository/mock"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

const ngNamespace = "tenant-acme"

func ngRecord(id, name string) model.ManagedResource {
	return model.ManagedResource{Base: model.Base{ID: id}, OwnerID: "c-1", Kind: kindNodeGroup,
		APIVersion: "eks.aws.m.upbound.io/v1beta1", Namespace: ngNamespace, Name: name}
}

func liveNodeGroup(name string, types ...string) *awseks.NodeGroup {
	ng := &awseks.NodeGroup{ObjectMeta: metav1.ObjectMeta{Namespace: ngNamespace, Name: name}}
	ng.Spec.ForProvider.Region = ptr.To("us-east-1")
	for _, t := range types {
		ng.Spec.ForProvider.InstanceTypes = append(ng.Spec.ForProvider.InstanceTypes, ptr.To(t))
	}
	ng.Spec.ForProvider.CapacityType = ptr.To("ON_DEMAND")
	ng.Spec.ForProvider.AMIType = ptr.To("AL2023_x86_64_STANDARD")
	return ng
}

func ngService(t *testing.T, records []model.ManagedResource, objects ...client.Object) (*ClusterService, *mock.MockManagedResources, client.Client) {
	t.Helper()
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	k := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	ctrl := gomock.NewController(t)
	store, mrs := mock.NewMockStore(ctrl), mock.NewMockManagedResources(ctrl)
	store.EXPECT().ManagedResources().Return(mrs).AnyTimes()
	mrs.EXPECT().ListByOwner(gomock.Any(), model.OwnerCluster, "c-1").Return(records, nil).AnyTimes()
	return NewClusterService(service.Deps{Store: store, Kube: k}), mrs, k
}

func TestNodeGroupName(t *testing.T) {
	plan := crossplaneaws.ClusterPlan{Prefix: "prod-1a2b3c4d", NodePool: crossplaneaws.NodePoolPlan{
		InstanceTypes: []string{"t3.small"}, CapacityType: "ON_DEMAND", AMIType: "AL2023_x86_64_STANDARD"}}
	row := &model.Cluster{Base: model.Base{ID: "c-1"}}
	replacement := crossplaneaws.ReplacementNodeGroupName(plan.Prefix, plan.NodePool)
	tests := []struct {
		name     string
		records  []model.ManagedResource
		live     []client.Object
		want     string
		wantKeep int
	}{
		{"new cluster", nil, nil, "prod-1a2b3c4d-default", 0},
		{"same settings", []model.ManagedResource{ngRecord("1", "prod-1a2b3c4d-default")},
			[]client.Object{liveNodeGroup("prod-1a2b3c4d-default", "t3.small")}, "prod-1a2b3c4d-default", 0},
		{"instance type changed", []model.ManagedResource{ngRecord("1", "prod-1a2b3c4d-default")},
			[]client.Object{liveNodeGroup("prod-1a2b3c4d-default", "t3.medium")}, replacement, 1},
		{"object gone", []model.ManagedResource{ngRecord("1", "prod-1a2b3c4d-default")}, nil, "prod-1a2b3c4d-default", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _, _ := ngService(t, tt.records, tt.live...)
			got, keep, err := s.nodeGroupName(context.Background(), row, plan)
			if err != nil || got != tt.want || len(keep) != tt.wantKeep {
				t.Errorf("got %q keep %d err %v; want %q keep %d", got, len(keep), err, tt.want, tt.wantKeep)
			}
		})
	}
}

func TestRetireNodeGroups(t *testing.T) {
	old, replacement := ngRecord("1", "prod-default"), ngRecord("2", "prod-default-abc123")
	records := []model.ManagedResource{old, replacement}
	view := func(name string, st cloudv1.ManagedResourceState) *cloudv1.ManagedResource {
		return &cloudv1.ManagedResource{Kind: kindNodeGroup, Name: name, State: st}
	}
	const ready, creating, missing = cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY,
		cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_CREATING, cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_MISSING

	// Replacement not ready: the old node group stays.
	s, _, k := ngService(t, records, liveNodeGroup("prod-default", "t3.medium"))
	if err := s.retireNodeGroups(context.Background(), records, []*cloudv1.ManagedResource{
		view(old.Name, ready), view(replacement.Name, creating)}); err != nil {
		t.Fatal(err)
	}
	if err := k.Get(context.Background(), client.ObjectKey{Namespace: ngNamespace, Name: old.Name}, &awseks.NodeGroup{}); err != nil {
		t.Fatalf("old node group deleted before its replacement was ready: %v", err)
	}

	// Replacement ready: the old one is deleted.
	if err := s.retireNodeGroups(context.Background(), records, []*cloudv1.ManagedResource{
		view(old.Name, ready), view(replacement.Name, ready)}); err != nil {
		t.Fatal(err)
	}
	if err := k.Get(context.Background(), client.ObjectKey{Namespace: ngNamespace, Name: old.Name}, &awseks.NodeGroup{}); err == nil {
		t.Fatal("old node group not deleted once the replacement was ready")
	}

	// Old one gone: its record is forgotten.
	s, mrs, _ := ngService(t, records)
	mrs.EXPECT().Delete(gomock.Any(), old.ID).Return(nil)
	if err := s.retireNodeGroups(context.Background(), records, []*cloudv1.ManagedResource{
		view(old.Name, missing), view(replacement.Name, ready)}); err != nil {
		t.Fatal(err)
	}
}
