package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// Result kinds of ClusterResultRef.Kind: the objects of a cluster's results
// bucket (docs/architecture.md#results-in-s3).
const (
	ResultKindDeprecations    = "deprecations"
	ResultKindRecommendations = "recommendations"
	ResultKindViolations      = "violations"
	ResultKindViolationEvents = "violation-events"
	ResultKindNodes           = "nodes"
)

// ClusterResultRef is the summary of one kind of scan result whose full data
// is in the cluster's results bucket: the object it names, when it was
// collected and how much it holds. The Cluster keeps this and not the lists,
// so a large cluster is not limited by the size of a Kubernetes object.
// Gates that read the cluster (an upgrade blocked by removed APIs in use)
// read the counts here.
type ClusterResultRef struct {
	// kind is what the result is.
	// +kubebuilder:validation:Enum=deprecations;recommendations;violations;violation-events;nodes
	Kind string `json:"kind"`
	// bucket is the S3 bucket of the cluster's results, in the cluster's
	// account.
	// +kubebuilder:validation:MaxLength=63
	Bucket string `json:"bucket"`
	// key is the "latest" object of the kind, for example
	// latest/deprecations.json; a sharded kind (nodes) names its index,
	// latest/nodes/index.json.
	// +kubebuilder:validation:MaxLength=256
	Key string `json:"key"`
	// etag is the ETag of that object when it was written: a reader that
	// finds another one knows the object is newer than this summary.
	// +kubebuilder:validation:MaxLength=128
	// +optional
	ETag string `json:"etag,omitempty"`
	// digest is a hash of the collected content (not of its times): a scan
	// whose content has the same digest does not rewrite the object.
	// +kubebuilder:validation:MaxLength=128
	// +optional
	Digest string `json:"digest,omitempty"`
	// collectedAt is when the result was collected (the last scan that
	// worked); with attemptedAt it is the input of staleness.
	// +optional
	CollectedAt *metav1.Time `json:"collectedAt,omitempty"`
	// attemptedAt is when the cluster was last scanned, or tried to.
	// +optional
	AttemptedAt *metav1.Time `json:"attemptedAt,omitempty"`
	// total is how many items the result holds (findings, recommendations,
	// violations, events).
	// +optional
	Total int32 `json:"total,omitempty"`
	// removed, for deprecations: how many items use an API version removed
	// at the target version.
	// +optional
	Removed int32 `json:"removed,omitempty"`
	// incomplete: the sources were not fully read.
	// +optional
	Incomplete bool `json:"incomplete,omitempty"`
	// truncated: more was found than the object holds.
	// +optional
	Truncated bool `json:"truncated,omitempty"`
}
