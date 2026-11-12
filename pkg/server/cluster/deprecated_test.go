package cluster

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/types/known/timestamppb"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	reconcilecluster "github.com/yindia/cluster-vending/pkg/reconcile/cluster"
)

func deprecated(apiVersion, kind, name, deprecatedIn, removedIn string) *cloudv1.DeprecatedResource {
	return &cloudv1.DeprecatedResource{ApiVersion: apiVersion, Kind: kind, Namespace: "web", Name: name,
		DeprecatedIn: deprecatedIn, RemovedIn: removedIn,
		Source: cloudv1.DeprecatedResourceSource_DEPRECATED_RESOURCE_SOURCE_LAST_APPLIED_CONFIGURATION}
}

// withScan records scan with the cluster's status, as the controller does.
func (w world) withScan(t *testing.T, scan *cloudv1.DeprecatedResourceScan) {
	t.Helper()
	row := w.cluster(t)
	raw, err := reconcilecluster.Encode(&cloudv1.ClusterStatus{DeprecatedResources: scan})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.store.Clusters().SetStatus(t.Context(), worldTenant, row.ID, row.ETag, raw); err != nil {
		t.Fatal(err)
	}
}

func names(scan *cloudv1.DeprecatedResourceScan) []string {
	out := make([]string, 0, len(scan.GetResources()))
	for _, r := range scan.GetResources() {
		out = append(out, r.GetName())
	}
	return out
}

func TestListDeprecatedResourcesNarrowsTheStoredScanToTheTarget(t *testing.T) {
	scanTime := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	stored := &cloudv1.DeprecatedResourceScan{
		ScanTime: timestamppb.New(scanTime), KubernetesVersion: "1.28", Total: 3,
		Resources: []*cloudv1.DeprecatedResource{
			deprecated("policy/v1beta1", "PodDisruptionBudget", "pdb", "1.21", "1.25"),
			deprecated("flowcontrol.apiserver.k8s.io/v1beta2", "FlowSchema", "fs-beta2", "1.26", "1.29"),
			deprecated("flowcontrol.apiserver.k8s.io/v1beta3", "FlowSchema", "fs-beta3", "1.29", "1.32"),
		},
	}
	tests := []struct {
		name       string
		target     string
		wantTarget string
		wantNames  []string
	}{
		{name: "no target: the version the cluster ran at the scan", wantTarget: "1.28", wantNames: []string{"pdb", "fs-beta2"}},
		{name: "the version of a planned upgrade", target: "1.29", wantTarget: "1.29", wantNames: []string{"pdb", "fs-beta2", "fs-beta3"}},
		{name: "an older version", target: "1.22", wantTarget: "1.22", wantNames: []string{"pdb"}},
		{name: "a version before every deprecation", target: "1.9", wantTarget: "1.9", wantNames: []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange
			w := newWorld(t, interceptor.Funcs{}, nil)
			w.withScan(t, stored)

			// Act
			res, err := w.svc.ListDeprecatedResources(t.Context(), &cloudv1.ListDeprecatedResourcesRequest{
				TenantId: worldTenant, ClusterId: w.row.ID, TargetVersion: tt.target})

			// Assert
			if err != nil {
				t.Fatalf("ListDeprecatedResources: %v", err)
			}
			if res.GetTargetVersion() != tt.wantTarget {
				t.Errorf("target %q, want %q", res.GetTargetVersion(), tt.wantTarget)
			}
			got := names(res.GetScan())
			if len(got) != len(tt.wantNames) {
				t.Fatalf("resources %q, want %q", got, tt.wantNames)
			}
			for i := range got {
				if got[i] != tt.wantNames[i] {
					t.Errorf("resources %q, want %q", got, tt.wantNames)
				}
			}
			scan := res.GetScan()
			if scan.GetTotal() != 3 || scan.GetKubernetesVersion() != "1.28" || !scan.GetScanTime().AsTime().Equal(scanTime) {
				t.Errorf("scan %v: what the scan found must stay", scan)
			}
		})
	}
}

func TestListDeprecatedResourcesOfAClusterThatWasNotScanned(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{}, nil)

	res, err := w.svc.ListDeprecatedResources(t.Context(), &cloudv1.ListDeprecatedResourcesRequest{
		TenantId: worldTenant, ClusterId: w.row.ID})

	if err != nil {
		t.Fatalf("ListDeprecatedResources: %v", err)
	}
	if res.GetScan() != nil || res.GetTargetVersion() != "" {
		t.Errorf("response %v, want no scan", res)
	}
}

// A cluster whose only scan failed has the reason and nothing to narrow.
func TestListDeprecatedResourcesReturnsWhyTheScanFailed(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{}, nil)
	w.withScan(t, &cloudv1.DeprecatedResourceScan{ScanTime: timestamppb.Now(), Message: "the tenant role was refused"})

	res, err := w.svc.ListDeprecatedResources(t.Context(), &cloudv1.ListDeprecatedResourcesRequest{
		TenantId: worldTenant, ClusterId: w.row.ID})

	if err != nil {
		t.Fatalf("ListDeprecatedResources: %v", err)
	}
	if res.GetScan().GetMessage() != "the tenant role was refused" || res.GetTargetVersion() != "" {
		t.Errorf("response %v", res)
	}
}

func TestListDeprecatedResourcesRefuses(t *testing.T) {
	tests := []struct {
		name string
		req  func(id string) *cloudv1.ListDeprecatedResourcesRequest
		want connect.Code
	}{
		{name: "a cluster that does not exist", want: connect.CodeNotFound,
			req: func(string) *cloudv1.ListDeprecatedResourcesRequest {
				return &cloudv1.ListDeprecatedResourcesRequest{TenantId: worldTenant, ClusterId: "0190a5b0-0000-7000-8000-000000000000"}
			}},
		{name: "another tenant's cluster", want: connect.CodeNotFound,
			req: func(id string) *cloudv1.ListDeprecatedResourcesRequest {
				return &cloudv1.ListDeprecatedResourcesRequest{TenantId: "other", ClusterId: id}
			}},
		{name: "a target that is not a minor version", want: connect.CodeInvalidArgument,
			req: func(id string) *cloudv1.ListDeprecatedResourcesRequest {
				return &cloudv1.ListDeprecatedResourcesRequest{TenantId: worldTenant, ClusterId: id, TargetVersion: "v1.31.2"}
			}},
		{name: "a cluster ID that is not a UUID", want: connect.CodeInvalidArgument,
			req: func(string) *cloudv1.ListDeprecatedResourcesRequest {
				return &cloudv1.ListDeprecatedResourcesRequest{TenantId: worldTenant, ClusterId: "prod"}
			}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t, interceptor.Funcs{}, nil)

			_, err := w.svc.ListDeprecatedResources(t.Context(), tt.req(w.row.ID))

			if connect.CodeOf(err) != tt.want {
				t.Errorf("error %v (%s), want %s", err, connect.CodeOf(err), tt.want)
			}
		})
	}
}

// The handler answers from the store: it reads nothing in the management
// cluster, and has no way into the tenant's.
func TestListDeprecatedResourcesReadsTheStoreOnly(t *testing.T) {
	// Arrange
	var reads atomic.Int32
	w := newWorld(t, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			reads.Add(1)
			return c.Get(ctx, key, obj, opts...)
		},
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			reads.Add(1)
			return c.List(ctx, list, opts...)
		},
	}, nil)
	w.withScan(t, &cloudv1.DeprecatedResourceScan{ScanTime: timestamppb.Now(), KubernetesVersion: "1.31"})
	reads.Store(0)

	// Act
	_, err := w.svc.ListDeprecatedResources(t.Context(), &cloudv1.ListDeprecatedResourcesRequest{
		TenantId: worldTenant, ClusterId: w.row.ID})

	// Assert
	if err != nil {
		t.Fatalf("ListDeprecatedResources: %v", err)
	}
	if reads.Load() != 0 {
		t.Errorf("%d reads of the management cluster", reads.Load())
	}
}
