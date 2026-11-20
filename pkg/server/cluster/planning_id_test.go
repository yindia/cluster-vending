package cluster

import (
	"testing"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// The etag of a request is checked on its own: a plan made with it is the
// plan made without it.
func TestPlanIDDoesNotCoverTheRequestEtag(t *testing.T) {
	req := func(etag string) *cloudv1.UpdateClusterRequest {
		return &cloudv1.UpdateClusterRequest{TenantId: "acme", Cluster: &cloudv1.Cluster{Id: "c-1", Name: "prod", Etag: etag}}
	}
	versions := []string{"cluster=c-1/e1"}
	without, err := planID(req(""), versions)
	if err != nil {
		t.Fatal(err)
	}
	with, err := planID(req("e1"), versions)
	if err != nil || with != without {
		t.Fatalf("plan id with the etag %s (%v), without %s", with, err, without)
	}
	other, err := planID(req(""), []string{"cluster=c-1/e2"})
	if err != nil || other == without {
		t.Fatal("the row's version no longer binds the plan")
	}
}
