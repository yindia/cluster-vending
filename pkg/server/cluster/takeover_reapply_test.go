package cluster

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect/v2"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/yindia/cluster-vending/pkg/crossplane"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/fault"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	clusterreconcile "github.com/yindia/cluster-vending/pkg/reconcile/cluster"
)

// A takeover used to read each carried-over object (one of the import
// that no spec field describes) and edit it in place; it now applies it
// again from the import's stored objects, owned. legacyOwn is the edit
// the old code made, kept here as the reference the new result is
// compared with, object by object.
func legacyOwn(t *testing.T, u *unstructured.Unstructured) {
	t.Helper()
	policies := []string{"*"}
	if u.GetAnnotations()[crossplaneaws.RetainAnnotation] == "true" {
		policies = policies[:0]
		for _, action := range crossplaneaws.PolicyNoDelete {
			policies = append(policies, string(action))
		}
	}
	if err := unstructured.SetNestedStringSlice(u.Object, policies, "spec", "managementPolicies"); err != nil {
		t.Fatal(err)
	}
}

// desired is what of an object the platform decides: its spec, labels and
// annotations.
func desired(t *testing.T, u *unstructured.Unstructured) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"spec": u.Object["spec"], "labels": u.GetLabels(), "annotations": u.GetAnnotations()})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func carriedOver(objects map[string]*unstructured.Unstructured) map[string]*unstructured.Unstructured {
	out := map[string]*unstructured.Unstructured{}
	for name, u := range objects {
		if u.GetAnnotations()[crossplane.CarriedOverAnnotation] == "true" {
			out[name] = u
		}
	}
	return out
}

func takeOver(t *testing.T, w world, id string) {
	t.Helper()
	if _, err := w.svc.TakeOverCluster(t.Context(), &cloudv1.TakeOverClusterRequest{TenantId: worldTenant, ClusterId: id,
		AcknowledgeNotImported: true}); err != nil {
		t.Fatalf("TakeOverCluster: %v", err)
	}
}

// A takeover leaves every carried-over object, of every kind an import
// carries over, as the in-place edit did: the imported object with other
// management policies (without Delete for a retained one), and nothing
// else of it changed.
func TestTakeoverReappliesTheCarriedOverObjects(t *testing.T) {
	w := importWorld(t, terraformCluster())
	c := importTF(t, w)

	want := carriedOver(objectsOf(t, w, c.GetId()))
	kinds, retained := map[string]bool{}, 0
	for _, u := range want {
		kinds[u.GetKind()] = true
		if u.GetAnnotations()[crossplaneaws.RetainAnnotation] == "true" {
			retained++
		}
		if got := policies(u); len(got) == 0 || got[0] != "Observe" {
			t.Fatalf("%s %s is imported with policies %v", u.GetKind(), u.GetName(), got)
		}
	}
	for _, kind := range []string{"RolePolicyAttachment", "OpenIDConnectProvider", "Group", "Key", "IdentityProviderConfig",
		"Addon", "PodIdentityAssociation", "SecurityGroup", "SecurityGroupIngressRule", "SecurityGroupEgressRule"} {
		if !kinds[kind] {
			t.Fatalf("the import carries no %s over: the comparison would not cover it", kind)
		}
	}
	if retained == 0 {
		t.Fatal("the import carries no retained object over")
	}

	takeOver(t, w, c.GetId())
	reconcile(t, w, c.GetId())
	for _, u := range want {
		legacyOwn(t, u)
	}
	for pass := 1; pass <= 2; pass++ { // applied again, nothing changes
		got := carriedOver(objectsOf(t, w, c.GetId()))
		if len(got) != len(want) {
			t.Errorf("pass %d: %d carried-over objects, the in-place takeover had %d", pass, len(got), len(want))
		}
		for name, wanted := range want {
			g, ok := got[name]
			if !ok {
				t.Errorf("pass %d: %s is gone", pass, name)
				continue
			}
			if have, expect := desired(t, g), desired(t, wanted); have != expect {
				t.Errorf("pass %d: %s differs from the in-place result\n got  %s\n want %s", pass, name, have, expect)
			}
		}
		reconcile(t, w, c.GetId())
	}

	// They stay the cluster's, never removed for not being rendered.
	records, err := w.store.ManagedResources().ListByOwner(t.Context(), model.OwnerCluster, c.GetId())
	if err != nil {
		t.Fatal(err)
	}
	kept := 0
	for _, r := range records {
		if r.CarriedOver {
			kept++
		}
	}
	if kept != len(want) {
		t.Errorf("%d carried-over records after the takeover, want %d", kept, len(want))
	}
}

// A taken-over cluster with carried-over objects and nothing kept from
// its import (it was imported before that was kept) has nothing to apply
// them from: it says so, and no object is touched.
// An imported cluster keeps the OIDC identity provider it came with: EKS
// takes one per cluster, so spec.access.oidc is refused next to it.
func TestOIDCIdentityProviderRefusedNextToAnImportedOne(t *testing.T) {
	w := importWorld(t, terraformCluster())
	c := importTF(t, w)
	takeOver(t, w, c.GetId())
	reconcile(t, w, c.GetId())

	req := updateRequest(w, &cloudv1.ClusterSpec{Access: &cloudv1.AccessSpec{Oidc: &cloudv1.OidcIdentityProvider{
		Name: "dex", IssuerUrl: "https://dex.example.com", ClientId: "kubernetes"}}}, "spec.access.oidc")
	req.Cluster.Id = c.GetId()
	_, err := update(t, w, req)
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "spec.access.oidc") {
		t.Fatalf("spec.access.oidc on a cluster imported with an identity provider: %v, want FAILED_PRECONDITION naming the field", err)
	}
	if got := objectsOfKind(objectsOf(t, w, c.GetId()), "IdentityProviderConfig"); len(got) != 1 {
		t.Errorf("%d identity providers, want only the imported one", len(got))
	}
}

func TestTakeoverWithoutStoredImportSaysToImportAgain(t *testing.T) {
	w := importWorld(t, terraformCluster())
	c := importTF(t, w)
	ctx := t.Context()
	row, err := w.store.Clusters().Get(ctx, worldTenant, c.GetId())
	if err != nil {
		t.Fatal(err)
	}
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(row.Adoption, &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "importObjects")
	delete(fields, "importPodIdentities")
	if row.Adoption, err = json.Marshal(fields); err != nil {
		t.Fatal(err)
	}
	if err := w.store.Clusters().Update(ctx, row); err != nil {
		t.Fatal(err)
	}
	takeOver(t, w, c.GetId())
	err = w.svc.Reconcile(ctx, worldTenant, c.GetId())
	if !fault.Is(err, fault.FailedPrecondition) {
		t.Fatalf("Reconcile = %v, want the fault that says to import again", err)
	}
	for name, u := range carriedOver(objectsOf(t, w, c.GetId())) {
		if got := policies(u); !slices.Equal(got, []string{"Observe", "LateInitialize"}) && !slices.Equal(got, []string{"Observe"}) {
			t.Errorf("%s policies = %v: nothing may be owned without the import's objects", name, got)
		}
	}
	if row, err = w.store.Clusters().Get(ctx, worldTenant, c.GetId()); err != nil || row.State == clusterreconcile.StateImported {
		t.Errorf("state %s (%v), want the takeover stored", row.State, err)
	}
}
