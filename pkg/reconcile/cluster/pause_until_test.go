package cluster

import (
	"testing"
	"time"

	awsiam "github.com/upbound/provider-aws/v2/apis/namespaced/iam/v1beta1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/yindia/cluster-vending/pkg/crossplane"
	"github.com/yindia/cluster-vending/pkg/crossplane/managed"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// providersPaused counts the cluster's IAM Roles (its managed resources of
// one kind) that carry the providers' pause.
func (w world) providersPaused(t *testing.T) (paused, all int) {
	t.Helper()
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(awsiam.CRDGroupVersion.WithKind(awsiam.Role_Kind + "List"))
	if err := w.kube.List(t.Context(), list, client.InNamespace(worldNS), client.MatchingLabels{crossplane.LabelCluster: w.row.ID}); err != nil {
		t.Fatal(err)
	}
	for _, u := range list.Items {
		all++
		if _, ok := u.GetAnnotations()[managed.PausedAnnotation]; ok {
			paused++
		}
	}
	return paused, all
}

// A pause asked with an until ends by itself: once it has passed, the
// cluster's pass removes the pause and the providers' pause on its
// resources, and the cluster is reconciled again. A pause of another
// scope without an until stays, and with it the providers' pause.
func TestPauseEndsAtItsUntil(t *testing.T) {
	ended := time.Now().Add(-time.Minute)
	tests := []struct {
		name       string
		own        bool // the cluster is also paused on its own, with no until
		wantPaused bool
	}{
		{"the only pause ended", false, false},
		{"its own pause stays", true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t, interceptor.Funcs{}, nil)
			ctx := t.Context()
			if err := w.svc.Reconcile(ctx, worldTenant, w.row.ID); err != nil {
				t.Fatal(err)
			}
			target := model.PauseTarget{ClusterID: w.row.ID}
			if tt.own {
				w.pause(t, alice)
			}
			tier := model.PauseTarget{ClusterID: w.row.ID, Selector: model.ChannelScope("beta", "0")}
			freeze := &model.Pause{By: "bob", Since: ended.Add(-time.Hour), Reason: "freeze", Until: &ended}
			if err := w.store.Tenants().SetPause(ctx, worldTenant, tier, freeze); err != nil {
				t.Fatal(err)
			}
			if n, err := reconcile.SetProvidersPaused(ctx, w.svc.deps, worldTenant, target, true); err != nil || n == 0 {
				t.Fatalf("pause the providers: %d, %v", n, err)
			}

			if err := w.svc.Reconcile(ctx, worldTenant, w.row.ID); err != nil {
				t.Fatal(err)
			}

			paused, all := w.providersPaused(t)
			row := w.cluster(t)
			switch {
			case tt.wantPaused && (row.Paused == nil || paused != all):
				t.Fatalf("paused %v, %d of %d resources paused: the cluster's own pause must stay", row.Paused, paused, all)
			case !tt.wantPaused && (row.Paused != nil || row.PauseLapsed || paused != 0):
				t.Fatalf("paused %v (lapsed %v), %d of %d resources paused: the pause ended", row.Paused, row.PauseLapsed, paused, all)
			}
		})
	}
}
