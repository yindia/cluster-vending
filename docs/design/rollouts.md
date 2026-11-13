# Rolling releases out by channel and tier

Status: design, not implemented. Nothing in this document exists in code
except where a file or identifier is named.

Each section marks its statements as **Decided** (by the project owner, not
reopened here), **Proposed** (this document's recommendation) or **Open**
(needs an answer; collected in section 17).

Components installed on clusters (Karpenter, Kyverno and so on) have their
own design: [components.md](components.md). How an operator locks versions
and channels (the `Release`, `ReleaseChannel` and `ReleaseHold` objects) is
in [release-admin.md](release-admin.md).

Terms used below:

- **Renderer version.** The platform turns a cluster's spec into cloud
  objects with code called the renderer. The renderer has a version number
  (`crossplaneaws.RenderVersion`). When the code changes what it produces,
  the number goes up, and clusters made with the old number are applied
  again.
- **Soak time.** How long a group of clusters must stay healthy after a
  release before the next group starts.
- **Hold.** A mark on one cluster that keeps releases away from it.
- **Stage.** One (channel, tier) pair, for example beta / tier 2.

## 1. Summary

A *release* is a named set of platform-chosen versions. Clusters in
*automatic* upgrade mode receive it in nine stages: the three channels in
order, and within each channel the three tiers from least to most sensitive.
A stage starts only when the previous one is healthy and has soaked.
Promotion from stage to stage, including from one channel to the next, is
automatic. Clusters in *manual* upgrade mode, the default, are not moved:
their tenant chooses versions. The rollout writes each cluster's target into
the cluster's *status*, never into its spec. A change that cannot be undone
on the cloud side (a Kubernetes minor upgrade) is stopped and fixed forward,
not rolled back.

## 2. What is decided

**Decided.**

Upgrade mode:

- A cluster has an upgrade mode: **manual** (the default) or **automatic**.
- **Manual.** The release channel does not apply to the cluster. The tenant
  chooses versions, and the cluster stays where it is until the tenant
  changes it. A version in the tenant's spec pins the cluster.
- **Automatic.** The channel applies. An unset channel means stable.
  Releases move the cluster automatically, including the versions the
  tenant wrote at creation: the spec's version is the starting point, not a
  pin.
- An unset tier is tier 0. This is implemented: `corecluster.ResolveSpec`
  stores a created cluster as `CLUSTER_TIER_0`.

Order and promotion:

- A cluster has a release channel (alpha, beta, stable) and a tier (tier 0,
  tier 1, tier 2). Tier 0 is the most sensitive. Both are on the contract
  (`ClusterSpec.release_channel`, `ClusterSpec.tier` in
  `idl/cloud/v1/cluster.proto`) and on the controller's `Cluster` kind
  (`spec.releaseChannel`, `spec.tier` in `api/v1alpha1`). Today
  they are stored and returned and nothing acts on them.
- Channels receive a release in the order alpha, beta, stable. Within a
  channel a release reaches tier 2 first, then tier 1, then tier 0.
- Promotion between tiers is automatic. Promotion between channels is
  automatic by default.
- Node AMI releases are not part of a release until a node group can pin
  one.
- Imported clusters join releases after takeover, as stable and tier 0,
  with their explicit versions pinned.
- Only the platform operator may hold a release on a cluster.

Kubernetes versions and support:

- The platform supports the last three Kubernetes minor versions. Older
  ones reach an end of support.
- Every supported Kubernetes version has its own list of component
  versions. A cluster on an older Kubernetes version gets the versions
  listed for that version.
- EKS add-on versions are not platform configuration. They come from the
  AWS API: the platform asks AWS which add-on version goes with the
  cluster's Kubernetes version. Only the platform's own components take
  their versions from the per-Kubernetes-version map in a release.

Timing:

- Rollouts are built at the end of the project, after the controller
  reconciles clusters.

**This document's reading of one decision.** "Imported clusters join as
stable and tier 0 with their explicit versions pinned" is read as: an
imported cluster is in *manual* mode after takeover, because manual is the
default and manual is what pins versions. Its channel is unset and its tier
is tier 0, so when the tenant later switches it to automatic it is in the
last stage, stable / tier 0. If the intent was that takeover puts the
cluster straight into automatic mode, section 12 changes. This is Open
question 8.

## 3. What exists today

| Mechanism | Where | What it does now |
|---|---|---|
| Renderer version | `crossplaneaws.RenderVersion` (`pkg/crossplane/aws/aws.go`), stored per row as `render_version` | Identifies what the renderers produce. A row applied with an older renderer is behind. |
| Renderer rollout | `reconcile.Deps.RenderTarget`, `RenderRollout`, `Deps.RenderBehind` (`pkg/reconcile/reconcile.go`); the controller's `--render-target` and `--render-rollout` flags were removed: the controller always brings every resource to its own build's renderer version | A renderer change reaches every resource as soon as the new build runs. There is no staged rollout of renderer changes until releases exist. |
| Version resolution | `resolveVersions`, `reresolveVersions`, `pinnedAddons` (`pkg/server/cluster/versions.go`); `ResolveEKSVersions` (`pkg/cloud/aws/eks.go`) | At create and update, an empty Kubernetes version becomes the EKS default and an add-on without a version becomes the one EKS recommends. The resolved versions are stored in the spec. Add-ons the client gave a version are pinned. |
| Upgrade gating | `Reconciler.GateUpgrade`, `CheckVersionStep` (`pkg/reconcile/cluster/upgrade.go`) | During a Kubernetes upgrade the node groups and add-ons stay at the running versions until the control plane reports the target. `CheckVersionStep` checks the step between the stored and the requested version. |
| Default add-ons | `crossplaneaws.DefaultAddons` (`pkg/crossplane/aws/addons.go`) | The add-ons every cluster gets: vpc-cni, coredns, kube-proxy, eks-pod-identity-agent, aws-ebs-csi-driver. |
| Ordered apply | `sequence.Engine` (`pkg/crossplane/sequence`) | Applies one owner's objects in order. It orders objects inside one cluster, not clusters. |
| Kinds and ownership | `api/v1alpha1`: CloudAccount owns Network owns Cluster | Status subresource with `observedGeneration`, `phase`, `conditions` (Ready, Synced), observed versions and an inventory. |
| Shipped release | `DefaultRelease`, `ShippedReleaseName`, `CreateReleaseDefaults` (`pkg/controller/release_defaults.go`) | Each build creates its release, named `shipped-<hash of its content>` and labelled `cloud.cluster-vending.io/shipped`, and points alpha at it while alpha is still pinned to a shipped release and otherwise untouched; beta and stable (`Automatic`) follow at once, since no rollout or soak is acted on yet. A cluster with platform components installed keeps the release they came from; one without follows its channel. See release-admin.md, section 7. |

Two facts follow from the code and shape the design:

- The platform installs nothing on a cluster except EKS add-ons. There are
  no platform components (no Helm charts, no agents) to release.
- The node AMI is not chosen by the platform. A node group renders an AMI
  type and a Kubernetes version; EKS picks the AMI release. There is no
  field to pin an AMI release today.

## 4. Upgrade mode

**Decided** (the behaviour). **Proposed** (the field and its name).

A new field on the cluster spec, proposed as `ClusterSpec.upgrade_mode` on
the contract and `spec.upgradeMode` on the `Cluster` kind:

| Value | Meaning |
|---|---|
| `UPGRADE_MODE_UNSPECIFIED` | Stored as manual. |
| `UPGRADE_MODE_MANUAL` | Default. Releases do not move the cluster. |
| `UPGRADE_MODE_AUTOMATIC` | Releases move the cluster, by channel and tier. |

What each mode means for each kind of version:

| Setting | Manual | Automatic |
|---|---|---|
| Kubernetes version | The spec's version. Changes only when the tenant updates the spec. | Starts at the spec's version (or the default when empty). Releases move it. |
| EKS add-on versions | From AWS, for the cluster's Kubernetes version, unless the tenant named one (section 5). | From AWS, for the cluster's Kubernetes version. When they are looked up again is Open question 9. |
| Component versions ([components.md](components.md)) | The version listed for the cluster's Kubernetes version when the component is enabled, or the one the tenant names. | The versions the cluster's release lists. |
| Renderer version | Open question 3. | The release's. |
| Release channel | Ignored. | Applies; unset means stable. |
| Tier | Stored; used only if the cluster becomes automatic. | Applies; unset is tier 0. |

The renderer version is not a tenant setting, and a manual cluster cannot be
left on an old renderer forever: objects rendered by old code are eventually
not understood by new code. **Proposed:** renderer changes reach manual
clusters too, last, after stable / tier 0, and change no version the tenant
chose. This is not in the decisions, so it is Open question 3.

## 5. What a release is

**Proposed.** A release lists, for each supported Kubernetes minor, the
component versions the platform uses for that minor. A release changes only
what it names.

| Content | Reversible on the cloud side | Exists today |
|---|---|---|
| Renderer version (`RenderVersion`) | Yes: apply again with the previous renderer. | Yes |
| EKS add-on versions | Not release content. **Decided:** they come from AWS (see below). | Looked up at create and at some updates |
| Component versions, per Kubernetes minor | Depends on the component. | No components exist yet |
| Kubernetes patch, and a move to the next minor | Patch: no, but low risk. Minor: no. | The minor is the client's or the EKS default at create |
| Node AMI release | Yes. | No field today. **Decided:** left out until a node group can pin one |

**Decided:** the support window is the last three Kubernetes minors, and
each has its own list of component versions. So a release has three entries
under `kubernetes`, one per supported minor, and a minor that is not listed
is out of support as of that release.

**Decided:** EKS add-on versions are not in a release. What the code does
today, and keeps doing:

- `ResolveEKSVersions` (`pkg/cloud/aws/eks.go`) asks AWS
  (`DescribeAddonVersions`, by add-on name and Kubernetes version) which
  versions are compatible with the cluster's Kubernetes version and which
  one AWS marks as the default for it. An add-on without a version in the
  tenant's request gets that default. An add-on the tenant gave a version
  keeps it, after a check that AWS lists it as compatible; an incompatible
  one is refused as an invalid argument.
- An empty Kubernetes version becomes the one AWS reports as the EKS
  default (`DescribeClusterVersions`).
- The lookup happens when the cluster is created (`resolveVersions`) and
  again when an update changes the Kubernetes version or the add-ons
  (`reresolveVersions`, `pkg/server/cluster/versions.go`). The add-ons the
  tenant pinned are read from the stored request (`pinnedAddons`); the
  others follow the new Kubernetes version.
- The result is stored in the spec. Nothing looks the versions up again
  later: a cluster that is not updated keeps the add-on versions it got,
  even when AWS has since marked a newer one as the default.
- During a Kubernetes upgrade the add-ons stay at their running versions
  until the control plane reports the target (`GateUpgrade`).

A release is an object, not configuration, for three reasons:

- It has state that changes while it runs (which stage, which clusters are
  done, why it stopped). Configuration has nowhere to hold that.
- Two releases can exist at once (section 13). Flags describe one.
- Pausing and resuming must not need a restart. The removed
  `--render-target` setting did.

`Release` is cluster-scoped: it is the platform operator's object and spans
every tenant namespace.

```yaml
apiVersion: cloud.cluster-vending.io/v1alpha1
kind: Release
metadata:
  name: 2026-11-b            # operator-chosen, sortable
spec:
  renderVersion: 2
  # One entry per supported Kubernetes minor: the last three.
  kubernetes:
    - minor: "1.34"
      default: true          # what a new cluster gets when it names none
      components:
        karpenter: 1.8.1
        kyverno: 3.5.2
    - minor: "1.33"
      components:
        karpenter: 1.8.1
        kyverno: 3.4.4
    - minor: "1.32"
      # Automatic clusters on the oldest supported minor are moved up.
      upgradeTo: "1.33"
      components:
        karpenter: 1.6.3
        kyverno: 3.4.4
  # When each minor stops being supported, for status and warnings.
  endOfSupport:
    "1.32": "2027-03-23"
  # How it moves.
  strategy:
    maxInProgress: 5         # clusters applying at once, per stage
    maxFailed: 1             # failures in a stage before the rollout stops
    soak: 24h                # healthy time before the next stage
    promotion: Automatic     # between channels; Manual also allowed
  paused: false
status:
  phase: Progressing          # Pending, Progressing, Soaking, Halted, Complete
  stage: {channel: Alpha, tier: Tier1}
  stages:
    - {channel: Alpha, tier: Tier2, total: 12, done: 12, failed: 0, completedAt: "2026-11-12T03:10:00Z"}
    - {channel: Alpha, tier: Tier1, total: 4, done: 1, failed: 0, inProgress: 3}
  conditions:
    - type: Healthy
      status: "True"
```

The version numbers and the date in the example are placeholders.

| Field | Meaning |
|---|---|
| `spec.renderVersion` | Renderer version clusters are brought to. Must not exceed the controller build's `RenderVersion`. |
| `spec.kubernetes[].minor` | A supported Kubernetes minor. The last three are listed. |
| `spec.kubernetes[].default` | The minor a new cluster gets when its spec names none. One entry has it. |
| `spec.kubernetes[].components` | Component versions for clusters on that minor ([components.md](components.md)). |
| `spec.kubernetes[].upgradeTo` | Move automatic clusters on that minor to the next minor. One step only, as `CheckVersionStep` requires. |
| `spec.endOfSupport` | Date after which a minor is unsupported, shown in cluster status. |
| `spec.strategy.*` | Gates, section 8. |
| `spec.paused` | Stops starting new clusters. Section 9. |
| `status.*` | Written by the rollout controller only. |

The component lists serve manual clusters too. A manual cluster does not
*move* with a release, but when a component is enabled on it, or when its
tenant changes its Kubernetes version, the component versions it gets are
the ones the newest completed release lists for its minor.

What this means for an automatic cluster's add-ons: a release that moves the
cluster to the next Kubernetes minor (`upgradeTo`) changes its Kubernetes
version, which is one of the two moments the add-on versions are looked up
again, so its unpinned add-ons then follow what AWS reports for the new
minor. Between Kubernetes upgrades nothing looks them up again today.
Whether an automatic cluster's add-ons should also follow a new AWS default
for the *same* Kubernetes version, and when, is Open question 9.

## 6. Admin configuration

**Proposed.**

Releases are admin-owned objects. An admin creates one in either of two
ways, which are equivalent:

- `kubectl apply -f release.yaml` against the management cluster, by hand or
  from a Git repository through whatever applies manifests there.
- An admin RPC that takes the same content (`AdminService.CreateRelease`).

Actions that are not just data are admin RPCs, with a CLI command for each.
They write fields on the objects, so `kubectl` works as well:

| Action | RPC (proposed) | Writes |
|---|---|---|
| Pause a release | `PauseRelease` | `Release.spec.paused: true` |
| Resume a release, also after a halt | `ResumeRelease` | `Release.spec.paused: false`, and clears the halt |
| Promote to the next channel, for a release that uses manual promotion | `PromoteRelease` | the approved channel on the release |
| Hold a cluster, and lift the hold | `HoldCluster`, `ReleaseClusterHold` | the hold on the cluster (section 11) |

How configuration grows:

- **A new version of a component**: a new value in the next release. No
  code change. (Add-on versions are not configured; they come from AWS.)
- **A new component**: a catalog entry ([components.md](components.md)) and
  a new key under `components` in the next release. No code change for a
  component that needs no cloud resources.
- **A new kind of setting** (for example node AMI releases, later): a new
  optional field on the `Release` kind. Releases written before the field
  existed do not have it and behave as before. Fields are added, never
  given a new meaning, the same rule the contract follows.

What it replaces: the `--render-target` and `--render-rollout` settings,
already removed from the controller (it always renders with its own build's
version); `reconcile.Deps.RenderTarget` and `RenderRollout` remain as the
mechanism a release will drive. Today a renderer change reaches every
resource as soon as the new controller build runs. With releases, `spec.renderVersion`
is the target and the stages are the list. The flags are removed in step 3
of the plan (section 14).

Permissions: `Release` objects and holds are writable by the platform
operator only. Tenants can read which release their cluster has, from the
cluster's status.

## 7. The order

**Decided.** Nine stages:

| # | Channel | Tier |
|---|---|---|
| 1 | Alpha | Tier 2 |
| 2 | Alpha | Tier 1 |
| 3 | Alpha | Tier 0 |
| 4 | Beta | Tier 2 |
| 5 | Beta | Tier 1 |
| 6 | Beta | Tier 0 |
| 7 | Stable | Tier 2 |
| 8 | Stable | Tier 1 |
| 9 | Stable | Tier 0 |

A stage with no clusters is complete at once and does not soak.

Only clusters in automatic mode are members of a stage.

**Decided.** Moving from one tier to the next inside a channel is
automatic, after the gates in section 8. Moving from one channel to the next
(after stages 3 and 6) is automatic by default as well.

**Proposed.** A release may still ask for a person at the channel boundary
with `strategy.promotion: Manual`; the operator then promotes it with
`PromoteRelease`. This is meant for a release that contains a Kubernetes
minor move, which cannot be undone. Because promotion is automatic, the soak
time is the only pause between channels, so it should not be set to zero.

## 8. Gates between stages

**Proposed.** A cluster is *done* for a release when all of these hold, read
from the `Cluster` status the controller already writes:

- `status.release.applied` names this release (section 10).
- `status.observedGeneration` equals `metadata.generation`.
- The `Ready` and `Synced` conditions are `True`.
- `status.currentKubernetesVersion` is the release's target for that
  cluster, when the release has one.
- The node group and every add-on in `status.inventory` is ready, which the
  `Ready` condition already aggregates.

A cluster is *failed* when it reports `phase: Failed`, or has not become
done within a timeout (proposed: 2 hours, the upper end of an EKS minor
upgrade with node replacement).

A stage is *healthy* when every cluster in it is done or was skipped
(held, section 11), and it has then stayed that way for
`strategy.soak`. A cluster that turns not-Ready during the soak restarts
the soak clock; it does not fail the release by itself.

| Limit | Default | Effect |
|---|---|---|
| `maxInProgress` | 5 | At most this many clusters of the stage are applying. Tier 0 stages use 1 regardless. |
| `maxFailed` | 1 | When more than this many clusters of a stage fail, the release goes to `Halted`. |
| `soak` | 24h | Time a healthy stage waits before the next one starts. |

`Halted` is a stop, not a rollback: clusters already applied stay as they
are, clusters in progress finish, no new cluster starts.

A worked example with the defaults. Stage beta / tier 2 has 12 automatic
clusters.

1. The rollout starts 5 of them (`maxInProgress`). As each finishes and is
   healthy, the next one starts, so 5 are in progress until all 12 have been
   started.
2. One cluster fails. That is within `maxFailed: 1`, so the other 11
   continue. If a second one fails, the release halts: nothing new starts
   anywhere until an operator looks and resumes.
3. All 12 are done (or the failed one is fixed or held). The 24-hour soak
   clock starts. If a cluster turns unhealthy during it, the clock restarts.
4. After 24 healthy hours, beta / tier 1 starts the same way. When the
   rollout reaches a tier 0 stage, it does one cluster at a time whatever
   `maxInProgress` says.

The four numbers are defaults and can be set per release. The owner has not
confirmed the starting values: Open question 1.

**A gate before a Kubernetes upgrade starts: removed API versions.** The
controller scans every running cluster for objects written with API
versions that the minor it moves to next removes
([deprecations](../deprecations.md)) and keeps the result in
`status.deprecations`. `deprecation.BlocksUpgrade(findings, target)` says
whether a cluster may move: it is true when any finding is removed at the
target, and gives the reason to show.

- An **automatic** cluster with such a finding is held: its Kubernetes
  upgrade does not start, and it counts as skipped for the stage (like a
  hold, section 11) until a later scan finds nothing removed. Platform
  fixes that do not change the Kubernetes version still reach it.
- A **manual** cluster is never held by it: the plan of the update that
  changes its version carries the findings as a warning, and the tenant
  decides.
- Deprecated-only findings never hold anything.

The scan, the status and the function exist; no upgrade flow calls the
function yet.

## 9. Pause, resume, rollback

| Action | What the operator does | What happens |
|---|---|---|
| Pause | `spec.paused: true` | No new cluster starts. Clusters in progress finish; stopping an EKS upgrade halfway is not possible. |
| Resume | `spec.paused: false` | The stage continues where it was. Soak time already served counts. |
| Resume after `Halted` | Fix the cause, then clear the halt (an annotation or `spec.resume` counter) | Failed clusters are retried first. |
| Roll back | Create a new release with the previous values | It runs through the same nine stages, covering only clusters that have the release being undone. |

**Proposed.** There is no "undo" button. A rollback is a new release, because
what can be undone depends on the content:

- **Renderer version:** reversible. A release with the lower
  `renderVersion` re-applies the old render. This requires the controller
  build to still contain the old renderer, which it does not today
  (`RenderVersion` is a constant of the build). Until renderers are
  versioned in code, a renderer rollback means deploying the previous
  controller build. This is stated as a limitation, not solved here.
- **Add-on versions:** usually reversible. EKS decides; a refused downgrade
  shows as a failed cluster.
- **Kubernetes minor:** not reversible. EKS cannot downgrade a control
  plane. A bad minor is halted and fixed forward. This is the reason a
  release with a minor move may ask for manual promotion (section 7).

## 10. How a cluster learns it should move

**Proposed.** The rollout controller writes the target into the cluster's
status; the cluster reconciler reads it when it renders.

```yaml
# Cluster
spec:
  upgradeMode: Automatic
  releaseChannel: Beta
  tier: Tier1
  controlPlane:
    kubernetesVersion: "1.33"    # where the cluster started
status:
  release:
    target: 2026-11-b            # written by the rollout controller
    applied: 2026-11-a           # written by the cluster reconciler when done
    startedAt: "2026-11-12T04:00:00Z"
    skipped: ""                  # reason, when the cluster is left out
  currentKubernetesVersion: "1.33"
```

Who writes what:

| Writer | Writes | Never writes |
|---|---|---|
| API server (tenant's request) | `Cluster.spec` | status |
| Rollout controller | `Release.status`, `Cluster.status.release.target`, `.startedAt`, `.skipped` | any spec |
| Cluster reconciler | the rest of `Cluster.status`, including `.release.applied` | any spec, `Release` |

Why status and not spec or an annotation:

- **Not spec.** The spec is the tenant's. If the rollout wrote
  `spec.controlPlane.kubernetesVersion`, the tenant's next apply of their
  own manifest would silently revert it, and the two would fight.
- **Not an annotation.** It is untyped, and anyone who can edit the object
  can edit it. Status has its own subresource and its own permission.
- **Status** keeps the existing rule intact: the API writes desired state,
  controllers reconcile and report.

**Decided.** How the two sources combine when the reconciler renders
depends on the upgrade mode:

| Setting | Manual cluster | Automatic cluster |
|---|---|---|
| Kubernetes version | The spec's. | The release's for the cluster; the spec's version was the starting point. |
| EKS add-on version | The spec's where the tenant named one; otherwise AWS's default for the cluster's Kubernetes version, as looked up at create or at the last change of the Kubernetes version or the add-ons. | AWS's default for the cluster's Kubernetes version (Open question 9 for when it is looked up again). |
| Component version | The component's spec where it names one; otherwise as resolved when it was enabled. | The release's. |
| Renderer version | Open question 3. | The release's. |

A consequence for automatic clusters: after the first release that moves
one, the spec's Kubernetes version is no longer what the cluster runs. The
running version is `status.currentKubernetesVersion`. **Proposed:** an
update to a version in the spec of an automatic cluster is refused with a
message naming the mode, so the spec never claims something the platform is
about to overrule; to choose a version the tenant switches to manual first
(Open question 6).

One thing in today's code matters for manual clusters: `resolveVersions`
writes the resolved versions into the stored spec, so "the tenant left it
empty" is lost after create. The stored *input* (the cluster as the client
sent it) is kept, and `pinnedAddons` already reads it to tell add-ons the
tenant gave a version from defaulted ones.

## 11. Pins, holds and end of support

**Decided.**

- A manual cluster is pinned by definition: nothing moves it.
- Only the platform operator may hold a release on a cluster. A hold keeps
  an *automatic* cluster out of releases while it is set.
- The platform supports the last three Kubernetes minors; older ones reach
  an end of support.

**Proposed.**

- **Hold.** Because only the operator may set it, the hold is not a field a
  tenant's update can change. It is `status.release.hold` (reason, who,
  since when), written through the `HoldCluster` admin RPC. The rollout
  controller records `status.release.skipped: held` and counts the cluster
  as skipped, so it does not block its stage. Held clusters are counted per
  stage on the `Release`, so holds do not go unnoticed.
- **End of support.** A cluster whose Kubernetes minor is not among the
  three in the newest completed release is *unsupported*. What the platform
  does then is Open question 4; the recommendation is to mark it and
  restrict it, never to force it.

**Deferred.** Maintenance windows (a cluster only moves inside a time
window), expiry of holds, and a tenant-set upper version limit for automatic
clusters.

## 12. Changing mode, channel or tier

**Today.** `spec.release_channel` and `spec.tier` cannot be changed after
creation: they are not among the update-mask paths of `UpdateCluster`
(`pkg/server/cluster/mutate.go`). There is no upgrade mode field yet.

**Proposed.** Allow the mode, channel and tier to be updated. For the mode
(Open question 5):

- Manual to automatic: allowed at any time. The cluster keeps its running
  versions and becomes a member of its (channel, tier) stage. If the newest
  release already completed that stage, the cluster joins it at once as a
  late member; it is not left behind.
- Automatic to manual: refused while a release is being applied to the
  cluster. Otherwise the spec's versions are set to what the cluster runs at
  that moment, so the pin is truthful.

For channel and tier, with these rules:

- The change takes effect for releases whose stage for the cluster's *new*
  (channel, tier) has not started.
- If a release is in progress and the new stage is already complete, the
  cluster joins the release immediately as a late member of that stage. It
  does not wait for the next release, or it would be left behind its
  neighbours.
- If the cluster is itself in progress (`status.release.target` differs
  from `.applied`), the update is refused with a precondition error until it
  is done.
- Moving to a more sensitive tier or a later channel never undoes a release
  the cluster already has.

## 13. Failure cases

**A tier-2 cluster fails mid-stage.** It counts toward `maxFailed`. With the
default of 1 the stage continues with the other clusters. A second failure
halts the release. The failed cluster keeps its target; the cluster
reconciler keeps retrying as it does for any failed apply. After the cause
is fixed, the operator resumes; the stage is healthy only when the failed
clusters are done or held.

**A cluster is created during a rollout.** A new manual cluster takes its
default versions from the newest completed release and is then left alone.
A new automatic cluster is created with the newest release whose stage for
its (channel, tier) is complete; that is its `status.release.applied` from
the start. If the release in progress
has not reached their stage, they join it when it does. A new cluster is
never created on a release that is `Halted`.

**A cluster is imported during a rollout.** An imported cluster is
read-only until takeover, so no release touches it. After takeover it is
manual (the default), with an unset channel and tier 0, and its versions are
the ones it was imported with: pinned. No release moves it. When its tenant
switches it to automatic it becomes a member of stable / tier 0, the last
stage, and the rules of section 12 apply. The import plan should say that
the cluster is manual and how to change that.

**A manual cluster falls out of support.** Section 11 and Open question 4.

**Two releases overlap.** A second release may start while the first is
still in beta or stable. Rule: a cluster has one target at a time, and a
later release never overtakes an earlier one on the same cluster. The second
release's stage waits for a cluster until the first release is applied
there or the first release is superseded. An operator can mark a release
`spec.supersedes: <name>`; clusters that have not started the older one then
go straight to the newer, which must contain everything the older one
changed. Without `supersedes`, releases queue.

## 14. Prerequisites and plan

Before any of this is built:

- The controller reconciles `Cluster` objects, and state lives on the
  controller kinds (status subresource in use).

Steps, each useful by itself:

| Step | Delivers |
|---|---|
| 1. `upgradeMode` on the contract and the kind, default manual; update path for mode, channel and tier | The fields exist and can be corrected after creation. Every cluster is manual; nothing moves. |
| 2. `status.release` on `Cluster`, written by the cluster reconciler | Every cluster reports which release it has. Useful for inventory alone. |
| 3. `Release` kind with the renderer version and the per-minor component lists; no stages yet | Newly enabled components take their versions from the release. Brings back a staged renderer rollout, which the removed `--render-target` / `--render-rollout` settings gave by hand. |
| 4. Support window: unsupported condition and end-of-support date in cluster status | Operators and tenants see which clusters are out of support. |
| 5. Stages for automatic clusters: health gates, `maxInProgress`, `maxFailed`, soak, automatic promotion | The nine-stage order runs by itself, for the renderer version. |
| 6. Pause, resume, hold, and the admin RPCs | The full control surface. |
| 7. Component versions in a release | After components exist ([components.md](components.md)). |
| 8. Kubernetes patch, then minor (`upgradeTo`), in a release, with optional manual promotion | The content that cannot be undone, last, once gates are trusted. |
| 9. Overlap rules, late joiners, mode switches during a release | The edge cases of sections 12 and 13. |

Steps 1 and 2 do not depend on the rest and can be done as soon as the
controller exists.

## 15. Comparison with existing systems

Only what informs a choice here.

- **GKE release channels.** A cluster subscribes to a channel (rapid,
  regular, stable) and versions become available per channel over time.
  The channel idea here is the same. The difference is that GKE's channel
  decides *which versions are offered*; ordering between a user's own
  clusters is a separate fleet-level feature (rollout sequencing with a
  soak time between groups). This design has both in one object, because
  the platform owns the clusters: the tier is the sequencing.
- **Argo CD ApplicationSet progressive sync.** It orders updates in steps
  selected by labels, with a maximum number updated at once, and waits for
  a step to be healthy before the next. `maxInProgress` and "healthy before
  the next stage" follow the same shape. It has no soak time and no notion
  of irreversible changes, which is why those are explicit here.

The conclusion drawn from both: label-selected stages plus a health gate is
a well-understood shape and enough; what must be added for this platform is
the soak, an optional manual gate for releases that cannot be undone, and
honesty about what cannot be undone.

## 16. Observability

- `kubectl get release`: name, phase, current stage, done/total of the
  stage, age.
- `kubectl get cvc`: the existing Channel and Tier columns, plus the upgrade
  mode, the applied release and whether the cluster is supported.
- `Release.status.stages`: per stage, totals of done, in progress, failed,
  skipped, with completion time.
- Events on the `Release` for stage start, stage healthy, halt (with the
  failing clusters named), and promotion awaited when it is manual.
- Events on the `Cluster` when it is given a target and when it is done.
- CLI: `release list`, `release get <name>`,
  `release pause|resume|promote`, `cluster hold|unhold`. The API exposes the
  same as read calls plus the admin RPCs of section 6.
- Metrics: clusters per (release, stage, state); time in stage.

## 17. Open questions

Each is explained in plain terms, with a recommendation.

1. **The default limits of a rollout.** A rollout does not update every
   cluster of a stage at once, and does not go straight on to the next
   stage. Four numbers control this: how many clusters are updated at the
   same time (proposed 5), how many may fail before everything stops
   (proposed 1), how long a finished stage must stay healthy before the next
   begins (proposed 24 hours), and tier 0 always one cluster at a time. The
   worked example in section 8 shows them in use. *Recommendation:* start
   with these values and let each release override them.

2. **Renderer rollback.** The renderer is the code that turns a cluster's
   spec into cloud objects, and it has a version number. If a new renderer
   turns out to be wrong, going back means producing the old objects again,
   and only the old code can do that: the running server contains one
   renderer, the newest. So today "undo a renderer change" means running the
   previous server build. The alternative is to keep several renderer
   versions inside one build, which is real work and not needed yet.
   *Recommendation:* accept "deploy the previous build" for now and write it
   in the operator documentation.

3. **Do renderer changes reach manual clusters?** A manual cluster's tenant
   chooses Kubernetes, add-on and component versions. The renderer version
   is not one of those; it is the platform's own code. If manual clusters
   never received renderer changes, old and new code would have to live side
   by side forever. *Recommendation:* yes, renderer changes reach manual
   clusters, last, after stable / tier 0, and they change no version the
   tenant chose.

4. **A manual cluster passes end of support.** Its Kubernetes minor is no
   longer one of the three the platform supports. *Recommendation:* the
   cluster's status says it is unsupported and since when; enabling new
   components on it is refused; components it already has keep the last
   versions listed for its minor; the platform never upgrades it by force.
   One limit is outside the platform: AWS itself upgrades an EKS cluster
   when its support ends. The contract already carries the setting that
   decides when (`spec.aws.upgrade_policy.support_type`, `AwsSupportType` in
   `idl/cloud/v1/aws.proto`): `STANDARD` is upgraded by AWS at the end of
   standard support, `EXTENDED` enters paid extended support first. A manual
   cluster that is to stay on an old version needs `EXTENDED`, and the
   status should show the date AWS will act.

5. **Switching between manual and automatic.** *Recommendation:* a tenant
   may switch at any time, with the two rules of section 12. Becoming
   automatic makes the cluster a member of its stage (late, if that stage
   already ran). Becoming manual is refused while a release is being applied
   to the cluster, and writes the running versions into the spec.

6. **A tenant updates a version on an automatic cluster.** The platform is
   about to choose that version itself, so the two would disagree.
   *Recommendation:* refuse the update and say why; the tenant switches to
   manual to choose versions.

7. **Manual promotion for some releases.** Promotion between channels is
   automatic by default, as decided. *Recommendation:* keep
   `promotion: Manual` available per release, for one that contains a
   Kubernetes minor move.

8. **Does takeover make an imported cluster manual or automatic?** This
   document reads the decision as manual (section 2). Confirm.

9. **When do an automatic cluster's add-ons follow AWS?** Add-on versions
   come from AWS, as decided. Today the platform asks AWS only when a
   cluster is created and when its Kubernetes version or add-ons are
   changed, and stores the answer. So an automatic cluster's add-ons move at
   a Kubernetes upgrade and at no other time; if AWS marks a newer add-on
   version as the default for the same Kubernetes version, the cluster does
   not get it. The choices are: leave it so; look the versions up again
   when a release reaches the cluster (the release then carries no add-on
   versions, it is only the moment of the lookup, and the lookup follows the
   stage order); or look them up continuously, which would move clusters
   outside any stage order. *Recommendation:* look them up again when a
   release reaches the cluster, so add-on changes go tier by tier like
   everything else. On a manual cluster an add-on version the tenant named
   is never changed; on an automatic cluster it is the starting point, as
   decided for every version.
