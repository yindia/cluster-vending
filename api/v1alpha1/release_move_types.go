package v1alpha1

// ChannelReleaseMoves is a channel's rollout of its release to the
// components of its automatic clusters, tier 2 first, planned like its
// Kubernetes rollout (policy, holds, maintenance windows).
type ChannelReleaseMoves struct {
	// release is the release the clusters move to.
	// +optional
	Release string `json:"release,omitempty"`
	// admitted are the clusters (namespace/name) that may start moving now.
	// +optional
	// +kubebuilder:validation:MaxItems=20
	Admitted []string `json:"admitted,omitempty"`
	// waiting says why no (more) cluster starts.
	// +optional
	Waiting string `json:"waiting,omitempty"`
	// halted: more clusters of a tier failed than the policy's maxFailed;
	// none starts until one is retried.
	// +optional
	Halted bool `json:"halted,omitempty"`
	// done: every member runs the release.
	// +optional
	Done bool `json:"done,omitempty"`
	// members count the clusters of each tier by state.
	// +optional
	Members []RolloutTierCount `json:"members,omitempty"`
	// blocked are clusters kept back by their own checks, with why.
	// +optional
	// +kubebuilder:validation:MaxItems=20
	Blocked []BlockedCluster `json:"blocked,omitempty"`
}
