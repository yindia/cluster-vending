package cluster

import (
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// scanned records a scan of the world's cluster: removed findings first,
// as the controller stores them.
func scanned(t *testing.T, w world, removed, deprecated int) *cloudv1.ClusterDeprecations {
	t.Helper()
	d := &cloudv1.ClusterDeprecations{TargetVersion: "1.37", Total: int32(removed + deprecated), Removed: int32(removed),
		ScannedAt: timestamppb.New(time.Date(2026, 11, 13, 4, 0, 0, 0, time.UTC))}
	for i := range removed + deprecated {
		d.Findings = append(d.Findings, &cloudv1.DeprecationFinding{Namespace: "web", Kind: "Ingress",
			Name: string(rune('a' + i)), ApiVersion: "extensions/v1beta1", Removed: i < removed})
	}
	raw, err := protojson.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.store.Clusters().SetDeprecations(t.Context(), worldTenant, w.row.ID, raw); err != nil {
		t.Fatalf("SetDeprecations: %v", err)
	}
	return d
}

func names(findings []*cloudv1.DeprecationFinding) string {
	var out []string
	for _, f := range findings {
		out = append(out, f.GetName())
	}
	return strings.Join(out, "")
}

func TestListClusterDeprecations(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{}, nil)
	ctx := t.Context()
	list := func(req *cloudv1.ListClusterDeprecationsRequest) *cloudv1.ListClusterDeprecationsResponse {
		t.Helper()
		req.TenantId, req.ClusterId = worldTenant, w.row.ID
		res, err := w.svc.ListClusterDeprecations(ctx, req)
		if err != nil {
			t.Fatalf("ListClusterDeprecations: %v", err)
		}
		return res
	}

	// Never scanned: nothing, and no scan time.
	if res := list(&cloudv1.ListClusterDeprecationsRequest{}); len(res.GetFindings()) != 0 || res.GetScannedAt() != nil {
		t.Fatalf("before any scan: %v", res)
	}
	etag := w.cluster(t).ETag
	want := scanned(t, w, 2, 3)

	tests := []struct {
		name      string
		req       *cloudv1.ListClusterDeprecationsRequest
		wantNames string
		wantNext  string
	}{
		{"everything", &cloudv1.ListClusterDeprecationsRequest{}, "abcde", ""},
		{"first page", &cloudv1.ListClusterDeprecationsRequest{PageSize: 2}, "ab", "2"},
		{"next page", &cloudv1.ListClusterDeprecationsRequest{PageSize: 2, PageToken: "2"}, "cd", "4"},
		{"last page", &cloudv1.ListClusterDeprecationsRequest{PageSize: 2, PageToken: "4"}, "e", ""},
		{"past the end", &cloudv1.ListClusterDeprecationsRequest{PageToken: "5"}, "", ""},
		{"only removed", &cloudv1.ListClusterDeprecationsRequest{OnlyRemoved: true}, "ab", ""},
		{"only removed, paged", &cloudv1.ListClusterDeprecationsRequest{OnlyRemoved: true, PageSize: 1}, "a", "1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := list(tt.req)

			if names(res.GetFindings()) != tt.wantNames || res.GetNextPageToken() != tt.wantNext {
				t.Errorf("got %q next %q, want %q next %q", names(res.GetFindings()), res.GetNextPageToken(), tt.wantNames, tt.wantNext)
			}
			if res.GetTargetVersion() != "1.37" || res.GetTotal() != 5 || res.GetRemoved() != 2 || !res.GetScannedAt().AsTime().Equal(want.GetScannedAt().AsTime()) {
				t.Errorf("summary = %s %d %d %v", res.GetTargetVersion(), res.GetTotal(), res.GetRemoved(), res.GetScannedAt())
			}
		})
	}

	// The scan is not a change of the cluster: the etag stays.
	if after := w.cluster(t).ETag; after != etag {
		t.Errorf("recording a scan changed the etag: %s -> %s", etag, after)
	}
}

func TestListClusterDeprecationsRefusals(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{}, nil)
	scanned(t, w, 1, 0)
	tests := []struct {
		name string
		req  *cloudv1.ListClusterDeprecationsRequest
		want connect.Code
	}{
		{"no tenant", &cloudv1.ListClusterDeprecationsRequest{ClusterId: w.row.ID}, connect.CodeInvalidArgument},
		{"not a cluster id", &cloudv1.ListClusterDeprecationsRequest{TenantId: worldTenant, ClusterId: "prod"}, connect.CodeInvalidArgument},
		{"token of something else", &cloudv1.ListClusterDeprecationsRequest{TenantId: worldTenant, ClusterId: w.row.ID, PageToken: "abc"}, connect.CodeInvalidArgument},
		{"token past the list", &cloudv1.ListClusterDeprecationsRequest{TenantId: worldTenant, ClusterId: w.row.ID, PageToken: "9"}, connect.CodeInvalidArgument},
		{"negative page size", &cloudv1.ListClusterDeprecationsRequest{TenantId: worldTenant, ClusterId: w.row.ID, PageSize: -1}, connect.CodeInvalidArgument},
		{"another tenant's cluster", &cloudv1.ListClusterDeprecationsRequest{TenantId: "other", ClusterId: w.row.ID}, connect.CodeNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := w.svc.ListClusterDeprecations(t.Context(), tt.req)
			if connect.CodeOf(err) != tt.want {
				t.Fatalf("got %v, want %s", err, tt.want)
			}
		})
	}
}

// A cluster's status carries the counts of the last scan, never the list.
func TestGetClusterCarriesTheScanSummary(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{}, nil)
	get := func() *cloudv1.ClusterDeprecations {
		t.Helper()
		res, err := w.svc.GetCluster(t.Context(), &cloudv1.GetClusterRequest{TenantId: worldTenant, ClusterId: w.row.ID})
		if err != nil {
			t.Fatalf("GetCluster: %v", err)
		}
		return res.GetCluster().GetStatus().GetDeprecations()
	}
	if d := get(); d != nil {
		t.Fatalf("before any scan: %v", d)
	}
	scanned(t, w, 1, 2)

	d := get()

	if d.GetTotal() != 3 || d.GetRemoved() != 1 || d.GetTargetVersion() != "1.37" || len(d.GetFindings()) != 0 {
		t.Fatalf("summary = %v", d)
	}
	// A pass of the reconciler records the cluster's status; the scan stays.
	if err := w.svc.Reconcile(t.Context(), worldTenant, w.row.ID); err != nil {
		t.Fatal(err)
	}
	if d := get(); d.GetTotal() != 3 {
		t.Fatalf("after a reconcile the scan is gone: %v", d)
	}
}
