package network

import (
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/yindia/cluster-vending/pkg/database/storetest"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

func TestViewOfClusterIDs(t *testing.T) {
	store := storetest.NewStore(t)
	storetest.MustTenant(t, store, "acme")
	network := storetest.MustNetwork(t, store, "acme", "shared")
	other := storetest.MustNetwork(t, store, "acme", "other")
	empty := storetest.MustNetwork(t, store, "acme", "empty")
	prod := storetest.MustCluster(t, store, "acme", "prod-eks", network)
	staging := storetest.MustCluster(t, store, "acme", "staging-eks", network)
	unrelated := storetest.MustCluster(t, store, "acme", "unrelated-eks", other)
	for _, id := range []string{prod.ID, staging.ID, unrelated.ID} {
		if _, err := uuid.Parse(id); err != nil {
			t.Fatalf("fixture cluster ID %q is not a UUID: %v", id, err)
		}
	}

	// Both the store and the observed management cluster use in-memory clients.
	s := New(reconcile.Deps{Store: store, Kube: storetest.NewClient(t)})
	for _, tc := range []struct {
		name string
		id   string
		want []string
	}{
		{"shared network", network.ID, []string{prod.ID, staging.ID}},
		{"other network", other.ID, []string{unrelated.ID}},
		{"empty network", empty.ID, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row, err := store.Networks().Get(t.Context(), "acme", tc.id)
			if err != nil {
				t.Fatal(err)
			}
			view, err := s.viewOf(t.Context(), row, nil)
			if err != nil {
				t.Fatal(err)
			}
			got := slices.Clone(view.GetStatus().GetClusterIds())
			want := slices.Clone(tc.want)
			slices.Sort(got)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Fatalf("status.cluster_ids = %v, want cluster UUIDs %v", got, want)
			}
		})
	}
}
