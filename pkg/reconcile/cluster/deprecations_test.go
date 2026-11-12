package cluster

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	awscloud "github.com/yindia/cluster-vending/pkg/cloud/aws"
	"github.com/yindia/cluster-vending/pkg/deprecation"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// clusterAPI is the way into a tenant's cluster, recording who asked.
type clusterAPI struct {
	err   error
	calls []string
}

func (f *clusterAPI) EKSKubernetesAccess(_ context.Context, access awscloud.Access, region, name string) (awscloud.KubernetesAccess, error) {
	f.calls = append(f.calls, access.RoleARN+" "+region+" "+name)
	if f.err != nil {
		return awscloud.KubernetesAccess{}, f.err
	}
	return awscloud.KubernetesAccess{Endpoint: "https://eks.example", CA: []byte("ca"), Token: "k8s-aws-v1.token"}, nil
}

func pdbFinding(name string) deprecation.Finding {
	return deprecation.Finding{
		Object: deprecation.Object{APIVersion: "policy/v1beta1", Kind: "PodDisruptionBudget", Namespace: "web", Name: name,
			Source: deprecation.SourceHelmRelease, HelmRelease: "web/shop"},
		Rule: deprecation.Rule{APIVersion: "policy/v1beta1", Kind: "PodDisruptionBudget", Replacement: "policy/v1",
			DeprecatedIn: 21, RemovedIn: 25},
	}
}

// scanWorld is a world whose reconciler scans: api is the way into the
// cluster, scan what the cluster holds. The configs the scanner was given
// are recorded.
func scanWorld(t *testing.T, api *clusterAPI, scan func() (deprecation.Result, error)) (world, *[]*rest.Config) {
	t.Helper()
	var configs []*rest.Config
	w := newWorld(t, interceptor.Funcs{}, nil, func(d *reconcile.Deps) {
		d.AWSClusterAPI = api
		d.ScanCluster = func(_ context.Context, cfg *rest.Config) (deprecation.Result, error) {
			configs = append(configs, cfg)
			return scan()
		}
	})
	return w, &configs
}

func at(t *testing.T, when time.Time) {
	t.Helper()
	before := now
	now = func() time.Time { return when }
	t.Cleanup(func() { now = before })
}

var scanClock = time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)

func TestScanDue(t *testing.T) {
	scanned := func(ago time.Duration, message string) *cloudv1.DeprecatedResourceScan {
		return &cloudv1.DeprecatedResourceScan{ScanTime: timestamppb.New(scanClock.Add(-ago)), Message: message}
	}
	tests := []struct {
		name string
		prev *cloudv1.DeprecatedResourceScan
		want bool
	}{
		{name: "never scanned", prev: nil, want: true},
		{name: "scanned a moment ago", prev: scanned(time.Minute, ""), want: false},
		{name: "scanned just under the interval ago", prev: scanned(scanInterval-time.Second, ""), want: false},
		{name: "scanned the interval ago", prev: scanned(scanInterval, ""), want: true},
		{name: "a failed scan is not tried again sooner", prev: scanned(time.Hour, "unreachable"), want: false},
		{name: "a failed scan is tried again after the interval", prev: scanned(scanInterval, "unreachable"), want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := scanDue(tt.prev, scanClock); got != tt.want {
				t.Errorf("scanDue = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestScanProtoKeepsTheFirstFindingsAndCountsAll(t *testing.T) {
	// Arrange
	result := deprecation.Result{Version: 31}
	for i := range maxDeprecatedResources + 5 {
		result.Findings = append(result.Findings, pdbFinding(fmt.Sprintf("pdb-%03d", i)))
	}

	// Act
	got := scanProto(result, scanClock)

	// Assert
	if got.GetTotal() != maxDeprecatedResources+5 || len(got.GetResources()) != maxDeprecatedResources {
		t.Fatalf("total %d, %d resources; want %d and %d", got.GetTotal(), len(got.GetResources()),
			maxDeprecatedResources+5, maxDeprecatedResources)
	}
	if got.GetKubernetesVersion() != "1.31" || !got.GetScanTime().AsTime().Equal(scanClock) || got.GetMessage() != "" {
		t.Errorf("version %q, time %v, message %q", got.GetKubernetesVersion(), got.GetScanTime().AsTime(), got.GetMessage())
	}
	first := got.GetResources()[0]
	want := &cloudv1.DeprecatedResource{ApiVersion: "policy/v1beta1", Kind: "PodDisruptionBudget", Namespace: "web", Name: "pdb-000",
		ReplacementApiVersion: "policy/v1", DeprecatedIn: "1.21", RemovedIn: "1.25",
		Source: cloudv1.DeprecatedResourceSource_DEPRECATED_RESOURCE_SOURCE_HELM_RELEASE, HelmRelease: "web/shop"}
	if first.String() != want.String() {
		t.Errorf("first resource\n got %v\nwant %v", first, want)
	}
}

func TestScanDeprecationsRecordsWhatARunningClusterHolds(t *testing.T) {
	// Arrange
	at(t, scanClock)
	api := &clusterAPI{}
	w, configs := scanWorld(t, api, func() (deprecation.Result, error) {
		return deprecation.Result{Version: 31, Findings: []deprecation.Finding{pdbFinding("shop")}}, nil
	})
	row := w.cluster(t)
	row.State = StateRunning
	st := &cloudv1.ClusterStatus{}

	// Act
	w.svc.scanDeprecations(t.Context(), row, st)

	// Assert
	scan := st.GetDeprecatedResources()
	if scan.GetTotal() != 1 || len(scan.GetResources()) != 1 || scan.GetResources()[0].GetName() != "shop" || scan.GetMessage() != "" {
		t.Fatalf("scan %v", scan)
	}
	// The cluster is reached as the tenant role of its account, by its EKS
	// name, with what EKS says of it: no Secret, no stored credential.
	if want := []string{"arn:aws:iam::444455556666:role/provisioner us-east-1 prod"}; len(api.calls) != 1 || api.calls[0] != want[0] {
		t.Errorf("access asked for %q, want %q", api.calls, want)
	}
	if len(*configs) != 1 {
		t.Fatalf("%d scans, want 1", len(*configs))
	}
	cfg := (*configs)[0]
	if cfg.Host != "https://eks.example" || cfg.BearerToken != "k8s-aws-v1.token" || string(cfg.CAData) != "ca" ||
		cfg.Timeout != scanRequestTimeout || cfg.ExecProvider != nil || cfg.Insecure {
		t.Errorf("config %+v", cfg)
	}
}

func TestScanDeprecationsLeavesAClusterAlone(t *testing.T) {
	recent := &cloudv1.DeprecatedResourceScan{ScanTime: timestamppb.New(scanClock.Add(-time.Hour)), KubernetesVersion: "1.31"}
	tests := []struct {
		name  string
		state string
		prev  *cloudv1.DeprecatedResourceScan
		noAPI bool
	}{
		{name: "that is being created", state: StateProvisioning},
		{name: "that is being updated", state: StateUpdating},
		{name: "that is being deleted", state: StateDeleting},
		{name: "that is only imported", state: StateImported},
		{name: "that was scanned within the interval", state: StateRunning, prev: recent},
		{name: "when nothing reaches into clusters (the API handlers)", state: StateRunning, noAPI: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange
			at(t, scanClock)
			api := &clusterAPI{}
			w, configs := scanWorld(t, api, func() (deprecation.Result, error) { return deprecation.Result{Version: 31}, nil })
			if tt.noAPI {
				w.svc.deps.AWSClusterAPI = nil
			}
			row := w.cluster(t)
			row.State = tt.state
			st := &cloudv1.ClusterStatus{DeprecatedResources: tt.prev}

			// Act
			w.svc.scanDeprecations(t.Context(), row, st)

			// Assert
			if len(api.calls) != 0 || len(*configs) != 0 {
				t.Errorf("the cluster was reached: %q, %d scans", api.calls, len(*configs))
			}
			if st.GetDeprecatedResources() != tt.prev {
				t.Errorf("the scan changed: %v", st.GetDeprecatedResources())
			}
		})
	}
}

func TestAFailedScanKeepsTheLastFindingsAndSaysWhy(t *testing.T) {
	prev := scanProto(deprecation.Result{Version: 30, Findings: []deprecation.Finding{pdbFinding("shop")}}, scanClock.Add(-scanInterval))
	tests := []struct {
		name        string
		accessErr   error
		scanErr     error
		wantMessage string
	}{
		{name: "the tenant role is refused", accessErr: fmt.Errorf("%w: assume role", awscloud.ErrAccessDenied),
			wantMessage: "the tenant role was refused"},
		{name: "EKS has no such cluster", accessErr: fmt.Errorf("%w: describe EKS cluster prod", awscloud.ErrNotFound),
			wantMessage: "EKS does not have the cluster"},
		{name: "the cluster does not answer", scanErr: fmt.Errorf("server version: %w", context.DeadlineExceeded),
			wantMessage: "did not answer in time"},
		{name: "the cluster refuses", scanErr: errors.New("list deployments.apps: forbidden"),
			wantMessage: "cannot read the cluster's Kubernetes API: list deployments.apps: forbidden"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange
			at(t, scanClock)
			w, _ := scanWorld(t, &clusterAPI{err: tt.accessErr}, func() (deprecation.Result, error) {
				return deprecation.Result{}, tt.scanErr
			})
			row := w.cluster(t)
			row.State = StateRunning
			st := &cloudv1.ClusterStatus{DeprecatedResources: prev}

			// Act
			w.svc.scanDeprecations(t.Context(), row, st)

			// Assert
			scan := st.GetDeprecatedResources()
			if !strings.Contains(scan.GetMessage(), tt.wantMessage) {
				t.Errorf("message %q, want it to contain %q", scan.GetMessage(), tt.wantMessage)
			}
			if !scan.GetScanTime().AsTime().Equal(scanClock) {
				t.Errorf("scan time %v: a failed scan must count, or it is tried on every pass", scan.GetScanTime().AsTime())
			}
			if scan.GetKubernetesVersion() != "1.30" || scan.GetTotal() != 1 || len(scan.GetResources()) != 1 {
				t.Errorf("the last findings were not kept: %v", scan)
			}
			if prev.GetMessage() != "" || !prev.GetScanTime().AsTime().Equal(scanClock.Add(-scanInterval)) {
				t.Error("the previous scan was changed in place")
			}
		})
	}
}

// A reconcile of a RUNNING cluster records the scan with its status, and
// every later view carries it: the pass after does not scan again.
func TestReconcileRecordsTheScanWithTheStatus(t *testing.T) {
	// Arrange
	at(t, scanClock)
	w, configs := scanWorld(t, &clusterAPI{}, func() (deprecation.Result, error) {
		return deprecation.Result{Version: 31, Findings: []deprecation.Finding{pdbFinding("shop")}}, nil
	})
	row := w.cluster(t)
	row.State, row.AppliedGeneration = StateRunning, row.Generation
	if err := w.store.Clusters().Update(t.Context(), row); err != nil {
		t.Fatal(err)
	}

	// Act
	for range 2 {
		if err := w.svc.Reconcile(t.Context(), worldTenant, row.ID); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
	}

	// Assert
	stored := w.cluster(t)
	if stored.State != StateRunning {
		t.Fatalf("the cluster left RUNNING: %s", stored.State)
	}
	scan := storedScan(stored)
	if scan.GetTotal() != 1 || scan.GetKubernetesVersion() != "1.31" {
		t.Errorf("stored scan %v", scan)
	}
	if len(*configs) != 1 {
		t.Errorf("%d scans in two passes, want 1", len(*configs))
	}
	view, err := w.svc.View(t.Context(), stored)
	if err != nil {
		t.Fatalf("View: %v", err)
	}
	if view.GetStatus().GetDeprecatedResources().GetTotal() != 1 {
		t.Error("the live view does not carry the scan")
	}
}
