package cluster

import (
	"reflect"
	"strings"
	"testing"

	"github.com/yindia/cluster-vending/pkg/reconcile"
)

func obj(kind, ns, name string, deleting bool) reconcile.WorkloadObject {
	return reconcile.WorkloadObject{Kind: kind, Namespace: ns, Name: name, Deleting: deleting}
}

// The load balancers go first (their controller still runs), then
// Karpenter's node pools and nodes, then its node classes; a later stage
// starts only once the earlier ones are gone.
func TestPlanTeardown(t *testing.T) {
	lb := obj(reconcile.KindService, "istio-system", "istio-ingressgateway", false)
	ing := obj(reconcile.KindIngress, "web", "shop", false)
	pool := obj(reconcile.KindNodePool, "", "default", false)
	claim := obj(reconcile.KindNodeClaim, "", "default-x2k", false)
	class := obj(reconcile.KindEC2NodeClass, "", "default", false)

	tests := []struct {
		name         string
		objs         []reconcile.WorkloadObject
		wantDelete   []reconcile.WorkloadObject
		wantWaiting  int
		wantMessage  string
		wantFinished bool
	}{
		{name: "nothing: done", wantFinished: true},
		{name: "load balancers first, Karpenter waits",
			objs:       []reconcile.WorkloadObject{pool, claim, class, lb, ing},
			wantDelete: []reconcile.WorkloadObject{ing, lb}, wantWaiting: 2,
			wantMessage: "deleting 2 load balancers (Ingress web/shop, Service istio-system/istio-ingressgateway)"},
		{name: "a load balancer being deleted is waited for, not deleted again",
			objs:        []reconcile.WorkloadObject{obj(reconcile.KindService, "istio-system", "istio-ingressgateway", true), pool},
			wantWaiting: 1, wantMessage: "deleting 1 load balancer (Service istio-system/istio-ingressgateway)"},
		{name: "then the node pools and their nodes",
			objs:       []reconcile.WorkloadObject{class, claim, pool},
			wantDelete: []reconcile.WorkloadObject{pool, claim}, wantWaiting: 2,
			wantMessage: "deleting Karpenter's node pools and nodes (NodePool default, NodeClaim default-x2k)"},
		{name: "then the node classes",
			objs:       []reconcile.WorkloadObject{class},
			wantDelete: []reconcile.WorkloadObject{class}, wantWaiting: 1,
			wantMessage: "deleting Karpenter's node classes (EC2NodeClass default)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := planTeardown(tt.objs)

			if got.finished() != tt.wantFinished {
				t.Fatalf("finished = %v, want %v (%+v)", got.finished(), tt.wantFinished, got)
			}
			if !reflect.DeepEqual(got.delete, tt.wantDelete) {
				t.Errorf("delete = %v, want %v", got.delete, tt.wantDelete)
			}
			if len(got.waiting) != tt.wantWaiting || got.message != tt.wantMessage {
				t.Errorf("waiting %d %q, want %d %q", len(got.waiting), got.message, tt.wantWaiting, tt.wantMessage)
			}
		})
	}
}

// A long list is cut in the message, with how many more there are.
func TestPlanTeardownMessageIsShort(t *testing.T) {
	var objs []reconcile.WorkloadObject
	for _, n := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		objs = append(objs, obj(reconcile.KindService, "ns", n, false))
	}

	got := planTeardown(objs).message

	if !strings.HasPrefix(got, "deleting 7 load balancers (") || !strings.Contains(got, "and 2 more") {
		t.Fatalf("message = %q", got)
	}
}
