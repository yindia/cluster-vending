package cluster

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/yindia/cluster-vending/pkg/database/model"
)

// New values (an UpdateComponent) reach the chart on the next pass: Helm
// upgrades it.
func TestTenantComponentUpdated(t *testing.T) {
	w, _, c := tenantWorld(t)
	w.baseDeployed(t)
	w.tenantPass(t)
	stored, _ := w.componentStatus(t, c.ID)
	stored.Values, stored.Generation = model.JSON(`{"mig":{"strategy":"mixed"}}`), stored.Generation+1
	if err := w.store.Components().Update(t.Context(), stored); err != nil {
		t.Fatal(err)
	}

	w.tenantPass(t)

	strategy, _, _ := unstructured.NestedString(w.release(t, "gpu").Object, "spec", "forProvider", "values", "mig", "strategy")
	if _, st := w.componentStatus(t, c.ID); strategy != "mixed" || st.GetObservedGeneration() != stored.Generation {
		t.Fatalf("mig.strategy %q, observed generation %d; want mixed at %d", strategy, st.GetObservedGeneration(), stored.Generation)
	}
}
