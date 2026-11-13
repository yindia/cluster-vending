# Upgrade flow: moving clusters from one release to the next

Status: plan only; nothing was changed. Read at `fd1e668` on main. Paths are relative to the repository root. Three things are marked **[verify]**: I inferred them from comments and did not read the code that proves them.

## 0. What the code does today (facts the plan rests on)

| Fact | Where |
|---|---|
| A cluster keeps the release its components were installed from; it leaves only when the release is gone or `Valid=False`. Mode, tier and holds are not read. | `pkg/reconcile/cluster/platform.go` `platformInput` |
| `release.Resolve` and `release.CheckVersion` have no caller. Create/update do not check the Kubernetes version against the offered release. | `pkg/release/release.go`; `pkg/server/cluster/mutate.go` only calls `CheckVersionStep` |
| `upgrade_mode` exists on the contract (field 13) and the kind, defaults to manual, and is updatable with channel and tier. | `idl/cloud/v1/cluster.proto`, `api/v1alpha1/cluster_types.go`, `pkg/cluster/plan.go`, `pkg/server/cluster/mutate.go` `releasePaths` |
| Beta and stable offer whatever upstream offers, at once. `CheckStep` is applied per channel, not per cluster. `status.stage` and `status.tiers` are never written. | `pkg/controller/release_controller.go` `target`, `release_catalog.go` `stepProblem` |
| The whole platform set is applied every pass. A dependent waits only for its requirement to be Ready and `deployed`, which stays true at the old version, so a release change would upgrade every chart at once. | `pkg/crossplane/aws/platform_components.go` `PlatformSet`, `installed` |
| "Ready" means Helm applied the chart, not that its workloads run. | `docs/catalog.md`, `component-usage.md` §7.2 item 6 |
| A component the release does not pin is reported `FAILED: not in release X` and left out of the set. | `pkg/component/platform.go` `planComponent`; `platform.go` `platformStatus` |
| **[verify]** The engine prunes recorded objects that the set no longer has (stated in the doc comment of `Reconciler.Apply`). If true for the platform set, a definition that is deleted or becomes unusable (`PlatformEntries` drops it) uninstalls that chart on every cluster. | `pkg/crossplane/sequence/sequence.go` `Engine.Apply` |
| The components' Kubernetes minor comes from the spec's version, not the running one, so pins for a new minor apply before the control plane is upgraded. | `platformInput`: `kubernetesMinor(spec...KubernetesVersion)` |
| `GateUpgrade` holds node groups and add-ons until the control plane reports the target; `status.upgrade` reports the phase. Add-on versions are looked up again only when the spec's version or add-ons change. | `pkg/reconcile/cluster/upgrade.go`, `resolve.go` |
| A second controller already writes one status field without touching the rest. | `repository.Clusters().SetDeprecations`, `pkg/database/cloudstore/clusters.go` |
| The controller already writes into workload clusters with the stored kubeconfig. | `pkg/reconcile/cluster/platform_recover.go`, `pkg/management/workload.go` |
| A release is "in use" only by channels and holds. A release that clusters run can be deleted, and those clusters then jump to the channel's release. | `ReleaseReconciler.namedBy` |
| Catalog definitions are create-only on start; an operator may replace one. `Valid` depends on the live catalog (`CheckGroups`, `CheckBundles`). | `pkg/catalog/catalog.go`, `ReleaseReconciler.Reconcile` |
| Tenant components are stored PENDING with the version and release of the cluster's kept release; a tenant cannot set a version. Nothing installs them. | `pkg/server/component/tenant.go` (`noVersion`, `check`, `pendingMessage`) |
| No Helm or OCI library is in `go.mod`. | `go.mod` |

## 1. Per-cluster state machine

### 1.1 Who decides what

| Decision | Owner | Written to |
|---|---|---|
| Which release a cluster should run | `release.Resolve`, called by the cluster reconciler | `status.release.target` |
| When an automatic cluster may start | Channel controller (section 2) | `status.release.admitted` through a new `Clusters().SetReleaseAdmission`, like `SetDeprecations` |
| When a manual cluster starts | The tenant (`UpgradeCluster`) | `spec.targetRelease` (new) |
| Moving the cluster, step by step | Cluster reconciler | The rest of `status.release`, `status.platformComponents` |

Until the channel controller exists (increment 4), admission is "always" for automatic clusters; the reconciler asks one function, `admitted(row, target)`, in every increment.

### 1.2 Status

New `ClusterStatus.release = 20` (`ClusterRelease`), mirrored as `status.release` on the kind. `platform_release = 19` stays, equal to `current`.

| Field | Meaning |
|---|---|
| `current` | Release every component is Ready on. Set only when a move finishes. |
| `target` | Release being moved to; empty when idle. |
| `available` | Release the channel offers when it differs from `current` (what "upgrade now" would take). |
| `admitted` | Release the channel controller let this cluster start. |
| `phase` | `IDLE`, `PENDING` (a gate blocks), `COMPONENTS`, `KUBERNETES`, `HALTED`. |
| `component`, `step`, `steps` | The component being moved and progress. |
| `gates[]` | `{code, blocking, message}`: why it does not start, or warnings. |
| `message`, `start_time` | Why halted; when the move began. |
| `held_by` | Name of the active `ReleaseHold`. |
| `supported`, `end_of_support` | From `Resolve`. |
| `history[]` | Last 10 moves: `{from, to, start, end, result, message}`. |

`PlatformComponent` gains `target_version = 11`, `crds_version = 12`, `definition_hash = 13`.

### 1.3 Release move (A to B), platform and tenant components

One ordered list per cluster: Platform entries plus the cluster's enabled tenant components, in `release.UpgradeOrder`. It must be one list because `kyverno-policies` (tenant) requires `kyverno` (platform) and shares its version group.

1. **Target.** `Resolve(facts, releases, channels, hold)` gives `Decision.Release`. For a manual cluster the target is `spec.targetRelease`, else `current`.
2. **Gates** (section 3). If any blocks: `PENDING` with the gates, nothing changes.
3. **One component at a time.** New pure function `component.PlanMove(in, current, target, observed)`:
   - A component takes its target pin only when every component before it in the order is settled at target. Otherwise it keeps the version its Release object asks for now (read from the object, so progress survives a restart).
   - Settled at target means: the Release's chart version equals the pin, its provider has caught up (`reconcile.Behind` is false), it is Ready and `deployed`, and Helm waited for the workloads (`forProvider.wait: true` on upgrades; **[verify]** the field exists in the provider-helm v1.5.0 CRD that `TestHelmReleaseFields` pins).
   - A component the target adds is installed at its turn; one the target drops is removed in reverse order after everything else has moved.
4. **Done.** All settled: `current = target`, `target` cleared, history entry written. Tenant `Component.status.installedVersion`, `targetVersion` and `release` (the fields exist) follow the same move.
5. **Timeout.** 30 minutes per component by default, overridable per definition. Time while the cluster's resources are paused does not count.

Tenant components: a tenant component that was not Ready before the move is skipped and does not halt it, so a tenant's bad values cannot stop a fleet rollout. A component enabled during a move takes the target's pin and its place in the order. The tenant-component installer (next on main) should be built on the same input and planner, so this needs only the entries list widened.

### 1.4 Failure: halt, not rollback

- A component that fails (Release FAILED, timeout, or stuck after `maxRecoveries`) sets `phase: HALTED` with the component and provider error. No later component moves. provider-helm keeps retrying the failed chart.
- No automatic rollback: Istio and applied CRDs do not go back safely, and components already moved are working.
- Ways out:
  - Fix forward with a new release.
  - `RetryClusterUpgrade` (admin) clears the halt.
  - A `ReleaseHold{release: A}` is the explicit rollback: `Resolve` already returns A as the target, and the move runs in reverse order with CRDs left alone.
- A definition that is missing or unusable never removes a component: the move halts with the reason and the recorded objects are kept (`sequence.Keeping`).

### 1.5 Kubernetes minor (separate phase)

Order for a release B that moves the cluster's minor (`upgradeTo`):

1. Components to B's pins for the **running** minor (section 1.3).
2. Gates: deprecations, one minor step, B pins the new minor.
3. Control plane, then add-ons and node group. This exists: `GateUpgrade` and `status.upgrade`. Add-on versions are looked up again for the new minor (`resolveVersions`).
4. Components to B's pins for the **new** minor (the cluster autoscaler's image tag).

Changes needed:
- `platformInput` takes the minor from `status.currentKubernetesVersion` while `status.upgrade` is set. This also fixes manual upgrades today.
- For an automatic cluster the version rendered is `status.release.kubernetes`, not the spec's (rollouts.md §10). An update of `kubernetes_version` on an automatic cluster is refused.
- A failed control-plane upgrade halts. It cannot be undone.

A manual cluster changes its minor as today (`UpdateCluster`), with two added checks: `release.CheckVersion` against the offered release, and that its current release pins the new minor ("upgrade the release first").

### 1.6 Manual clusters

- **Upgrade now.** `ClusterService.UpgradeCluster{tenant_id, cluster_id, etag, validate_only, plan_id, acknowledge_warnings}` sets `spec.targetRelease` to the release the cluster's channel offers (unset: stable). No other release can be chosen, so tenants still cannot choose versions. `validate_only` returns the preview (section 5). Refused on automatic clusters.
- CLI: `cluster-vending cluster upgrade <id> --tenant t [--plan] [--plan-id x] [--yes] [--wait]`.
- **Renderer fixes still flow.** A manual cluster renders with the running build, and its components are planned from its **current** release's pins plus the live definition's `platformValues`. So a values or renderer fix reaches it and no version changes. A test pins this.

## 2. Rollout across clusters (channel controller)

Extend `ReleaseChannelReconciler` and watch `Cluster` and `ReleaseHold`. The decisions go in a new pure file, `pkg/release/rollout.go`:

```go
func Rollout(policy Policy, release string, members []Member, prev State, now time.Time) (State, []Admission)
```

- **Members:** automatic clusters of the channel (unset: stable) that are not imported, blocked or being deleted.
- **Classification per member:**

| Class | Rule |
|---|---|
| `Done` | `current` is the release, Ready, components Ready, minor at target |
| `InProgress` | admitted, not done |
| `Failed` | `HALTED`, or over the total timeout |
| `Held` | an active `ReleaseHold` |
| `Skipped` | a blocking gate: deprecations, not Running, paused, unsupported |
| `Pending` | none of the above |

- **Tier order:** Tier2, Tier1, Tier0. In the current tier, admit `Pending` clusters oldest first while `InProgress < maxInProgress` (Tier0: always 1).
- **maxFailed:** more than `maxFailed` failed in a tier latches a halt, `status.halt{id, release, tier, clusters, at}` with condition `Halted`. No admissions until `spec.acknowledgedHalt == status.halt.id` (`ResumeChannel` sets it). Clusters in progress finish.
- **Soak:** a tier is complete when no member is `Pending`, `InProgress` or `Failed`. `completedAt` is stored in status so a restart keeps it. If a `Done` cluster is not Ready for more than 10 minutes, the clock restarts. The next tier starts at `completedAt + soakFor(tier)`. An empty tier completes without soak (decided in rollouts.md §7).
- **Pause:** `spec.paused` stops admissions; `Resolve` already keeps clusters where they are.
- **Holds:** read by cluster ID (`cloudstore.LabelID`), counted per tier, and passed to `Resolve` by the cluster reconciler.
- **Promotion:** add `status.completed{release, at}`, set when Tier0 has completed and soaked and the channel is not halted. An Automatic channel's `target()` reads its upstream's `status.completed.release` instead of `status.release`. A channel takes a new release only when its current rollout completed, unless the operator changes `spec.release` (supersede).
- **Reporting:** `status.stage`, `status.tiers[]` (add `pending`, `skipped`, `soakUntil`), conditions `Progressing`, `Soaking`, `Halted`, `Complete`, failed cluster IDs (at most 20), and events on the channel and the cluster.

**The `0s` soak.** `Soak` is a `metav1.Duration` struct, so `omitempty` never omits it: the Go client sends `"0s"` and the CRD default of 24h never applies (`asShipped` in `release_defaults.go` already works around it). `MaxFailed` has the mirror problem: an explicit 0 is dropped and becomes 1.

- Make `Soak *metav1.Duration` and `MaxInProgress`, `MaxFailed` `*int32`; remove the CRD defaults; apply defaults in code (`Policy.SoakFor(tier)`, 24h when nil).
- One-time fix for stored objects: a policy that is exactly `{5, 1, 0s}` with no `soakPerTier` is treated as unset and rewritten, with an annotation so it runs once. After that an explicit `0s` is honoured, and the channel shows a warning when it promotes automatically with zero soak.

## 3. Gates

| Gate | Automatic | Manual (upgrade now) | Source |
|---|---|---|---|
| Not `RUNNING`, or a spec change being applied | wait | refuse | row state |
| Resources paused (`Paused` condition) | wait; never a failure | refuse | `reconcile.PausedCondition` |
| Imported, or taken over | not a member | refuse | `row.Imported` |
| Active hold | held | not applicable | `ReleaseHold` |
| Channel paused, or nothing offered | wait | refuse | `Resolve` notes |
| Component step: `CheckStep(current, target)` per cluster | wait, reason names the component | refuse | `pkg/release/catalog.go` |
| Version group consistent in target | (release is not Valid) | same | `CheckGroups` |
| Removed APIs at the target minor | block (counts as skipped) | warn; needs `acknowledge_warnings` | `status.deprecations.removed`, `targetVersion`; `BlocksUpgrade` |
| Deprecation scan older than 24h or failed | wait and request a scan | warn | `status.deprecations.scannedAt` |
| Kubernetes moves more than one minor | never planned | refused | `CheckVersionStep` |
| CRDs of a chart with `crds/` not applied | the component waits | same | section 3.1 |
| Minor unsupported in target | block | warn | `Decision.Supported` |

New pure function `release.Gates(facts, decision, clusterState) []Gate`, used by the reconciler, the channel controller and the preview.

### 3.1 CRD pre-step

| Option | Verdict |
|---|---|
| Separate CRD chart (`install.helm.crds`, already supported) | Rejected. It contradicts "one upstream chart"; AWS LBC has none; CRDs installed from `crds/` carry no Helm ownership, so the CRD chart cannot adopt them. |
| provider-kubernetes `Object` per CRD | Rejected: a new provider, and large CRDs stored in the management cluster. |
| Hook Job in the workload cluster | Rejected: needs a platform chart or image. |
| Manifest URL per definition | Workable, but a second source to pin and trust. |
| **Controller applies the chart's own `crds/` through the workload kubeconfig** | **Recommended.** |

Design:
- Definition field `install.helm.crdUpgrade: FromChart`, set on `karpenter` and `aws-load-balancer-controller`.
- New `pkg/chartsource`: fetch the chart at the pinned version (HTTPS repo and OCI), read `crds/`, cache by repository, name and version.
- New `Workloads.CRDs(ctx, namespace, secretRef)` in `pkg/reconcile/workload.go` and `pkg/management/workload.go`. Server-side apply of `CustomResourceDefinition` documents only; refuse a CRD that drops a version listed in `status.storedVersions`; wait for `Established`.
- The result is recorded as `crds_version`. The planner keeps the main chart at its old version until `crds_version` equals the pin. CRDs are never downgraded.
- Cost: a chart-fetch dependency, and the controller needs egress to the chart repositories.

Until this exists (increments 1 to 2), a component with `crdUpgrade: FromChart` whose version changes is held at its version, and the move reports `PENDING` with that reason.

ztunnel and istio-cni: the planner's "one at a time, wait for the rollout" plus the definition's `maxUnavailable: 0` limits the interruption to one node at a time. Add `disruptive: <text>` on those definitions so the preview and plan show it. Maintenance windows stay deferred.

## 4. "Not in release" and the catalog coupling

**A new component on a cluster that keeps an older release.**
- A Platform definition that the cluster's current release does not pin, with nothing installed, is not listed in `platform_components` and does not affect `PlatformComponentsReady`.
- It appears in the preview as "added by release B". If the target pins it, it is `PENDING` during the move.
- If objects of it exist, it stays listed.
- The release controller adds a non-failing `Complete` condition naming the Platform definitions a release does not pin, so an authoring gap in a new release is visible.

**A definition changes under installed clusters.**
1. A kept release is left only when it is deleted. `Valid=False` caused by a catalog change no longer moves clusters; it only stops channels offering the release. Add the clusters running a release to `InUse` and the finalizer (`ReleaseReconciler.namedBy` lists clusters by `status.release.current` and `target`).
2. Removal is explicit only (section 1.4): never because a definition vanished.
3. Values that belong to a chart version go in the pin's `values`, not the definition. A test fails when a definition's `platformValues` differ between two shipped builds without a version change note.
4. A definition change is applied through the same one-at-a-time planner, with halting, using `definition_hash` to detect it. It still reaches all clusters (decided), but no longer every chart at once.
5. Structural fields (`install`, `namespace`, `requires`, `includes`, `versionGroup`) changing on a definition that clusters have installed: the definition controller sets `Valid=False` with the reason, and clusters keep what they have.

## 5. API and CLI

**Tenant (`ClusterService`)**

| RPC | CLI | Purpose |
|---|---|---|
| `GetCluster` (existing) now carries `status.release` | `cluster get` | current, target, phase, gates |
| `GetClusterUpgrade` returns `UpgradePreview` | `cluster upgrade <id> --plan` | from and to per component, Kubernetes, add-ons ("known after upgrade"), gates, disruptive notes, plan ID |
| `UpgradeCluster` | `cluster upgrade <id> [--yes] [--wait]` | manual upgrade now |
| `ListClusterUpgrades` (paged) | `cluster upgrades <id>` | history |
| `ListComponents` (existing) shows `target_version` | `component list` | tenant components during a move |

**Admin (`AdminService`; it already requires the platform admin role where the server authenticates)**

| RPC | CLI |
|---|---|
| `ListReleases`, `GetRelease` | `admin release list|get` |
| `ListReleaseChannels`, `GetReleaseChannel` | `admin channel list|get`, `admin rollout status [--channel c]` |
| `PauseChannel`, `ResumeChannel` (also acknowledges a halt) | `admin channel pause|resume` |
| `SetChannelRelease` | `admin channel set <c> --release r --promotion p` |
| `HoldCluster`, `UnholdCluster`, `ListHolds` | `admin hold create|delete|list` |
| `RetryClusterUpgrade` | `admin cluster retry-upgrade <id>` |

Every admin action writes a field on the kinds, so `kubectl` keeps working.

## 6. Phasing

| # | Ships | Files | Tests (no real cluster) | Risks |
|---|---|---|---|---|
| 1 | An automatic cluster follows its channel's release, one component at a time, halting on failure. Holds and channel pause are read. `status.release`. Chart-`crds/` components held. The "not in release" fix. Removal guard. | `pkg/release` (`Gates`), `pkg/component` (`PlanMove`), `pkg/reconcile/cluster/platform.go` plus a new `release_move.go`, `pkg/reconcile/reconcile.go` (`PlatformSource`: channels, holds), `pkg/management/platform.go`, `pkg/crossplane/aws/platform_components.go` (`wait`), `idl/cloud/v1/cluster.proto`, `api/v1alpha1/cluster_types.go`, `pkg/convert` | Table tests of `PlanMove`: order, halt, restart mid-move, added and dropped components. Reconciler tests with a fake client: a manual cluster does not move but takes a `platformValues` change; hold; pause; a release edit in `release.yaml` produces a new shipped name and the cluster reaches it. | All automatic clusters of every channel move together until increment 4; automatic is opt-in, and pause and holds work. The provider-helm `wait` field. |
| 2 | Manual upgrade now, preview, history; `CheckVersion` on create and update. | `idl/cloud/v1/cluster_service.proto`, `pkg/server/cluster` (new `upgrade.go`, `mutate.go`, `planning.go`), `pkg/cluster/plan.go`, `pkg/cli/cluster.go`, `cmd/client.go` | Handler tests: refused on automatic, stale plan, only the offered release, version not offered. CLI golden output. | `spec.targetRelease` is a new spec field (open question 1). |
| 3 | CRD pre-step; tenant components in the move (needs the tenant installer). | `pkg/chartsource` (new), `pkg/reconcile/workload.go`, `pkg/management/workload.go`, `api/v1alpha1/component_types.go`, `pkg/catalog/definitions/{karpenter,aws-load-balancer-controller,ztunnel,istio-cni}.yaml`, `pkg/server/component` | Fetcher against a local HTTP and OCI test server. envtest apply of CRDs: stored-version refusal, no downgrade. Planner: main chart waits for `crds_version`. A tenant component that was failing before the move does not halt it. | New dependency and egress. Size of the CRD apply. |
| 4 | Channel controller: tiers, limits, soak, halt, promotion; the soak type fix; admin RPCs and CLI. | `pkg/release/rollout.go` (new), `pkg/controller/release_controller.go`, `release_defaults.go`, `api/v1alpha1/release_types.go`, `pkg/database/repository`, `cloudstore/clusters.go`, `idl/cloud/v1/admin_service.proto`, `pkg/server/admin`, `pkg/cli/admin.go` | Table tests of `Rollout` with a fake clock: the worked example of rollouts.md §8, Tier0 one at a time, soak restart, halt and acknowledge, empty tier. envtest: a controller-created channel stores no `0s`; the one-time rewrite; stable takes a release only after beta completed. | Changing field types on a served CRD (pointers are wire-compatible). Promotion timing changes for existing installations. |
| 5 | Kubernetes minor for automatic clusters; the deprecation gate; components follow the running minor. | `pkg/reconcile/cluster/{upgrade.go,resolve.go,platform.go}`, `pkg/release`, `pkg/server/cluster/mutate.go`, `pkg/controller/deprecation_controller.go` (scan on request) | Reconciler tests on the existing fake EKS objects: phases in order, add-ons looked up again, blocked by a removed API, manual warned, the autoscaler tag changes only after the control plane reports the minor. | Irreversible. Ship with `upgradeTo` honoured only on channels with `policy.kubernetesUpgrades: true`. |
| 6 | Edges: stepping through intermediate releases, late joiners, mode, channel or tier change during a move, withdrawn releases, release `InUse` by clusters, definition-change hardening, metrics. | `pkg/release`, `pkg/controller`, `pkg/server/cluster/mutate.go`, `pkg/component` | Table tests per rule of rollouts.md §12 and §13. | Mostly policy; low code risk. |

Increments 1 and 2 are independent of the tenant installer; 3 depends on it.

## 7. Open questions

1. **How is a manual "upgrade now" stored?** Recommend a spec field `targetRelease`, restricted to the release the channel offers: declarative and usable from `kubectl`. The alternative is a row field outside the spec, which keeps the spec purely the tenant's wishes but has no `kubectl` path.
2. **Can a tenant abort or roll back a manual upgrade?** Recommend no in the first version: halt and fix forward; rollback is the operator's hold.
3. **Where does an automatic cluster's Kubernetes version live?** Recommend status, as rollouts.md §10 proposes, and refuse spec updates of it. Note the reconciler already writes resolved versions into the stored spec, so "spec" would be less work but invites a fight with the tenant's manifest.
4. **Chart fetching for the CRD step:** the Helm SDK, or a small fetcher (HTTPS index plus an OCI client)? Recommend the small fetcher; confirm the controller may reach the chart repositories.
5. **Does a failed or stale deprecation scan block an automatic Kubernetes upgrade?** Recommend yes: wait and report.
6. **A channel with no automatic clusters promotes at once** (alpha usually has none), so beta gets a release with no evidence. Recommend keeping the decided rule but showing a `NoClusters` warning; alternatively apply the soak to empty channels too.
7. **Existing stored `soak: 0s`:** rewrite to unset once when the policy is otherwise untouched? Recommend yes.
8. **Definition changes: all clusters at once, or by tier?** Recommend all clusters, but one component at a time with halting (section 4). Going by tier needs a catalog revision in the release.
9. **Taken-over clusters:** they have no platform components. Should "upgrade now" install them? Recommend no for now; say so in the status.
10. **Do tenant-component failures count toward `maxFailed`?** Recommend only when the component was Ready before the move.
11. **Default limits:** 5 in progress, 1 failed, 24h soak, 30 minutes per component, 2 hours per cluster. Confirm.
12. **Kubernetes upgrades opt-in per channel** (`policy.kubernetesUpgrades`) for the first releases? Recommend yes.
