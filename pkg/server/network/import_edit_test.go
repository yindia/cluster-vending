package network

import (
	"strings"
	"testing"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	netreconcile "github.com/yindia/cluster-vending/pkg/reconcile/network"
)

// editNetwork changes the imported network's display name and labels
// behind the API's back, as kubectl edit of its object does (the row is
// read from the object). No call of the API can do this while the
// network is imported.
func editNetwork(t *testing.T, f fixture, id string) {
	t.Helper()
	row, err := f.store.Networks().Get(t.Context(), tenant, id)
	if err != nil {
		t.Fatalf("read the network: %v", err)
	}
	row.DisplayName = "edited"
	row.Labels = map[string]string{"edited": "true"}
	if err := f.store.Networks().Update(t.Context(), row); err != nil {
		t.Fatalf("edit the network: %v", err)
	}
}

// An imported network is read-only: what was changed on its object behind
// the API's back is put back by the next pass, and the status says so.
func TestEditOfAnImportedNetworkIsIgnoredAndReported(t *testing.T) {
	f := newFixture(t)
	f.svc.deps.AWSDiscovery = fakeDiscoverer{network: existingVPC()}
	res, err := f.svc.ImportNetwork(t.Context(), importRequest(f.accountID))
	if err != nil {
		t.Fatal(err)
	}
	id := res.GetNetwork().GetId()
	f.reconcile(t, id)
	f.reconcile(t, id)
	discovered := f.get(t, id)

	editNetwork(t, f, id)
	f.reconcile(t, id)

	got := f.get(t, id)
	if got.GetDisplayName() != discovered.GetDisplayName() || len(got.GetLabels()) != len(discovered.GetLabels()) {
		t.Errorf("display name %q, labels %v; want the discovered %q, %v", got.GetDisplayName(), got.GetLabels(),
			discovered.GetDisplayName(), discovered.GetLabels())
	}
	st := got.GetStatus()
	found := false
	for _, c := range st.GetConditions() {
		if c.GetType() == netreconcile.ConditionSpecEditIgnored && c.GetStatus() == cloudv1.ConditionStatus_CONDITION_STATUS_TRUE {
			found = true
		}
	}
	if !found || !strings.Contains(st.GetMessage(), "ignored while imported") {
		t.Errorf("status = %q, conditions %v; want the ignored edit reported", st.GetMessage(), st.GetConditions())
	}
	before := f.get(t, id).GetEtag()
	f.reconcile(t, id)
	if after := f.get(t, id).GetEtag(); after != before {
		t.Error("a pass without an edit wrote the network again")
	}
}

// A takeover applies the plan that was reviewed, not an edit made behind
// the API's back just before it.
func TestTakeoverOfANetworkDropsAnEdit(t *testing.T) {
	f := newFixture(t)
	f.svc.deps.AWSDiscovery = fakeDiscoverer{network: existingVPC()}
	res, err := f.svc.ImportNetwork(t.Context(), importRequest(f.accountID))
	if err != nil {
		t.Fatal(err)
	}
	id := res.GetNetwork().GetId()
	f.reconcile(t, id)
	f.reconcile(t, id)
	discovered := f.get(t, id)

	editNetwork(t, f, id)
	taken, err := f.svc.TakeOverNetwork(t.Context(), &cloudv1.TakeOverNetworkRequest{TenantId: tenant, NetworkId: id,
		AcknowledgeNotImported: true})

	if err != nil {
		t.Fatalf("TakeOverNetwork: %v", err)
	}
	if got := taken.GetNetwork(); got.GetDisplayName() != discovered.GetDisplayName() || len(got.GetLabels()) != len(discovered.GetLabels()) {
		t.Errorf("taken over with display name %q and labels %v; want %q, %v", got.GetDisplayName(), got.GetLabels(),
			discovered.GetDisplayName(), discovered.GetLabels())
	}
}
