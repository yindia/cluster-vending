package cluster

import (
	"strings"
	"testing"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	reconcilecluster "github.com/yindia/cluster-vending/pkg/reconcile/cluster"
)

// spec.naas (field 15) was removed with sub-tenants. A binary create from
// a client built against the old contract still carries it: refused,
// saying why. Nothing is stored. (JSON drops the unknown field.)
func TestCreateClusterRefusesTheRemovedNaaSField(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{}, nil)
	req := createRequest(w, "tenancy")
	req.Cluster.Spec = withVarintField(t, req.GetCluster().GetSpec(), 15)

	_, err := w.svc.CreateCluster(t.Context(), req)

	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), reconcilecluster.ErrNaaSRemoved) {
		t.Errorf("err = %v, want invalid_argument saying spec.naas was removed", err)
	}
	if rows, err := w.store.Clusters().ListByName(t.Context(), worldTenant, "tenancy"); err != nil || len(rows) != 0 {
		t.Errorf("stored %d clusters (%v), want none", len(rows), err)
	}
}

// An update that names spec.naas is refused the same way, plan or not, and
// changes nothing.
func TestUpdateRefusesTheRemovedNaaSPath(t *testing.T) {
	w := appliedWorld(t)
	before := w.cluster(t)
	for _, validateOnly := range []bool{true, false} {
		req := planRequest(w, &cloudv1.ClusterSpec{}, "spec.naas")
		req.ValidateOnly = validateOnly
		_, err := update(t, w, req)
		if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), reconcilecluster.ErrNaaSRemoved) {
			t.Errorf("validate_only %v: err = %v, want invalid_argument saying spec.naas was removed", validateOnly, err)
		}
	}
	if after := w.cluster(t); after.Generation != before.Generation || after.ETag != before.ETag {
		t.Errorf("a refused update changed the cluster: generation %d -> %d", before.Generation, after.Generation)
	}
}

// A cluster stored by a build with NaaS still has "naas": true in its
// spec: it is ignored on read, and the cluster is served without it.
func TestAStoredNaaSIsIgnored(t *testing.T) {
	w := appliedWorld(t)
	row := w.cluster(t)
	row.Spec = []byte(strings.Replace(string(row.Spec), "{", `{"naas":true,`, 1))
	if err := w.store.Clusters().Update(t.Context(), row); err != nil {
		t.Fatal(err)
	}

	res, err := w.svc.GetCluster(t.Context(), &cloudv1.GetClusterRequest{TenantId: worldTenant, ClusterId: row.ID})

	if err != nil {
		t.Fatalf("GetCluster of a cluster stored with naas: %v", err)
	}
	if len(res.GetCluster().GetSpec().ProtoReflect().GetUnknown()) != 0 {
		t.Error("the stored naas came back as an unknown field")
	}
}

// withVarintField is m with a varint field number set to 1 (true), as an
// old client sends a bool: an unknown field once decoded.
func withVarintField[M proto.Message](t *testing.T, m M, number protowire.Number) M {
	t.Helper()
	raw, err := proto.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	raw = protowire.AppendVarint(protowire.AppendTag(raw, number, protowire.VarintType), 1)
	out := m.ProtoReflect().New().Interface().(M)
	if err := proto.Unmarshal(raw, out); err != nil {
		t.Fatal(err)
	}
	return out
}
