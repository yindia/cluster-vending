package cluster

import (
	"context"
	"encoding/json"
	"testing"

	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

// The node count is the autoscaler's after creation: the apply body sets
// desiredSize in initProvider, never in forProvider.
func TestApplyNodeGroupSendsDesiredSizeAtCreateOnly(t *testing.T) {
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	k := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Apply: func(_ context.Context, _ client.WithWatch, obj runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
			raw, err := json.Marshal(obj)
			if err != nil {
				return err
			}
			return json.Unmarshal(raw, &body)
		},
	}).Build()
	ng := &awseks.NodeGroup{ObjectMeta: metav1.ObjectMeta{Namespace: "t", Name: "prod-default"}}
	ng.Spec.ForProvider.Region = ptr.To("us-east-1")
	ng.Spec.ForProvider.ScalingConfig = &awseks.ScalingConfigParameters{
		MinSize: ptr.To(1.0), MaxSize: ptr.To(3.0), DesiredSize: ptr.To(2.0)}
	if err := NewClusterService(service.Deps{Kube: k}).applyOne(context.Background(), ng); err != nil {
		t.Fatal(err)
	}
	spec := body["spec"].(map[string]any)
	forProvider := spec["forProvider"].(map[string]any)["scalingConfig"].(map[string]any)
	if _, ok := forProvider["desiredSize"]; ok || forProvider["maxSize"] != 3.0 {
		t.Errorf("forProvider.scalingConfig = %v, want min/max without desiredSize", forProvider)
	}
	initProvider, _ := spec["initProvider"].(map[string]any)
	if got := initProvider["scalingConfig"].(map[string]any)["desiredSize"]; got != 2.0 {
		t.Errorf("initProvider.scalingConfig.desiredSize = %v, want 2", got)
	}
}
