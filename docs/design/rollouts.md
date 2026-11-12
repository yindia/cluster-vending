# Rolling releases out by channel and tier

Status: design, not implemented. Nothing in this document exists in code
except where a file or identifier is named.

Each section marks its statements as **Decided** (by the project owner, not
reopened here), **Proposed** (this document's recommendation) or **Open**
(needs an answer; collected in section 12).

## 1. Summary

A *release* is a named set of platform-chosen versions. It moves through the
clusters in nine stages: the three channels in order, and within each
channel the three tiers from least to most sensitive. A stage starts only
when the previous one is healthy and has soaked. The rollout writes each
cluster's target into the cluster's *status*, never into its spec, so a
tenant's own choices are not overwritten. A release that cannot be undone on
the cloud side (a Kubernetes minor upgrade) is stopped and fixed forward,
not rolled back.

## 2. What is decided

**Decided.**

- A cluster has a release channel (alpha, beta, stable) and a tier (tier 0,
  tier 1, tier 2). Tier 0 is the most sensitive.
- Both are on the contract (`ClusterSpec.release_channel` and
  `ClusterSpec.tier` in `idl/cloud/v1/cluster.proto`) and on the
  controller's `Cluster` kind (`spec.releaseChannel`, `spec.tier` in
  `controller/api/v1alpha1`). Today they are stored and returned and nothing
  acts on them.
- Channels receive a release in the order alpha, beta, stable. Within a
  channel a release reaches tier 2 first, then tier 1, then tier 0.
- An unset tier is tier 0. `corecluster.ResolveSpec` stores a created
  cluster as `CLUSTER_TIER_0`.
- Rollouts are built at the end of the project, after the controller
  reconciles clusters.

**Proposed.** An unset channel is treated as stable. With an unset tier
being tier 0, a cluster nobody classified is then in the last stage.

## 3. What exists today

| Mechanism | Where | What it does now |
|---|---|---|
| Renderer version | `crossplaneaws.RenderVersion` (`pkg/crossplane/aws/aws.go`), stored per row as `render_version` | Identifies what the renderers produce. A row applied with an older renderer is behind. |
| Renderer rollout | `reconcile.Deps.RenderTarget`, `RenderRollout`, `Deps.RenderBehind` (`pkg/reconcile/reconcile.go`); flags `--render-target`, `--render-rollout` | The background loop re-applies rows whose `render_version` is below the target, limited to the listed tenant, network or cluster IDs. This is a rollout by hand-maintained list. |
| Version resolution | `resolveVersions`, `reresolveVersions`, `pinnedAddons` (`pkg/server/cluster/versions.go`); `ResolveEKSVersions` (`pkg/cloud/aws/eks.go`) | At create and update, an empty Kubernetes version becomes the EKS default and an add-on without a version becomes the one EKS recommends. The resolved versions are stored in the spec. Add-ons the client gave a version are pinned. |
| Upgrade gating | `Reconciler.GateUpgrade`, `CheckVersionStep` (`pkg/reconcile/cluster/upgrade.go`) | During a Kubernetes upgrade the node groups and add-ons stay at the running versions until the control plane reports the target. `CheckVersionStep` checks the step between the stored and the requested version. |
| Default add-ons | `crossplaneaws.DefaultAddons` (`pkg/crossplane/aws/addons.go`) | The add-ons every cluster gets: vpc-cni, coredns, kube-proxy, eks-pod-identity-agent, aws-ebs-csi-driver. |
| Ordered apply | `sequence.Engine` (`pkg/crossplane/sequence`) | Applies one owner's objects in order. It orders objects inside one cluster, not clusters. |
| Kinds and ownership | `controller/api/v1alpha1`: CloudAccount owns Network owns Cluster | Status subresource with `observedGeneration`, `phase`, `conditions` (Ready, Synced), observed versions and an inventory. |

Two facts follow from the code and shape the design:

- The platform installs nothing on a cluster except EKS add-ons. There are
  no platform components (no Helm charts, no agents) to release.
- The node AMI is not chosen by the platform. A node group renders an AMI
  type and a Kubernetes version; EKS picks the AMI release. There is no
  field to pin an AMI release today.

## 4. What a release is

**Proposed.** A release can carry four things. Each is optional; a release
changes only what it names.

| Content | Reversible on the cloud side | Exists today |
|---|---|---|
| Renderer version (`RenderVersion`) | Yes: re-apply with the previous renderer. | Yes |
| Default add-on versions, per Kubernetes minor | Mostly: EKS accepts an older add-on version in most cases, not all. | Resolved once at create/update; not re-resolved later |
| Default Kubernetes patch and minor | Patch: no, but low risk. Minor: no. | Minor is the client's or the EKS default at create |
| Node AMI release | Yes: a node group can roll to an older release. | No field today; **deferred** |

Platform components on clusters would be a fifth row. None exist, so the
release has no such field. It is added when the first component is.

A release is an object, not configuration, for three reasons:

- It has state that changes while it runs (which stage, which clusters are
  done, why it stopped). Configuration has nowhere to hold that.
- Two releases can exist at once (section 10). Flags describe one.
- Pausing and resuming must not need a server restart. `--render-target`
  does today.

`Release` is cluster-scoped: it is the platform operator's object and spans
every tenant namespace.

```yaml
apiVersion: cloud.cluster-vending.io/v1alpha1
kind: Release
metadata:
  name: 2026-11-b            # operator-chosen, sortable
spec:
  # What changes. Omitted entries are left as each cluster has them.
  renderVersion: 2
  kubernetes:
    # For clusters on this minor, the patch-level default and add-ons.
    - minor: "1.33"
      addons:
        vpc-cni: v1.20.4-eksbuild.1
        coredns: v1.12.3-eksbuild.1
    # A minor move is explicit: from 1.32 to 1.33.
    - minor: "1.32"
      upgradeTo: "1.33"
  # How it moves.
  strategy:
    maxInProgress: 5         # clusters applying at once, per stage
    maxFailed: 1             # failures in a stage before the rollout stops
    soak: 24h                # healthy time before the next stage
    promotion: Manual        # between channels; Automatic also allowed
  paused: false
  # Promotion to the next channel, when promotion is Manual.
  approvedChannels: [Alpha]
status:
  phase: Progressing          # Pending, Progressing, Soaking, AwaitingApproval, Halted, Complete
  stage: {channel: Alpha, tier: Tier1}
  stages:
    - {channel: Alpha, tier: Tier2, total: 12, done: 12, failed: 0, completedAt: "2026-11-12T03:10:00Z"}
    - {channel: Alpha, tier: Tier1, total: 4, done: 1, failed: 0, inProgress: 3}
  conditions:
    - type: Healthy
      status: "True"
```

| Field | Meaning |
|---|---|
| `spec.renderVersion` | Renderer version clusters are brought to. Must not exceed the controller build's `RenderVersion`. |
| `spec.kubernetes[].minor` | The clusters this entry applies to, by the minor they run. |
| `spec.kubernetes[].addons` | Add-on versions for clusters on that minor. Applies only to add-ons the tenant did not pin. |
| `spec.kubernetes[].upgradeTo` | Move clusters on that minor to the next minor. One step only, as `CheckVersionStep` requires. |
| `spec.strategy.*` | Gates, section 6. |
| `spec.paused` | Stops starting new clusters. Section 7. |
| `spec.approvedChannels` | Channels the operator has approved; only read when promotion is Manual. |
| `status.*` | Written by the rollout controller only. |

## 5. The order

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

**Proposed.** Moving from one tier to the next inside a channel is
automatic, after the gates in section 6. Moving from one channel to the next
(after stages 3 and 6) is manual: the operator adds the channel to
`spec.approvedChannels`. The reason is that a channel boundary is where a
release meets clusters with a different risk level, and it is the natural
point for a person to look.

The alternative is fully automatic promotion with a longer soak at channel
boundaries (`promotion: Automatic`). It needs less attention and is the
right choice once the health gates have been trusted for a while. The field
exists so the choice is per release.

## 6. Gates between stages

**Proposed.** A cluster is *done* for a release when all of these hold, read
from the `Cluster` status the controller already writes:

- `status.release.applied` names this release (section 8).
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
(pinned, section 9), and it has then stayed that way for
`strategy.soak`. A cluster that turns not-Ready during the soak restarts
the soak clock; it does not fail the release by itself.

| Limit | Default | Effect |
|---|---|---|
| `maxInProgress` | 5 | At most this many clusters of the stage are applying. Tier 0 stages use 1 regardless. |
| `maxFailed` | 1 | When more than this many clusters of a stage fail, the release goes to `Halted`. |
| `soak` | 24h | Time a healthy stage waits before the next one starts. |

`Halted` is a stop, not a rollback: clusters already applied stay as they
are, clusters in progress finish, no new cluster starts.

## 7. Pause, resume, rollback

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
  plane. A bad minor is halted and fixed forward. This is the reason minor
  upgrades get the manual channel gate by default.

## 8. How a cluster learns it should move

**Proposed.** The rollout controller writes the target into the cluster's
status; the cluster reconciler reads it when it renders.

```yaml
# Cluster
spec:
  releaseChannel: Beta
  tier: Tier1
  controlPlane:
    kubernetesVersion: ""        # empty: the platform chooses
status:
  release:
    target: 2026-11-b            # written by the rollout controller
    applied: 2026-11-a           # written by the cluster reconciler when done
    startedAt: "2026-11-12T04:00:00Z"
    skipped: ""                  # reason, when a pin keeps the cluster out
  currentKubernetesVersion: "1.32"
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

How the two sources combine when the reconciler renders:

| Setting | Tenant set it in spec | Tenant left it empty |
|---|---|---|
| Kubernetes version | Spec wins. The release does not move the cluster (it is pinned). | The release's value for the cluster's minor. |
| Add-on version | Spec wins for that add-on. | The release's value. |
| Renderer version | Not a tenant setting. | The release's value. |

This changes one thing that exists: today `resolveVersions` writes the
resolved versions into the stored spec, so "the tenant left it empty" is
lost after create. The stored *input* (the cluster as the client sent it) is
kept, and `pinnedAddons` already reads it to tell pinned add-ons from
defaulted ones. The same test extends to the Kubernetes version.

## 9. Pins and maintenance windows

**Proposed, minimum.**

- **Pin by spec.** A cluster whose spec names a Kubernetes version or an
  add-on version is pinned for that setting, as in section 8. No new field.
- **Hold.** `spec.releaseHold: true` on the cluster keeps it out of every
  release. The rollout controller records `status.release.skipped: held`
  and counts the cluster as skipped, so it does not block its stage.
- A held or pinned cluster is reported on the `Release` (count of skipped
  clusters per stage), so pins do not go unnoticed.

**Deferred.** Maintenance windows (a cluster only moves inside a time
window), per-cluster maximum version, and expiry of holds. They need a
decision on who may set them and for how long, and none is required for the
order the owner asked for.

## 10. Changing channel or tier

**Today.** `spec.release_channel` and `spec.tier` cannot be changed after
creation: they are not among the update-mask paths of `UpdateCluster`
(`pkg/server/cluster/mutate.go`).

**Proposed.** Allow both to be updated, with these rules:

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

## 11. Failure cases

**A tier-2 cluster fails mid-stage.** It counts toward `maxFailed`. With the
default of 1 the stage continues with the other clusters. A second failure
halts the release. The failed cluster keeps its target; the cluster
reconciler keeps retrying as it does for any failed apply. After the cause
is fixed, the operator resumes; the stage is healthy only when the failed
clusters are done or held.

**A cluster is created during a rollout.** New clusters are created with the
newest release whose stage for their (channel, tier) is complete; that is
their `status.release.applied` from the start. If the release in progress
has not reached their stage, they join it when it does. A new cluster is
never created on a release that is `Halted`.

**A cluster is imported during a rollout.** An imported cluster has an unset
tier (tier 0) and an unset channel (stable, as proposed): the last stage. It
is also read-only until takeover, so the rollout skips it with
`skipped: imported` while its phase is `Imported`. After takeover it is a
normal member of stable / tier 0. Its `applied` release is empty; the first
release that reaches it brings it to that release's values, subject to pins.
Because an imported cluster's spec is derived from what exists, most of its
versions are explicit and therefore pinned; the import plan should say so.

**Two releases overlap.** A second release may start while the first is
still in beta or stable. Rule: a cluster has one target at a time, and a
later release never overtakes an earlier one on the same cluster. The second
release's stage waits for a cluster until the first release is applied
there or the first release is superseded. An operator can mark a release
`spec.supersedes: <name>`; clusters that have not started the older one then
go straight to the newer, which must contain everything the older one
changed. Without `supersedes`, releases queue.

## 12. Comparison with existing systems

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
the soak, the manual channel gate, and honesty about what cannot be undone.

## 13. Prerequisites and plan

Before any of this is built:

- The controller reconciles `Cluster` objects, and state lives on the
  controller kinds (status subresource in use).
- The cluster's stored input distinguishes "tenant chose this version" from
  "platform chose it" for the Kubernetes version, as it already does for
  add-ons.

Steps, each useful by itself:

| Step | Delivers |
|---|---|
| 1. Update path for `releaseChannel` and `tier` | The fields can be corrected after creation. No rollout yet. |
| 2. `status.release` on `Cluster`, written by the cluster reconciler | Every cluster reports which release it has. Useful for inventory alone. |
| 3. `Release` kind, renderer version only, manual stage advance | Replaces `--render-target` / `--render-rollout`: the operator advances stages by hand and sees progress. |
| 4. Health gates, `maxInProgress`, `maxFailed`, soak, automatic tier advance | The nine-stage order runs by itself inside a channel. |
| 5. Channel approval and pause/resume | The full control surface for renderer releases. |
| 6. Add-on versions in a release | The first content that changes tenant workloads. |
| 7. Kubernetes patch, then minor, in a release | The irreversible content, last, once gates are trusted. |
| 8. Holds, overlap rules, late joiners | The edge cases of sections 9 to 11. |

Steps 1 and 2 do not depend on the rest and can be done as soon as the
controller exists.

## 14. Observability

- `kubectl get release`: name, phase, current stage, done/total of the
  stage, age.
- `kubectl get cvc`: the existing Channel and Tier columns, plus the applied
  release.
- `Release.status.stages`: per stage, totals of done, in progress, failed,
  skipped, with completion time.
- Events on the `Release` for stage start, stage healthy, halt (with the
  failing clusters named), approval awaited.
- Events on the `Cluster` when it is given a target and when it is done.
- CLI: `release list`, `release get <name>`, `release pause|resume|approve`.
  The API exposes the same as read calls plus those three writes.
- Metrics: clusters per (release, stage, state); time in stage.

## 15. Open questions

1. **What does an unset channel mean?** Recommendation: stable.
2. **Is promotion between channels manual by default?** Recommendation:
   yes, with `promotion: Automatic` available per release.
3. **Does a version named in the tenant's spec pin the cluster?**
   Recommendation: yes. The alternative, a platform that overrides tenant
   versions, needs a policy for who wins and is harder to explain.
4. **Default soak and limits** (24h, 5 in progress, 1 failure, tier 0 one at
   a time). Recommendation: start there and make them per release.
5. **Are node AMI releases part of a release?** Recommendation: defer until
   the node group has a field to pin an AMI release; today EKS chooses.
6. **Renderer rollback.** It needs old renderers kept in the build, or a
   deploy of the previous build. Recommendation: accept the deploy for now
   and state it in the operator documentation.
7. **Who may set `releaseHold`?** Recommendation: the platform operator
   only, until maintenance windows are designed.
8. **Should imported clusters join releases automatically after takeover?**
   Recommendation: yes, as stable / tier 0, with their explicit versions
   pinned.
