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

// withUnknownField is m with a length-delimited field number appended, as a
// client built against an older contract sends it: an unknown field once
// decoded.
func withUnknownField[M proto.Message](t *testing.T, m M, number protowire.Number) M {
	t.Helper()
	raw, err := proto.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	raw = protowire.AppendTag(raw, number, protowire.BytesType)
	raw = protowire.AppendBytes(raw, []byte{0x0a, 0x09, 'm', '6', 'i', '.', 'l', 'a', 'r', 'g', 'e'})
	out := m.ProtoReflect().New().Interface().(M)
	if err := proto.Unmarshal(raw, out); err != nil {
		t.Fatal(err)
	}
	return out
}

// A binary create that still carries a removed node group field
// (ClusterSpec.default_node_pool 4, node_groups 16,
// AwsClusterSpec.default_node_pool 4) is refused: the platform owns node
// groups. Nothing is stored.
func TestCreateClusterRefusesTheRemovedNodeGroupFields(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{}, nil)
	for name, edit := range map[string]func(*cloudv1.ClusterSpec) *cloudv1.ClusterSpec{
		"spec.default_node_pool": func(s *cloudv1.ClusterSpec) *cloudv1.ClusterSpec { return withUnknownField(t, s, 4) },
		"spec.node_groups":       func(s *cloudv1.ClusterSpec) *cloudv1.ClusterSpec { return withUnknownField(t, s, 16) },
		"spec.aws.default_node_pool": func(s *cloudv1.ClusterSpec) *cloudv1.ClusterSpec {
			s.Provider = &cloudv1.ClusterSpec_Aws{Aws: withUnknownField(t, s.GetAws(), 4)}
			return s
		},
	} {
		req := createRequest(w, "refused")
		req.Cluster.Spec = edit(req.GetCluster().GetSpec())
		_, err := w.svc.CreateCluster(t.Context(), req)
		if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), reconcilecluster.ErrNodeGroupsPlatformOwned) {
			t.Errorf("%s: err = %v, want invalid_argument naming that the platform owns node groups", name, err)
		}
	}
	rows, err := w.store.Clusters().ListByName(t.Context(), worldTenant, "refused")
	if err != nil || len(rows) != 0 {
		t.Errorf("stored %d clusters (%v), want none", len(rows), err)
	}

	// Unknown fields of other numbers are not refused.
	req := createRequest(w, "accepted")
	req.Cluster.Spec = withUnknownField(t, req.GetCluster().GetSpec(), 99)
	if _, err := w.svc.CreateCluster(t.Context(), req); err != nil {
		t.Errorf("spec with an unknown field 99: %v, want accepted", err)
	}
}

// A create names no node group: the stored spec has none of the old fields.
func TestCreateClusterStoresNoNodeGroups(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{}, nil)
	res, err := w.svc.CreateCluster(t.Context(), createRequest(w, "plain"))
	if err != nil {
		t.Fatal(err)
	}
	row, err := w.store.Clusters().Get(t.Context(), worldTenant, res.GetCluster().GetId())
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"defaultNodePool", "nodeGroups"} {
		if strings.Contains(string(row.Spec), field) || strings.Contains(string(row.Input), field) {
			t.Errorf("stored spec or input has %s: %s", field, row.Spec)
		}
	}
}
