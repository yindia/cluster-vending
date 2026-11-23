package cloudstore

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
)

// A store status write keeps the conditions other controllers own.
func TestSetStatusKeepsForeignConditions(t *testing.T) {
	obj := &cloudv1alpha1.Cluster{Status: cloudv1alpha1.ClusterStatus{Conditions: []metav1.Condition{
		{Type: cloudv1alpha1.ConditionDeprecationScan, Status: metav1.ConditionTrue, Reason: "Scanned"},
		{Type: "Stale", Status: metav1.ConditionTrue, Reason: "Old"},
	}}}
	clusters{}.setStatus(obj, []byte(`{}`), "CLUSTER_STATE_RUNNING", nil)
	for _, kind := range []string{cloudv1alpha1.ConditionDeprecationScan} {
		if !meta.IsStatusConditionTrue(obj.Status.Conditions, kind) {
			t.Fatalf("%s dropped: %+v", kind, obj.Status.Conditions)
		}
	}
	if meta.FindStatusCondition(obj.Status.Conditions, "Stale") != nil {
		t.Fatal("a contract condition survived a status that no longer has it")
	}
}
