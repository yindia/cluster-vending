package cli

import (
	"bytes"
	"strings"
	"testing"

	v1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

func TestUpdatesReviewResourcePlanBeforeApply(t *testing.T) {
	for _, network := range []bool{false, true} {
		for _, mode := range []string{"apply", "preview", "stale", "rejected"} {
			name := "cluster/" + mode
			if network {
				name = "network/" + mode
			}
			t.Run(name, func(t *testing.T) {
				plan := &v1.Plan{PlanId: "reviewed", Applicable: mode != "rejected", Changes: []*v1.PlannedChange{{Action: v1.PlannedAction_PLANNED_ACTION_DELETE, Kind: "FlowLog", Name: "removed-item", Detail: "cloud resource will be deleted"}}}
				cl := &updateCluster{current: storedCluster(), plan: plan}
				net := &updateNetwork{current: &v1.Network{TenantId: "acme", Id: inferCluster, Name: "prod", Etag: "etag-1", Spec: &v1.NetworkSpec{Region: "us-west-1"}}, plan: plan}
				manifest := clusterHead + "spec: {releaseChannel: RELEASE_CHANNEL_ALPHA}\n"
				op := ClusterUpdate
				if network {
					op = NetworkUpdate
					manifest = "apiVersion: cluster-vending.io/v1\nkind: Network\nmetadata: {name: prod, tenant: acme, id: " + inferCluster + "}\nspec: {flowLogs: {enabled: true}}\n"
				}
				var out, progress bytes.Buffer
				opts := Options{Input: writeManifest(t, manifest), Progress: &progress, Plan: mode == "preview"}
				if mode == "stale" {
					opts.PlanID = "old"
				}
				err := Run(t.Context(), op, Clients{Cluster: cl, Network: net}, opts, &out, OutputTable)
				if (err != nil) != (mode == "stale" || mode == "rejected") {
					t.Fatalf("error: %v", err)
				}
				if !strings.Contains(out.String()+progress.String(), "removed-item") {
					t.Fatalf("removal not shown: %s %s", &out, &progress)
				}
				var previews, applies int
				for _, req := range cl.requests {
					if req.ValidateOnly {
						previews++
					} else {
						applies++
						if req.PlanId != plan.PlanId {
							t.Fatal("apply not bound to preview")
						}
					}
				}
				for _, req := range net.requests {
					if req.ValidateOnly {
						previews++
					} else {
						applies++
						if req.PlanId != plan.PlanId {
							t.Fatal("apply not bound to preview")
						}
					}
				}
				wantApplies := 0
				if mode == "apply" {
					wantApplies = 1
				}
				if previews != 1 || applies != wantApplies {
					t.Fatalf("preview calls=%d, apply calls=%d", previews, applies)
				}
			})
		}
	}
}
