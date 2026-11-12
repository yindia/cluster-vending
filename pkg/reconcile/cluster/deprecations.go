package cluster

import (
	"context"
	"errors"
	"math"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/client-go/rest"

	awscloud "github.com/yindia/cluster-vending/pkg/cloud/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/deprecation"
	"github.com/yindia/cluster-vending/pkg/fault"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// How a cluster is scanned for objects written with deprecated API
// versions (status.deprecated_resources).
const (
	// scanInterval is how long a scan stands before the cluster is scanned
	// again, whether it succeeded or failed. What a cluster holds of
	// deprecated versions changes slowly, and a cluster the platform cannot
	// reach (a private endpoint) stays out of reach: a failed scan is not
	// worth trying sooner, each try holds the reconciler for its timeout.
	scanInterval = 6 * time.Hour
	// scanTimeout bounds a whole scan; scanRequestTimeout one request of it
	// (a cluster that cannot be reached fails at the first).
	scanTimeout        = 2 * time.Minute
	scanRequestTimeout = 10 * time.Second
	// maxDeprecatedResources is how many findings a status keeps: the
	// status is a part of the Cluster object, read with every GetCluster.
	// The findings are ordered, so what breaks first is kept.
	maxDeprecatedResources = 100
)

// storedScan is the scan of the status recorded with the row, or nil.
func storedScan(row *model.Cluster) *cloudv1.DeprecatedResourceScan {
	if len(row.Status) == 0 {
		return nil
	}
	st := &cloudv1.ClusterStatus{}
	if err := Lenient.Unmarshal(row.Status, st); err != nil {
		return nil
	}
	return st.GetDeprecatedResources()
}

// scanDue reports whether a cluster whose last scan is prev (nil: never
// scanned) is to be scanned at.
func scanDue(prev *cloudv1.DeprecatedResourceScan, at time.Time) bool {
	return prev.GetScanTime() == nil || at.Sub(prev.GetScanTime().AsTime()) >= scanInterval
}

// scanDeprecations scans the cluster for objects written with deprecated
// API versions when a scan is due, and sets what it found on st, the
// status about to be recorded (which carries the last scan). Only a
// RUNNING cluster is scanned: its Kubernetes API is there and the tenant
// role is known to it. Only what reconciles scans (Deps.AWSClusterAPI);
// the API handlers never do.
//
// A scan that fails is not a failure of the cluster: it is recorded with
// the scan (message), next to what the last successful scan found.
func (s *Reconciler) scanDeprecations(ctx context.Context, row *model.Cluster, st *cloudv1.ClusterStatus) {
	if s.deps.AWSClusterAPI == nil || row.State != StateRunning || !scanDue(st.GetDeprecatedResources(), now()) {
		return
	}
	result, err := s.scan(ctx, row)
	if err != nil {
		s.logger().WarnContext(ctx, "scan for deprecated resources", "cluster_id", row.ID, "error", err)
		st.DeprecatedResources = failedScan(st.GetDeprecatedResources(), err, now())
		return
	}
	st.DeprecatedResources = scanProto(result, now())
	s.logger().InfoContext(ctx, "scanned for deprecated resources", "cluster_id", row.ID,
		"kubernetes_version", result.Version.String(), "found", len(result.Findings))
}

// scan reads the cluster's own Kubernetes API as the tenant role of its
// account: EKS says where the API is and the role's credentials sign a
// short-lived token for it. No Secret is read, and nothing is written.
func (s *Reconciler) scan(ctx context.Context, row *model.Cluster) (deprecation.Result, error) {
	ctx, cancel := context.WithTimeout(ctx, scanTimeout)
	defer cancel()
	account, err := s.deps.Store.Tenants().GetCloudAccountByID(ctx, row.TenantID, row.CloudAccountID)
	if err != nil {
		return deprecation.Result{}, storeError(err)
	}
	access, err := s.deps.AWSAccess(ctx, account)
	if err != nil {
		return deprecation.Result{}, err
	}
	api, err := s.deps.AWSClusterAPI.EKSKubernetesAccess(ctx, access, row.Region, row.Name)
	if err != nil {
		return deprecation.Result{}, err
	}
	scanCluster := s.deps.ScanCluster
	if scanCluster == nil {
		scanCluster = deprecation.ScanCluster
	}
	return scanCluster(ctx, &rest.Config{
		Host:            api.Endpoint,
		BearerToken:     api.Token,
		TLSClientConfig: rest.TLSClientConfig{CAData: api.CA},
		Timeout:         scanRequestTimeout,
	})
}

// scanProto is a scan's result as the status holds it: the first
// maxDeprecatedResources findings, and how many there were.
func scanProto(result deprecation.Result, at time.Time) *cloudv1.DeprecatedResourceScan {
	out := &cloudv1.DeprecatedResourceScan{
		ScanTime:          timestamppb.New(at),
		KubernetesVersion: result.Version.String(),
		Total:             int32(min(len(result.Findings), math.MaxInt32)), //nolint:gosec // bounded by min
	}
	for _, f := range result.Findings[:min(len(result.Findings), maxDeprecatedResources)] {
		out.Resources = append(out.Resources, &cloudv1.DeprecatedResource{
			ApiVersion: f.Object.APIVersion, Kind: f.Object.Kind, Namespace: f.Object.Namespace, Name: f.Object.Name,
			ReplacementApiVersion: f.Rule.Replacement,
			DeprecatedIn:          f.Rule.DeprecatedIn.String(), RemovedIn: f.Rule.RemovedIn.String(),
			Source: sourceProto(f.Object.Source), HelmRelease: f.Object.HelmRelease,
		})
	}
	return out
}

func sourceProto(s deprecation.Source) cloudv1.DeprecatedResourceSource {
	switch s {
	case deprecation.SourceLastApplied:
		return cloudv1.DeprecatedResourceSource_DEPRECATED_RESOURCE_SOURCE_LAST_APPLIED_CONFIGURATION
	case deprecation.SourceHelmRelease:
		return cloudv1.DeprecatedResourceSource_DEPRECATED_RESOURCE_SOURCE_HELM_RELEASE
	default:
		return cloudv1.DeprecatedResourceSource_DEPRECATED_RESOURCE_SOURCE_UNSPECIFIED
	}
}

// failedScan is prev (what the last successful scan found; nil: none)
// with a scan that failed at with err.
func failedScan(prev *cloudv1.DeprecatedResourceScan, err error, at time.Time) *cloudv1.DeprecatedResourceScan {
	out := &cloudv1.DeprecatedResourceScan{}
	if prev != nil {
		out = proto.CloneOf(prev)
	}
	out.ScanTime, out.Message = timestamppb.New(at), scanFailure(err)
	return out
}

// scanFailure is what a failed scan says to who reads the cluster. The
// platform's own failures are faults: their message is for the caller.
func scanFailure(err error) string {
	if f, ok := fault.As(err); ok {
		return f.Message
	}
	switch {
	case errors.Is(err, awscloud.ErrAccessDenied):
		return "the tenant role was refused: " + err.Error()
	case errors.Is(err, awscloud.ErrNotFound):
		return "EKS does not have the cluster: " + err.Error()
	case errors.Is(err, context.DeadlineExceeded):
		return "the cluster's Kubernetes API did not answer in time; is its endpoint reachable from the platform?"
	default:
		return "cannot read the cluster's Kubernetes API: " + err.Error()
	}
}
