package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// The operator's release objects (docs/design/release-admin.md): a
// Release locks what clusters run, a ReleaseChannel says which release a
// channel offers and how it rolls out, a ReleaseHold keeps one cluster out
// of rollouts. All cluster-scoped, written by the operator (release-admin
// role) with kubectl or from Git; the controller writes their status.
//
// EKS add-ons are not part of a release: which add-ons exist is the
// contract's (AwsAddon.name) and the Cluster kind's list, and their
// versions come from AWS. A release pins the components the platform
// installs itself.

// Condition types of the release objects.
const (
	// ReleaseConditionValid: the checks CEL cannot make passed (the minors
	// are consecutive, upgradeTo is the next minor and present, the
	// renderer version is not newer than the controller's).
	ReleaseConditionValid = "Valid"
	// ReleaseConditionInUse: a channel or a hold names the release, so it
	// cannot be deleted.
	ReleaseConditionInUse = "InUse"
	// ChannelConditionOffering: the channel offers a valid release.
	ChannelConditionOffering = "Offering"
	// HoldConditionResolved: the held cluster exists (and the release the
	// hold names, if any).
	HoldConditionResolved = "Resolved"
)

// ReleaseFinalizer keeps a Release while a channel or a hold names it.
const ReleaseFinalizer = "cloud.cluster-vending.io/release-in-use"

// LabelShipped ("true") marks a Release the controller created itself: the
// release a build ships, named shipped-<hash of its content>. A new build
// moves the alpha channel off such a release to its own; a release the
// operator published never carries the label.
const LabelShipped = "cloud.cluster-vending.io/shipped"

// ComponentPin is one component the platform installs itself (Helm or
// Argo CD) at a fixed version, on one Kubernetes minor.
type ComponentPin struct {
	// Name of the component.
	// +required
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`
	Name string `json:"name"`
	// Version installed, e.g. the chart version. A component's CRD chart
	// (ComponentDefinition install.helm.crds) is installed at this version
	// too: there is no separate pin.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	Version string `json:"version"`
	// values are Helm values the release sets for the component on this
	// Kubernetes minor (cluster-autoscaler's image tag follows the
	// minor). They are merged over the definition's platformValues and
	// locked the same way: a tenant who touches one of their paths is
	// refused. Placeholders are allowed. Unset: none.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:validation:Type=object
	Values *runtime.RawExtension `json:"values,omitempty"`
}

// KubernetesRelease is what clusters on one supported Kubernetes minor run.
type KubernetesRelease struct {
	// Minor, e.g. "1.37".
	// +required
	// +kubebuilder:validation:Pattern=`^1\.[0-9]{2}$`
	Minor string `json:"minor"`
	// Default marks the minor a new cluster on no channel gets when it
	// names none. A cluster on a channel gets the channel's minor.
	// +optional
	Default bool `json:"default,omitempty"`
	// Channel is the channel whose clusters run this minor. A release puts
	// either no minor on a channel, or exactly one on each of alpha, beta
	// and stable, alpha's the newest. A minor on no channel is still
	// supported; its users are warned to move off it. Unset: none.
	// +optional
	// +kubebuilder:validation:Enum=alpha;beta;stable
	Channel string `json:"channel,omitempty"`
	// UpgradeTo moves automatic clusters on this minor to the next one
	// (one minor at a time). Unset: they stay.
	// +optional
	// +kubebuilder:validation:Pattern=`^1\.[0-9]{2}$`
	UpgradeTo string `json:"upgradeTo,omitempty"`
	// EndOfSupport is when this minor stops being supported.
	// +required
	EndOfSupport metav1.Time `json:"endOfSupport"`
	// Components the platform installs on this minor, at these versions.
	// +optional
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=64
	Components []ComponentPin `json:"components,omitempty"`
}

// ReleaseContent is what a release locks. It never changes once published.
// +kubebuilder:validation:XValidation:rule="self.kubernetes.exists_one(k, has(k.default) && k.default)",message="exactly one minor is the default"
type ReleaseContent struct {
	// RenderVersion is the renderer version clusters are brought to
	// (crossplaneaws.RenderVersion); not newer than the controller's.
	// +required
	// +kubebuilder:validation:Minimum=1
	RenderVersion int64 `json:"renderVersion"`
	// Kubernetes: the supported minors, consecutive: one per channel, and
	// those still supported on no channel.
	// +required
	// +listType=map
	// +listMapKey=minor
	// +kubebuilder:validation:MinItems=3
	// +kubebuilder:validation:MaxItems=6
	Kubernetes []KubernetesRelease `json:"kubernetes"`
}

// ReleaseSpec is a published release.
// +kubebuilder:validation:XValidation:rule="!(has(oldSelf.withdrawn) && oldSelf.withdrawn) || (has(self.withdrawn) && self.withdrawn)",message="a withdrawn release stays withdrawn"
type ReleaseSpec struct {
	// Content is what the release locks: immutable, publish a new release
	// to change a version.
	// +required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="release content is immutable; publish a new release"
	Content ReleaseContent `json:"content"`
	// Notes for operators and tenants.
	// +optional
	// +kubebuilder:validation:MaxLength=4096
	Notes string `json:"notes,omitempty"`
	// Withdrawn: no channel may offer it any more. Cannot be undone.
	// +optional
	Withdrawn bool `json:"withdrawn,omitempty"`
}

// ReleaseStatus is written by the controller.
type ReleaseStatus struct {
	// Conditions: Valid, InUse.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// Channels offering the release now.
	// +optional
	Channels []string `json:"channels,omitempty"`
	// Holds naming the release (their names).
	// +optional
	Holds []string `json:"holds,omitempty"`
}

// Release locks what clusters run: the renderer version and, per supported
// Kubernetes minor, the components the platform installs.
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=cvrel
// +kubebuilder:subresource:status
// +kubebuilder:validation:XValidation:rule="self.metadata.name.matches('^([0-9]{4}-[0-9]{2}-[a-z]|shipped-[0-9a-f]{8})$')",message="a release is named YYYY-MM-<letter>, e.g. 2026-11-a (shipped-<8 hex digits> is the controller's own)"
// +kubebuilder:printcolumn:name="Render",type=integer,JSONPath=`.spec.content.renderVersion`
// +kubebuilder:printcolumn:name="Withdrawn",type=boolean,JSONPath=`.spec.withdrawn`
// +kubebuilder:printcolumn:name="Valid",type=string,JSONPath=`.status.conditions[?(@.type=="Valid")].status`
// +kubebuilder:printcolumn:name="Channels",type=string,JSONPath=`.status.channels`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Release struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	// +required
	Spec ReleaseSpec `json:"spec"`
	// +optional
	Status ReleaseStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ReleaseList contains a list of Release.
type ReleaseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Release `json:"items"`
}

// Promotion is how a channel moves to a new release.
// +kubebuilder:validation:Enum=Automatic;Manual;Pinned
type Promotion string

// Promotions.
const (
	// PromotionAutomatic: the channel takes each release its upstream
	// channel completed (beta follows alpha, stable follows beta).
	PromotionAutomatic Promotion = "Automatic"
	// PromotionManual: the channel offers spec.release, moved by hand.
	PromotionManual Promotion = "Manual"
	// PromotionPinned: the channel offers spec.release and never moves.
	PromotionPinned Promotion = "Pinned"
)

// Channel names: the ReleaseChannel objects.
const (
	ChannelAlpha  = "alpha"
	ChannelBeta   = "beta"
	ChannelStable = "stable"
)

// RolloutPolicy is how a release moves through a channel's tiers.
type RolloutPolicy struct {
	// MaxInProgress clusters applying at once per tier (Tier0: always 1).
	// +optional
	// +kubebuilder:default=5
	// +kubebuilder:validation:Minimum=1
	MaxInProgress int32 `json:"maxInProgress,omitempty"`
	// MaxFailed failures in a tier before the channel halts.
	// +optional
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	MaxFailed int32 `json:"maxFailed,omitempty"`
	// Soak is the healthy time after a tier before the next one starts.
	// +optional
	// +kubebuilder:default="24h"
	Soak metav1.Duration `json:"soak,omitempty"`
	// SoakPerTier overrides Soak for a tier (Tier0, Tier1, Tier2).
	// +optional
	// +kubebuilder:validation:MaxProperties=3
	SoakPerTier map[ClusterTier]metav1.Duration `json:"soakPerTier,omitempty"`
}

// ReleaseChannelSpec is the operator's settings for one channel.
// +kubebuilder:validation:XValidation:rule="!has(self.release) || self.promotion != 'Automatic'",message="spec.release is the release of a Pinned or Manual channel; an Automatic channel takes its upstream's"
// +kubebuilder:validation:XValidation:rule="self.promotion == 'Automatic' || has(self.release)",message="a Pinned or Manual channel names its spec.release"
type ReleaseChannelSpec struct {
	// Promotion: Automatic takes each release the upstream channel
	// completed; Manual and Pinned offer spec.release.
	// +optional
	// +kubebuilder:default=Automatic
	Promotion Promotion `json:"promotion,omitempty"`
	// Release offered by a Manual or Pinned channel.
	// +optional
	// +kubebuilder:validation:Pattern=`^([0-9]{4}-[0-9]{2}-[a-z]|shipped-[0-9a-f]{8})$`
	Release string `json:"release,omitempty"`
	// Policy of the rollout through the channel's tiers.
	// +optional
	Policy RolloutPolicy `json:"policy,omitempty"`
	// Paused: no cluster starts a new release; clusters in progress finish.
	// +optional
	Paused bool `json:"paused,omitempty"`
}

// TierStatus is the progress of the offered release in one tier.
type TierStatus struct {
	Tier ClusterTier `json:"tier"`
	// +optional
	Total int32 `json:"total,omitempty"`
	// +optional
	Done int32 `json:"done,omitempty"`
	// +optional
	InProgress int32 `json:"inProgress,omitempty"`
	// +optional
	Failed int32 `json:"failed,omitempty"`
	// +optional
	Held int32 `json:"held,omitempty"`
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
}

// MaxRolloutClusters is how many clusters ChannelRollout lists in
// wouldStart and in blocked.
const MaxRolloutClusters = 20

// ChannelRollout is the rollout of a channel's Kubernetes minor to its
// automatic clusters, one minor at a time.
type ChannelRollout struct {
	// TargetMinor is the minor the channel's clusters move to: the one
	// its release puts on the channel.
	// +optional
	TargetMinor string `json:"targetMinor,omitempty"`
	// ReportOnly: automatic upgrades are off (the controller's
	// automatic-upgrades setting); the rollout is planned and reported,
	// no cluster is upgraded.
	// +optional
	ReportOnly bool `json:"reportOnly,omitempty"`
	// Waiting says why no (more) cluster starts now; when Halted, why the
	// rollout halted.
	// +optional
	Waiting string `json:"waiting,omitempty"`
	// Halted: more clusters of a tier failed than spec.policy.maxFailed
	// allows; no cluster starts until they are fixed.
	// +optional
	Halted bool `json:"halted,omitempty"`
	// Done: every cluster of the channel runs its minor, or is kept back
	// by its own gates (blocked).
	// +optional
	Done bool `json:"done,omitempty"`
	// WouldStart are the clusters (namespace/name) whose upgrade starts
	// now; with ReportOnly, those it would start. At most 20.
	// +optional
	// +kubebuilder:validation:MaxItems=20
	WouldStart []string `json:"wouldStart,omitempty"`
	// Members counts the channel's clusters per tier and state.
	// +optional
	Members []RolloutTierCount `json:"members,omitempty"`
	// Blocked are clusters kept back by their own gates, with the first
	// reason; they do not hold the rollout back. At most 20.
	// +optional
	// +kubebuilder:validation:MaxItems=20
	Blocked []BlockedCluster `json:"blocked,omitempty"`
}

// RolloutTierCount counts one tier's clusters by rollout state.
type RolloutTierCount struct {
	Tier ClusterTier `json:"tier"`
	// +optional
	Pending int32 `json:"pending,omitempty"`
	// +optional
	InProgress int32 `json:"inProgress,omitempty"`
	// +optional
	Succeeded int32 `json:"succeeded,omitempty"`
	// +optional
	Failed int32 `json:"failed,omitempty"`
	// +optional
	Blocked int32 `json:"blocked,omitempty"`
}

// BlockedCluster is a cluster the rollout passes over, and why.
type BlockedCluster struct {
	// Cluster is namespace/name.
	Cluster string `json:"cluster"`
	// Gate is the code of the first reason (MANUAL, MAINTENANCE_WINDOW,
	// DEPRECATIONS, ...).
	Gate string `json:"gate"`
	// Message is that reason, for the cluster's users.
	// +optional
	Message string `json:"message,omitempty"`
}

// ChannelHistoryEntry is one release a channel offered.
type ChannelHistoryEntry struct {
	Release string `json:"release"`
	// From is when the channel started offering it.
	From metav1.Time `json:"from"`
	// To is when it stopped; unset for the release offered now.
	// +optional
	To *metav1.Time `json:"to,omitempty"`
	// Reason the channel offered it (promotion, pinned).
	// +optional
	Reason string `json:"reason,omitempty"`
}

// ReleaseChannelStatus is written by the controller.
type ReleaseChannelStatus struct {
	// Release the channel offers now: spec.release, or for an Automatic
	// channel the release its upstream offers (until rollouts exist, a
	// release counts as completed upstream as soon as the upstream offers
	// it).
	// +optional
	Release string `json:"release,omitempty"`
	// KubernetesMinor is the minor that release puts on this channel: the
	// minor its automatic clusters run. Empty when it puts none.
	// +optional
	KubernetesMinor string `json:"kubernetesMinor,omitempty"`
	// Stage is the tier the Kubernetes rollout is at (or waits for):
	// Tier2 first, then Tier1, then Tier0.
	// +optional
	Stage ClusterTier `json:"stage,omitempty"`
	// Tiers is the rollout's progress per tier: Done clusters run the
	// channel's minor, Held ones are kept back by a ReleaseHold.
	// +optional
	Tiers []TierStatus `json:"tiers,omitempty"`
	// Rollout is what the rollout of the channel's Kubernetes minor does
	// now: which clusters start, or why none does.
	// +optional
	Rollout *ChannelRollout `json:"rollout,omitempty"`
	// History: the releases the channel offered, newest first, at most 20.
	// +optional
	// +kubebuilder:validation:MaxItems=20
	History []ChannelHistoryEntry `json:"history,omitempty"`
	// Conditions: Offering.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ReleaseChannel says which release a channel (alpha, beta, stable)
// offers and how it rolls out. Beta follows alpha and stable follows beta.
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=cvch
// +kubebuilder:subresource:status
// +kubebuilder:validation:XValidation:rule="self.metadata.name in ['alpha', 'beta', 'stable']",message="channels are alpha, beta and stable"
// +kubebuilder:validation:XValidation:rule="self.metadata.name != 'alpha' || self.spec.promotion != 'Automatic'",message="alpha has no upstream channel: it is Pinned or Manual to a release"
// +kubebuilder:printcolumn:name="Release",type=string,JSONPath=`.status.release`
// +kubebuilder:printcolumn:name="Kubernetes",type=string,JSONPath=`.status.kubernetesMinor`
// +kubebuilder:printcolumn:name="Promotion",type=string,JSONPath=`.spec.promotion`
// +kubebuilder:printcolumn:name="Paused",type=boolean,JSONPath=`.spec.paused`
// +kubebuilder:printcolumn:name="Offering",type=string,JSONPath=`.status.conditions[?(@.type=="Offering")].status`
// +kubebuilder:printcolumn:name="Stage",type=string,JSONPath=`.status.stage`
// +kubebuilder:printcolumn:name="Waiting",type=string,JSONPath=`.status.rollout.waiting`
type ReleaseChannel struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	// +required
	Spec ReleaseChannelSpec `json:"spec"`
	// +optional
	Status ReleaseChannelStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ReleaseChannelList contains a list of ReleaseChannel.
type ReleaseChannelList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ReleaseChannel `json:"items"`
}

// ReleaseHoldSpec keeps clusters out of rollouts (AdminService.PauseReconcile
// with only_upgrades): one cluster, a tenant's clusters, or a release
// channel's (every tier, or one). Exactly one of clusterID, tenantID and
// channel is set; tier only with channel.
// +kubebuilder:validation:XValidation:rule="[has(self.clusterID), has(self.tenantID), has(self.channel)].filter(x, x).size() == 1",message="set exactly one of clusterID, tenantID and channel"
// +kubebuilder:validation:XValidation:rule="!has(self.tier) || has(self.channel)",message="tier needs channel"
type ReleaseHoldSpec struct {
	// ClusterID is the held cluster's ID (the API's Cluster.id).
	// +optional
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="clusterID is immutable; create another hold"
	ClusterID string `json:"clusterID,omitempty"`
	// TenantID holds every cluster of the tenant, those created later too.
	// +optional
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([a-z0-9-]{0,54}[a-z0-9])?$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="tenantID is immutable; create another hold"
	TenantID string `json:"tenantID,omitempty"`
	// Channel holds every cluster on the release channel (alpha, beta,
	// stable), those that join it later too; with Tier, only that tier's.
	// +optional
	// +kubebuilder:validation:Enum=alpha;beta;stable
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="channel is immutable; create another hold"
	Channel string `json:"channel,omitempty"`
	// Tier narrows Channel to one tier.
	// +optional
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="tier is immutable; create another hold"
	Tier ClusterTier `json:"tier,omitempty"`
	// Release, when set, keeps the cluster on exactly this release (a
	// lock); unset, the cluster stays on what it has now (a freeze).
	// +optional
	// +kubebuilder:validation:Pattern=`^([0-9]{4}-[0-9]{2}-[a-z]|shipped-[0-9a-f]{8})$`
	Release string `json:"release,omitempty"`
	// Reason, shown on the cluster's status.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	Reason string `json:"reason"`
	// Until, when set, ends the hold by itself.
	// +optional
	Until *metav1.Time `json:"until,omitempty"`
	// By is who asked for it (AdminService.PauseReconcile sets it).
	// +optional
	// +kubebuilder:validation:MaxLength=256
	By string `json:"by,omitempty"`
}

// ReleaseHoldStatus is written by the controller.
type ReleaseHoldStatus struct {
	// Namespace and Name of the Cluster object the ID resolved to (for
	// people; the hold follows the ID). Empty for a hold of a tenant or a
	// channel.
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// +optional
	Name string `json:"name,omitempty"`
	// Clusters is how many clusters the hold matches now.
	// +optional
	Clusters int32 `json:"clusters,omitempty"`
	// Conditions: Resolved.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ReleaseHold keeps clusters out of rollouts: one cluster on a named
// release (a lock) or where it is (a freeze), or a tenant's or a channel's
// clusters where they are. Only the operator holds a cluster; users see it
// as a pause of upgrades (admin pause --only upgrades).
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=cvhold
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Cluster-ID",type=string,JSONPath=`.spec.clusterID`
// +kubebuilder:printcolumn:name="Tenant",type=string,JSONPath=`.spec.tenantID`
// +kubebuilder:printcolumn:name="Channel",type=string,JSONPath=`.spec.channel`
// +kubebuilder:printcolumn:name="Tier",type=string,JSONPath=`.spec.tier`
// +kubebuilder:printcolumn:name="Clusters",type=integer,JSONPath=`.status.clusters`
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.status.name`
// +kubebuilder:printcolumn:name="Namespace",type=string,JSONPath=`.status.namespace`
// +kubebuilder:printcolumn:name="Release",type=string,JSONPath=`.spec.release`
// +kubebuilder:printcolumn:name="Until",type=date,JSONPath=`.spec.until`
// +kubebuilder:printcolumn:name="Resolved",type=string,JSONPath=`.status.conditions[?(@.type=="Resolved")].status`
type ReleaseHold struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	// +required
	Spec ReleaseHoldSpec `json:"spec"`
	// +optional
	Status ReleaseHoldStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ReleaseHoldList contains a list of ReleaseHold.
type ReleaseHoldList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ReleaseHold `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Release{}, &ReleaseList{}, &ReleaseChannel{}, &ReleaseChannelList{},
			&ReleaseHold{}, &ReleaseHoldList{})
		return nil
	})
}
