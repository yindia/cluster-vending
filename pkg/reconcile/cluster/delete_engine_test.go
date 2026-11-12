package cluster

import (
	"errors"
	"testing"

	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	awsiam "github.com/upbound/provider-aws/v2/apis/namespaced/iam/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/crossplane/managed"
	"github.com/yindia/cluster-vending/pkg/fault"
)

// An object the deletion cannot read is UNAVAILABLE, named; any other
// error passes as it is.
func TestDeleteErrorMapping(t *testing.T) {
	other := errors.New("delete tier 0: boom")
	for name, tt := range map[string]struct {
		err  error
		kind fault.Kind // 0: not a fault, the error as it is
		msg  string
	}{
		"unreadable object": {&managed.ReadError{Ref: managed.Ref{Kind: "NodeGroup", Name: "prod-default"}, Err: errors.New("timeout")},
			fault.Unavailable, "cannot read NodeGroup prod-default from the management cluster"},
		"other": {other, 0, other.Error()},
	} {
		got := deleteError(tt.err)
		f, isFault := fault.As(got)
		switch {
		case tt.kind == 0 && (isFault || got.Error() != tt.msg):
			t.Errorf("%s: %v, want the error as it is", name, got)
		case tt.kind != 0 && (!isFault || f.Kind != tt.kind || f.Message != tt.msg):
			t.Errorf("%s: %v, want fault %d %q", name, got, tt.kind, tt.msg)
		}
	}
}

// A cluster's objects have no delete tiers: all are deleted at once, and
// their Usages order the deletion.
func TestClusterDeleteOrderHasNoTiers(t *testing.T) {
	if len(crossplaneaws.ClusterDeleteOrder) != 0 {
		t.Errorf("ClusterDeleteOrder = %v, want no tiers", crossplaneaws.ClusterDeleteOrder)
	}
}

// An import's objects are applied as rendered; only the node group's
// apply body is adjusted.
func TestImportSet(t *testing.T) {
	objects := []client.Object{
		&awsiam.Role{ObjectMeta: metav1.ObjectMeta{Name: "role"}},
		&awseks.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "prod"}},
		&awseks.NodeGroup{ObjectMeta: metav1.ObjectMeta{Name: "prod-default"}},
		&awseks.Addon{ObjectMeta: metav1.ObjectMeta{Name: "prod-vpc-cni"}},
	}
	set := importSet(objects)
	if len(set) != len(objects) {
		t.Fatalf("%d items for %d objects", len(set), len(objects))
	}
	for i, item := range set {
		_, isNodeGroup := item.Object.(*awseks.NodeGroup)
		if item.Object != objects[i] || len(item.After) != 0 || (item.Adjust != nil) != isNodeGroup {
			t.Errorf("item %d (%T): object kept %t, waits for %d, adjusted %t", i, item.Object,
				item.Object == objects[i], len(item.After), item.Adjust != nil)
		}
	}
}
