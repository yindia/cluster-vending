package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// ClusterRecommendations is the summary of the usage the controller
// records to recommend resource requests for the cluster's workloads. It
// reads the workload cluster every five minutes (workloads, the usage
// metrics-server serves, the cluster's own Vertical Pod Autoscalers) and
// installs nothing there. The rolling week of usage itself is the
// ConfigMap <cluster>-usage-history in the cluster's namespace.
type ClusterRecommendations struct {
	// sampledAt is when the cluster was last sampled.
	// +optional
	SampledAt *metav1.Time `json:"sampledAt,omitempty"`
	// attemptedAt is when the cluster was last sampled, or tried to.
	// +optional
	AttemptedAt *metav1.Time `json:"attemptedAt,omitempty"`
	// workloads and containers are how many are recorded.
	// +optional
	Workloads int32 `json:"workloads,omitempty"`
	// +optional
	Containers int32 `json:"containers,omitempty"`
	// windowDays is how many days of usage are kept.
	// +optional
	WindowDays int32 `json:"windowDays,omitempty"`
	// metricsAvailable: the cluster serves the metrics API.
	// +optional
	MetricsAvailable bool `json:"metricsAvailable,omitempty"`
	// vpaAvailable: the cluster has the VerticalPodAutoscaler kind.
	// +optional
	VPAAvailable bool `json:"vpaAvailable,omitempty"`
	// truncated: the cluster has more containers than are recorded.
	// +optional
	Truncated bool `json:"truncated,omitempty"`
	// error is why the last sample failed; empty when it worked.
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	Error string `json:"error,omitempty"`
	// message is what stands in the way of recommendations, if anything.
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	Message string `json:"message,omitempty"`
}
