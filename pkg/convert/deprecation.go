package convert

import (
	"strconv"

	"google.golang.org/protobuf/types/known/timestamppb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/deprecation"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// maxDeprecationError is the longest scan error the kind keeps.
const maxDeprecationError = 1024

var deprecationSources = map[cloudv1.DeprecationSource]cloudv1alpha1.DeprecationSource{
	cloudv1.DeprecationSource_DEPRECATION_SOURCE_MANIFEST:     cloudv1alpha1.DeprecationSourceManifest,
	cloudv1.DeprecationSource_DEPRECATION_SOURCE_HELM_RELEASE: cloudv1alpha1.DeprecationSourceHelmRelease,
}

func deprecationSourceFromAPI(s cloudv1alpha1.DeprecationSource) cloudv1.DeprecationSource {
	for contract, kind := range deprecationSources {
		if kind == s {
			return contract
		}
	}
	return cloudv1.DeprecationSource_DEPRECATION_SOURCE_UNSPECIFIED
}

// DeprecationFinding is a scan's finding as the contract has it.
func DeprecationFinding(f deprecation.Finding) *cloudv1.DeprecationFinding {
	source := cloudv1.DeprecationSource_DEPRECATION_SOURCE_MANIFEST
	if f.Source == deprecation.SourceHelmRelease {
		source = cloudv1.DeprecationSource_DEPRECATION_SOURCE_HELM_RELEASE
	}
	return &cloudv1.DeprecationFinding{Namespace: f.Namespace, Kind: f.Kind, Name: f.Name, ApiVersion: f.APIVersion,
		Component: f.Component, DeprecatedIn: f.DeprecatedIn, RemovedIn: f.RemovedIn, ReplacementApi: f.ReplacementAPI,
		ReplacementAvailableIn: f.ReplacementAvailableIn, Source: source, HelmRelease: f.HelmRelease, Removed: f.Removed}
}

// DeprecationsToAPI is the kind's view of a scan result; nil for none.
// The findings are cut to what the kind holds and the error to its
// longest.
func DeprecationsToAPI(p *cloudv1.ClusterDeprecations) *cloudv1alpha1.ClusterDeprecations {
	if p == nil {
		return nil
	}
	out := &cloudv1alpha1.ClusterDeprecations{TargetVersion: p.GetTargetVersion(), Total: p.GetTotal(), Removed: p.GetRemoved(),
		Truncated: p.GetTruncated(), Error: p.GetError(), ScannedAt: timeOf(p.GetScannedAt()), AttemptedAt: timeOf(p.GetAttemptedAt())}
	if len(out.Error) > maxDeprecationError {
		out.Error = out.Error[:maxDeprecationError]
	}
	findings := p.GetFindings()
	if len(findings) > cloudv1alpha1.MaxDeprecationFindings {
		findings, out.Truncated = findings[:cloudv1alpha1.MaxDeprecationFindings], true
	}
	for _, f := range findings {
		out.Findings = append(out.Findings, cloudv1alpha1.DeprecationFinding{Namespace: f.GetNamespace(), Kind: f.GetKind(),
			Name: f.GetName(), APIVersion: f.GetApiVersion(), Component: f.GetComponent(), DeprecatedIn: f.GetDeprecatedIn(),
			RemovedIn: f.GetRemovedIn(), ReplacementAPI: f.GetReplacementApi(), ReplacementAvailableIn: f.GetReplacementAvailableIn(),
			Source: deprecationSources[f.GetSource()], HelmRelease: f.GetHelmRelease(), Removed: f.GetRemoved()})
	}
	return out
}

// DeprecationsFromAPI is the contract's view of what the kind holds; nil
// for none.
func DeprecationsFromAPI(a *cloudv1alpha1.ClusterDeprecations) *cloudv1.ClusterDeprecations {
	if a == nil {
		return nil
	}
	out := &cloudv1.ClusterDeprecations{TargetVersion: a.TargetVersion, Total: a.Total, Removed: a.Removed,
		Truncated: a.Truncated, Error: a.Error, ScannedAt: timestampOf(a.ScannedAt), AttemptedAt: timestampOf(a.AttemptedAt)}
	for _, f := range a.Findings {
		out.Findings = append(out.Findings, &cloudv1.DeprecationFinding{Namespace: f.Namespace, Kind: f.Kind, Name: f.Name,
			ApiVersion: f.APIVersion, Component: f.Component, DeprecatedIn: f.DeprecatedIn, RemovedIn: f.RemovedIn,
			ReplacementApi: f.ReplacementAPI, ReplacementAvailableIn: f.ReplacementAvailableIn,
			Source: deprecationSourceFromAPI(f.Source), HelmRelease: f.HelmRelease, Removed: f.Removed})
	}
	return out
}

// DeprecationCondition is the DeprecationScan condition of a scan result:
// True when the last scan worked, False with the error when it did not.
func DeprecationCondition(d *cloudv1alpha1.ClusterDeprecations, generation int64) metav1.Condition {
	cond := metav1.Condition{Type: cloudv1alpha1.ConditionDeprecationScan, Status: metav1.ConditionTrue, Reason: "Scanned",
		ObservedGeneration: generation}
	switch {
	case d.Error != "":
		cond.Status, cond.Reason, cond.Message = metav1.ConditionFalse, "ScanFailed", d.Error
	case d.Removed > 0:
		cond.Reason = "RemovedAPIsInUse"
		cond.Message = plural(d.Removed) + " an API version removed in Kubernetes " + d.TargetVersion
	case d.Total > 0:
		cond.Reason = "DeprecatedAPIsInUse"
		cond.Message = plural(d.Total) + " a deprecated API version"
	default:
		cond.Message = "no deprecated API versions in use for Kubernetes " + d.TargetVersion
	}
	return cond
}

func plural(n int32) string {
	if n == 1 {
		return "1 object uses"
	}
	return strconv.Itoa(int(n)) + " objects use"
}

func timeOf(t *timestamppb.Timestamp) *metav1.Time {
	if t == nil {
		return nil
	}
	return &metav1.Time{Time: t.AsTime()}
}

func timestampOf(t *metav1.Time) *timestamppb.Timestamp {
	if t == nil {
		return nil
	}
	return timestamppb.New(t.Time)
}
