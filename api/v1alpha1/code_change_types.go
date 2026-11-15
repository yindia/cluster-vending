package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// CodeChange is a change of what the platform renders for a cluster or a
// network that nobody asked for (a new controller build), applied in turn
// by its channel's rollout.
type CodeChange struct {
	// phase: Pending, Applying, Done or Halted.
	// +optional
	Phase string `json:"phase,omitempty"`
	// summary says what changes: "3 objects change (1 added)".
	// +optional
	Summary string `json:"summary,omitempty"`
	// objects is how many objects change.
	// +optional
	Objects int32 `json:"objects,omitempty"`
	// message says what it waits for, or what failed.
	// +optional
	Message string `json:"message,omitempty"`
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`
	// finishTime is when it was applied in full: the soak of its tier
	// counts from there.
	// +optional
	FinishTime *metav1.Time `json:"finishTime,omitempty"`
}
