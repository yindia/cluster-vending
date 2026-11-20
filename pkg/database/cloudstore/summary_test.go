package cloudstore_test

import (
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
)

// The summary a pass records (node totals, Reachable) is kept by the
// reconciler's own status writes, which rebuild the status from the contract.
func TestSetSummarySurvivesStatusWrites(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	c := w.cluster(t, "prod")
	row, err := w.s.Clusters().Get(ctx, "acme", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	summary := model.JSON(`{"reachable":true,"at":"2026-11-20T12:00:00Z",` +
		`"nodes":{"total":3,"ready":2,"notReady":1,"nodesCollectedAt":"2026-11-20T12:00:00Z"},` +
		`"nodeGroups":{"system":{"ready":2,"allocatableCPUMillis":4000,"requestedCPUMillis":900,"podCount":7}},"requestsKnown":true}`)

	if err := w.s.Clusters().SetSummary(ctx, "acme", c.ID, summary); err != nil {
		t.Fatal(err)
	}
	if err := w.s.Clusters().SetStatus(ctx, "acme", c.ID, row.ETag, model.JSON(`{"state":"CLUSTER_STATE_RUNNING","nodeGroups":[{"name":"system","nodes":3}]}`)); err != nil {
		t.Fatal(err)
	}

	obj := &cloudv1alpha1.Cluster{}
	if err := w.k.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "prod"}, obj); err != nil {
		t.Fatal(err)
	}
	if obj.Status.Nodes == nil || obj.Status.Nodes.Total != 3 {
		t.Fatalf("nodes = %+v", obj.Status.Nodes)
	}
	if c := meta.FindStatusCondition(obj.Status.Conditions, cloudv1alpha1.ConditionReachable); c == nil || c.Status != "True" {
		t.Fatalf("reachable = %+v", c)
	}
	// The group listed by the contract keeps the summary's usage.
	if len(obj.Status.NodeGroups) != 1 || obj.Status.NodeGroups[0].RequestedCPUMillis != 900 {
		t.Fatalf("groups = %+v", obj.Status.NodeGroups)
	}
	if err := w.s.Clusters().SetSummary(ctx, "acme", c.ID, model.JSON("not json")); !errors.Is(err, repository.ErrInvalid) {
		t.Errorf("garbage stored: %v", err)
	}
}
