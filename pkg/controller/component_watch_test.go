package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
)

// A Component wakes the cluster it is on, and nothing for a cluster that
// does not exist.
func TestComponentWakesItsCluster(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	const id = "01a1d0a6-69d5-7e15-965e-fc6a53fa3d33"
	obj := handMadeCluster(t, "prod", "net")
	obj.Labels = map[string]string{cloudstore.LabelTenant: tenant, cloudstore.LabelID: id}
	w.must(t, w.k.Create(ctx, obj))
	tests := []struct {
		name    string
		cluster string
		want    int
	}{
		{"on a cluster", id, 1},
		{"on no cluster", "01a1d0a6-0000-7000-8000-000000000000", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &cloudv1alpha1.Component{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "prod-gpu",
				Labels: map[string]string{cloudstore.LabelClusterID: tt.cluster}}}

			reqs := componentCluster(&owners{}, w.k)(ctx, c)

			if len(reqs) != tt.want || (tt.want == 1 && reqs[0] != request("prod")) {
				t.Fatalf("requests %v, want %d for prod", reqs, tt.want)
			}
		})
	}
}
