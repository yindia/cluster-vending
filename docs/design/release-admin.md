# Release administration: locking versions and channels

Status: steps 1 and 4 of section 9 are implemented
(`api/v1alpha1/release_types.go`, `pkg/controller/release_*.go`); the rest
is design. Implemented as decided, with three changes the project owner
made, recorded where they apply:

- A release does not mention EKS add-ons at all. Which add-ons exist is
  defined in code (the contract's `AwsAddon.name` list and the Cluster
  kind's enum) and their versions come from AWS. A release pins only the
  components the platform installs itself, each with a required version.
- A `ReleaseHold` names its cluster by the cluster's ID (`spec.clusterID`,
  the API's `Cluster.id`), not by namespace and name.
- The open questions of section 10 are answered as recommended.

Where this document says "Decided (proposed)" for something not
implemented yet, it is still a proposal. This is the first step of building release
channels, tiers and manual/automatic upgrades: it fixes how a platform
operator locks what clusters run, before any rollout logic is written.

It builds on [rollouts.md](rollouts.md) and [components.md](components.md)
and keeps every decision recorded there. How tenants enable components and
override their values: [component-usage.md](component-usage.md). It refines two proposals of
rollouts.md, said so where it happens:

- rollouts.md section 5 put the rollout strategy and `paused` on the
  `Release`. Here a release is immutable, so those move to a new
  `ReleaseChannel` object.
- rollouts.md section 11 put a cluster's hold in `Cluster.status`, written by
  an RPC. Here it is a separate operator-owned object, `ReleaseHold`.

Every choice is marked **Decided (proposed)**: this document's decision, for
the project owner to confirm. Each names the alternative it rejects.

## 1. What is already decided (not reopened)

From rollouts.md section 2 and components.md section 2:

| Topic | Decision |
|---|---|
| Upgrade mode | Manual (default) or automatic. Manual clusters ignore channels; the tenant chooses versions. |
| Channel | Alpha, beta, stable, in that order. Unset means stable, for automatic clusters. |
| Tier | Tier 2, then 1, then 0 within a channel. Unset is tier 0 (implemented: `corecluster.ResolveSpec`). |
| Promotion | Automatic between tiers, and between channels by default. |
| Support window | The last three Kubernetes minors; older ones reach end of support. |
| Version sources | EKS add-on versions come from the AWS API. Only the platform's own components take versions from the release's per-minor map. |
| Holds | Only the platform operator may hold a release on a cluster. |
| Out of scope | Node AMI releases; components themselves (a separate resource and API, install mechanism undecided). |
| Imported clusters | Join as stable / tier 0 with their versions pinned (read in rollouts.md as: manual after takeover). |

## 2. What exists today

Checked against the code at the time of writing:

| Piece | Where | State |
|---|---|---|
| Channel and tier on a cluster | `ClusterSpec.release_channel` (8), `.tier` (9) in `idl/cloud/v1/cluster.proto`; `spec.releaseChannel` (`Alpha`/`Beta`/`Stable`), `spec.tier` (`Tier0`/`Tier1`/`Tier2`) in `api/v1alpha1/cluster_types.go`, both as print columns | Stored and returned; not updatable (`pkg/server/cluster/mutate.go` has no mask path); nothing acts on them. |
| Version lookups | `Reconciler.resolve`, `resolveVersions`, `PinnedAddons` in `pkg/reconcile/cluster/resolve.go`; `ResolveEKSVersions` in `pkg/cloud/aws/eks.go` | The controller asks AWS once per generation (`ResolvedGeneration`): an empty Kubernetes version becomes the EKS default, unpinned add-ons the version AWS recommends; the answer is written to the stored spec. |
| Upgrade progress | `status.upgrade` on `Cluster`; `GateUpgrade` in `pkg/reconcile/cluster/upgrade.go` | Node groups and add-ons wait for the control plane during an upgrade. |
| Renderer version | `crossplaneaws.RenderVersion` (now 2) in `pkg/crossplane/aws/aws.go`; `Deps.RenderTarget`, `RenderRollout`, `RenderBehind` in `pkg/reconcile/reconcile.go` | The controller sets `RenderTarget` to its own build's version (`pkg/controller/manager/manager.go`); the flags that limited it are gone. |
| Add-ons with IAM | `addonSetups` in `pkg/crossplane/aws/addons.go` | EBS, EFS, FSx and Mountpoint S3 CSI drivers get a pod-identity role. |
| Admin API | `AdminService` in `idl/cloud/v1/admin_service.proto` | `PauseReconcile`, `ResumeReconcile` only. No caller authentication exists on any service. |
| CRD install | `cloudstore.Install` (`pkg/database/cloudstore/install.go`) from the embedded `config/crd` | The controller installs the platform's kinds at start. |

## 3. The objects an operator edits

Three new cluster-scoped kinds in `cloud.cluster-vending.io/v1alpha1`,
next to `CloudAccount`, `Network` and `Cluster`:

| Kind | One per | Mutable | Says |
|---|---|---|---|
| `Release` | published set of versions | No (content immutable; only withdrawable) | What a cluster on each supported minor runs. |
| `ReleaseChannel` | channel: `alpha`, `beta`, `stable` | Yes | Which release the channel offers now, and how it rolls out. |
| `ReleaseHold` | held cluster | Yes | That one cluster is kept out of rollouts, or kept on one release. |

**Decided (proposed): three kinds, release content separate from channel
state.** Rejected: one `Release` that also carries strategy, pause and
progress (rollouts.md section 5). An object that is both "the versions" and
"where the rollout is" cannot be immutable, and immutability is what makes
"locked" mean something: a cluster that has release `2026-11-b` runs exactly
what `2026-11-b` says, forever.

**Decided (proposed): cluster-scoped.** They belong to the operator and
apply across tenant namespaces. Rejected: namespaced in a platform namespace,
which adds a namespace to every reference for no isolation gain.

### 3.1 Release

```go
// ComponentPin is one component the platform installs itself (Helm or
// Argo CD) at a fixed version, on one Kubernetes minor.
type ComponentPin struct {
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`
	Name string `json:"name"`
	// Version installed, e.g. the chart version. Required.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	Version string `json:"version"`
}

// KubernetesRelease is what clusters on one supported minor run.
type KubernetesRelease struct {
	// Minor, e.g. "1.34".
	// +kubebuilder:validation:Pattern=`^1\.[0-9]{2}$`
	Minor string `json:"minor"`
	// Default marks the minor a new cluster gets when it names none.
	// +optional
	Default bool `json:"default,omitempty"`
	// UpgradeTo moves automatic clusters on this minor to the next one
	// (one step, as CheckVersionStep requires). Unset: they stay.
	// +optional
	UpgradeTo string `json:"upgradeTo,omitempty"`
	// EndOfSupport is the date after which this minor is unsupported.
	EndOfSupport metav1.Time `json:"endOfSupport"`
	// Components offered on this minor. Absent: not offered.
	// +optional
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=64
	Components []ComponentPin `json:"components,omitempty"`
}

// ReleaseContent is what a release locks. Immutable.
// +kubebuilder:validation:XValidation:rule="self.kubernetes.exists_one(k, k.default)",message="exactly one minor is the default"
type ReleaseContent struct {
	// RenderVersion clusters are brought to (crossplaneaws.RenderVersion).
	// +kubebuilder:validation:Minimum=1
	RenderVersion int64 `json:"renderVersion"`
	// Kubernetes: exactly the last three supported minors.
	// +listType=map
	// +listMapKey=minor
	// +kubebuilder:validation:MinItems=3
	// +kubebuilder:validation:MaxItems=3
	Kubernetes []KubernetesRelease `json:"kubernetes"`
}

// ReleaseSpec is a published release.
// +kubebuilder:validation:XValidation:rule="!oldSelf.withdrawn || self.withdrawn",message="a withdrawn release stays withdrawn"
type ReleaseSpec struct {
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="release content is immutable; publish a new release"
	Content ReleaseContent `json:"content"`
	// Notes for operators and tenants.
	// +optional
	// +kubebuilder:validation:MaxLength=4096
	Notes string `json:"notes,omitempty"`
	// Withdrawn: no channel may offer it any more, and clusters on it are
	// moved by the next release their channel offers. Never undone.
	// +optional
	Withdrawn bool `json:"withdrawn,omitempty"`
}

// ReleaseStatus is written by the controller only.
type ReleaseStatus struct {
	// Conditions: Valid (the checks CEL cannot do, section 3.4).
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// Channels offering it now, and the clusters that have it applied.
	// +optional
	Channels []string `json:"channels,omitempty"`
	// +optional
	Clusters int32 `json:"clusters,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=cvrel
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Render",type=integer,JSONPath=`.spec.content.renderVersion`
// +kubebuilder:printcolumn:name="Default",type=string,JSONPath=`.spec.content.kubernetes[?(@.default==true)].minor`
// +kubebuilder:printcolumn:name="Withdrawn",type=boolean,JSONPath=`.spec.withdrawn`
// +kubebuilder:printcolumn:name="Valid",type=string,JSONPath=`.status.conditions[?(@.type=="Valid")].status`
// +kubebuilder:printcolumn:name="Clusters",type=integer,JSONPath=`.status.clusters`
type Release struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec   ReleaseSpec   `json:"spec"`
	Status ReleaseStatus `json:"status,omitempty"`
}
```

```yaml
apiVersion: cloud.cluster-vending.io/v1alpha1
kind: Release
metadata:
  name: 2026-11-b
spec:
  notes: "Renderer 2: Usage names; cert-manager offered on all minors."
  content:
    renderVersion: 2
    kubernetes:
      - minor: "1.35"
        default: true
        endOfSupport: "2027-12-01T00:00:00Z"
        components:
          - {name: karpenter, version: "1.8.1"}
      - minor: "1.34"
        endOfSupport: "2027-08-01T00:00:00Z"
        components:
          - {name: karpenter, version: "1.8.1"}
      - minor: "1.33"
        upgradeTo: "1.34"
        endOfSupport: "2027-04-01T00:00:00Z"
        components:
          - {name: karpenter, version: "1.6.3"}
```

Versions and dates are placeholders.

**Where versions come from (decided by the project owner):**

| Kind of thing | Version comes from | In a release |
|---|---|---|
| EKS add-ons: the defaults (vpc-cni, coredns, kube-proxy, eks-pod-identity-agent, aws-ebs-csi-driver) and those a tenant lists in `spec.aws.addons` (EFS/FSx CSI, cert-manager, metrics-server...) | AWS (`ResolveEKSVersions`) | Never. Which add-ons exist is defined in code: the contract's `AwsAddon.name` list and the Cluster kind's enum. |
| A component the platform installs itself (Helm or Argo CD) | The release | Listed with its version, which is required. |

Rejected: a source per entry (`EKSAddon` or `Platform`) with CEL forbidding
a version on an add-on entry. It put add-ons into an object whose purpose
is to lock versions the platform controls, for a list the code already
holds.

### 3.2 ReleaseChannel

```go
// RolloutPolicy is how a release moves through a channel's tiers.
type RolloutPolicy struct {
	// MaxInProgress clusters applying at once per tier (Tier0: always 1).
	// +kubebuilder:default=5
	// +kubebuilder:validation:Minimum=1
	MaxInProgress int32 `json:"maxInProgress,omitempty"`
	// MaxFailed failures in a tier before the channel halts.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	MaxFailed int32 `json:"maxFailed,omitempty"`
	// Soak: healthy time after a tier before the next starts.
	// +kubebuilder:default="24h"
	Soak metav1.Duration `json:"soak,omitempty"`
	// SoakPerTier overrides Soak for a tier.
	// +optional
	SoakPerTier map[ClusterTier]metav1.Duration `json:"soakPerTier,omitempty"`
}

// ReleaseChannelSpec is the operator's settings for one channel.
// +kubebuilder:validation:XValidation:rule="!has(self.release) || self.promotion == 'Pinned' || self.promotion == 'Manual'",message="spec.release is the release a Pinned or Manual channel offers"
type ReleaseChannelSpec struct {
	// Promotion: Automatic takes each release the upstream channel
	// completed (alpha has none upstream: it takes spec.release); Manual
	// and Pinned offer spec.release only. Default Automatic (decided).
	// +kubebuilder:validation:Enum=Automatic;Manual;Pinned
	// +kubebuilder:default=Automatic
	Promotion string `json:"promotion,omitempty"`
	// Release offered, for alpha always and for a Manual or Pinned channel.
	// +optional
	Release string `json:"release,omitempty"`
	Policy RolloutPolicy `json:"policy,omitempty"`
	// Paused: no cluster starts a new release. Clusters in progress finish.
	// +optional
	Paused bool `json:"paused,omitempty"`
}

// ReleaseChannelStatus is written by the controller only.
type ReleaseChannelStatus struct {
	// Release the channel offers now (spec.release, or the upstream's).
	Release string `json:"release,omitempty"`
	// Stage: the tier being rolled out, and per tier done/total/failed/held.
	Stage  ClusterTier  `json:"stage,omitempty"`
	Tiers  []TierStatus `json:"tiers,omitempty"`
	// History: the releases the channel offered, newest first, at most 20.
	History    []ChannelHistoryEntry `json:"history,omitempty"`
	Conditions []metav1.Condition    `json:"conditions,omitempty"` // Offering (implemented); Progressing, Halted, Complete with rollouts
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=cvch
// +kubebuilder:subresource:status
// +kubebuilder:validation:XValidation:rule="self.metadata.name in ['alpha', 'beta', 'stable']",message="channels are alpha, beta and stable"
// +kubebuilder:printcolumn:name="Release",type=string,JSONPath=`.status.release`
// +kubebuilder:printcolumn:name="Promotion",type=string,JSONPath=`.spec.promotion`
// +kubebuilder:printcolumn:name="Stage",type=string,JSONPath=`.status.stage`
// +kubebuilder:printcolumn:name="Paused",type=boolean,JSONPath=`.spec.paused`
type ReleaseChannel struct { /* TypeMeta, ObjectMeta, Spec, Status */ }
```

`TierStatus` holds `tier`, `total`, `done`, `inProgress`, `failed`, `held`,
`completedAt`; `ChannelHistoryEntry` holds `release`, `from`, `to`, `reason`.

```yaml
apiVersion: cloud.cluster-vending.io/v1alpha1
kind: ReleaseChannel
metadata: {name: alpha}
spec:
  release: 2026-11-b             # alpha always names its release
  policy: {maxInProgress: 5, maxFailed: 1, soak: 24h}
---
apiVersion: cloud.cluster-vending.io/v1alpha1
kind: ReleaseChannel
metadata: {name: stable}
spec:
  promotion: Automatic           # takes what beta completed
  policy: {soak: 72h, soakPerTier: {Tier0: 168h}}
```

The upstream of a channel is fixed by its name: beta follows alpha, stable
follows beta. **Decided (proposed): fixed order, not configurable.**
Rejected: a `follows` field, which allows cycles and contradicts the decided
order.

### 3.3 ReleaseHold

```go
// ReleaseHoldSpec keeps one cluster out of rollouts.
type ReleaseHoldSpec struct {
	// ClusterID is the held cluster's ID (the API's Cluster.id): a UUID,
	// immutable. The controller resolves it to the Cluster object (by the
	// store's ID label) and shows that namespace and name in the status.
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="clusterID is immutable; create another hold"
	ClusterID string `json:"clusterID"`
	// Release, when set, keeps the cluster on exactly this release (a
	// lock); unset, the cluster stays on whatever it has now (a freeze).
	// +optional
	Release string `json:"release,omitempty"`
	// Reason is shown on the cluster's status.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	Reason string `json:"reason"`
	// Until, when set, ends the hold by itself.
	// +optional
	Until *metav1.Time `json:"until,omitempty"`
}
```

**Decided (proposed): a separate operator-owned kind.** Rejected: a field
or annotation on `Cluster` (anyone who can edit the cluster could lift it,
and the decision is that only the operator holds), and status written by an
RPC (rollouts.md section 11; status belongs to the controller, and an RPC
needs caller authentication the platform does not have yet).

**Implemented status of channels (before rollouts exist):**
`status.release` is the release the channel offers (Pinned and Manual:
`spec.release`; Automatic: the release its upstream offers, because there
is no rollout progress yet to say a release completed upstream), with an
`Offering` condition. A release that is missing, withdrawn, being deleted
or not `Valid` is not offered: the channel keeps offering what it did and
`Offering=False` says why. `history` records each change. `stage` and
`tiers` stay empty until step 6.

### 3.4 How locking works

| To lock | The operator | Effect |
|---|---|---|
| A component version for everyone | Publishes a new `Release` with that version and moves the channels to it (by promotion, or `spec.release`). | Every automatic cluster ends up on it, stage by stage. Manual clusters get it when they enable the component or change their minor. |
| A channel on a release | Sets `promotion: Pinned` and `release: <name>` on the `ReleaseChannel`. | The channel stops taking upstream releases; its clusters stay on, or move to, that release. |
| Promotion behind a person | `promotion: Manual`; to promote, changes `release`. | Like Pinned, but meant to be advanced by hand each time. |
| Everything, now | `paused: true` on each channel. | No cluster starts a new release. |
| One cluster | Creates a `ReleaseHold` (with `release` to lock it, without to freeze it). | The rollout skips it and counts it as held. |
| Never use a release again | Sets `withdrawn: true` on it. | No channel may offer it; clusters on it move with their channel's next release. |

How immutability is enforced:

- `self == oldSelf` on `spec.content` (CEL, by the API server): no write can
  change a published release's versions. To change a version, publish a new
  release. Rejected: immutability by convention, or by a webhook the
  platform would have to run.
- `withdrawn` can only go from false to true (CEL transition rule).
- Checks CEL cannot do are made by the controller and written as the
  `Valid` condition: the three minors are consecutive and include none past
  end of support; `renderVersion` is not above the running build's
  `RenderVersion`; every component is a known `ComponentDefinition`. A
  channel never offers a release that is not `Valid=True`; pointing one at
  it shows `Offering=False` with the reason.
- Deleting a `Release` that a channel offers or a cluster has is refused:
  the controller adds a finalizer while it is referenced.

Publishing is creating the object. Superseding is a channel taking a newer
release. Withdrawing is the flag. Nothing else changes a release.

## 4. Who may write what

**Decided (proposed):**

| Principal | Release, ReleaseChannel, ReleaseHold | Cluster spec | Cluster status |
|---|---|---|---|
| Platform operator (`release-admin` ClusterRole, new) | create, update, delete (content immutable) | - | - |
| Anyone with read access (`release-viewer` ClusterRole, new) | get, list, watch | - | - |
| Controller (`config/rbac`) | get, list, watch; update status and finalizers | read | write |
| API server (`config/server`) | get, list, watch (to answer tenants) | write (tenant's requests) | - |
| Tenants | none directly; they see `status.release` on their cluster | through the API | - |

The authoritative way to edit these objects is `kubectl` or Git with the
`release-admin` role. **Decided (proposed): admin RPCs come later, after
the API authenticates callers.** Rejected: admin RPCs now. `AdminService` is
protected only by not being exposed today; a `CreateRelease` RPC without
authentication would let anyone who reaches the API lock every cluster.

The RPCs, additive to `AdminService`, all proposed:

```protobuf
service AdminService {
  // existing: PauseReconcile, ResumeReconcile
  rpc ListReleases(ListReleasesRequest) returns (ListReleasesResponse);
  rpc GetRelease(GetReleaseRequest) returns (GetReleaseResponse);
  rpc PublishRelease(PublishReleaseRequest) returns (PublishReleaseResponse);   // creates
  rpc WithdrawRelease(WithdrawReleaseRequest) returns (WithdrawReleaseResponse);
  rpc ListReleaseChannels(ListReleaseChannelsRequest) returns (ListReleaseChannelsResponse);
  rpc SetChannelRelease(SetChannelReleaseRequest) returns (SetChannelReleaseResponse); // promotion + release
  rpc PauseChannel(PauseChannelRequest) returns (PauseChannelResponse);
  rpc ResumeChannel(ResumeChannelRequest) returns (ResumeChannelResponse);       // also clears a halt
  rpc HoldCluster(HoldClusterRequest) returns (HoldClusterResponse);             // creates a ReleaseHold
  rpc UnholdCluster(UnholdClusterRequest) returns (UnholdClusterResponse);
}

message Release {
  string name = 1;
  int64 render_version = 2;
  repeated KubernetesRelease kubernetes = 3;
  string notes = 4;
  bool withdrawn = 5;
}
message KubernetesRelease {
  string minor = 1;
  bool default = 2;
  string upgrade_to = 3;
  google.protobuf.Timestamp end_of_support = 4;
  repeated ComponentPin components = 5;
}
message ComponentPin {
  string name = 1;
  string version = 2;          // required
}
message SetChannelReleaseRequest {
  ReleaseChannelName channel = 1;   // ALPHA | BETA | STABLE
  ChannelPromotion promotion = 2;   // AUTOMATIC | MANUAL | PINNED
  string release = 3;
}
message HoldClusterRequest {
  string tenant_id = 1;
  string cluster_id = 2;
  string release = 3;               // empty: freeze where it is
  string reason = 4;
  google.protobuf.Timestamp until = 5;
}
```

Tenants get no new RPC: `GetCluster` returns `status.release` (section 5).

## 5. How a cluster resolves what it runs

**Decided (proposed): a pure package, `pkg/release`,** with no client, no
store, no RPC library, like `pkg/network` and `pkg/cluster`.

```go
// Input is everything the decision needs; the caller reads it.
type Input struct {
	Mode            UpgradeMode   // Manual | Automatic
	Channel         ReleaseChannel
	Tier            ClusterTier
	SpecVersion     string            // spec.controlPlane.kubernetesVersion
	RunningVersion  string            // status.currentKubernetesVersion
	PinnedAddons    map[string]string // reconcile/cluster.PinnedAddons
	Components      []string          // enabled components (later)
	Offered         *ReleaseContent   // the channel's release (nil: none)
	Newest          *ReleaseContent   // newest Valid release on any channel
	Hold            *Hold             // nil: not held
	Now             time.Time
}

// Desired is what the cluster should run, and why.
type Desired struct {
	KubernetesVersion string
	RenderVersion     int64
	Components        map[string]ComponentPin // the platform's components and their versions
	Release           string                  // the release this follows ("" for manual)
	Supported         bool
	EndOfSupport      *time.Time
	Reason            string                  // e.g. "manual", "held: <reason>", "channel paused"
}

func Resolve(in Input) (Desired, error)
```

Rules:

| Case | Kubernetes version | Components | Release |
|---|---|---|---|
| Manual | The spec's (empty: the newest release's default minor, then the EKS default if no release exists). | The newest release's pins for the cluster's minor, when enabled or when the minor changes. | None. Channel ignored. |
| Automatic, not held | The spec's, moved by the offered release's `upgradeTo` for the cluster's minor. | The offered release's pins for the cluster's minor. | The channel's offered release. |
| Automatic, held with a release | As that release says. | That release's pins. | The hold's release. |
| Automatic, held without one | Unchanged. | Unchanged. | Unchanged. |
| Any, minor not in the newest release, or past `endOfSupport` | Unchanged (never forced). | Unchanged; enabling new components refused. | As above; `Supported=false`. |

EKS add-on versions are not in `Desired`. They stay with the existing
lookup (`resolveVersions`), which runs whenever the Kubernetes version
changes, so a release that moves the minor also moves the unpinned add-ons
(rollouts.md section 5). When automatic clusters also re-look-up add-ons for
the same minor is rollouts.md Open question 9; the recommended answer (at
the moment a release reaches the cluster) needs only the reconciler to bump
`ResolvedGeneration`-style lookup when `Desired.Release` changes.

Where it is used:

- **Which release, when:** the channel controller decides *when* a cluster
  starts a release (tier order, `maxInProgress`, soak, pause, holds) and
  writes `status.release.target` on the `Cluster`.
- **What it means:** the cluster reconciler calls `Resolve` with the
  target's content and renders from `Desired` instead of the spec for
  automatic clusters. For manual clusters it calls `Resolve` only to fill
  `Supported`, `EndOfSupport` and component pins.
- **Reported:** `Cluster.status.release` gains `target`, `applied`,
  `supported`, `endOfSupport`, `heldBy`. Print columns: `Mode`, `Release`.

Rejected: putting the decision inside the reconciler. The rules are a table
that needs to be tested on its own with every combination, and both the
channel controller and the API's validate-only plan need the same answer.

## 6. The upgrade mode field

**Decided (proposed), the field:**

| Where | Name | Values | Default |
|---|---|---|---|
| Contract | `ClusterSpec.upgrade_mode = 13` (next free; 10 is `aws`, 11 and 12 are kept for gcp and azure) | `UPGRADE_MODE_UNSPECIFIED`, `UPGRADE_MODE_MANUAL`, `UPGRADE_MODE_AUTOMATIC` | `ResolveSpec` stores unspecified as `MANUAL`, as it does for the tier |
| Kind | `spec.upgradeMode` | `Manual`, `Automatic` | `+kubebuilder:default=Manual` |

**Updatable, with these mask paths** (today none of the three is): 
`spec.upgrade_mode`, `spec.release_channel`, `spec.tier`. Rules, from
rollouts.md section 12:

- Manual to automatic: allowed. The cluster joins its (channel, tier) stage;
  if that stage is already done for the offered release, at once.
- Automatic to manual: refused while `status.release.target` differs from
  `.applied` (a release is being applied). Otherwise the running versions
  are written into the spec, so the pin is true.
- Channel or tier: refused while a release is being applied to the cluster;
  otherwise takes effect for the next stage the cluster reaches.
- On an automatic cluster, an update naming `kubernetes_version` is refused
  with a message saying the mode decides it (rollouts.md Open question 6).
- Imported clusters are stored manual at takeover.

## 7. Bootstrapping

**Implemented:** the defaults are Go values in the controller
(`pkg/controller/release_defaults.go`: a release with 1.37 default, 1.36
and 1.35 upgrading one minor at a time, placeholder end-of-support dates,
this build's renderer and the shipped catalog's versions; alpha `Pinned` to
it, beta and stable `Automatic`), created by the controller at start when
absent. `config/samples` holds the same content as an operator's release
(`2026-11-a`) plus an example hold. Not `go:embed`ded YAML as first
proposed below: the values are typed and a test keeps the samples' content
equal to them.

**The shipped release is named after its content.** A release's content is
immutable, so a build that ships other versions cannot change the release
an older build created: under one fixed name, an existing installation
would keep the old build's pins for ever. The shipped release is therefore
named `shipped-<first 8 hex digits of the SHA-256 of spec.content as
JSON>` and labelled `cloud.cluster-vending.io/shipped: "true"`. The same
content always has the same name; a build with other content creates
another release next to the old ones, which stay (clusters may still run
them, and a release is never deleted by the controller).

**A new build takes alpha over only while alpha is a build's.** At start,
after creating its release, the controller points alpha at it when all of
this holds:

- alpha is `Pinned`, not paused, and its rollout policy is unset (or the
  defaults the API server fills in);
- the release alpha names exists and is a shipped one: it carries the
  label, or it is `2026-11-a` with the shipped notes (what builds created
  before releases were named after their content).

Anything else is the operator's and is never touched: a release they
published (no label), alpha made `Manual`, paused, or given a policy, and
alpha naming a release that is gone. beta and stable are never written.
This replaces the recommendation of open question 4 below for
installations that never published a release: there a controller upgrade
does move alpha, because nobody else would. An operator who wants to
decide each move pins alpha to a release of their own, once.

**What the operator sees after a controller upgrade.** `kubectl get
releases` lists the new `shipped-…` release next to the old one; alpha's
`spec.release` and `status.release` name the new one, and its history has
an entry for it. beta and stable are `Automatic`: each offers what its
upstream offers on its next reconcile, which today is at once, since a
release counts as completed upstream as soon as the upstream offers it
(rollouts, tiers and the soak time of `spec.policy` are not acted on yet;
rollouts.md). When they are, beta and stable take a new shipped release
only after alpha's rollout completed and soaked, like any other release.
A step the channel may not take (a component marked
`upgradeOneMinorAtATime` skipping a minor) leaves the channel on the old
release with the reason in its `Offering` condition. No cluster that has
platform components installed changes release by this: a cluster keeps the
release it was installed from. A cluster with none installed yet follows
its channel.

A fresh installation gets, as before, one release and the three channels,
all offering it.

**Decided (proposed): the platform ships a default release and the three
channels, embedded in the controller binary** (`config/releases/*.yaml`,
`go:embed`, installed at start next to the CRDs by `cloudstore.Install`).

- At start the controller creates the embedded release **by name** if no
  release of that name exists, and the three channels if they do not exist
  (alpha offering the embedded release, beta and stable `Automatic`). It
  never updates an existing object: once an operator has changed a channel,
  a new build does not undo it. A new build may ship a new release name; the
  operator decides whether alpha offers it.
- **Fresh install, no release** (an operator deleted them, or install was
  skipped): clusters behave as today. Manual clusters are unaffected;
  automatic clusters are not moved; new clusters get the EKS default minor
  as today; `ReleaseChannel` status says `NoRelease`.

Rejected: requiring the operator to create a release before the first
cluster. A first install would then have no default minor and no supported
window until someone writes YAML.

## 8. Renderer versions and what replaces RenderTarget

Today the controller's `RenderTarget` is its own build's `RenderVersion`,
so any build with a new renderer re-applies every cluster at once.

**Decided (proposed):** `RenderBehind` stops reading a global target and
reads the cluster's own: a cluster is re-applied for a renderer change only
when `Desired.RenderVersion` (its target release's `renderVersion`) is above
its row's `render_version`. `Deps.RenderTarget` and `RenderRollout` are
deleted.

- A build can only render its own version. A release's `renderVersion` above
  the build's is invalid (`Valid=False`); one below it means "do not re-apply
  unchanged clusters yet". It is a gate on *proactive* re-applies only: a
  cluster whose spec changes is rendered by the running build immediately,
  whatever its release says. This is stated as a limit, not hidden.
- Manual clusters take renderer changes after stable / tier 0 completes
  (rollouts.md Open question 3, recommended there).
- Rolling a renderer back still means deploying the previous build
  (rollouts.md Open question 2).

## 9. Implementation steps

Each is mergeable alone and leaves the platform working.

| # | Delivers | Touches |
|---|---|---|
| 1 | `Release`, `ReleaseChannel`, `ReleaseHold` types and CRDs with the CEL rules; `release-admin` / `release-viewer` roles; controller and server RBAC; install with the other CRDs. No behaviour. | `api/v1alpha1`, `config/crd`, `config/rbac`, `config/server`, `pkg/database/cloudstore` (install list) |
| 2 | `upgrade_mode` on the contract and the kind, default manual; mask paths for mode, channel and tier with the section 6 rules; conversion. | `idl/cloud/v1/cluster.proto`, `api/v1alpha1`, `pkg/convert`, `pkg/cluster`, `pkg/server/cluster/mutate.go` |
| 3 | `pkg/release.Resolve` with table tests for every row of section 5. No caller yet. | `pkg/release` (new) |
| 4 | Embedded default release and channels (section 7); release validation controller writing `Valid`; finalizer against deleting a referenced release. | `config/releases`, `pkg/controller` |
| 5 | `Cluster.status.release` for every cluster from `Resolve`: supported, end of support, the release a manual cluster's defaults come from. Unsupported condition. | `pkg/reconcile/cluster`, `api/v1alpha1`, `pkg/convert` |
| 6 | Channel controller: offered release (pinned, manual, automatic promotion after upstream completes), tiers, `maxInProgress`, `maxFailed`, soak, pause, holds; writes `status.release.target`. | `pkg/controller` (new reconciler), `pkg/release` |
| 7 | Cluster reconciler renders automatic clusters from `Desired`; `RenderBehind` per cluster; `RenderTarget`/`RenderRollout` deleted. | `pkg/reconcile`, `pkg/reconcile/cluster`, `pkg/controller/manager` |
| 8 | CLI: `release list|get`, `channel list|get`, read-only first; then admin RPCs and the matching commands once callers are authenticated. | `pkg/cli`, `cmd`, later `idl/cloud/v1/admin_service.proto`, `pkg/server/admin` |
| 9 | Component pins used when components exist (components.md). | `pkg/release`, components work |

Steps 1 to 3 can be done in parallel.

## 10. Open questions

All six were answered as recommended by the project owner.

1. **Per-cluster component versions outside any release.** A hold can lock
   a cluster to a release, not to an arbitrary version. *Recommendation:*
   keep it that way; a one-off version is a new release offered to that
   cluster through a hold with `release`.
2. **Who may publish before authentication exists.** *Recommendation:*
   `kubectl` and Git with the `release-admin` role only, as in section 4.
3. **Release naming.** *Recommendation:* `YYYY-MM-<letter>`, sortable,
   checked by a CEL pattern on `metadata.name`. *Implemented*, with one
   more form the pattern accepts, `shipped-<8 hex digits>`: the
   controller's own releases (section 7).
4. **Embedded release updates.** A new build with a new embedded release:
   should alpha move to it automatically? *Recommendation:* no; the operator
   points alpha at it, so a controller upgrade never starts a rollout.
   *Settled otherwise for installations whose alpha is still a build's*
   (section 7): there nobody would point alpha at the new release, and the
   installation kept an old build's pins for ever. An alpha the operator
   has taken over is never moved.
5. **EKS add-ons and releases.** Settled by the project owner: releases
   never list EKS add-ons (see section 3.1).
6. **Hold expiry.** `until` ends a hold by itself. *Recommendation:* keep it
   optional; holds without it show on the channel's `held` count so they are
   not forgotten.

## Resolver notes

`pkg/release` implements section 5 with these choices where the section left
room; they are proposals like the rest of this document.

- **API shape.** `Resolve(ClusterFacts, releases, channels, *Hold) Decision`
  and `CheckVersion(releases, channels, channel, version) error`, with the
  package's own plain types instead of the `Input`/`Desired` sketch, so the
  package depends on nothing. An adapter from the kinds comes with the
  callers.
- **Manual clusters** are checked against the release their channel offers
  (unset: stable), not "the newest release on any channel": one rule for
  both modes, and the channel is what the tenant chose.
- **Manual protects versions, not bugs** (decided by the project owner): a
  manual cluster's Kubernetes and component versions stay the tenant's, but
  its renderer version is the offered release's, so platform fixes reach it
  (note code `RendererFixes`). Component pins are not applied to manual
  clusters.
- **Staying means nothing moves.** When a cluster stays (paused channel,
  unknown channel, channel offering nothing usable, frozen hold), neither
  the release nor the Kubernetes minor changes, even if the current
  release has an `upgradeTo` for that minor.
- **A pause wins over a withdrawal.** A cluster on a withdrawn release does
  not move while its channel is paused; it is warned and moves once the
  channel resumes.
- **Holds apply to automatic clusters only;** a manual cluster's versions are
  already the tenant's.
- **The tier is not an input.** It orders the rollout (the channel
  controller), not its target.
- **`CheckVersion` checks the minor** (`1.34.2` as `1.34`) against the
  offered release and passes when there is nothing to check against (no
  release, unknown channel, nothing offered), so a fresh install behaves as
  today.
