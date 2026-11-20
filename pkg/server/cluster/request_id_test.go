package cluster

import (
	"testing"

	"connectrpc.com/connect/v2"
	"github.com/google/uuid"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

func importRequest(w world, name, rid string) *cloudv1.ImportClusterRequest {
	return &cloudv1.ImportClusterRequest{TenantId: worldTenant, Name: name, NetworkId: w.row.NetworkID, RequestId: rid,
		Provider: &cloudv1.ImportClusterRequest_Aws{Aws: &cloudv1.AwsImportCluster{DefaultNodeGroupName: "default-ng"}}}
}

func wantRejectedReuse(t *testing.T, err error) {
	t.Helper()
	code := connect.CodeOf(err)
	if code != connect.CodeInvalidArgument && code != connect.CodeFailedPrecondition {
		t.Fatalf("a request ID reused for another request must be rejected, got %v", err)
	}
}

func TestImportClusterRejectsRequestIDOfACreate(t *testing.T) {
	w := importWorld(t, terraformCluster())
	rid := uuid.NewString()
	create := createRequest(w, "managed-one")
	create.RequestId = rid
	if _, err := w.svc.CreateCluster(t.Context(), create); err != nil {
		t.Fatal(err)
	}
	res, err := w.svc.ImportCluster(t.Context(), importRequest(w, "existing-two", rid))
	if err == nil {
		t.Fatalf("import returned %s for a create's request ID", res.GetCluster().GetName())
	}
	wantRejectedReuse(t, err)
}

func TestCreateClusterRejectsRequestIDOfAnImport(t *testing.T) {
	w := importWorld(t, terraformCluster())
	rid := uuid.NewString()
	if _, err := w.svc.ImportCluster(t.Context(), importRequest(w, "tf", rid)); err != nil {
		t.Fatal(err)
	}
	create := createRequest(w, "managed-one")
	create.RequestId = rid
	_, err := w.svc.CreateCluster(t.Context(), create)
	wantRejectedReuse(t, err)
}

func TestRequestIDReuseWithAnotherPayloadIsRejectedAndSamePayloadReturnsTheCluster(t *testing.T) {
	w := importWorld(t, terraformCluster())
	rid := uuid.NewString()
	first := createRequest(w, "managed-one")
	first.RequestId = rid
	created, err := w.svc.CreateCluster(t.Context(), first)
	if err != nil {
		t.Fatal(err)
	}
	same := createRequest(w, "managed-one")
	same.RequestId = rid
	again, err := w.svc.CreateCluster(t.Context(), same)
	if err != nil || again.GetCluster().GetId() != created.GetCluster().GetId() {
		t.Fatalf("identical retry: %v", err)
	}
	other := createRequest(w, "managed-two")
	other.RequestId = rid
	_, err = w.svc.CreateCluster(t.Context(), other)
	wantRejectedReuse(t, err)

	rid2 := uuid.NewString()
	imp, err := w.svc.ImportCluster(t.Context(), importRequest(w, "tf", rid2))
	if err != nil {
		t.Fatal(err)
	}
	if back, err := w.svc.ImportCluster(t.Context(), importRequest(w, "tf", rid2)); err != nil || back.GetCluster().GetId() != imp.GetCluster().GetId() {
		t.Fatalf("identical import retry: %v", err)
	}
	_, err = w.svc.ImportCluster(t.Context(), importRequest(w, "other-name", rid2))
	wantRejectedReuse(t, err)
}
