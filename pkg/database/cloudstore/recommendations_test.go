package cloudstore_test

import (
	"bytes"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
)

// The recorded usage is a ConfigMap next to the Cluster object, owned by
// it so that it goes with the cluster.
func TestUsageHistory(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	c := w.cluster(t, "prod")
	clusters := w.s.Clusters()

	empty, err := clusters.UsageHistory(ctx, "acme", c.ID)
	if err != nil || empty != nil {
		t.Fatalf("before any sample: %q, %v", empty, err)
	}
	for _, history := range [][]byte{[]byte("first"), []byte("second")} {
		if err := clusters.SetUsageHistory(ctx, "acme", c.ID, history); err != nil {
			t.Fatal(err)
		}
		got, err := clusters.UsageHistory(ctx, "acme", c.ID)
		if err != nil || !bytes.Equal(got, history) {
			t.Fatalf("read back %q, %v; want %q", got, err, history)
		}
	}

	cm := &corev1.ConfigMap{}
	if err := w.k.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "prod-usage-history"}, cm); err != nil {
		t.Fatal(err)
	}
	if len(cm.OwnerReferences) != 1 || cm.OwnerReferences[0].Kind != "Cluster" || cm.OwnerReferences[0].Name != "prod" ||
		cm.Labels[cloudstore.LabelID] != c.ID || cm.Labels[cloudstore.LabelUsageHistory] != "true" {
		t.Errorf("configmap = %+v", cm.ObjectMeta)
	}
	if _, err := clusters.UsageHistory(ctx, "other", c.ID); !errors.Is(err, repository.ErrNotFound) {
		t.Errorf("another tenant's read: %v", err)
	}
}

// The summary is stored on the Cluster's status and read back on the row;
// the reconciler's own status writes keep it.
func TestSetRecommendations(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	c := w.cluster(t, "prod")
	summary := model.JSON(`{"workloads":3,"containers":4,"windowDays":7,"metricsAvailable":true}`)

	if err := w.s.Clusters().SetRecommendations(ctx, "acme", c.ID, summary); err != nil {
		t.Fatal(err)
	}
	row, err := w.s.Clusters().Get(ctx, "acme", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.s.Clusters().SetStatus(ctx, "acme", c.ID, row.ETag, model.JSON(`{"state":"CLUSTER_STATE_RUNNING"}`)); err != nil {
		t.Fatal(err)
	}
	row, err = w.s.Clusters().Get(ctx, "acme", c.ID)

	if err != nil || !bytes.Contains(row.Recommendations, []byte(`"containers":4`)) {
		t.Fatalf("recommendations = %s, %v", row.Recommendations, err)
	}
	if err := w.s.Clusters().SetRecommendations(ctx, "acme", c.ID, model.JSON("not json")); !errors.Is(err, repository.ErrInvalid) {
		t.Errorf("garbage stored: %v", err)
	}
}

// The recorded usage goes with the cluster's deletion: the Cluster is
// removed with orphan propagation, so the garbage collector never deletes
// its ConfigMap.
func TestDeleteRemovesTheUsageHistory(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	c := w.cluster(t, "prod")
	if err := w.s.Clusters().SetUsageHistory(ctx, "acme", c.ID, []byte("history")); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKey{Namespace: namespace, Name: "prod-usage-history"}
	if err := w.k.Get(ctx, key, &corev1.ConfigMap{}); err != nil {
		t.Fatalf("the usage history is not stored: %v", err)
	}

	if err := w.s.Clusters().Delete(ctx, "acme", c.ID); err != nil {
		t.Fatal(err)
	}

	if err := w.k.Get(ctx, key, &corev1.ConfigMap{}); err == nil {
		t.Error("the usage history is left behind")
	}
}

// Another cluster's history of the same name, left behind, is not deleted
// with this one.
func TestDeleteLeavesAnotherClustersUsageHistory(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	c := w.cluster(t, "prod")
	other := &corev1.ConfigMap{}
	other.Namespace, other.Name = namespace, "prod-usage-history"
	other.Labels = map[string]string{cloudstore.LabelID: "someone-else"}
	if err := w.k.Create(ctx, other); err != nil {
		t.Fatal(err)
	}

	if err := w.s.Clusters().Delete(ctx, "acme", c.ID); err != nil {
		t.Fatal(err)
	}

	if err := w.k.Get(ctx, client.ObjectKeyFromObject(other), &corev1.ConfigMap{}); err != nil {
		t.Errorf("another cluster's usage history was deleted: %v", err)
	}
}
