# PROP-01: File-based platform configuration (components, node groups, releases, channels)

| | |
|---|---|
| Status | Proposed |
| Author | `<author>` |
| Date | 2026-11-20 |
| Reviewers | `<reviewers>` |
| Related | [architecture.md](../architecture.md): 5. Configuration, Platform components, Node layout, Releases, channels and rollouts, Helm operations, Freeze and pause, 4. CR definitions |

## 1. Summary

The platform's behaviour is set in four places today: the build (component definitions,
`pkg/catalog/release.yaml`), hand edits of custom resources with `kubectl`
(`ComponentDefinition.spec`, `ReleaseChannel.spec`), the `nodeGroups` section of two
config files that are restarted separately, and nowhere at all (release order is the
creation time, and a release cannot be authored without a build).

This proposal makes one platform configuration **a file, in Git**:

- One YAML file, `platform.yaml`, delivered as a ConfigMap by a kustomize overlay or a
  Helm values block. `serve` and `controller` read it through the existing `--config`
  with the same struct and the same validation. Flags and environment variables keep
  their precedence over it.
- Four sections: `components`, `nodeGroups`, `releases` and `channels`.
- The controller is the single writer of the `ComponentDefinition`, `Release` and
  `ReleaseChannel` objects. It renders them from the build's shipped definitions (the
  base) with the file applied on top (the overlay). A hand edit of a rendered object is
  reverted on the next pass.
- The same validation runs in `cluster-vending controller --config platform.yaml --check`,
  in CI, as a pre-deploy step, and at controller and server start. An invalid file is
  never applied; the last good state stays.
- Changes that reach running clusters use the existing rollouts: channel tiers,
  `maxInProgress`, `maxFailed`, soak, maintenance windows and the platform freeze.

There is no new admin RPC, no `PlatformConfig` kind, no runtime mutation surface and no
console editing. Git is the audit trail. The console shows the loaded configuration
read-only.

The additions are small: one CRD field (`Release.spec.serial`), a `serial` in the build's
`release.yaml`, a config-hash annotation on the rendered objects, and one read-only HTTP
endpoint on `serve` (`GET /config`). Nothing an admin does at runtime changes the cluster
state directly; the next start or pass renders it from the file.

## 2. Problems today

### 2.1 Component definitions: the hash model freezes them

`pkg/catalog/catalog.go` (`Sync`) records `cloud.cluster-vending.io/shipped-hash` on each
shipped definition and updates it only while its spec still has that hash:

| State | What a build does today |
|---|---|
| Shipped, spec hash equal to the recorded hash | Replaces the spec with the build's |
| Shipped, spec hash differs (any hand edit) | Leaves it, logs "edited", **for ever** |
| Not labelled, different from the build | Leaves it |

In the incident that prompted this proposal, someone edited `spec.description` on the
`karpenter` definition. Every later build skipped that definition, including a build that
fixed Karpenter's IAM policy. Nothing on the object said so; the only record was a log line
at controller start. The way back was a manual `kubectl replace`.

Other gaps:

- **Disable** is a `kubectl patch` of `spec.disabled`. A disabled platform component is
  removed from every cluster on the next pass, with no tiers and no soak.
- **Order** is `spec.sequence` (Karpenter 0, default 100), set only in YAML. Nothing checks
  that `requires` does not point to a higher sequence.
- **Values** of a platform component are a hand edit of `spec.platformValues`, which stops
  later builds from updating that definition. **Custom components** are created with
  `kubectl apply`, and `Valid` is set only after the apply.

### 2.2 Node groups: two files that must agree

`pkg/config/node_groups.go` validates the `nodeGroups` section (system group, Karpenter
allowlist and defaults). The server validates from its config and the controller renders
from its config (`cmd/serve.go`, `cmd/controller.go`). Nothing enforces that they match.
Today they are kept equal by copying values by hand, in four places:

- `config/manager/manager.yaml`: the `CV_SYSTEM_NODE_GROUP_*` and `CV_NODE_GROUP_*`
  environment variables of the controller.
- `config/server/base/kustomization.yaml`: the same variables in `server-config`.
- `hack/controller.local.yaml` and `hack/config.local.yaml`: the `nodeGroups` section.

A change restarts both processes and reaches every cluster at once, with no tier order. No
sizing check runs at change time (`pkg/catalog/system_group_sizing_test.go` checks only the
build's defaults), and the limits per group are hard-coded or absent.

### 2.3 Releases come from the build

- `pkg/catalog/release.yaml` is the only place versions are set. The controller creates
  `shipped-<hash>` from it (`pkg/controller/release_defaults.go`). An admin who wants a new
  chart version or a new Kubernetes minor needs a new build.
- **Release order is the creation time.** `ListReleases` (`pkg/server/admin/releases.go`)
  sorts by `CreationTimestamp`, and `notNewer` (`pkg/reconcile/cluster/release_move.go`)
  compares creation timestamps. Re-creating a release object (restore, migration to a new
  management cluster) changes the order.
- `CreateReleaseDefaults` creates the channels only when they are missing, and
  `takeOverAlpha` moves `alpha` to a new shipped release only while nobody has touched it.
  - A channel's promotion, pause and policy are edited with `kubectl`. Nothing validates the
  edit before it is applied, and there is no record of who made it.

## 3. Goals and non-goals

### Goals

1. One platform configuration file, reviewed and versioned in Git, that both `serve` and
   `controller` read through one struct with one validation.
2. A build never overwrites what the admin set in the file, and the file never stops a
   build from updating what the build owns (the base).
3. The system node group and the Karpenter policy are defined once. The server and the
   controller cannot disagree, because they read the same file.
4. Releases are defined in the file with an explicit `serial`. Channels name the release
   they offer and carry their promotion, soak and tier policy.
5. Every change that affects running clusters rolls out by channel and tier, respects the
   freeze, and is visible in the existing rollout views.
6. Invalid configuration is refused with every problem listed, and the last good
   configuration stays in effect.

### Non-goals

- Admin write RPCs, a `PlatformConfig` or any other new kind, admin CLI commands that
  mutate state, and console pages that edit configuration. Changes are commits.
- Runtime reloading of the file. A change restarts the two processes (section 4.3).
- Custom channel names beyond `alpha`, `beta` and `stable`.
- Several supported versions of a component per Kubernetes minor (deferred), tenant-chosen
  versions, and per-cluster component overrides by the admin (a `ReleaseHold` still pins).
- Freeze in the file. `admin freeze` stays a runtime switch: it is an operational stop,
  not policy.
- A new storage backend. The custom resources stay the state the controller reconciles.

## 4. Proposed design

### 4.1 One file, two readers

`platform.yaml` is read by `cluster-vending serve` and `cluster-vending controller`
through `--config` / `CV_CONFIG` (`pkg/config/file.go`, `FileFrom`). Rules:

- **Precedence is unchanged**: flag, environment variable, file, default. A file key is
  therefore overridden by any `CV_*` variable set in the Deployment. The processes log a
  warning at start for each environment variable that overrides a file key. The kustomize
  bases stop setting node group variables (section 4.8), so the file is the only source.
- **Deployment identity stays in flags and environment**: the management role ARN, the
  lease namespace, the service account, the kubeconfig, the log format. The file holds
  policy.
- **New top-level sections** `components`, `releases` and `channels` are parsed by the
  shared loader, as `nodeGroups` is. Today a top-level key that is not a setting is an
  "unknown setting" error (`checkKeys`), so each new section needs explicit handling.
- **`auth` stays in the file**, read by the server only. `--config` takes one path, so the
  auth overlay's separate `server.yaml` (`config/server/auth`) becomes a section of
  `platform.yaml`.
- **Secrets stay out.** There is no field for secret material. Credentials are referenced
  by Secret name (`install.helm.credentials`). AWS keys are never in the file (architecture:
  AWS credentials).

### 4.2 The sections

```yaml
# platform.yaml

auth:                                  # server only; unchanged (architecture: Authentication)
  mode: required
  issuers: []

nodeGroups:                            # same keys and checks as today (pkg/config/node_groups.go)
  system:
    instanceType: t3.medium            # new clusters only
    minSize: 2                         # >= 2; in place on existing clusters
    maxSize: 6                         # >= minSize; in place
    capacityType: ON_DEMAND            # new clusters only
    diskSizeGiB: 0                     # new clusters only; 0 = EKS default
  karpenter:
    instanceFamilies: [t3]             # empty or none: no restriction
    instanceTypes: []
    defaults:
      architectures: [AMD64]
      capacityTypes: [ON_DEMAND]
    limits:                            # new: ceilings on what a tenant group may ask for
      maxGroupsPerCluster: 20          # at most 20 (the Node layout limit)
      maxCPUPerGroup: "1000"
      maxMemoryGiBPerGroup: 4000
    amiPolicy: Release                 # Release (the release's nodeAmiPin) | Latest

components:                            # overlay on the build's definitions, by name
  karpenter:
    sequence: 0                        # fixed at 0; enabled must stay true
  external-secrets:
    sequence: 150                      # 1 to 10000; default 100 (the build's)
    requires: [kyverno]                # added to the build's requires; never removes one
    values:                            # deep-merged over the build's platformValues
      resources: { requests: { cpu: 50m } }
    installTimeout: 10m                # 1 m to 60 m
  kyverno-policies:                    # a tenant component
    enabled: true                      # false: tenants can no longer enable it
  edge-agent:                          # a custom component: its whole definition
    definition:                        # the ComponentDefinition spec, as kubectl apply takes it
      description: Edge agent
      management: Platform
      namespace: edge-system
      install: { helm: { repository: oci://registry.example.com/charts, chart: edge-agent }, timeout: 5m }
      sequence: 120
  # A disabled platform component: enabled: false here, and every enabled component that
  # requires it is also disabled in this file (no implicit cascade).

releases:                              # the build's release.yaml is always present
  2026-12-a:
    serial: 12                         # explicit order; above every serial already rendered; never changed
    file: 2026-12-a.yaml               # same shape as release.yaml, without channels (or inline: content)
    notes: "karpenter 1.16.0; kyverno 3.11.0 on 1.37"
  2026-11-b:
    serial: 11
    withdrawn: true                    # no channel may offer it again; cannot be undone

channels:
  alpha:
    promotion: Pinned                  # Pinned | Manual (alpha: not Automatic)
    release: 2026-12-a                 # what alpha offers; unset: the build's shipped release
  beta:
    promotion: Automatic               # follows alpha after releaseSoak
    policy:
      releaseSoak: 168h
      maxInProgress: 5
      maxFailed: 1
      soak: 24h
      soakPerTier: { Tier0: 72h }
  stable:
    promotion: Automatic
    paused: false
```

**Components.** The file overlays the build's definition of the same name. The overlay
fields map onto the existing `ComponentDefinition` fields, so the CRD does not change: `enabled`
becomes `spec.disabled`, `sequence` becomes `spec.sequence`, `requires` extends
`spec.requires`, `values` is merged into `spec.platformValues`, and `installTimeout` becomes
`spec.install.timeout`. A `definition` entry is a custom component, validated with the same
checks as the `Valid` condition today (bundles, requirements, cloud and chart rules, the
`userValues` schema). A custom definition may also be a file in the same ConfigMap
(`definitionFile: edge-agent.yaml`), which keeps long charts out of `platform.yaml`.

**Node groups.** The keys above are today's, plus `limits` and `amiPolicy` (new). Tenant
overrides of the Karpenter allowlist are a later phase (phase 7) and would be a list under
`nodeGroups.tenantOverrides`, additive to the platform's allowlist.

**Releases.** A release file is `release.yaml`'s shape without `channels` (`kubernetes`,
`components`). `serial` is an explicit integer the admin writes. The build's shipped release
takes its serial from `release.yaml` (a new `serial` field, set by the build above every
release it has shipped). Ordering everywhere uses `serial`, not creation time (section
4.4). **Rollback is forward**: add a file release with the older pins and a higher serial,
then point the channel at it. Rendered content is immutable: a change to the pins of a
release that exists requires a new name.

**Channels.** `promotion`, `release`, `paused` and `policy` are the existing
`ReleaseChannelSpec`. Promotion is an edit of `release` (a "promote now" on an Automatic
channel is `promotion: Manual` and a `release`, shown in the review as a change of
promotion). A channel that names no release offers the build's shipped release when it is
Pinned, and its upstream's release when it is Automatic. The rules the CRD and the rollout
already enforce stay: alpha is not Automatic, Tier0 `maxInProgress` is 1.

### 4.3 How the file reaches the cluster state

The controller is the **single writer** of `ComponentDefinition.spec`, `Release.spec` and
`ReleaseChannel.spec` for the objects it renders. On start, and only then, it:

1. validates the file (section 4.4); if it is invalid, it does not render (below);
2. renders each ComponentDefinition as the build's base merged with the file's overlay,
   and each Release from `release.yaml` or its file release (through the existing
   `ContentOf`);
3. renders each ReleaseChannel from the build's channel defaults with the file's channel
   settings;
4. applies these with server-side apply and records on each object the annotation
   `cloud.cluster-vending.io/config-hash` (SHA-256 of the file as loaded).

**Base and overlay.** The base is the build's definition, replaced on every start, so a
build update reaches clusters without any overlay conflict. The overlay is the file's
entry for that name, applied on top, key by key for `values`. If a build changes a value
the file also sets, the file's value wins; the validator warns when an overlay key no
longer exists in the build's schema.

**Drift.** On every pass the controller compares each rendered object with what the file
renders. A difference (a hand edit with `kubectl`, or another tool) is reverted at once,
with an Event `ConfigDriftReverted` on the object naming the fields, and the counter
`cluster_vending_config_drift_reverted_total`. The file is the only way to change these
objects. The earlier "edited, left for ever" state and the `shipped-hash` logic are removed.

**Reload: restart, not watch.** The file changes by a new ConfigMap name or pod template
annotation, and both Deployments restart. The alternative, a controller that watches the
mounted file and re-renders live, was rejected for four reasons:

- One code path. Rendering happens on start only, the path that is tested and reasoned
  about. A live re-render adds a second path that must handle a half-changed file.
- A bad file cannot take effect partially. The new pod fails its readiness (below) and the
  rollout stops, with the old pod still running.
- The server and the controller change together. The server's validation of node group
  requests needs a restart anyway; a watch would only cover the controller.
- Kubelet propagation of a changed ConfigMap volume takes up to a minute and is not
  under the controller's control. The restart is explicit in the pod spec.

The cost is one controller restart per change. Reconciles are idempotent (server-side
apply, status on the objects), and the leader hands over after the lease expires. kustomize
does this automatically: a `configMapGenerator` name carries a content hash, so the
Deployment's volume reference changes with the file. Helm needs a `checksum` annotation on
the pod template (section 4.6).

**Invalid files.** A new pod with an invalid file is not Ready (`/readyz` includes the
validation), so the rollout stops with the old pod leading. If an invalid pod ever leads
(the old pod is gone), it does not render. It leaves the rendered objects as they are,
sets the metric `cluster_vending_platform_config_valid` to 0, logs the errors, and emits a
Warning Event on `ReleaseChannel/alpha` listing them. `serve` refuses to start with an
invalid file, as it does today; the old server pods keep serving during the rollout.

A fresh install with an invalid file has nothing rendered, and the fix is in the file.

**Removal.** A definition dropped from the file or the build is deleted unless a cluster
uses it (`definition-in-use`). A release used by a cluster or named by a channel cannot be
removed (section 4.4).

### 4.4 Validation

One validation function in `pkg/config` and `pkg/catalog` serves four callers: the
controller at start, the server at start, `--check` in CI and kustomize or Helm pre-deploy,
and the controller's refusal of a change at runtime. Every problem is reported in one
answer, with the setting's path.

```text
cluster-vending controller --config platform.yaml --check            # offline: the file and the build
cluster-vending controller --config platform.yaml --check --kubeconfig ~/.kube/mgmt --plan
cluster-vending controller --config platform.yaml --check --charts   # also reaches chart repositories (CI)
cluster-vending controller --check --kubeconfig ~/.kube/mgmt --export # section 4.8
```

Exit status 0 for a valid file, 1 for any problem. `--plan` (with the cluster) prints the
objects that would change and the rollout plan the existing planner would produce, without
writing.

| Rule | Checked |
|---|---|
| `karpenter` is enabled and its sequence is 0; no other component has a sequence at or below 0 | Offline |
| For each effective `requires` edge A to B, sequence(B) <= sequence(A) | Offline |
| The effective `requires` graph (build and overlay, across bundles) has no cycle | Offline |
| Disabling a component that an enabled component requires is refused unless the file disables the dependent too | Offline |
| Sequences are 1 to 10000; a bundle member's sequence is set by its bundle and cannot be set alone | Offline |
| A custom definition passes the `Valid` checks (bundles, requirements, cloud and chart rules, `userValues` against the schema) | Offline |
| A values overlay keeps every tenant `Component`'s values valid against `valuesSchema` | With the cluster |
| `minSize >= 2`, `maxSize >= minSize`, instance type, family, capacity and architecture patterns | Offline (the existing checks, in one shared place) |
| System group sizing: the effective requests of the enabled platform components, add-ons and DaemonSets against 85 % of `minSize` x the allocatable of the instance type (the build's `CheckSystemGroupSizing`); an instance type missing from the static table is a warning | Offline |
| `defaults` lie inside the allowlist | Offline |
| Release: 3 to 6 consecutive minors, one default, exactly one minor per channel, `upgradeTo` on offer, `nodeAmiPin` format, `endOfSupport` a day | Offline (`release.go` checks, extended) |
| Release: every enabled platform component and tenant component has a pin on every minor; `renderVersion` is not above the controller's | Offline |
| Release: every pin's chart exists in its repository | `--charts` only (not at start) |
| Release `serial` is unique and above every serial already rendered; a rendered release's content and serial never change | With the cluster |
| `withdrawn` cannot be reverted | With the cluster |
| A release used by a cluster (`status.platformRelease`, a release move target, a hold) or named by a channel cannot be removed | With the cluster |
| A channel names a release that is in the file or on the cluster, not withdrawn; Manual and Pinned name one; alpha is not Automatic; Tier0 `maxInProgress` is 1 | Offline, or with the cluster for names not in the file |
| Credentials are references (Secret name and key), never values | Offline (the schema has no value field) |

**Start-up behaviour.** Both processes validate before anything else. The controller
renders only a valid file. Checks that need the cluster run at start too: if one fails, the
controller keeps the last rendered state and reports as in section 4.3.

### 4.5 Rollout of a change to existing clusters

All of these are gated like a release move or a code change: per channel, tier order
(Tier2, Tier1, Tier0), `maxInProgress`, soak, halt past `maxFailed`, the per-cluster gates
(manual mode, paused, frozen, not ready, a maintenance window) and the platform freeze.

| File change | Effect on existing clusters | Rollout |
|---|---|---|
| Component enabled, disabled, added or removed; values, timeout, requires | The effective definition changes. The controller computes a *catalog generation* (a hash over the effective definitions) and each cluster records the generation it applied (`status.catalog.applied`). | A per-channel `catalogChanges` rollout uses the same planner as release moves (previous proposal, 4.5, unchanged). Disable is an uninstall, highest sequence first. Within a cluster, one component step at a time. |
| Sequence only | No reinstall. Applies to new clusters, to later moves, and to deletion order. | Shown in the plan; no step. |
| `nodeGroups.system.minSize`, `maxSize` | The node group's scaling changes in place. | Rendered change of an owned object: the existing code-change rollout. |
| `nodeGroups.system.instanceType`, `capacityType`, `diskSizeGiB` | New clusters only. EKS cannot change them in place. | Not rolled out (open question 3). |
| `nodeGroups.karpenter` defaults, `amiPolicy` | New groups, groups leaving the field unset, and the AMI alias of every Karpenter `EC2NodeClass` (drift replaces nodes). | Code-change rollout. |
| `nodeGroups.karpenter` allowlist, limits | Nothing is removed. Existing tenant groups outside the new policy keep running and are counted; a cluster update that touches such a group must bring it inside. | Validation for new requests; the cluster condition `NodeGroupOutsideAllowlist`. |
| A new release, or a new serial | Nothing, until a channel names it. | The existing release move, by tier and soak. |
| Channel `release`, `promotion` | The channel offers the new release. | The existing rollout: a Kubernetes rollout, then release moves. |
| Channel `policy`, `paused` | Takes effect on the next pass; `paused` starts nothing new. | The plan shown by `admin channel get`. |

The `--plan` preview (section 4.4) shows the same plan before the commit merges. During a
platform freeze the controller stores the rendered objects and shows the plan, but nothing
starts until the freeze ends.

### 4.6 Delivery

**kustomize.** A base (`config/platform/`) holds `platform.yaml` and its release files. An
environment overlay replaces the ConfigMap with its own copies of those files (kustomize
files may not leave their root, so overlays copy them).

```yaml
# config/platform/kustomization.yaml (base)
configMapGenerator:
- name: cluster-vending-platform
  files:
  - platform.yaml
  - 2026-12-a.yaml

# overlays/prod/kustomization.yaml
resources:
- ../../config/platform
configMapGenerator:
- name: cluster-vending-platform
  behavior: replace
  files:
  - platform.yaml
  - 2026-12-a.yaml
```

Both Deployments get the same mount and argument (a patch in each base or overlay):

```yaml
args: ["--config=/etc/cluster-vending/platform.yaml"]
volumeMounts:
- { name: platform, mountPath: /etc/cluster-vending, readOnly: true }
volumes:
- name: platform
  configMap: { name: cluster-vending-platform }   # kustomize rewrites this to the hashed name
```

Because kustomize rewrites the reference to the generated name, a changed file changes the
pod template and both processes roll (section 4.3). The ConfigMap is limited to 1 MiB by
Kubernetes, which is ample for this file; large custom definitions go in their own keys.

**Helm.** There is no Helm chart in this repository today. A chart would expose this
values block, and the snippet below is the contract it must meet:

```yaml
# values.yaml
platform:
  config:                  # rendered verbatim as platform.yaml in ConfigMap <release>-platform
    nodeGroups:
      system: { minSize: 3, maxSize: 9 }
    components:
      external-secrets: { sequence: 150 }
    channels:
      beta: { policy: { maxInProgress: 3 } }
    releases:
      2026-12-a:
        serial: 12
        content: { kubernetes: [] }    # inline, in place of file:
```

```yaml
# templates/deployment.yaml (pod template, both processes)
metadata:
  annotations:
    checksum/platform: {{ toYaml .Values.platform.config | sha256sum }}
```

Secrets are not in either delivery. Helm repository credentials are Secret references in
the file, and the Secret is created outside the chart.

### 4.7 Read-only view

The console gets one read-only **Configuration** page for platform admins: the loaded file
(it holds no secrets), its SHA-256, its validation result and the controller's applied hash.
It is fed by a read-only `GET /config` on `serve` (PlatformAdmin). It is not an RPC and
accepts no writes.

### 4.8 Migration

1. **Node groups** move unchanged from the two config files into `platform.yaml`. Same keys,
   same checks.
2. **Environment duplicates are removed.** The `CV_SYSTEM_NODE_GROUP_*` and
   `CV_NODE_GROUP_*` variables in `config/manager/manager.yaml` and in the server base's
   `configMapGenerator` would override the file. The `hack/*.local.yaml` files and the
   Makefile's `--config` targets (`run`, `run-controller`) point at the same file.
3. **Auth merges**: `config/server/auth/server.yaml` becomes a section of `platform.yaml`,
   since `--config` takes one path.
4. **Shipped definitions and `release.yaml` are the base.** `release.yaml` gets its `serial`.
   `pkg/catalog.Sync`'s hash rules, `takeOverAlpha` and the "edited" state are removed in
   one release. On its first start the controller renders everything from base and file.
5. **Export of today's state**: `cluster-vending controller --check --kubeconfig ... --export`
   prints a `platform.yaml` that reproduces the live objects:
   - definitions that differ from the build, as overlay entries where the file can express
     the difference (`disabled`, `sequence`, `platformValues`, `install.timeout`);
   - the differences the file cannot express (chart source, IAM policy, schema, removed
     `requires`, `description`) listed under `# not representable`. They are reverted to
     the build on the first render, with `ConfigDriftReverted`. The karpenter incident is
     one of these, so the build's IAM policy reaches clusters on that first render. The
     export is reviewed before the first deploy of the new controller;
   - the channels' current `spec` (so `alpha`, `beta` and `stable` keep their settings);
   - the releases not from the build, with `serial` backfilled in creation order (ties by
     name), as phase 1 does.
6. **Cutover**: deploy the file that equals the export, with the new controller. The first
   pass is a no-op for everything the file expresses, so no cluster is rolled.

## 5. Alternatives considered

| Alternative | Why not |
|---|---|
| **Dynamic admin API, a `PlatformConfig` CRD, overlay fields on `ComponentDefinition`, admin CLI and console editing** (the previous version of this proposal) | Rejected on the user's direction: configuration is GitOps, with fewer moving parts and no runtime mutation surface. Runtime writes need an authorization surface on every RPC, an audit trail outside Git, and a second source of truth (the CR) that can disagree with what is reviewed. The file in Git gives review, history, diff and rollback. |
| Keep the hash model, add an "accept build" action | Still freezes a definition on any edit, and the admin must notice and act for every build. |
| Overlay fields on the CRD (base and overlay in one object) | Two writers and a new CRD shape for the same answer. Rendering keeps the CRDs as they are. |
| The controller watches the mounted file and re-renders live | A second code path; a partially changed file can take effect; the server still restarts for its own validation; ConfigMap propagation is slow and outside our control. Restart on change was chosen (section 4.3). |
| Store the admin settings in a ConfigMap that the controller reads as its state | The ConfigMap is only the transport. The CRs stay the state the controller reconciles, and hand edits of the CRs are reverted rather than read from elsewhere. |
| Node groups in separate config files, shared by convention | This is today: the two files drift (section 2.2). |
| Per-channel node group settings | Couples version promotion with node policy. Channels decide versions; node policy is the platform's. |
| Custom channels | Touches the contract's channel enum, the release's channel enum and the fixed promotion chain, for no requested use case. |
| Order releases by name | Shipped releases are content-hash named. An explicit serial orders both kinds. |
| Apply catalog changes to all clusters at once | No tiers or soak; one bad values overlay reaches every cluster. |

## 6. Rollout plan

Each phase is shippable on its own and leaves the system consistent.

### Phase 1: Release serial ordering

- `Release.spec.serial`; `release.yaml` gets `serial` for the shipped release; backfill of
  existing releases in creation-time order (ties by name); `ListReleases` and `notNewer`
  use the serial; `serial` shown in `admin release list`.
- **Accepted when**: re-creating a release object does not change the order or a release
  move's decision.
- **Tests**: backfill (ties, missing timestamps); `notNewer` on serials; list ordering; the
  CEL rule that a serial is immutable once set.

### Phase 2: Platform file and one loader

- `platform.yaml` sections parsed by `pkg/config` (`components`, `releases`, `channels`
  added to `nodeGroups` and `auth`); `--check` in the controller command; the start-time
  validation in both processes; the environment-override warning; the readiness gate;
  restart-on-change through the generator hash and the Helm checksum; the duplicated
  environment variables removed; the auth file merged; the kustomize bases and `hack/` files
  switched to the one file.
- **Accepted when**: one file drives both processes; `maxSize` changed in the file rolls both
  pods and reaches the clusters through the code-change rollout; an invalid file fails the
  new pod's readiness and leaves the old pod leading; `--check` exits 1 with every problem
  listed.
- **Tests**: loader tests for the new sections; table tests of every offline rule in 4.4;
  the server and the controller reject the same file; no base sets a `nodeGroups` variable.

### Phase 3: Render the components

- The controller renders each ComponentDefinition from the build's base and the file's
  overlay; the `shipped-hash` logic and "edited" state are removed; drift reverted with
  `ConfigDriftReverted`; `--export` for components.
- **Accepted when**: the `karpenter` incident's definition returns to the build on the first
  render; a `description` edit made with `kubectl` is reverted within one pass, with an
  Event and the metric; a removed overlay returns the build's value.
- **Tests**: base and overlay merge tables; a drift regression test for the description
  edit; export round-trip (export, render, no change).

### Phase 4: Catalog changes and the plan

- The catalog generation and `status.catalog` on clusters; `ReleaseChannel.status.catalogChanges`
  in the planner; the Karpenter and requires/sequence guardrails; `--check --plan`.
- **Accepted when**: enable, disable, sequence, and values changes work end to end on the
  fake API server, in tier order, respecting freeze, gates and halts; a disable of a
  required component is refused unless the file disables the dependent too.
- **Tests**: planner tests (tiers, soak, halt, Manual clusters included); validation tables
  (cycles, requires vs sequence, Karpenter, bundle members); `--plan` output tests.

### Phase 5: Releases and channels in the file

- Release files and inline content; the release renderer's checks; `--charts` in CI; the
  serial checks against the cluster; the removal guards; channel settings rendered; the
  `takeOverAlpha` logic removed; export of channels and releases.
- **Accepted when**: an admin adds a release file, points `beta` at it with `Manual`, and
  the rollout moves clusters by tier; a release used by a cluster cannot be removed; a
  build's new shipped release does not move `alpha` while the file names a release.
- **Tests**: release validation (pins, schema, `renderVersion`, serial order); channel rules;
  a forward rollback release moved by a release move.

### Phase 6: Read-only configuration page

- `GET /config` on `serve`; the console Configuration page.
- **Accepted when**: the page shows the loaded file, its hash, its validation result and the
  controller's applied hash; non-admins get no page and no data.
- **Tests**: handler authorization test; a Playwright check at 320, 768, 1024 and 1440.

### Phase 7: Per-tenant Karpenter overrides

- `nodeGroups.tenantOverrides` (at most 200 entries), additive to the platform's allowlist,
  for the Karpenter allowlist and limits only (the system group stays platform-wide).
- **Accepted when**: a tenant with an override can create a `g5` group and another tenant
  cannot; removing an override marks that tenant's groups `NodeGroupOutsideAllowlist`.
- **Tests**: merge and validation tests, including removal of an override.

## 7. Open questions

1. **Manual clusters and catalog changes.** Catalog changes (disable, values) reach Manual
   clusters in tier order, as proposed. Or should Manual clusters take them only on their
   next tenant-initiated upgrade?
2. **Tenant components on a disabled definition.** Proposed: they stay installed and are
   marked `Degraded`. Should a disable optionally uninstall them?
3. **Instance type change of the system group.** New clusters only today. Should the file
   be able to roll a replacement group through the tiers (the reconciler can replace a
   group)?
4. **Chart existence check.** It runs in CI with `--charts`, not at controller start, so the
   management cluster does not depend on chart repositories to apply a file. Is outbound
   access from CI to every repository acceptable?
5. **Drift: revert or report?** Proposed: revert at once, with an Event. A report-only
   period for the first release of this design would show the drift that exists today
   before anything is reverted. Is that worth the extra mode?
6. **Audit.** Git is the record of who changed what and why. Should rendered objects also
   carry the commit revision, set by the GitOps tool or by a `--config-revision` flag, so a
   cluster object links to its commit?
7. **Read-only endpoint.** `GET /config` (section 4.7) is the one addition to the server's
   HTTP surface. Is it acceptable, or should the page read what the existing read RPCs
   already return?
8. **File wins over the build for channel pointers.** Proposed: when the file names a release
   for `alpha`, a new build does not move it. Confirm.
