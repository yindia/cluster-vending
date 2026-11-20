# PROP-01: Admin-level configuration of components, node groups, Karpenter and releases

| | |
|---|---|
| Status | Proposed |
| Author | `<author>` |
| Date | `<YYYY-MM-DD>` |
| Reviewers | `<reviewers>` |
| Related | [architecture.md](../architecture.md): Platform components, Node layout, Releases, channels and rollouts, Freeze and pause, CR definitions, Configuration |

## 1. Summary

Today a platform admin changes the platform's behaviour in four different ways: by
shipping a new build (component definitions, `release.yaml`), by editing custom
resources with `kubectl` (`spec.disabled`, `ReleaseChannel.spec`), by editing the
`nodeGroups` section of two config files and restarting two processes, or not at all
(release ordering, release authoring). This proposal makes every one of these an
admin-owned, validated, audited setting with one surface (API, CLI, console):

- **Component definitions**: each definition is split into a *shipped base* (owned by
  the build) and an *admin overlay* (owned by the admin: enabled, sequence, extra
  requirements, values overlay, install timeout). A build never overwrites the overlay,
  and an overlay never stops a build from updating the base. Disable, enable, reorder
  and "reset to shipped" are single operations with a preview of the affected clusters.
- **Default node group and Karpenter policy**: move from the `nodeGroups` config section
  into a cluster-scoped singleton `PlatformConfig` that the server and the controller
  both read. The config file seeds it once.
- **Release channels and the release process**: releases get a draft -> validate ->
  publish lifecycle, an explicit order (`spec.serial`), and admin RPCs to author,
  publish, withdraw and promote. Channel settings (promotion, soak, tier policy, pause)
  become editable through the API.
- Every change that affects running clusters goes through the existing channel rollout
  (tiers, `maxInProgress`, `maxFailed`, soak, maintenance windows, freeze).

The API changes are additive. The CRD changes add fields and keep existing objects valid.

## 2. Motivation and problems today

### 2.1 Component definitions belong to the binary

`ComponentDefinition` objects are created by the controller's catalog sync
(`pkg/catalog/catalog.go`, `Sync`) from `pkg/catalog/definitions`. Sync records
`cloud.cluster-vending.io/shipped-hash` (the SHA-256 of the spec, ignoring `disabled`
and a default `sequence`) and decides per definition:

| State | What a build does |
|---|---|
| Shipped, spec hash equal to the recorded hash | Replaces the spec with the build's |
| Shipped, spec hash differs (any hand edit) | Leaves it, logs "edited", **for ever** |
| Not labelled, different from the build | Leaves it |

Any edit to any field counts as "the platform team's edit", including a harmless one.
In the incident that prompted this proposal, someone edited `spec.description` on the
`karpenter` definition. From then on, every later build skipped that definition,
including a build that fixed Karpenter's IAM policy, so the definition kept the old
policy. Nothing on the object or in the API said so; it was only logged at controller
start. The only way back is a manual `kubectl` replace with the build's spec.

Other gaps:

- **Disable** is `kubectl patch componentdefinition <name> --type merge -p
  '{"spec":{"disabled":true}}'`. There is no RPC, no preview of the clusters that lose
  the component, and no check that the component is not Karpenter. A disabled platform
  component is removed from *every* cluster on its next pass: no tiers, no soak.
- **Order** is `spec.sequence` (default 100, Karpenter 0). It can only be set in YAML.
  Nothing checks that a component's `requires` point to the same or a lower sequence,
  or that nothing is moved before Karpenter.
- **Values**: changing a platform component's locked values means editing
  `spec.platformValues`, which is a hand edit (see above) and stops every later build
  update of that definition.
- **Custom components**: possible with `kubectl apply`, but with no validation before
  apply (the `Valid` condition is set afterwards) and no view of where they are used.

### 2.2 Node group and Karpenter settings live in config files

The `nodeGroups` section (`pkg/config/node_groups.go`) holds the system node group
(`instanceType`, `minSize` >= 2, `maxSize`, `capacityType`, `diskSizeGiB`) and the
Karpenter allowlist (`instanceFamilies`, `instanceTypes`, `defaults.architectures`,
`defaults.capacityTypes`). Problems:

- The **server validates** and the **controller renders** from the same section of two
  separate config files. The architecture document says "the two must get the same file
  or values". Nothing enforces this; a difference shows up as a request the server
  accepts and the controller renders differently.
- A change needs a **restart of both processes**, and it reaches every cluster at once
  (`minSize`/`maxSize` apply in place) with no tier order.
- **Per-environment drift**: each management cluster's values are in its deployment
  manifests, not visible through the API.
- **No sizing check at change time**: `pkg/catalog/system_group_sizing_test.go` fails
  the *build* when the shipped requests exceed 85 % of the default group (2 x `t3.medium`).
  An admin who sets `instanceType: t3.small` gets no warning.
- **No overrides**: one allowlist for every tenant. A tenant that needs GPU families
  gets them for everyone, or not at all.
- **Limits** such as the 20-group cap, and the CPU/memory limit ceilings, are
  hard-coded or absent.

### 2.3 Releases come from the build

- `pkg/catalog/release.yaml` is the one place versions are set. The controller turns it
  into a `Release` named `shipped-<hash>`, and `alpha` is pinned to it. An admin who
  wants a new chart version, or a new Kubernetes minor, needs a new build.
- Operator-published releases (`YYYY-MM-<letter>`) are allowed by the CRD, but there is
  no RPC or CLI to create one, and no validation before apply.
- **Release order is the creation time.** `ListReleases`
  (`pkg/server/admin/releases.go`) sorts by `CreationTimestamp`, and the release move's
  "is the target newer" check (`notNewer`, `pkg/reconcile/cluster/release_move.go`)
  compares creation timestamps. Re-creating an object (restore from backup, migration
  to a new management cluster) changes the order.
- **Channels** (`ReleaseChannel`) are edited with `kubectl`. The API has read RPCs only
  (`ListReleaseChannels`, `GetReleaseChannel`). Promotion `Manual` exists in the type,
  but there is no supported way to promote.
- There is no rollback concept: "There is no rollback" (release moves), and a withdrawn
  release cannot be un-withdrawn.

### 2.4 No single admin surface or audit

`AdminService` has `Freeze`, `Unfreeze`, `GetFreeze` and four release/channel reads. The
CLI's `admin` group says "Releases and channels are read-only". The console's
releases page (`ui/src/components/console/releases-page.tsx`) is read-only, and the
components page is tenant-facing. Changes made with `kubectl` record no "who" or "why"
beyond the Kubernetes API server's audit log.

## 3. Goals and non-goals

### Goals

1. An admin can disable, enable, reorder, re-value and reset any component definition
   through the API, CLI and console, with validation and a preview of affected clusters.
2. A build never overwrites admin settings, and admin settings never stop a build from
   updating what the build owns.
3. The system node group and the Karpenter policy are one cluster-scoped object that the
   server and the controller both read. Changes need no restart.
4. An admin can author, validate, publish, withdraw and promote a release without a build.
   Releases have an explicit order.
5. Channel promotion, soak and tier policy are editable through the API.
6. Every change that affects running clusters rolls out by channel and tier, respects
   the platform freeze, and is visible in the existing rollout views.
7. Every admin write records who, when and why.

### Non-goals

- Custom channel names beyond `alpha`, `beta`, `stable` (see 4.4.1).
- Several supported versions of a component per Kubernetes minor (deferred by earlier
  decision; the design keeps room for it).
- Tenant-chosen component versions.
- Per-cluster component overrides by the admin (a cluster `ReleaseHold` already pins a
  release).
- A new storage backend. The custom resources stay the store.
- GitOps tooling. Objects remain plain CRs, so Git-driven management stays possible.

## 4. Proposed design

### 4.1 Configuration model

| Setting | Today | Proposed owner | Kind |
|---|---|---|---|
| Component chart, IAM, schema, base values | Build (`pkg/catalog/definitions`) | Build | `ComponentDefinition.spec` (base) |
| Component enabled, sequence, extra requires, values overlay, timeout | `kubectl` edit (counts as "edited") | Admin | `ComponentDefinition.spec.overlay` (new) |
| Custom component | `kubectl apply` | Admin | `ComponentDefinition` without the shipped label |
| System node group | `nodeGroups.system` in two config files | Admin | `PlatformConfig.spec.nodeGroups.system` (new kind) |
| Karpenter allowlist, defaults, limits, AMI policy | `nodeGroups.karpenter` in two config files | Admin | `PlatformConfig.spec.nodeGroups.karpenter` |
| Release content | Build (`release.yaml`) | Build (shipped) or admin (published) | `Release` with `spec.state` and `spec.serial` (new fields) |
| Channel promotion, soak, policy, pause | `kubectl` edit | Admin | `ReleaseChannel.spec` (unchanged shape) |
| Freeze | `admin freeze` | Admin | `ReleaseHold` (unchanged) |

Config-file values become **bootstrap defaults only**: the controller creates
`PlatformConfig` from them when it does not exist, and ignores them afterwards (with a
warning at startup when they differ from the object).

### 4.2 Component definitions

#### 4.2.1 Ownership: base and overlay

`ComponentDefinitionSpec` keeps every existing field as the **base**. A new optional
`spec.overlay` holds what the admin owns:

```yaml
apiVersion: cloud.cluster-vending.io/v1alpha1
kind: ComponentDefinition
metadata:
  name: external-secrets
  labels:
    cloud.cluster-vending.io/shipped: "true"
  annotations:
    cloud.cluster-vending.io/shipped-hash: 3f1c...   # hash of the base only
    cloud.cluster-vending.io/changed-by: alice@example.com
    cloud.cluster-vending.io/change-reason: "move after kyverno; raise timeout"
spec:
  # --- base: written by the catalog sync, replaced by every build ---
  description: External Secrets Operator
  management: Platform
  namespace: external-secrets
  install: { helm: { repository: ..., chart: external-secrets }, timeout: 5m }
  sequence: 100
  # --- overlay: written by the admin, never touched by a build ---
  overlay:
    enabled: true            # unset = enabled; false = disabled (replaces spec.disabled)
    sequence: 150            # unset = base sequence
    requires: [kyverno]      # added to the base's requires, never removes one
    platformValues:          # deep-merged over base platformValues, locked for tenants
      resources: { requests: { cpu: 50m } }
    installTimeout: 10m      # unset = base install.timeout
status:
  conditions:
    - type: Valid
    - type: Synced           # new: base equals the build's (or the reason it does not)
  effective:                 # new: what clusters get
    enabled: true
    sequence: 150
    requires: [kyverno]
    installTimeout: 10m
    baseHash: 3f1c...
    overlayHash: 9ab2...
  usage:                     # new: bounded counts
    clusters: 42
    components: 0
```

Rules:

- `SpecHash` covers the base only (excluding `overlay` and the legacy `disabled`). An
  overlay change is never an edit.
- The catalog sync **always** replaces the base of a shipped definition with the build's,
  and keeps `overlay` as it is. The "edited, left for ever" state goes away.
- A hand edit of a base field is reverted on the next sync, with the event
  `ShippedFieldReverted` naming the fields. An escape hatch exists for emergencies: the
  annotation `cloud.cluster-vending.io/sync: Detached` makes the sync leave the base, and
  sets `Synced=False` with reason `Detached` on the object. It is visible in every admin
  view and in a metric, so it cannot be forgotten silently as in the incident.
- `spec.disabled` stays readable for compatibility. The effective `enabled` is
  `overlay.enabled` when set, else `!spec.disabled`. New writes go to `overlay.enabled`.
- **Reset to shipped** clears `overlay` (and `Detached`). It is an update that sets the
  overlay to empty.
- Custom definitions (not labelled shipped) have no base/overlay distinction: the admin
  owns the whole spec. They may still use `overlay` for uniformity.

The effective definition is computed in one pure function in `pkg/component`
(`Effective(def) EffectiveDefinition`). The installer, the deletion stages, the release
move, the tenant API and the admin API use only this function.

#### 4.2.2 Disable and enable

| Kind | Disable effect | Enable effect |
|---|---|---|
| Platform | Uninstalled from each cluster in rollout order (4.5), highest sequence first among the disabled ones. Definitions that require it are disabled with it **only** when the request says so (`cascade`), otherwise the request is refused, listing them. | Installed on each cluster in rollout order. |
| Tenant | New `EnableComponent` calls are refused. Existing `Component` objects stay installed and are marked `Degraded` with reason `DefinitionDisabled`; the tenant can still disable them. | Enabling works again. |

Refused (`FAILED_PRECONDITION`):

- Disabling `karpenter` (the node groups and the deletion stages depend on it).
- Disabling a component that an enabled component requires, without `cascade`.
- Disabling `capsule` while any cluster has `naas: true` and sub-tenants (the existing
  `SubTenantSafety` rule, now checked at admin time).

Uninstall uses the existing Helm path: one operation per cluster at a time, backoff,
stuck-release recovery, and its IAM role removed after its Helm releases. A disable
does **not** lift a pause set by a person or a freeze.

#### 4.2.3 Reorder (sequence)

The admin sets `overlay.sequence`. Validation, all at write time, all reported together:

| Check | Rule |
|---|---|
| Karpenter first | `karpenter` keeps sequence 0, and no other component may use 0 or less. |
| Requires vs sequence | For each effective `requires` edge `A -> B`, `seq(B) <= seq(A)`. A component cannot be ordered before something it requires. |
| Cycles | The effective `requires` graph (base + overlay additions, across bundles) has no cycle. The existing check in `pkg/component/management.go` runs on the effective graph. |
| Bundles | Members of a bundle (`internal: true`) take the bundle's sequence; setting a member's sequence is refused. |
| Range | 1 to 10000. |

Effect on running clusters: no component is reinstalled because its order changed. The
new order applies to new clusters, to future release moves and upgrades (one component
at a time, in install order), and to deletion stage 3 (highest sequence first). The
preview says so.

#### 4.2.4 Values overlay and timeout

`overlay.platformValues` is deep-merged over the base `platformValues`, and under the
release's per-minor `values` (the release wins, as today). The merged result must still
validate against `valuesSchema` for tenant components, and every tenant `Component` on
the definition is re-validated: a values overlay that makes an existing tenant's values
invalid is refused, listing the components. A change of the merged values changes the
Helm digest, and rolls out like any catalog change (4.5).

`overlay.installTimeout` (1 m to 60 m) replaces `install.timeout`. The stuck-release
threshold follows it (timeout + 2 m).

For platform components, a values overlay that changes resource requests is checked
against the system node group sizing (4.3.2).

#### 4.2.5 Custom components

`CreateComponentDefinition` takes a full spec, runs the same checks the controller runs
for `Valid` (bundles, requirements, cloud rules, chart rules, schema of `userValues`)
**before** storing it, and supports `validate_only`. A custom platform component is
installed on every cluster through the rollout, like an enable. A custom definition can
be deleted when nothing uses it (the existing `definition-in-use` finalizer). A shipped
definition cannot be deleted through the API: disable it.

#### 4.2.6 Migration of existing definitions

On the first sync of a build with this feature, for each shipped definition marked
"edited" today:

1. Fields that map to the overlay (`disabled`, `sequence`, `platformValues` differences,
   `install.timeout`) are moved into `overlay`.
2. `description` differences are dropped (informational only; the build's description wins).
3. Any other difference (chart source, IAM policy, schema, requires removed) is **not**
   applied silently: the definition gets `sync: Detached`, `Synced=False` with a message
   listing the fields, and an event. The admin either resets it or keeps it detached.
4. The shipped hash is recomputed over the base.

This brings the `karpenter` definition from the incident back under the build at once.

### 4.3 Default node group and Karpenter: `PlatformConfig`

#### 4.3.1 The object

Cluster-scoped, singleton, named `platform` (CEL rule on the name), short name `cvpc`.

```yaml
apiVersion: cloud.cluster-vending.io/v1alpha1
kind: PlatformConfig
metadata:
  name: platform
spec:
  nodeGroups:
    system:
      instanceType: t3.medium      # new clusters only
      capacityType: OnDemand       # new clusters only
      diskSizeGiB: 0               # new clusters only; 0 = EKS default
      minSize: 2                   # >= 2; existing clusters, in place, by rollout
      maxSize: 6                   # >= minSize; existing clusters, in place, by rollout
    karpenter:
      allowlist:
        instanceFamilies: [t3, m7i, c7i]   # empty = no restriction in this dimension
        instanceTypes: [g5.xlarge]         # extra types besides the families
        architectures: [Amd64, Arm64]      # allowed (new)
        capacityTypes: [OnDemand, Spot]    # allowed (new)
      defaults:
        architectures: [Amd64]
        capacityTypes: [OnDemand]
      limits:
        maxGroupsPerCluster: 20            # <= 20 (the CRD's MaxItems)
        maxCPUPerGroup: "1000"             # ceiling for NodeGroupSpec.limits.cpu; unset = none
        maxMemoryGiBPerGroup: 4000
      amiPolicy: Release                   # Release (nodeAmiPin of the release) | Latest
status:
  observedGeneration: 7
  conditions:
    - type: Valid                          # all checks below passed
    - type: Sizing                         # result of the sizing check (warning detail)
  sizing:
    instanceType: t3.medium
    cpuPercent: 71
    memoryPercent: 64
    podsPercent: 82
  outsideAllowlist:                        # existing tenant groups the policy no longer allows
    count: 3
    clusters: [tenant-a/prod]              # at most 20
```

Both processes read it through an informer and apply a change on the next pass. No restart.

#### 4.3.2 Validation at admin time

All errors are reported together (as the config loader does today):

- `minSize >= 2`, `maxSize >= minSize`, `maxSize <= 1000`; the instance type, family and
  enum patterns from `pkg/config/node_groups.go` (moved into a shared package so the
  config loader and the API use one copy).
- `defaults` must be inside `allowlist`.
- **Sizing check**: the logic of `system_group_sizing_test.go` moves into
  `pkg/catalog.CheckSystemGroupSizing(effectiveDefs, release, instanceType, minSize)`. It
  sums the effective requests of the enabled platform components, the add-ons and the
  DaemonSets against `minSize x` the allocatable CPU, memory and pods of the instance type
  (a static table of instance types, generated like the deprecation rules). Over 85 %:
  refused. An instance type missing from the table: accepted with a `Sizing` warning
  ("cannot check"). The build test stays and calls the same function for the defaults.
- The sizing check also runs when a component is enabled, or a values overlay changes
  requests (4.2.4).

#### 4.3.3 How changes reach clusters

| Field | Existing clusters | How |
|---|---|---|
| `system.minSize`, `maxSize` | In place | The node group's `scalingConfig` render changes. Rolled out like a code change: per channel, tier order, `maxInProgress`, soak, maintenance window, freeze. |
| `system.instanceType`, `capacityType`, `diskSizeGiB` | New clusters only | EKS cannot change them in place. A replacement would re-create the group on every cluster; this is out of scope (open question 3). |
| Karpenter allowlist, limits | Validation only | The server checks new and updated `nodeGroups`. Existing groups outside the new policy keep running; they are counted in `status.outsideAllowlist`, and the cluster gets the condition `NodeGroupOutsideAllowlist`. A cluster update that touches such a group must bring it inside. |
| Karpenter `defaults` | New groups, and groups that leave the field unset | A group that leaves a field unset re-renders with the new default (its `NodePool` requirements change; Karpenter drift replaces nodes). Rolled out like a code change. |
| `amiPolicy` | Every Karpenter `EC2NodeClass` | Changes the AMI alias; drift replaces nodes. Rolled out like a code change. |

#### 4.3.4 Overrides: decision

- **Per channel: no.** Channels decide *versions*. Mixing node policy into channels would
  make a channel move change instance families, which would surprise tenants and make
  the rollout harder to reason about.
- **Per tenant: yes, for the Karpenter allowlist and limits only, as a later phase
  (phase 7).** The real need is a tenant with GPU or large-memory workloads. The overlay
  is additive: `spec.tenantOverrides[]` with `tenantID` and `allowlist`/`limits` that
  extend the platform's. The system group stays platform-wide, because its sizing is
  tied to the platform's own component requests, which are the same on every cluster.

```yaml
spec:
  tenantOverrides:          # phase 7; at most 200
    - tenantID: gpu-research
      reason: "GPU training, approved 2026-11"
      karpenter:
        allowlist: { instanceFamilies: [g5, p5] }
        limits: { maxCPUPerGroup: "4000" }
```

#### 4.3.5 Bootstrap and migration

1. On start, when `PlatformConfig/platform` is missing, the controller creates it from
   its `nodeGroups` settings (flags, environment, file, defaults: the current precedence).
2. When it exists, the `nodeGroups` settings are ignored. If they differ from the object,
   both processes log a warning once, naming the keys.
3. The server stops reading `nodeGroups` for validation once the object exists. Until the
   controller has created it, the server uses its own settings (as today).
4. The `nodeGroups` section and its flags stay accepted for two minor releases of
   cluster-vending, then are removed from the documentation (still accepted as bootstrap
   input).

### 4.4 Release channels and the release process

#### 4.4.1 Channels

The three channels stay `alpha`, `beta`, `stable`. Custom channels are out of scope:
`Cluster.spec.releaseChannel` is an enum in the contract, `KubernetesRelease.channel`
puts exactly one minor on each, and the promotion chain (`alpha -> beta -> stable`) is
fixed in the rollout. Adding channels would change all three for little gain.

What becomes editable through the API is the existing `ReleaseChannelSpec`:

```yaml
apiVersion: cloud.cluster-vending.io/v1alpha1
kind: ReleaseChannel
metadata:
  name: beta
spec:
  promotion: Automatic        # Automatic | Manual | Pinned (alpha: Pinned or Manual)
  # release: 2026-12-a        # Manual / Pinned only
  paused: false
  policy:
    maxInProgress: 5
    maxFailed: 1
    soak: 24h
    soakPerTier: { Tier0: 72h }
    releaseSoak: 168h         # how long alpha must have offered a release
```

The rules the CRD and the rollout already enforce stay (alpha is not Automatic; Manual
and Pinned name a release; Tier0 `maxInProgress` is always 1). New write-time checks:
the named release is `Published`, not withdrawn, and not older (by serial) than what
the channel offers now, unless the request sets `allowOlder` (see rollback, 4.4.4).

**Promotion**:

- *Automatic* (beta, stable): unchanged. Takes the upstream's release after `releaseSoak`.
- *Manual*: promoting is an update of `spec.release`. A "promote now" on an Automatic
  channel is the same update, switching it to `Manual` for that release; the preview says
  the channel will no longer follow its upstream until set back to `Automatic`.

#### 4.4.2 Release lifecycle

New fields on `Release.spec` (outside `content`):

```yaml
apiVersion: cloud.cluster-vending.io/v1alpha1
kind: Release
metadata:
  name: 2026-12-a
spec:
  state: Draft          # Draft | Published (new). Shipped releases are Published.
  serial: 0             # assigned at publish (new). Immutable once non-zero.
  basedOn: 2026-11-b    # informational: the release it was copied from (new)
  notes: "karpenter 1.16.0; kyverno 3.11.0 on 1.37"
  withdrawn: false
  content:
    renderVersion: 12
    kubernetes:
      - minor: "1.37"
        channel: alpha
        endOfSupport: "2027-11-01T00:00:00Z"
        nodeAmiPin: v20261201
        components:
          - { name: karpenter, version: 1.16.0 }
          - { name: kyverno, version: 3.11.0 }
          # one pin per enabled platform component and per tenant component
      - minor: "1.36"
        channel: beta
        upgradeTo: "1.37"
        # ...
      - minor: "1.35"
        channel: stable
        upgradeTo: "1.36"
        default: true
        # ...
```

CEL changes: the content immutability rule (`self == oldSelf`) moves to the spec level as
`oldSelf.state == 'Draft' || self.content == oldSelf.content`; `state` can only go
`Draft -> Published`; `serial` is immutable once set. A channel or hold may not name a
`Draft` release (checked by the API and by the controller's `Offering` condition).

Flow:

| Step | What happens | Checks |
|---|---|---|
| **Create draft** | From an existing release or a channel's release (`--from`), or from a full manifest. Name `YYYY-MM-<letter>`, next free letter suggested. | Name pattern; source exists. |
| **Edit draft** | Pins, values, minors, notes. Any number of times. | CRD checks. |
| **Validate** (`validate_only` on update, or the `Valid` condition) | Consecutive minors, `upgradeTo` present; `renderVersion` <= the controller's; **every enabled platform component and every tenant component has a pin on every minor**; every pin's chart version exists in its repository (the controller resolves the chart index or OCI tag, 10 s timeout, reported per pin); per-minor values validate against the definition's schema and the sizing check; `nodeAmiPin` format. | All problems in one answer. |
| **Publish** | `state: Published`, `serial = max(serial) + 1`. Content frozen. | Must be `Valid=True`. |
| **Promote** | Set `alpha` (Pinned/Manual) `spec.release` to it; beta and stable follow by promotion. | 4.4.1. |
| **Withdraw** | Existing `spec.withdrawn: true`. Channels offering it are refused until they name another release. | Cannot be undone (unchanged). |
| **Delete** | Drafts at any time; published releases only when no channel, hold or cluster (`status.platformRelease`, `status.releaseMove.target`) names them. | Finalizer `release-in-use` extended to clusters. |

Shipped releases (`shipped-<hash>`) keep being created from `release.yaml`, as
`Published`, with the next serial. A build therefore still "proposes" a release, and
`alpha` still moves to it **unless** the admin has pinned alpha to a release with a
higher serial (today a new build always moves alpha off a shipped release). This keeps
"a build ships a tested default" and lets the admin stay ahead of it.

#### 4.4.3 Explicit order

`spec.serial` replaces `CreationTimestamp` wherever release order matters:

- `ListReleases` sorts by serial (newest first), then name.
- `notNewer` in the release move compares serials.
- A channel update to a lower serial needs `allowOlder`.

Migration: the controller backfills `serial` once, in creation-time order (ties by
name), for every release without one. After that, creation time is only shown.

#### 4.4.4 Rollback

There is still no Kubernetes downgrade (EKS cannot). For components, rollback is
**forward**: publish a new release whose pins are the older versions (`--from` the older
release), and promote it. It gets a new, higher serial, so the release move treats it as
newer and moves components to it, one at a time, by tier. Validation warns when a pin
goes down for a component with `upgradeOneMinorAtATime` (Istio) or a separate CRD chart,
because Helm does not downgrade CRDs. `allowOlder` exists only to point a channel back at
an existing older release while a forward fix is prepared. It is refused when the older
release offers a different Kubernetes minor on that channel.

#### 4.4.5 Freeze integration

Unchanged principle: the API accepts and stores admin writes during a freeze, and the
cloud and workload changes wait. Specifically, during a platform freeze: channel updates
are stored and the plan is shown, but no cluster starts; publishing is allowed; catalog
and node group rollouts do not start. Each admin preview shows "frozen: nothing starts
until unfreeze" when that is the case.

### 4.5 How catalog and node group changes roll out

A new per-channel rollout, **catalog changes**, uses the same planner
(`pkg/rollout`) as release moves and code changes:

- The controller computes a *catalog generation*: a hash over the effective definitions
  (enabled, sequence, requires, merged values, timeout) and the in-place fields of
  `PlatformConfig`. Each cluster records the generation it has applied
  (`status.catalog.applied`).
- A cluster whose applied generation differs has a catalog change pending
  (`status.catalog`: `phase` `Pending|Applying|Done|Halted`, `summary`, e.g.
  `disable istio; external-secrets values`).
- `ReleaseChannel.status.catalogChanges` plans it like `releaseMoves`: tier order,
  `maxInProgress`, soak, halt past `maxFailed`, and the existing per-cluster gates
  (frozen, paused, not ready, maintenance window).
- Within a cluster, component steps run one at a time: uninstalls highest sequence first,
  then installs and value changes in install order. Each step has the release move's
  30-minute watch and 3 recoveries, then halts until `cluster retry`.
- **Manual clusters are included**, because upgrade mode governs versions the tenant
  chooses, not the platform's own configuration (open question 1).
- New clusters always get the current generation at creation.

Node group in-place changes (`minSize`, `maxSize`, Karpenter defaults, AMI policy) are
rendered changes of objects the cluster already owns, so they use the existing
**code change** rollout unchanged.

### 4.6 Admin surfaces

#### 4.6.1 API (additive, `AdminService`)

All new RPCs need `PlatformAdmin`, carry no `tenant_id`, and are added to the
authorization classification (the test that fails on an unclassified RPC covers them).
Writes take an `etag` (as other updates) and an `update_mask`, and support
`validate_only`, which returns the same `ChangePreview` without storing anything.

```proto
service AdminService {
  // existing: Freeze, Unfreeze, GetFreeze, ListReleases, GetRelease,
  //           ListReleaseChannels, GetReleaseChannel

  rpc ListComponentDefinitions(AdminListComponentDefinitionsRequest)
      returns (AdminListComponentDefinitionsResponse) { option idempotency_level = NO_SIDE_EFFECTS; }
  rpc GetComponentDefinition(AdminGetComponentDefinitionRequest)
      returns (AdminGetComponentDefinitionResponse) { option idempotency_level = NO_SIDE_EFFECTS; }
  rpc CreateComponentDefinition(CreateComponentDefinitionRequest)
      returns (CreateComponentDefinitionResponse);
  rpc UpdateComponentDefinition(UpdateComponentDefinitionRequest)
      returns (UpdateComponentDefinitionResponse);   // overlay; reset = clear overlay
  rpc DeleteComponentDefinition(DeleteComponentDefinitionRequest)
      returns (DeleteComponentDefinitionResponse);   // custom only

  rpc GetPlatformConfig(GetPlatformConfigRequest)
      returns (GetPlatformConfigResponse) { option idempotency_level = NO_SIDE_EFFECTS; }
  rpc UpdatePlatformConfig(UpdatePlatformConfigRequest)
      returns (UpdatePlatformConfigResponse);

  rpc CreateRelease(CreateReleaseRequest) returns (CreateReleaseResponse);   // draft
  rpc UpdateRelease(UpdateReleaseRequest) returns (UpdateReleaseResponse);   // edit, publish, withdraw
  rpc DeleteRelease(DeleteReleaseRequest) returns (DeleteReleaseResponse);

  rpc UpdateReleaseChannel(UpdateReleaseChannelRequest)
      returns (UpdateReleaseChannelResponse);
}

message ComponentOverlay {
  optional bool enabled = 1;
  optional int32 sequence = 2;
  repeated string requires = 3;
  google.protobuf.Struct platform_values = 4;
  google.protobuf.Duration install_timeout = 5;
}

message UpdateComponentDefinitionRequest {
  string name = 1;
  string etag = 2;
  ComponentOverlay overlay = 3;
  google.protobuf.FieldMask update_mask = 4;  // paths under overlay; "overlay" alone = reset
  bool cascade = 5;                            // disable dependents too
  bool validate_only = 6;
  string reason = 7;                           // required, recorded
}

message UpdateComponentDefinitionResponse {
  AdminComponentDefinition definition = 1;    // base, overlay, effective, usage, sync state
  ChangePreview preview = 2;
}

message ChangePreview {
  repeated string changes = 1;                 // "sequence: 100 -> 150", "disable: istio"
  repeated string warnings = 2;                // sizing, CRD downgrade, detached, frozen
  int32 clusters_affected = 3;
  repeated AffectedTier tiers = 4;             // per channel and tier: counts, first batch
  repeated string blocked = 5;                 // clusters kept back by their gates, at most 20
}

message UpdateReleaseRequest {
  string id = 1;
  string etag = 2;
  ReleaseContent content = 3;                  // drafts only
  string notes = 4;
  bool publish = 5;
  bool withdraw = 6;
  google.protobuf.FieldMask update_mask = 7;
  bool validate_only = 8;
  string reason = 9;
}

message UpdateReleaseChannelRequest {
  string id = 1;                               // alpha | beta | stable
  string etag = 2;
  ReleaseChannelSpec spec = 3;                 // promotion, release, paused, policy
  google.protobuf.FieldMask update_mask = 4;
  bool allow_older = 5;
  bool validate_only = 6;
  string reason = 7;
}
```

The tenant-facing `ComponentService.ListComponentDefinitions` keeps its response, now
built from the effective definition (disabled definitions hidden, as today).

#### 4.6.2 CLI

New nouns under the existing `admin` group, with verbs the CLI already uses. No new
top-level command. `--plan` (as `network update` and `cluster update`) prints the
preview without storing; `--reason` is required on writes.

| Command | Does |
|---|---|
| `admin component list [-o wide]` | Name, management, enabled, effective sequence, sync state (`Synced`, `Detached`), clusters using it |
| `admin component get <name>` | Base, overlay, effective, usage, recent events |
| `admin component update <name> [--enable \| --disable [--cascade]] [--sequence N] [--requires a,b] [--values-file f] [--timeout 10m] [--reset] [--plan] --reason` | Overlay changes and reset |
| `admin component create -f def.yaml [--plan]`, `admin component delete <name>` | Custom definitions |
| `admin config get`, `admin config update -f platform.yaml [--plan] --reason` | `PlatformConfig` (`update -f` prints each change like `cluster update`) |
| `admin release create <name> --from <release\|channel> [-f pins.yaml]` | Draft |
| `admin release update --id <name> [-f pins.yaml] [--publish] [--withdraw] [--plan] --reason` | Edit, validate (`--plan`), publish, withdraw |
| `admin release delete --id <name>` | Draft or unused |
| `admin channel update --id <ch> [--release R] [--promotion P] [--pause \| --resume] [-f policy.yaml] [--allow-older] [--plan] --reason` | Channel spec; promotion is `--release` |

Existing `admin release list|get` and `admin channel list|get` stay; `list` gains the
serial and state columns.

#### 4.6.3 Console

Three admin pages, shown only when `WhoAmI` returns `PlatformAdmin`. Every write opens a
preview (the `validate_only` answer) that names the changes, warnings, and affected
clusters per channel and tier before the admin confirms with a reason.

- **Components**: a table ordered by effective sequence (Karpenter fixed at the top),
  with enable switches, sequence fields (with the requires/sequence check inline), and
  sync state badges (`Detached` prominent). A detail panel shows base, overlay and
  effective side by side, the values overlay editor (schema-validated), the clusters
  using it, and "Reset to shipped". The existing tenant components page is unchanged.
- **Node groups & Karpenter**: a form for the system group (new-clusters-only fields
  labelled as such), the sizing result as a bar per dimension (CPU, memory, pods against
  the 85 % line), the allowlist and defaults, the limits, the AMI policy, and the count
  of existing groups outside the allowlist.
- **Releases & Channels**: extends `releases-page.tsx`. The release list ordered by
  serial with state; a draft editor (pins table per minor, with "copy from release",
  per-pin validation results); Publish and Withdraw actions; channel cards with
  promotion, release, soak and tier policy editable, the existing rollout plan, and the
  new catalog change rollout next to release moves and code changes.

#### 4.6.4 Authorization

- API: `PlatformAdmin` only, enforced by the existing interceptor classification. With
  `auth.mode: none` the API is open as today (an existing known limit). The console
  hides the pages for other roles, but the backend check is authoritative.
- Kubernetes: the API server's service account gains `create`, `update`, `delete` on
  `componentdefinitions`, `platformconfigs`, `releases`, and `update` on
  `releasechannels` (it already writes `releaseholds`). The controller keeps writing
  status only on these kinds, plus the base of shipped definitions and shipped releases.

#### 4.6.5 Audit

Each admin write:

1. Sets annotations `cloud.cluster-vending.io/changed-by` (the caller's username from
   the token, or `anonymous`), `changed-at`, and `change-reason` on the object.
2. Emits an event on the object (reason `AdminChange`, message the preview's change list),
   and keeps it in the object's `status.recentEvents` (added to these kinds).
3. Logs a structured line on the API server with the caller, RPC, object, etag before/after
   and the change list.
4. Increments `cluster_vending_admin_changes_total{kind,action}`.

Events expire with Kubernetes' retention, so the durable record is the server log and the
Kubernetes API server's audit log. This is stated as a limit (open question 5).

### 4.7 Safety summary

| Guardrail | Where |
|---|---|
| Karpenter cannot be disabled, reordered, or have its sequence shared | Admin API, and a CEL rule on `overlay.sequence` for `karpenter` |
| A required component cannot be disabled without `cascade` and a preview | Admin API |
| `requires` must not point to a higher sequence; no cycles | Admin API and the `Valid` condition |
| Values overlay must keep tenant values valid and pass sizing | Admin API |
| System group `minSize >= 2` and sizing <= 85 % | Admin API, CRD minimum |
| Releases must be `Valid` to publish, `Published` to be offered | Admin API, CEL, `Offering` condition |
| Channel cannot go to an older serial without `allowOlder`, nor change a channel's minor backwards | Admin API |
| Every cluster-affecting change rolls out by tier with soak, halts on failures, respects freeze and maintenance windows | Rollout (4.5) |
| Every write is etag-checked, previewable, and attributed | Admin API |
| A build cannot overwrite admin choices, and cannot be silently blocked by them | Catalog sync (4.2.1) |

### 4.8 Backward compatibility

- CRDs: new optional fields only (`overlay`, `status.effective`, `status.usage`,
  `Release.spec.state/serial/basedOn`, new kind `PlatformConfig`). Existing objects stay
  valid: a release without `state` is `Published`.
- `spec.disabled` keeps working and is honoured when `overlay.enabled` is unset.
- The `nodeGroups` config section keeps working as bootstrap input.
- The contract is additive; `make breaking` passes.
- `kubectl`/Git management of these objects keeps working, with the same validation on
  the controller side (`Valid` conditions). The API adds checks before storing.

## 5. Alternatives considered

| Alternative | Why not |
|---|---|
| Keep the hash model, add an "accept build" action | Still freezes a definition on any edit; the admin must notice and act for every build. The failure mode stays silent. |
| A separate `ComponentOverride` kind per definition | Two objects to read for one answer, ordering questions between them, more RBAC. One object with base and overlay is easier to read and to audit. |
| Store admin settings in a ConfigMap | No schema, no CEL, no status, no per-field validation. |
| Keep node groups in config files, add hot reload | Still two files that must agree, still not visible through the API, still per-environment drift. |
| Per-channel node group settings | Couples version promotion with node policy (4.3.4). |
| Custom channels | Touches the cluster contract enum, the release's channel enum and the fixed promotion chain, for no requested use case. |
| Order releases by name | Shipped releases are content-hash named; names do not order across shipped and published releases. |
| Apply catalog changes to all clusters at once (today's behaviour for `disabled`) | No tiers or soak; one bad values overlay reaches every cluster. |

## 6. Rollout plan

Each phase is shippable on its own, behind no flag, and leaves the system consistent.

### Phase 1: Release serial ordering

- `Release.spec.serial`, backfill in creation-time order, `ListReleases` and `notNewer`
  use it; `serial` shown in `admin release list`.
- **Accepted when**: re-creating a release object does not change the order or a
  release move's decision.
- **Tests**: unit tests for backfill (ties, missing timestamps), `notNewer` on serials,
  list ordering; CEL immutability test.

### Phase 2: Component base/overlay and sync fix

- `spec.overlay`, `status.effective`, `status.usage`, `Synced` condition, `Detached`
  annotation; `SpecHash` over the base; sync always updates the base; migration (4.2.6);
  `pkg/component.Effective` used by the installer, deletion and tenant API.
- **Accepted when**: an edited `description` no longer blocks a build update; the
  incident's `karpenter` definition is brought back under the build; a `Detached`
  definition is visible in status, event and metric.
- **Tests**: table tests for every sync case including migration; `Effective` tests
  (enabled precedence, requires union, values merge order with release values); a
  regression test reproducing the description-edit incident.

### Phase 3: Admin component RPCs, CLI and catalog change rollout

- `AdminService` component RPCs with `validate_only`; `admin component` CLI; validation
  (4.2.3, 4.2.4); catalog generation and `status.catalog` on clusters;
  `ReleaseChannel.status.catalogChanges` in the planner.
- **Accepted when**: disable, enable, reorder, values overlay and reset work end to end on
  the fake API server, in tier order, respecting freeze, gates and halts; Karpenter guards
  refuse.
- **Tests**: validation table tests (cycles, requires vs sequence, Karpenter, cascade);
  planner tests for catalog changes (tiers, soak, halt, Manual clusters included);
  authorization classification test covers the new RPCs; CLI output tests.

### Phase 4: `PlatformConfig`

- New kind; bootstrap from `nodeGroups` settings; server and controller read the object;
  sizing check function shared with the build test; `admin config` CLI and RPCs; in-place
  fields roll out as code changes; `outsideAllowlist` status and cluster condition.
- **Accepted when**: changing `maxSize` needs no restart and rolls out by tier; the server
  and controller cannot disagree; `t3.small` is refused by the sizing check.
- **Tests**: bootstrap (missing, present, differing settings warning); validation table
  tests moved from `pkg/config`; sizing function tests reusing the build test's fixtures;
  server validation reads the object.

### Phase 5: Release authoring and channel updates

- `Release.spec.state`, `basedOn`, CEL changes; create/update/delete RPCs; chart
  existence check in the controller; publish assigns serial; `UpdateReleaseChannel`;
  shipped releases do not move alpha off a newer published release.
- **Accepted when**: an admin creates a draft from `stable`'s release, bumps a pin,
  validates (with a missing-pin and a bad-chart error reported together), publishes,
  promotes alpha, and beta and stable follow after their soaks.
- **Tests**: lifecycle state machine tests; validation (missing pins, schema, renderVersion);
  channel update checks (draft refused, older serial needs `allowOlder`, minor going
  backwards refused); release move with a forward rollback release.

### Phase 6: Console admin pages

- Components, Node groups & Karpenter, Releases & Channels pages, preview dialogs.
- **Accepted when**: every CLI write is possible from the console with the same preview;
  pages are hidden for non-admins.
- **Tests**: component tests for the forms and preview; Playwright flows (disable with
  preview, publish a draft, change `maxSize`) at 320, 768, 1024 and 1440; accessibility
  checks (keyboard reordering, focus in dialogs).

### Phase 7: Per-tenant Karpenter overrides

- `PlatformConfig.spec.tenantOverrides`; server validation merges the tenant's override.
- **Accepted when**: a tenant with an override can create a `g5` group and another tenant
  cannot.
- **Tests**: merge and validation tests; removal of an override marks the tenant's groups
  `NodeGroupOutsideAllowlist`.

## 7. Open questions

1. **Manual clusters and catalog changes.** This proposal applies catalog changes
   (disable, values overlay) to Manual clusters too, in tier order. Is that right, or
   should Manual clusters take them only on their next tenant-initiated upgrade?
2. **Tenant components on a disabled definition.** Proposed: they stay installed and are
   marked `Degraded`. Should a disable optionally uninstall them (`--remove-installed`)?
3. **Instance type change of the system group on existing clusters.** Today new clusters
   only. Should the admin be able to roll a replacement node group through the tiers (the
   reconciler can already replace a group)?
4. **Chart existence check.** The controller would reach chart repositories at
   validation time. Is outbound access from the management cluster to every repository
   acceptable, or should the check be optional per repository?
5. **Durable audit.** Events expire. Is the API server log plus the Kubernetes audit log
   enough, or should admin changes be kept in a bounded history on each object (like
   `ReleaseChannel.status.history`)?
6. **Shipped release vs published release.** Proposed: a new build does not move alpha off
   a published release with a higher serial. Should a build ever be allowed to override
   the admin's alpha pin?
7. **Several component versions per minor.** Deferred by an earlier decision. The overlay
   and release shapes leave room (a pin could become a list with a default). Confirm it
   stays out of this proposal.
