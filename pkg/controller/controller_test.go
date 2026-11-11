package controller

import (
	"context"
	"errors"
	"testing"

	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/yindia/cluster-vending/pkg/crossplane"
)

func TestOwnerOf(t *testing.T) {
	ng := &awseks.NodeGroup{ObjectMeta: metav1.ObjectMeta{Labels: crossplane.ClusterLabels("acme", "c-1")}}
	got := ownerOf(context.Background(), ng)
	if len(got) != 1 || got[0].Namespace != "acme" || got[0].Name != "cluster/c-1" {
		t.Errorf("owner = %v", got)
	}
	if got := ownerOf(context.Background(), &awseks.NodeGroup{}); got != nil {
		t.Errorf("unlabelled object mapped to %v", got)
	}
}

type fake struct {
	got      string
	changing bool
	err      error
}

func (f *fake) Reconcile(_ context.Context, tenant, id string) (bool, error) {
	f.got = tenant + "/" + id
	return f.changing, f.err
}

func TestReconcileRoutesAndRequeues(t *testing.T) {
	networks, clusters := &fake{}, &fake{changing: true}
	r := &reconciler{networks: networks, clusters: clusters}
	res, err := r.Reconcile(context.Background(), request("acme", kindCluster, "c-1"))
	if err != nil || clusters.got != "acme/c-1" || res.RequeueAfter != requeueAfter {
		t.Errorf("cluster: %+v %v, got %q", res, err, clusters.got)
	}
	res, err = r.Reconcile(context.Background(), request("acme", kindNetwork, "n-1"))
	if err != nil || networks.got != "acme/n-1" || res != (reconcile.Result{}) {
		t.Errorf("network: %+v %v, got %q", res, err, networks.got)
	}
	networks.err = errors.New("boom")
	if _, err := r.Reconcile(context.Background(), request("acme", kindNetwork, "n-1")); err == nil {
		t.Error("error not returned for backoff")
	}
}
