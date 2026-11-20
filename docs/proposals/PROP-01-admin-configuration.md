# PROP-01: Platform catalog as manifests (components, releases, channels)

| | |
|---|---|
| Status | Proposed |
| Author | `<author>` |
| Date | 2026-11-20 |
| Reviewers | `<reviewers>` |
| Related | [architecture.md](../architecture.md): 2. What we have, 3. Platform components, Releases, channels and rollouts, Helm operations, 4. CR definitions, 5. Configuration, Node groups configuration |

## 1. Summary

The platform's component definitions, releases and channels become plain Kubernetes
manifests in Git. They are delivered as a kustomize base (`config/catalog/`) and as Helm
chart templates, or applied by Argo CD or Flux. An admin customizes them with kustomize
overlays or Helm values. The server and the controller only read these objects.

- The binary keeps the code that understands the three kinds and the checks. It no longer
  contains catalog content, and it writes no `spec` of these kinds.
- Removed from the platform: the embedded catalog sync at start (`catalog.Install`,
  `catalog.Sync`), the shipped label and shipped-hash "edited" logic, `CreateReleaseDefaults`
  and `takeOverAlpha`, every runtime read of `release.yaml`, and the controller's write verbs
  on these kinds. No controller code renders a definition.
- Drift is owned by whatever applies the manifests (a GitOps tool, `kubectl apply`, or
  `helm upgrade`). The controller does not revert or report hand edits. No admin API, no
  `PlatformConfig` kind, no runtime write surface.
- Node groups and the Karpenter allowlist stay in the server and controller config file,
  delivered by the same kustomize or Helm path. Its `nodeGroups` section is the only
  platform setting outside the manifests.
- Validation has three layers: CEL at apply time, an offline check mode of the existing
  binary over rendered manifests (CI and pre-deploy), and runtime checks on the live objects.
  An invalid object gets a condition and an event, and the controller does not act on it
  (section 4.6).
- Changes that reach running clusters use the existing rollouts. Disable and value changes
  of platform components become gated rollouts too (section 4.7).

Added: `Release.spec.serial`, CEL rules, a schema annotation, a check mode and a one-time export
on the `controller` command, a last-valid view in the controller, and a Helm chart (the repository
has none today).

## 2. Problems today

Checked against `main`.

### 2.1 Definitions: the hash model freezes them

- `pkg/catalog/catalog.go` (`Sync`) records `cloud.cluster-vending.io/shipped-hash`. A
  definition whose spec changed since is "edited" and no later build updates it. In the
  incident behind this proposal, a `description` edit on `karpenter` blocked a build that fixed
  Karpenter's IAM policy. The only record was a log line at start.
- Disable is ungated. A platform component is removed from every cluster on the next pass,
  with no tiers and no soak (architecture.md, Catalog sync). Definitions that require it are
  switched off with it.
- `spec.sequence` has a CRD minimum of 0 and no maximum. `install.timeout` has no bounds. No
  CRD rule protects `karpenter` (sequence 0, enabled). The sequence-0 rule exists only in the
  shipped data and its tests.

### 2.2 Releases and channels come from the build and from runtime code

- `pkg/catalog/release.yaml` is embedded. The controller turns it into `Release` and
  `ReleaseChannel` objects (`pkg/controller/release_defaults.go`). A new version needs a new build.
- Release order is the creation time. `ListReleases` (`pkg/server/admin/releases.go`) sorts by
  `CreationTimestamp`, and `pkg/reconcile/cluster/release_move.go` compares creation timestamps
  (line 285).
- `takeOverAlpha` moves `alpha` to a new shipped release on upgrade when nobody touched it.
  The admin does not see that a build changed their channel. Channel edits are `kubectl` edits
  with no check before they apply.
- `shippedAMIPins` (`pkg/reconcile/cluster/nodegroups.go`) reads `release.yaml` at runtime.

### 2.3 Node groups are set in four places

`config/manager/manager.yaml` (the `CV_SYSTEM_NODE_GROUP_*` and `CV_NODE_GROUP_*` variables),
the `configMapGenerator` of `config/server/base/kustomization.yaml`, `hack/config.local.yaml`
and `hack/controller.local.yaml`. Nothing checks that they agree. Environment variables
override the file (`pkg/config/file.go`).

### 2.4 Corrections to the previous revision

- `CheckSystemGroupSizing` does not exist. The sizing rule is a test
  (`pkg/catalog/system_group_sizing_test.go`). "Sequence 1 to 10000" and "install timeout 1 m to
  60 m" are new rules, not current ones.
- Tier0 `maxInProgress` is always 1 in the rollout code, so it is not a spec error.
- A `Release` used by a cluster is not guarded: `release-in-use` covers channels and holds only.
- `admin release list` shows a `Shipped` field taken from the label
  (`pkg/server/admin/release_convert.go`, line 88).

## 3. Goals and non-goals

### Goals

1. Catalog content is manifests in Git, applied by a tool that owns drift.
2. The server and the controller only read these objects. No code path writes their `spec`.
3. Every change is a patch, a new manifest or a values entry, reviewed as a diff.
4. Invalid content is refused offline with every problem listed. The controller never acts on
   an invalid live object.
5. Changes that reach clusters go through the existing rollouts, including disable and values.
6. One `nodeGroups` section, read by both processes, with no duplicated environment variables.

### Non-goals

- Admin write RPCs, a `PlatformConfig` kind, admin commands that mutate state, console editing.
- Runtime reload, custom channels, several versions of a component per minor.
- Tenant node group overrides and `nodeGroups.limits` or `amiPolicy`: separate proposals.
- Freeze in manifests. `admin freeze` stays a runtime switch.

## 4. Design

### 4.1 Who writes what

| Object | Written by | Read by | Controller writes |
|---|---|---|---|
| `ComponentDefinition` (spec) | Manifests: kustomize, Helm, or GitOps | Controller, server (`pkg/management/platform.go`) | Finalizer and status only |
| `Release` (spec) | Manifests | Controller, server | Status only |
| `ReleaseChannel` (spec) | Manifests | Controller, server | Status only |
| `ReleaseHold` | Unchanged: created by the admin commands, as today | Controller | Status only |
| Node group settings | The config file, through the same delivery | Controller and server, identically | Nothing |
| Tenant `Component` | The API, unchanged | Controller | Status |

### 4.2 Layout

```text
config/catalog/
  kustomization.yaml    # the base: resources and commonAnnotations (catalog-schema)
  definitions/          # one ComponentDefinition per file (moved from pkg/catalog/definitions)
  releases/             # one Release per file
  channels/             # alpha.yaml, beta.yaml, stable.yaml
```

- Definitions keep their names. Existing releases keep their `shipped-<hash>` names. New
  releases are named by hand as `YYYY-MM-<letter>`, which the CRD already accepts.
- Each channel names its release explicitly. No build moves a channel.
- A `Release` file holds the full `spec.content` (minors and pins). The `release.yaml`
  shorthand is retired. `ContentOf` and the NaaS bootstrap expansion
  (`pkg/catalog/bootstrap.go`) run once, when a release is written, not at runtime.

### 4.3 Changes an admin makes

| Change | Write | Applies to clusters |
|---|---|---|
| Disable a platform component | Overlay patch: `spec.disabled: true` | Gated uninstall (4.7) |
| Reorder | Patch `spec.sequence` | New clusters, later moves, deletion order. No reinstall. |
| Values, timeout | Patch `spec.platformValues` or `spec.install.timeout` | Gated code-change rollout (4.7) |
| Add a custom component | New `ComponentDefinition` file | New clusters, then tenants may enable it |
| Add a release | New `Release` file with `spec.serial` above every other serial | Nothing until a channel names it |
| Promote | Patch `ReleaseChannel`: `spec.release` (alpha). For beta and stable, `promotion: Manual` plus `release`, since CEL forbids `release` on an `Automatic` channel | Existing release move, by tier and soak |
| Node group settings | The config file (4.4) | Per the table in 4.7 |
| Remove a definition or release | Delete its file; GitOps prune or `kubectl apply --prune` | Definition: held by `definition-in-use` until no component uses it. Release: refused while a cluster or channel uses it (4.5). |

### 4.4 Node groups and the Karpenter policy

The config file already read by `serve` and `controller` keeps its `nodeGroups` section, with
the keys of `pkg/config/node_groups.go` and no new keys:

```yaml
nodeGroups:
  system:                 # new clusters: instanceType, capacityType, diskSizeGiB. In place: minSize, maxSize.
    instanceType: t3.medium
    minSize: 2
    maxSize: 6
    capacityType: ON_DEMAND
    diskSizeGiB: 0
  karpenter:              # what tenants may create
    instanceFamilies: [t3]
    instanceTypes: []
    defaults:
      architectures: [AMD64]
      capacityTypes: [ON_DEMAND]
```

- Delivery: a kustomize `configMapGenerator` (its name carries a content hash, so both
  Deployments roll) or a Helm ConfigMap with a checksum annotation on both pod templates.
- The `CV_SYSTEM_NODE_GROUP_*` and `CV_NODE_GROUP_*` variables are removed from
  `config/manager/manager.yaml` and `config/server/base/kustomization.yaml`. The `hack/*.local.yaml`
  files point at the same file. A variable that still sets one of these keys logs a warning at start.
- Karpenter's sequence and enablement are not in this file. They are manifest fields, checked
  by the rules in 4.5.

### 4.5 Validation

Three layers. Each rule has one implementation, shared by the offline and the runtime checks.

**CEL, at apply time (new rules).**

- `ComponentDefinition.spec.sequence`: schema maximum 10000.
- `ComponentDefinition.spec.install.timeout`: between 1 minute and 60 minutes (`duration()`).
- The definition named `karpenter` has sequence 0 and is not disabled.
- `Release.spec.serial`: at least 1, and cannot change once set (a transition rule on
  `oldSelf`).
- `ReleaseChannel.spec.policy.releaseSoak` is set only for an `Automatic` channel.

**Already in the CRDs (kept).** DNS-label names; a definition does not require itself;
exactly one of `install` and `includes`; `namespace` with `install`; a bundle is not internal;
`provider` immutable; the release name pattern; release content immutable; a withdrawn release
stays withdrawn; exactly one default minor; 3 to 6 minors, each matching `1.NN`; `alpha` is not
`Automatic`; channel names `alpha`, `beta` and `stable`; `release` set only for `Pinned` and
`Manual`.

**Offline check mode (`cluster-vending controller --config <file> --check -f -`).** It reads
the rendered manifests from `-f`, makes no cluster call, and exits 1 with every problem and its
path listed. The rules:

- The three channels exist, once each. Each channel's minor is on exactly one minor.
- Every `requires` target exists, and the `requires` graph has no cycle (`pkg/component`,
  `pkg/release`, existing).
- For each `requires` edge A to B, sequence(B) <= sequence(A). This rule is new.
- No other definition has sequence 0.
- A disabled definition that an enabled one requires is an error, unless the dependent is
  disabled too (see open question 2).
- Release serials are unique, and each is above every other serial in the set (an overlay's
  new release must be above the base's).
- Release minors are consecutive, `upgradeTo` names a minor on offer, and each channel is on
  one minor (`pkg/release`, existing).
- Every enabled platform and tenant component has a pin on every minor of every release
  (`pkg/release`, existing).
- A `ReleaseChannel.spec.release` names a `Release` in the set.
- Each platform definition passes the checks of its `Valid` condition: bundles, cloud and chart
  rules, and `userValues` against `valuesSchema` (`pkg/component`, existing).
- System group sizing: the requests of enabled platform components, add-ons and DaemonSets
  against 85 % of the minimum system group, using `nodeGroups.system` from the config file. The
  rule of `system_group_sizing_test.go`, moved into code.
- `--charts` (CI only): each chart version exists in its repository.

The offline check cannot see the cluster. The release-in-use rule and the checks against live
objects run only at runtime.

**Runtime, on the live objects.** The controller runs the same functions over the objects in the
cluster, pass by pass for the objects that changed. It adds the cluster-only rules:

- A `Release` used by a cluster (`status.platformRelease`, a release move's target, a hold) or
  named by a channel cannot be removed. This extends the `release-in-use` finalizer, which covers
  channels and holds today.
- A `Release`'s serial is unique against the serials in the cluster.
- A channel's `release` exists in the cluster.

### 4.6 When an object is invalid at runtime

Validity is computed per object and for the set. An invalid object gets:

- On a definition or release: `Valid=False` with a reason. On a channel: `Offering=False`. The
  reasons are `Invalid`, `Inconsistent`, `ReleaseMissing`, `SchemaNewer` and `SchemaOld`.
- A Warning event on the object, and the metric `cluster_vending_catalog_invalid_objects` by kind.

The controller keeps, in memory, the last spec of each object that passed (the **last valid
view**). After a restart, an object that is invalid at that moment has no view.

What "does not act on it" means:

- **Installs.** An install never starts from an invalid definition. It uses the last valid view
  of this process. With no view, the component stays `Pending`, with the reason in the cluster's
  component status. Other components go on.
- **Moves** (release move, code change, Kubernetes upgrade of platform components).
  - Invalid definition: the step for that component does not start. The move halts at that
    component with reason `TargetInvalid`. Components already moved stay where they are. A
    `cluster retry` continues once the object is fixed. There is no rollback.
  - Invalid release: the channel keeps offering the release it last offered validly. A move
    toward the invalid release does not start. A new cluster on that channel gets the last valid
    release. With none, the channel offers nothing and cluster creation waits, with the reason
    on the channel.
  - Invalid channel: no new cluster starts or moves under it. Clusters in the middle of a step
    finish it. The channel keeps its last valid settings.
- **Deletes.** An invalid object never holds a deletion. The uninstall uses the last valid view.
  Without one, it uses the cluster's recorded inventory (`status.platformComponents`,
  `status.componentInventory`). The `definition-in-use` hold is not a validity question and is
  unchanged.
- **Karpenter.** If the `karpenter` definition is invalid and has no view, no platform component
  installs or moves on any cluster, because all of them wait behind sequence 0. Each cluster's
  `PlatformComponentsReady` reports reason `CatalogInvalid`.
- **Node group config.** An invalid file stops the process from starting (today's behaviour). The
  old pods keep serving and the rollout stops.
- **Isolation.** One invalid object does not block an unrelated object.

### 4.7 Rollout to existing clusters

| Change | Effect on clusters | Rollout |
|---|---|---|
| Platform component disabled, enabled, added or removed; values, timeout, requires | The effective definition changes | New (phase 5). Each cluster records a catalog fingerprint: a hash of the effective definitions its release uses. A change is a pending catalog change, admitted by the existing code-change rollout: tiers Tier2, Tier1, Tier0, `maxInProgress`, soak, maintenance windows, the platform freeze, and a halt past `maxFailed`. Disable uninstalls, highest sequence first. |
| Sequence only | None now | No reinstall. It applies to new clusters, later moves and deletion order. Shown in the plan. |
| Release added, or serial | None | Nothing until a channel names it. Then the existing release move. |
| Channel `release` or `promotion` | The channel offers a new release | Existing: Kubernetes rollout, then release moves. |
| Channel `policy`, `paused` | Next pass | `paused` starts nothing new. |
| `nodeGroups.system` `minSize`, `maxSize` | In place | Existing code-change rollout. |
| `nodeGroups.system` `instanceType`, `capacityType`, `diskSizeGiB` | New clusters only | Not rolled out (EKS cannot change them in place). |
| `nodeGroups.karpenter` defaults | New groups, and groups that leave the field unset | Existing code-change rollout. |
| `nodeGroups.karpenter` allowlist | Nothing is removed | Validation for new requests. Existing groups outside it are reported. |

The code-change fingerprint covers the cluster's Crossplane objects (`pkg/reconcile/cluster/code_change.go`).
Whether it covers platform component Helm releases is unconfirmed (open question 4).

### 4.8 Compatibility

- Each catalog object carries `cloud.cluster-vending.io/catalog-schema: "1"`. The base sets it with
  `commonAnnotations`, and the export adds it. A missing annotation makes the object invalid.
- The binary reads its own schema and the one before. A newer schema is invalid
  (`SchemaNewer`, the last view stays). An older one than the previous is invalid (`SchemaOld`,
  re-export it).
- Apply order does not matter. A new catalog on an old controller is refused, and a new
  controller reads old catalog objects.
- A field the installed CRD lacks is pruned by the API server, so the object reads as missing it
  and is invalid. The CRDs apply before the catalog: kustomize resource order, an Argo sync wave,
  or Helm's `crds/` directory.
- A platform version moves as one unit (controller image, CRDs, catalog base), from one kustomize
  ref or one chart version. The schema changes only when a field's meaning changes or a required
  field is added.

### 4.9 Delivery

**kustomize.** An environment overlay pins the platform version, patches what it changes, and
adds its own releases:

```yaml
# overlays/prod/kustomization.yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
- github.com/<org>/cluster-vending//config/catalog?ref=<platform-version>
- releases/2026-12-a.yaml                     # a release for this environment
patches:
- target: {group: cloud.cluster-vending.io, version: v1alpha1, kind: ComponentDefinition, name: kyverno}
  patch: |-                                   # disable
    apiVersion: cloud.cluster-vending.io/v1alpha1
    kind: ComponentDefinition
    metadata: {name: kyverno}
    spec: {disabled: true}
- target: {group: cloud.cluster-vending.io, version: v1alpha1, kind: ComponentDefinition, name: external-secrets}
  patch: |-                                   # sequence change
    apiVersion: cloud.cluster-vending.io/v1alpha1
    kind: ComponentDefinition
    metadata: {name: external-secrets}
    spec: {sequence: 150}
- target: {group: cloud.cluster-vending.io, version: v1alpha1, kind: ReleaseChannel, name: beta}
  patch: |-                                   # promote beta to the new release
    apiVersion: cloud.cluster-vending.io/v1alpha1
    kind: ReleaseChannel
    metadata: {name: beta}
    spec: {promotion: Manual, release: 2026-12-a}
```

`releases/2026-12-a.yaml` is a `Release` with `spec.serial: 12`, `notes`, and the full
`spec.content`, in the shape of the base's releases.

**Helm.** The chart does not exist yet (phase 6). Its `values.yaml` holds the base, generated
from `config/catalog`. Site values deep-merge over it, and `null` removes an entry:

```yaml
# site values
catalog:
  definitions:
    kyverno: {spec: {disabled: true}}
    external-secrets: {spec: {sequence: 150}}
    gpu-operator: null                         # removes the entry from the base (a tenant component)
  channels:
    beta: {spec: {promotion: Manual, release: 2026-12-a}}
  releases:
    2026-12-a: {serial: 12, notes: "karpenter 1.16.0", content: {}}   # content as in the base
nodeGroups:
  system: {minSize: 3, maxSize: 9}
```

```yaml
# templates/catalog/definitions.yaml
{{- range $name, $def := .Values.catalog.definitions }}
---
apiVersion: cloud.cluster-vending.io/v1alpha1
kind: ComponentDefinition
metadata:
  name: {{ $name }}
  annotations:
    cloud.cluster-vending.io/catalog-schema: "1"
spec:
  {{- toYaml $def.spec | nindent 2 }}
{{- end }}
```

Releases and channels are templated the same way. The config file is a ConfigMap rendered from
`nodeGroups`, with its checksum on both pod templates. Credentials are Secret references, and the
Secrets are created outside either delivery.

**CI and pre-deploy.** The same check runs on the rendered output:

```text
kustomize build overlays/prod | cluster-vending controller --config platform.yaml --check -f -
helm template cluster-vending charts/cluster-vending -f site.yaml | cluster-vending controller --config platform.yaml --check -f -
```

**Platform upgrades.** Bumping the kustomize ref or the chart version brings a new base. A
release the overlay does not mention arrives as a new file in the diff. A base change to a field
the overlay patches does not show in the result, because the overlay's value wins. The review
diff of the bump shows that change, so the admin decides whether to keep the overlay's value.

### 4.10 Migration

1. **Phase 0 adds the export.** `cluster-vending controller --export --kubeconfig <mgmt>` only
   reads. It writes `config/catalog` from the live objects, not from the build, adds the schema
   annotation, and backfills `serial` in creation order (ties by name). On stderr it lists each
   difference from the build's embedded catalog, such as the `karpenter` description edit.
2. **Review.** Intended differences move into overlays. Unintended ones are dropped later, by a
   visible rollout after cutover, not at cutover. The build's Karpenter IAM policy reaches
   clusters only when that change is taken.
3. **Cutover, in one deploy.** The new controller and the catalog objects are applied together
   (one sync or one `helm upgrade`). The old `Sync` must never run next to the new objects.
4. **Labels.** The `shipped` label and `shipped-hash` annotation are harmless afterwards. The export
   strips them.
5. **Existing clusters keep running.** Names are kept, and the export is the live state, so no
   cluster reconciles differently at cutover.
6. **Cleanup.** `--install-catalog` is accepted and ignored with a warning in the cutover release,
   and removed in the next one, together with `--export`.

## 5. Simplification

**Removed.**

| Where | What goes |
|---|---|
| `pkg/catalog/catalog.go` | The whole file: `Sync`, `syncOne`, `SpecHash`, `asShipped`, `isShipped`, `unedited`, `Result`, `ShippedHashAnnotation`. |
| `pkg/catalog/definitions.go` | The embedded `definitions/`, `Install`, `Definitions`, `Raw`. The strict decoding moves to the validator. |
| `pkg/catalog/definitions/*.yaml` | Moved to `config/catalog/definitions/`, content unchanged. |
| `pkg/catalog/release.yaml`, `pkg/catalog/release.go` | The shorthand `Release`, `KubernetesVersion`, `Channel`, `ShippedRelease`, `ParseRelease`. The checks move to the validator. |
| `pkg/catalog/bootstrap.go` | `CapsuleBootstrapValues` at runtime. Its output is written into the release manifest. |
| `pkg/controller/release_defaults.go` | The whole file: `ShippedReleaseName`, `isShipped`, `DefaultRelease`, `ContentOf`, `DefaultChannels`, `CreateReleaseDefaults`, `asShipped`, `takeOverAlpha`, `releaseDefaults`. |
| `pkg/controller/release_controller.go` | `mgr.Add(releaseDefaults{...})`, and the create and update markers on releases and channels. |
| `pkg/controller/manager/manager.go` | `catalog.Install` and its logs (about lines 171 to 186). |
| `pkg/config/controller.go` | The `install-catalog` setting: flag, variable and field. Deprecated, then removed. |
| `pkg/reconcile/cluster/nodegroups.go` | `shippedAMIPins`, the runtime read of `release.yaml` (open question 1). |
| `pkg/server/admin/release_convert.go` | The `Shipped` output field (a contract change in `idl/cloud/v1`). |
| `config/rbac` (generated) | `create` and `delete` on `componentdefinitions`; `create` on `releases` and `releasechannels`. `update` and `patch` stay, for status and finalizers. |
| `config/manager/manager.yaml`, `config/server/base/kustomization.yaml` | The duplicated `CV_*NODE_GROUP*` variables. |
| Tests | `pkg/catalog/catalog_test.go`, `update_test.go`, `bootstrap_test.go`; `pkg/controller/release_defaults_test.go`, `release_bootstrap_test.go`. Also `release_shipped_test.go` and `platform_shipped_test.go`, whose content is to be checked. |

**Stays.**

- The API types and CRDs in `api/v1alpha1`, with the CEL of 4.5.
- The checks in `pkg/release` and `pkg/component` (pins, minors, channels, requirements, cycles,
  `Valid`). They run offline and at runtime.
- The `definition-in-use` and `release-in-use` finalizers, and the `Valid` and `Offering`
  conditions.
- The release, channel and hold controllers, the rollouts, moves and code changes.
- `pkg/config/node_groups.go` and the config loader.
- `pkg/management/platform.go` (`PlatformSource`), read-only, used by the server.

**New.**

- The validator in `pkg/catalog` (offline and runtime rules), with the `--check`, `-f` and
  `--export` flags on `controller`.
- The last-valid view and its reasons, in `pkg/controller`.
- The catalog-schema reader.
- The CEL markers of 4.5.
- The catalog fingerprint (phase 5) and the Helm chart (phase 6).

The controller stops writing definitions and releases at start.

## 6. Alternatives considered

| Alternative | Why not |
|---|---|
| **(a) Dynamic admin API, a `PlatformConfig` CRD, admin CLI and console editing** | Rejected on direction: configuration is GitOps, with fewer moving parts. A runtime write surface needs authorization on every RPC, an audit trail outside Git, and a second source of truth that can disagree with what was reviewed. |
| **(b) One `platform.yaml` rendered by the controller** (the previous revision) | Rejected: two sources of truth (the file and the objects), and the controller becomes a writer with its own merge and drift logic. |
| **(c) Keep the embedded catalog and add overlays** | Rejected: the binary stays the catalog's source, every change needs a build, and the overlay must merge at runtime. That is the model with the freeze problem in 2.1. |

## 7. Rollout plan

Each phase ships on its own and leaves the system consistent.

### Phase 0: Export, check mode and CEL

- `controller --export` (read-only) and `controller --check -f`. The rules of 4.5 in the
  validator, reusing `pkg/release` and `pkg/component`. The sizing rule moves from the test into
  code. The CEL rules of 4.5 in the CRD markers.
- **Accepted when**: an export of a live cluster passes the check. `--check` exits 1 and lists
  every problem. CEL rejects a sequence of 10001, and `karpenter` with sequence 100.
- **Tests**: table tests per rule. An export round trip: export, check, apply to a fake client,
  no diff.

### Phase 1: Release serial

- `Release.spec.serial`, immutable once set. `ListReleases` and the creation-order comparison in
  `release_move.go` use the serial. `serial` is shown in `admin release list`.
- **Accepted when**: re-creating a release does not change the order or a move's decision.
- **Tests**: backfill with ties and missing timestamps, comparison on serials, list order, the
  transition rule.

### Phase 2: Node groups in one file

- One config file for both processes. The duplicated variables are removed from
  `config/manager/manager.yaml` and the server base. The `hack/` files point at the same file.
  Variables that still set these keys warn.
- **Accepted when**: a `maxSize` change in the file rolls both pods. No base sets a `nodeGroups`
  variable. The server and the controller reject the same file.
- **Tests**: the loader on the same file for both commands. The warning for a leftover variable.

### Phase 3: Cutover to manifests

- `config/catalog/` with the definitions, releases and channels from the export. The removals of
  section 5. The export step of 4.10.
- **Accepted when**: a fresh install from `config/catalog` creates the same objects. The
  cutover of an existing management cluster changes no rendered object (the fingerprints match).
  The controller's RBAC has no create or delete on these kinds.
- **Tests**: a fresh install test. A cutover test that the export of a live state is a no-op. A
  test that the controller issues no write to a definition's or release's spec.

### Phase 4: Runtime validity

- The last-valid view. The reasons and conditions of 4.6. The events and the metric. The
  catalog-schema check. The release-in-use guard extended to clusters.
- **Accepted when**: an invalid definition edit leaves installs `Pending` and moves halted with
  `TargetInvalid`, and deletes complete. A restart with an invalid object behaves as in 4.6.
- **Tests**: a table with one row per row of 4.6, on the fake client.

### Phase 5: Gated catalog changes

- The catalog fingerprint per cluster. Disable, sequence and values changes admitted by the
  code-change rollout. The `requires` and disabled-dependent rules of 4.5 in the offline check.
- **Accepted when**: a disable of a platform component on a three-tier fake fleet uninstalls
  in tier order, with soak, halt and freeze respected. A disable that leaves an enabled dependent is
  refused offline.
- **Tests**: planner tests with tiers, Manual clusters, halt and freeze. `--check` tables for
  `requires`, cycles and sequence 0.

### Phase 6: Helm chart and CI

- `charts/cluster-vending` with the catalog and the config file as values and templates. Checksum
  annotations. CI runs the check on `helm template` output. `--charts` runs in CI.
- **Accepted when**: `helm template | --check` passes on the base, and a `null` in site values
  removes a definition.
- **Tests**: a golden render of the chart, checked as above.

## 8. Open questions

**Decided (2026-11-20):**

- **AMI pins of old releases:** fall back to `al2023@latest` with the existing notice; no
  constant table.
- **Dependents of a disabled component:** keep today's cascade; a disable switches its
  dependents off with it. The "refuse the disable" rule of 4.5 is dropped.
- **Manual clusters:** take catalog changes in tier order, like Automatic ones.
- **Helm chart:** out of scope for now; kustomize only. Phase 6 is deferred.

The questions below are kept for the record; the four above are closed.

1. **AMI pins of old releases.** `shippedAMIPins` (`pkg/reconcile/cluster/nodegroups.go`) fills
   in the pins of releases published before `nodeAmiPin` existed, from `release.yaml`. That file
   goes. Choose: a constant table of those pins in code, or accept `latest` with the existing notice.
2. **Dependents of a disabled component.** Today a dependent is switched off with it
   (architecture.md). This proposal refuses the disable unless the dependent is disabled too. The
   behaviour changes, so confirm it, or keep the cascade.
3. **Manual clusters and gated disable.** Do Manual clusters take catalog changes in tier order, as
   proposed, or only at their next tenant-initiated upgrade?
4. **Fingerprint coverage.** Does the code-change fingerprint include the Helm releases of platform
   components? Confirm before phase 5.
5. **Promotion of `beta` and `stable`.** CEL forbids `release` on an `Automatic` channel, so a
   pinned promotion means `promotion: Manual`, and returning to `Automatic` means removing `release`.
   Is that acceptable, or should a pin on an `Automatic` channel be allowed as a hold?
6. **Release names.** New releases are named by hand (`YYYY-MM-<letter>`), and the `shipped-<hash>`
   naming retires. Confirm.
7. **Helm chart.** The repository has no chart. Where it lives (`charts/cluster-vending`), and who
   versions it with the controller image.
8. **The `Shipped` field** of `admin release list` (`idl/cloud/v1`). Remove it, or keep it as
   "from the catalog"?
9. **Drift without a GitOps tool.** A `kubectl apply` user gets no drift signal, and a hand edit
   stays until the next apply. Accept that, or require a GitOps tool for production?
10. **Prune.** Removing a file only removes the object when pruning is on (`kubectl apply --prune`
    or the GitOps tool's prune). Confirm pruning is on for the production path.
