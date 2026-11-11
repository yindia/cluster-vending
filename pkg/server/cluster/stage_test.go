package cluster

import (
	"context"
	"testing"

	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	awsiam "github.com/upbound/provider-aws/v2/apis/namespaced/iam/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/database/model"
)

func eksCluster(status string) *awseks.Cluster {
	c := &awseks.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: ngNamespace, Name: "prod"}}
	c.Spec.ForProvider.Region = ptr.To("us-east-1")
	if status != "" {
		c.Status.AtProvider.Status = ptr.To(status)
	}
	return c
}

func TestHoldBackUntilEKSActive(t *testing.T) {
	row := &model.Cluster{Base: model.Base{ID: "c-1"}}
	clusterRec := model.ManagedResource{Kind: "Cluster", Namespace: ngNamespace, Name: "prod"}
	addonRec := model.ManagedResource{Kind: "Addon", Namespace: ngNamespace, Name: "prod-addon-coredns"}
	tests := []struct {
		name    string
		records []model.ManagedResource
		live    []client.Object
		want    bool
	}{
		{"new cluster", nil, nil, true},
		{"EKS creating", []model.ManagedResource{clusterRec}, []client.Object{eksCluster("CREATING")}, true},
		{"EKS active", []model.ManagedResource{clusterRec}, []client.Object{eksCluster("ACTIVE")}, false},
		// An upgrade makes EKS UPDATING: add-ons created before stay applied.
		{"add-ons created, EKS updating", []model.ManagedResource{clusterRec, addonRec},
			[]client.Object{eksCluster("UPDATING")}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _, _ := ngService(t, tt.records, tt.live...)
			got, err := s.holdBack(context.Background(), row)
			if err != nil || got != tt.want {
				t.Errorf("holdBack = %v, %v; want %v", got, err, tt.want)
			}
		})
	}
}

func TestWithoutAfterActive(t *testing.T) {
	s, _, _ := ngService(t, nil)
	got, err := s.withoutAfterActive([]client.Object{&awsiam.Role{}, &awseks.Cluster{}, &awseks.NodeGroup{},
		&awseks.Addon{}, &awseks.PodIdentityAssociation{}, &awseks.AccessEntry{}, &awseks.AccessPolicyAssociation{},
		&awseks.ClusterAuth{}})
	if err != nil || len(got) != 4 {
		t.Fatalf("kept %d objects (%v), want role, cluster, node group, cluster auth", len(got), err)
	}
}
