package cluster

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yindia/cluster-vending/pkg/reconcile"
)

func obj(kind, ns, name string, deleting bool) reconcile.WorkloadObject {
	return reconcile.WorkloadObject{Kind: kind, Namespace: ns, Name: name, Deleting: deleting}
}

// Only the load balancers are the first stage: Karpenter's objects are left
// to the node groups' stage.
func TestPlanLoadBalancers(t *testing.T) {
	lb := obj(reconcile.KindService, "istio-system", "istio-ingressgateway", false)
	ing := obj(reconcile.KindIngress, "web", "shop", false)
	pool := obj(reconcile.KindNodePool, "", "default", false)

	tests := []struct {
		name         string
		objs         []reconcile.WorkloadObject
		wantDelete   []reconcile.WorkloadObject
		wantWaiting  int
		wantMessage  string
		wantFinished bool
	}{
		{name: "nothing: done", wantFinished: true},
		{name: "only Karpenter's objects: done", objs: []reconcile.WorkloadObject{pool}, wantFinished: true},
		{name: "ingresses, then services",
			objs:       []reconcile.WorkloadObject{pool, lb, ing},
			wantDelete: []reconcile.WorkloadObject{ing, lb}, wantWaiting: 2,
			wantMessage: "deleting 2 load balancers (Ingress web/shop, Service istio-system/istio-ingressgateway)"},
		{name: "a load balancer being deleted is waited for, not deleted again",
			objs:        []reconcile.WorkloadObject{obj(reconcile.KindService, "istio-system", "istio-ingressgateway", true), pool},
			wantWaiting: 1, wantMessage: "deleting 1 load balancer (Service istio-system/istio-ingressgateway)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := planTeardown(loadBalancerStages, tt.objs)

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

// The node groups: user pools, then the platform pool, then the NodeClaims
// left, then the node classes; each only once the one before is gone.
func TestPlanNodeGroups(t *testing.T) {
	user := obj(reconcile.KindNodePool, "", "gpu", false)
	platformPool := obj(reconcile.KindNodePool, "", "platform", false)
	claim := obj(reconcile.KindNodeClaim, "", "gpu-x2k", false)
	class := obj(reconcile.KindEC2NodeClass, "", "gpu", false)
	at := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	deleting := reconcile.WorkloadObject{Kind: reconcile.KindNodePool, Name: "gpu", Deleting: true, DeletedAt: at}

	tests := []struct {
		name        string
		objs        []reconcile.WorkloadObject
		wantDelete  []reconcile.WorkloadObject
		wantMessage string
		wantSince   time.Time
	}{
		{name: "nothing: done"},
		{name: "a load balancer is not its business", objs: []reconcile.WorkloadObject{obj(reconcile.KindService, "ns", "lb", false)}},
		{name: "the user pools first; the platform pool, claims and classes wait",
			objs:       []reconcile.WorkloadObject{class, claim, platformPool, user},
			wantDelete: []reconcile.WorkloadObject{user}, wantMessage: "deleting the user node pools and waiting for Karpenter to terminate their nodes (NodePool gpu)"},
		{name: "a pool being deleted is waited for since its request",
			objs: []reconcile.WorkloadObject{platformPool, deleting}, wantSince: at,
			wantMessage: "deleting the user node pools and waiting for Karpenter to terminate their nodes (NodePool gpu)"},
		{name: "then the platform pool",
			objs:       []reconcile.WorkloadObject{class, claim, platformPool},
			wantDelete: []reconcile.WorkloadObject{platformPool}, wantMessage: "deleting the platform node pool and waiting for Karpenter to terminate its nodes (NodePool platform)"},
		{name: "then what nodes are left",
			objs:        []reconcile.WorkloadObject{class, claim},
			wantMessage: "waiting for Karpenter to terminate its remaining nodes (NodeClaim gpu-x2k)", wantDelete: []reconcile.WorkloadObject{claim}},
		{name: "then the node classes",
			objs:       []reconcile.WorkloadObject{class},
			wantDelete: []reconcile.WorkloadObject{class}, wantMessage: "deleting Karpenter's node classes and waiting for it to remove their instance profiles (EC2NodeClass gpu)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := planTeardown(nodeGroupStages, tt.objs)

			if !reflect.DeepEqual(got.delete, tt.wantDelete) || got.message != tt.wantMessage || !got.since.Equal(tt.wantSince) {
				t.Errorf("delete %v message %q since %v, want %v %q %v", got.delete, got.message, got.since, tt.wantDelete, tt.wantMessage, tt.wantSince)
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

	got := planTeardown(loadBalancerStages, objs).message

	if !strings.HasPrefix(got, "deleting 7 load balancers (") || !strings.Contains(got, "and 2 more") {
		t.Fatalf("message = %q", got)
	}
}
