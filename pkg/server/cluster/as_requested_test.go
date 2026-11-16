package cluster

import (
	"testing"

	"google.golang.org/protobuf/proto"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// as_requested returns the cluster as the client asked for it (the stored
// input), not the spec the server resolved: update -f compares a manifest
// with it, so the defaults the server filled in are no change.
func TestGetClusterAsRequested(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{}, nil)
	ctx := t.Context()
	req := createRequest(w, "asked")
	req.Cluster.Labels = map[string]string{"team": "a"}
	created, err := w.svc.CreateCluster(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	id := created.GetCluster().GetId()

	asked, err := w.svc.GetCluster(ctx, &cloudv1.GetClusterRequest{TenantId: worldTenant, ClusterId: id, AsRequested: true})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := w.svc.GetCluster(ctx, &cloudv1.GetClusterRequest{TenantId: worldTenant, ClusterId: id})
	if err != nil {
		t.Fatal(err)
	}

	if !asked.GetAsRequested() || !proto.Equal(asked.GetCluster().GetSpec(), req.GetCluster().GetSpec()) {
		t.Errorf("as requested (%v): spec %v\nwant the request's %v", asked.GetAsRequested(), asked.GetCluster().GetSpec(), req.GetCluster().GetSpec())
	}
	if asked.GetCluster().GetLabels()["team"] != "a" || asked.GetCluster().GetEtag() != resolved.GetCluster().GetEtag() {
		t.Errorf("labels %v, etag %q (resolved %q)", asked.GetCluster().GetLabels(), asked.GetCluster().GetEtag(), resolved.GetCluster().GetEtag())
	}
	if resolved.GetAsRequested() || resolved.GetCluster().GetStatus().GetTier() == cloudv1.ClusterTier_CLUSTER_TIER_UNSPECIFIED {
		t.Errorf("without as_requested: as_requested %v, tier %v; want the resolved spec", resolved.GetAsRequested(), resolved.GetCluster().GetStatus().GetTier())
	}
}

// A cluster stored without an input (the world's, made by the fixture
// like an import's) gets its resolved spec, and says so.
func TestGetClusterAsRequestedWithoutInput(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{}, nil)

	res, err := w.svc.GetCluster(t.Context(), &cloudv1.GetClusterRequest{TenantId: worldTenant, ClusterId: w.row.ID, AsRequested: true})

	if err != nil || res.GetAsRequested() || res.GetCluster().GetSpec() == nil {
		t.Fatalf("as_requested %v, spec %v, %v; want the resolved spec", res.GetAsRequested(), res.GetCluster().GetSpec(), err)
	}
}
