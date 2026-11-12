package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ConditionDeprecationScan is the Cluster condition of the scan for
// deprecated API versions: True when the last scan worked, False with the
// reason when it could not read the cluster.
const ConditionDeprecationScan = "DeprecationScan"

// MaxDeprecationFindings is how many findings a Cluster's status keeps;
// status.deprecations.total counts all of them.
const MaxDeprecationFindings = 200

// DeprecationSource is where the manifest of a finding was read.
// +kubebuilder:validation:Enum=Manifest;HelmRelease
type DeprecationSource string

const (
	// DeprecationSourceManifest is the kubectl last-applied-configuration
	// annotation of a live object.
	DeprecationSourceManifest DeprecationSource = "Manifest"
	// DeprecationSourceHelmRelease is the manifest of a deployed Helm
	// release.
	DeprecationSourceHelmRelease DeprecationSource = "HelmRelease"
)

// DeprecationFinding is one object of the cluster that was written with an
// API version that is deprecated or removed at the target version.
type DeprecationFinding struct {
	// namespace of the object; empty for a cluster-scoped one.
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// kind of the object.
	// +required
	Kind string `json:"kind"`
	// name of the object.
	// +optional
	Name string `json:"name,omitempty"`
	// apiVersion the object was written with, e.g. policy/v1beta1.
	// +required
	APIVersion string `json:"apiVersion"`
	// component the API version belongs to: k8s, or a project such as
	// cert-manager or istio. deprecatedIn and removedIn are its releases.
	// +optional
	Component string `json:"component,omitempty"`
	// deprecatedIn is the release that deprecates the API version.
	// +optional
	DeprecatedIn string `json:"deprecatedIn,omitempty"`
	// removedIn is the release that removes it; empty when none does yet.
	// +optional
	RemovedIn string `json:"removedIn,omitempty"`
	// replacementAPI is the API version to write the object with instead.
	// +optional
	ReplacementAPI string `json:"replacementAPI,omitempty"`
	// replacementAvailableIn is the release the replacement exists from.
	// +optional
	ReplacementAvailableIn string `json:"replacementAvailableIn,omitempty"`
	// source is where the object's manifest was read.
	// +optional
	Source DeprecationSource `json:"source,omitempty"`
	// helmRelease is <namespace>/<name> of the Helm release, for source
	// HelmRelease.
	// +optional
	HelmRelease string `json:"helmRelease,omitempty"`
	// removed: the API version is gone at the target version. False:
	// deprecated only.
	// +optional
	Removed bool `json:"removed,omitempty"`
}

// ClusterDeprecations is what the last scan of the cluster found: the
// objects written with API versions that are deprecated or removed at the
// Kubernetes version the cluster moves to next. Only objects whose
// original manifest is kept are seen (kubectl apply, Helm releases).
type ClusterDeprecations struct {
	// targetVersion is the Kubernetes minor the cluster was judged
	// against, e.g. 1.37.
	// +optional
	TargetVersion string `json:"targetVersion,omitempty"`
	// scannedAt is when the last scan that worked ran.
	// +optional
	ScannedAt *metav1.Time `json:"scannedAt,omitempty"`
	// attemptedAt is when the cluster was last scanned, or tried to.
	// +optional
	AttemptedAt *metav1.Time `json:"attemptedAt,omitempty"`
	// total is how many objects were found in all.
	// +optional
	Total int32 `json:"total,omitempty"`
	// removed is how many of them use an API version removed at
	// targetVersion.
	// +optional
	Removed int32 `json:"removed,omitempty"`
	// truncated: more were found than findings holds.
	// +optional
	Truncated bool `json:"truncated,omitempty"`
	// error is why the last scan failed; empty when it worked. The counts
	// and findings are those of the last scan that worked.
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	Error string `json:"error,omitempty"`
	// findings, removed first; at most 200.
	// +listType=atomic
	// +optional
	// +kubebuilder:validation:MaxItems=200
	Findings []DeprecationFinding `json:"findings,omitempty"`
}
