# PROP-06: Component delivery with Argo CD instead of provider-helm

| | |
|---|---|
| Status | Draft (2026-11-26) |
| Author | `<author>` |
| Date | 2026-11-26 |
| Reviewers | `<reviewers>` |
| Related | [architecture.md](../architecture.md): Platform components, Components and the catalog, Helm operations, Releases, channels and rollouts, Deletion, Freeze and pause; [PROP-01](PROP-01-admin-configuration.md) 4.7 (gated catalog changes); [PROP-02](PROP-02-cluster-data-live-and-snapshot.md) 5.5 (who holds workload credentials); [PROP-03](PROP-03-component-placement.md) |

## 1. Summary

Decision to evaluate (user, 2026-11-26): Argo CD looks like the right replacement for provider-helm.

- **Argo CD runs in the management cluster** (hub). It reaches each workload cluster through a cluster
  Secret the controller writes. The API server still holds no workload credentials.
- **One Argo `Application` per Helm release per cluster** (a component's CRD chart, main chart and
  companions are separate Applications). The controller writes them directly, without ApplicationSets. Each
  Application lives in the tenant's namespace and belongs to that tenant's `AppProject`.
- **The platform decides WHEN, Argo decides HOW.** The controller still renders the values (placeholders
  resolved). It still orders components by sequence and `requires`, and still gates catalog changes,
  release moves and tier rollouts. All of these only decide when an Application's spec changes. Argo
  renders the chart, applies it with server-side apply, runs the hooks, retries, corrects drift and
  judges health. The controller reads Application status in the management cluster and turns it into
  component phases.
- **Chart audit (section 4):** all 12 charts at the pinned versions render and can be delivered by Argo.
  4 work as they are. 8 need Argo settings: server-side apply for 11 CRDs over 256 KiB, `ignoreDifferences`
  for runtime-patched webhooks and one `lookup`/`genCA` cert, Argo 3.3 or later for `PreDelete` hooks,
  and `skipCrds` for Capsule. No chart needs a fork or a post-renderer.
- **Migration is per definition and gated:** a new field `install.engine: helm | argocd` is part of the
  catalog fingerprint. A switch is therefore a catalog change: it rolls out by channel and tier, and it
  can be rolled back. Argo adopts the live objects by applying the same manifests with the same release
  name. The Crossplane Release is then let go (no Delete), and the Helm release Secrets are removed after a
  soak.
- **What goes away:** Helm backoff and give-up, stuck-release recovery, Helm release records read through
  the workload kubeconfig, the failed-hook cleanup, the workload rollout check, error classification of
  provider-helm messages, the Helm ProviderConfig and the provider-helm runtime. That is about 1,200 lines
  of Go plus tests (section 6.6). The ordering gate stays, reduced to "write the next Application".

## 2. How it works today

Checked against `main` and the change in progress (Releases with `wait: false`, readiness judged by the platform).

| Concern | Where | What it does |
|---|---|---|
| Rendering | `pkg/crossplane/aws/platform_components.go` (`PlatformPlan.helmRelease`) | One provider-helm `Release` (`helm.m.crossplane.io/v1beta1`) per chart, in the tenant namespace. Values are final (placeholders filled). `wait: false`, `waitTimeout` = install timeout (bounds hooks), `maxHistory: 5`, `rollbackLimit: 3`, digest annotation `cloud.cluster-vending.io/helm-digest`. A component can have a CRD chart (`<name>-crds`, `crdsReleaseSuffix`), a main chart with `skipCRDs`, and companions. |
| Ordering | `pkg/reconcile/cluster/helm_gate.go` (440 lines) | At most one Release of a cluster installs or upgrades at a time, in sequence then `requires` order. A Release is written only when its digest differs and nothing it comes after is still rolling out. |
| Readiness | `pkg/helmrel/rollout.go` | Reads the Deployments, StatefulSets and DaemonSets Helm owns, through the workload kubeconfig. A release is settled when it is deployed and they are available within the install timeout. |
| Failures | `pkg/helmrel/{step,backoff,classify}.go`, `pkg/reconcile/cluster/helm_backoff.go` | Classifies provider-helm messages as transient, chart or release errors. Pauses the Release with exponential backoff (1 m up to 30 m) and gives up after 5 failures (`Failed`, `HelmReleaseFailed`). |
| Stuck releases | `pkg/reconcile/cluster/platform_recover.go`, `pkg/reconcile/workload.go` | Releases in `pending-*` or `uninstalling` are rolled back or uninstalled with the Helm SDK through the workload kubeconfig, at most 3 times. |
| Catalog changes | `pkg/reconcile/cluster/catalog_change.go` | One fingerprint per definition. Changes are gated by the channel's code rollout and applied one entry at a time. |
| Release moves | `pkg/reconcile/cluster/release_move.go` | Components move to the channel's release one at a time, in install order and tier order, each recovered 3 times. |
| Deletion | `pkg/reconcile/cluster/delete_platform.go`, `cleanup.go` | Seven stages: tenant components, then platform components above sequence 0 (highest first), node groups, Karpenter, add-ons. An unreachable cluster's Releases are let go with `managementPolicies` that do not include Delete. |
| Credentials | `pkg/crossplane/aws/cluster_helm.go` | A Helm `ProviderConfig` per cluster reads the kubeconfig Secret that the cluster's `ClusterAuth` writes, using the tenant role. |
| Runtime | `crossplaneaws.DefaultProviderRuntime` | provider-helm: 2 replicas, polls every 15 s, 50 reconciles/s, 1 CPU and 2 GiB requested. |

## 3. Problems today

1. **The platform reimplements a delivery engine.** Backoff, give-up, stuck-state recovery, hook-Job
   cleanup and rollout judgement are about 1,200 lines that exist only because Helm's release state
   machine can wedge (`pending-upgrade` after a dropped connection) and provider-helm reports opaque
   strings that the platform has to classify (`pkg/helmrel/classify.go`).
2. **No drift correction.** A person or an operator that edits a platform object on a workload cluster
   is not reverted until the next Helm upgrade. provider-helm does not diff the live objects.
3. **Helm never upgrades `crds/`.** Upgrading Karpenter, ALBC or GPU operator CRDs needs a manual apply
   (architecture.md, Components and the catalog).
4. **The controller reads workload clusters for rollout status** (`helmrel.Unavailable`) and keeps Helm SDK
   code paths that write to them. That is a second write path into every cluster.
5. **Scale.** At 100 clusters there are about 1,100 Releases. Each provider-helm reconcile pulls and renders
   a chart, and two replicas with leader election give one active worker pool.
6. **Visibility.** There is no per-object diff or resource tree. The status shows one Helm error string.

## 4. Chart audit

### 4.1 Method

Scratch work: `/private/tmp/claude-502/-Users-evalsocket-workspace-oss-cluster-vending/b874d434-321b-420a-95e6-04c6d408b166/scratchpad/`
(`charts/`, `values/`, `rendered/`, `scripts/`).

- Each chart was pulled at its pin in `pkg/catalog/release.yaml` with `helm pull --untar` (Helm v4.1.0).
  All 12 pulls succeeded, OCI ones included (Capsule from `ghcr.io`, Karpenter from `public.ecr.aws`).
- Each chart was rendered with `helm template --kube-version 1.35.0 --include-crds`, using the
  definition's `platformValues` plus its default `userValues`. Placeholders were replaced with plausible
  values: `${platform.*}` and `${system.*}` became a node selector and a toleration, and `${cluster.name}`,
  `${region}`, `${iam.roleArn}`, `${network.vpcId}` and `${karpenter.interruptionQueue}` became
  realistic strings.
- The templates were searched for `lookup`, `randAlpha*`, `genCA`, `genSignedCert`, `genPrivateKey`,
  `.Release.IsUpgrade`/`IsInstall` and `.Capabilities.*`. The rendered output was searched for
  `helm.sh/hook` (kind, weight, delete policy), CRD sizes, webhook `caBundle`/`failurePolicy`,
  cert-manager injection, aggregated ClusterRoles and `helm.sh/resource-policy`.
- `istio` is a bundle without a chart (`includes:` only), so it has no Application. Its status comes from
  its members.

### 4.2 How Argo treats what the audit looks for

From the Argo CD docs (stable, Helm v4 since 3.5) and the repo-server source:

| Helm feature | Under Argo CD |
|---|---|
| `pre-install`, `pre-upgrade` | `PreSync`. Argo cannot tell an install from an upgrade, so both run on **every** sync, the first one included. |
| `post-install`, `post-upgrade` | `PostSync`, on every sync, after every resource is Healthy. |
| `pre-delete` | `PreDelete`. Added in **Argo CD 3.3**. Runs only when the Application is deleted. A failing hook blocks deletion with a `DeletionError` condition. |
| `post-delete` | `PostDelete` (2.10 and later). |
| `test`, `rollback` hooks | Ignored, not applied. |
| `crd-install` | Treated as normal CRD handling. |
| `hook-weight` | Becomes the sync wave. With no delete policy, `BeforeHookCreation` is assumed. |
| `crds/` folder | Applied **and updated** on every sync, unless `skipCrds: true`. Helm only applies it on first install. |
| `lookup` | Always empty: the repo-server renders without cluster access. |
| `randAlphaNum`, `genCA`, ... | New value on every render. The app is always OutOfSync unless the value is pinned or ignored. |
| `.Release.IsUpgrade` | Always false: every render is an install. |
| `.Capabilities.KubeVersion` / `APIVersions` | Taken from the destination cluster. A cluster upgrade or a new CRD changes the render without any change to the Application. |
| `kind: List` | Unpacked by the repo-server (`reposerver/repository/repository.go`, `obj.IsList()`). |
| Objects over 262,144 bytes | Client-side apply fails (last-applied annotation too large). `ServerSideApply=true` is required. |
| `helm.sh/resource-policy: keep` | Not honoured. Argo deletes the object when the Application is deleted. |
| Fields patched at runtime that the chart does not render (e.g. a `caBundle` the chart omits) | No diff under server-side apply. Only fields that the chart renders and something else changes need `ignoreDifferences`. |

### 4.3 Per chart

`SSA` = `ServerSideApply=true`. `RID` = `RespectIgnoreDifferences=true`. "PreSync every sync" means the
Helm hook runs on every Argo sync, not only on upgrade.

| Chart | Version | Hooks | `lookup` | Generated values | CRDs (count, location, > 256 KiB) | Webhooks / caBundle | Other | Verdict | Argo settings |
|---|---|---|---|---|---|---|---|---|---|
| `karpenter` | 1.15.1 (OCI) | none | none | none | 5, `crds/`, none | none | `Capabilities` only for the ServiceMonitor | **Works** | SSA (uniform). Argo now updates the CRDs on a version bump (Helm did not). |
| `aws-load-balancer-controller` | 3.6.2 | none | **yes**: Secret `aws-load-balancer-tls` (`keepTLSSecret`) | **`genCA` + `genSignedCert`** when the lookup is empty, which is every render under Argo | 7, `crds/`, none | Mutating and Validating `aws-load-balancer-webhook` **render `caBundle`** | `IngressClassParams` + `IngressClass` inside a `kind: List` (unpacked) | **Works with settings** | `ignoreDifferences`: Secret `aws-load-balancer-tls` `/data`, both webhook configurations `jqPathExpressions: ['.webhooks[]?.clientConfig.caBundle']`. SSA, RID (so a sync keeps the live cert instead of rotating it). `enableCertManager: true` would also work, but cert-manager is not a default add-on (`pkg/crossplane/aws/addons.yaml`). |
| `external-secrets` | 2.15.0 | none | none | none | 25, `templates/crds`, **2** (`secretstores`, `clustersecretstores`, 703 KiB) | `secretstore-validate`, `externalsecret-validate`: `caBundle` not rendered (cert-controller injects it). Secret `external-secrets-webhook` rendered without `data` | none | **Works with settings** | SSA. No `ignoreDifferences` is needed under SSA. |
| `kyverno` | 3.10.0 | `post-upgrade` migrate-resources (SA, ClusterRole, binding weight 100, Job 200) → PostSync every sync. `pre-delete` Jobs `kyverno-scale-to-zero` (90) and `kyverno-rm-webhooks` (100) → **PreDelete**. 5 `test` Pods → ignored | none | none | 18, sub-chart templates, **5** (up to 463 KiB) | Kyverno creates its admission webhooks at runtime (not in the chart), so there is nothing to diff. **They are fail-closed and only removed by the pre-delete hook** | 4 aggregated ClusterRoles render without `rules` (no diff under SSA). ConfigMap `kyverno` has `resource-policy: keep` (ignored). Do not set `config.webhookLabels` to Argo's instance label, or Argo prunes the runtime webhooks | **Works with settings** | SSA. **Argo ≥ 3.3** (without PreDelete, deleting Kyverno leaves webhooks that block every write in tenant namespaces). |
| `kyverno-policies` | 3.10.0 | none | **yes**: Deployment `kyverno` image tag (`kyvernoVersion: autodetect`). Empty under Argo, so it falls back to `.Chart.AppVersion` (v1.20.0, the same group version) | none | none (12 `ValidatingPolicy`) | none | `podSecurityVersion: latest` reads `.Capabilities.KubeVersion`, so under Argo the policies re-render when the control plane minor changes, before the `TenantComponents` upgrade step | **Works with settings** | Set `kyvernoVersion` from the release pin. The platform resolves `latest` to the cluster's minor when it renders the values, so a Kubernetes upgrade changes it in the `TenantComponents` step and not earlier. |
| `istio-base` | 1.32.0 | none | none | none | 15, templates, **1** (`destinationrules`, 404 KiB), all `resource-policy: keep` | `istiod-default-validator`: rendered `failurePolicy: Ignore` (`not .Release.IsUpgrade`), **flipped to `Fail` by istiod**. `caBundle` not rendered | Argo deletes the CRDs (and every Istio object) when the Application is deleted (Helm kept them) | **Works with settings** | SSA. `ignoreDifferences` on that webhook configuration `jqPathExpressions: ['.webhooks[]?.failurePolicy']`, RID. Alternative: `base.validationFailurePolicy: Fail`. |
| `istiod` | 1.32.0 | none | none | none | none | `istio-validator-istio-system`: same `failurePolicy` flip. `istio-sidecar-injector`: `caBundle` not rendered | HPA, Deployment renders no `replicas` | **Works with settings** | `ignoreDifferences` `.webhooks[]?.failurePolicy` on `istio-validator-istio-system`, RID. |
| `istio-cni` | 1.32.0 | none | none | none | none | none | `Capabilities.KubeVersion` only | **Works** | defaults |
| `ztunnel` | 1.32.0 | none | none | none | none | none | none | **Works** | defaults |
| `istio-ingress-gateway` (`gateway`) | 1.32.0 | none | none | none | none | none | HPA, no `replicas` rendered | **Works** | defaults |
| `gpu-operator` | v26.7.2 | `pre-upgrade` upgrade-crd (SA, ClusterRole, binding weight 0, Job 1) → **PreSync every sync, the first install too**. `pre-delete` `cleanup-gpucluster` (and `cleanup-crd` when `operator.cleanupCRD`) → PreDelete. `post-delete` NFD prune (SA, ClusterRole, binding, Job) → PostDelete | none | none | 8, `crds/` + NFD sub-chart, none (max 148 KiB) | none | Renders a `ClusterPolicy` CR of its own CRD (Argo applies CRDs first) | **Works with settings** | SSA, **Argo ≥ 3.3**. |
| `capsule` | 0.15.2 (OCI) | `pre-install,pre-upgrade` crd-lifecycle: 12 ConfigMaps (-5), SA (-4), ClusterRole (-3), binding (-2), Job (-1). The Job runs `kubectl apply --server-side --field-manager=capsule/crd-lifecycle` of the patched CRDs (conversion webhook and cert-manager injection), `ttlSecondsAfterFinished: 60`, no delete policy → PreSync every sync. `pre-delete` Job deletes the TLS Secret and the namespace deleter/provisioner ClusterRoles. `post-delete` Job deletes the `capsule-dynamic-webhook` configurations the controller creates | none | none | 14 in `crds/` **and** the same CRDs installed by the hook Job. **2** over 256 KiB (`rulestatuses` 699 KiB, `tenants` 417 KiB) | cert-manager `Certificate`/`Issuer`. cainjector injects into the CRD conversion webhook and the dynamic webhooks. None of this is rendered in Argo's objects | The highest-risk chart: two owners of the CRDs unless `crds/` is skipped | **Works with settings** | `skipCrds: true` (the Job owns the CRDs, as it does after Helm's first install). SSA, **Argo ≥ 3.3**. Raise `global.jobs.kubectl.ttlSecondsAfterFinished` (e.g. 600) so Argo always sees the Job finish. |

Summary: **works**: karpenter, istio-cni, ztunnel, istio-ingress-gateway. **Works with settings**:
aws-load-balancer-controller, external-secrets, kyverno, kyverno-policies, istio-base, istiod, gpu-operator,
capsule. **Needs change**: none. None of the charts uses Argo annotations, so Helm hooks are not disabled.

### 4.4 Settings applied to every Application

```yaml
spec:
  syncPolicy:
    automated: {prune: true, selfHeal: true}
    syncOptions: [ServerSideApply=true, CreateNamespace=true, PruneLast=true, FailOnSharedResource=true]
    retry: {limit: 5, backoff: {duration: 1m, factor: 2, maxDuration: 30m}}
```

The per-chart settings in 4.3 are added on top. They belong to the definition: a new field
`install.argocd` holds `skipCrds`, `ignoreDifferences` and extra `syncOptions`, so a new chart needs no Go
change.

## 5. Goals and non-goals

### Goals

1. Argo CD replaces provider-helm for platform and tenant components. One engine at the end.
2. The platform keeps every WHEN decision: sequence and `requires`, catalog changes, release moves,
   Kubernetes upgrade steps, tiers, maintenance windows, freeze.
3. The API server holds no workload credentials (PROP-02 5.5). Tenants have no Argo access.
4. Existing clusters migrate without restarting or recreating any workload, per definition and per tier,
   with a rollback.
5. Component phases stay as they are (`status.platformComponents`, `PlatformComponentsReady`), read from
   Application status in the management cluster.

### Non-goals

- Git as the source of truth. Applications use the chart repositories directly, with values inline
  (`helm.valuesObject`). Nothing is committed per cluster.
- Tenants writing Applications, or seeing the Argo UI.
- Replacing the platform's own direct applies (NodePool, EC2NodeClass, load balancer teardown).
- ApplicationSets or Argo progressive syncs: tiers and gates stay in `pkg/rollout`.
- Moving EKS add-ons to Argo. They stay EKS-managed.

## 6. Design

### 6.1 Topology

| Piece | Choice |
|---|---|
| Where | Argo CD in the management cluster (EKS), namespace `argocd`, installed by the management cluster's install (the platform's Helm chart, `d4fa0e5`) as a pinned dependency. It is not a catalog component. |
| Version | Pinned latest patch of a supported minor (v3.6.x today; **minimum 3.3** for `PreDelete`). |
| Components run | `application-controller` (StatefulSet, sharded), `repo-server` (3 replicas), `redis-ha` (3, sentinel) or plain Redis (it is a cache), `argocd-server` (2, internal only). ApplicationSet, notifications and Dex are not installed. |
| Applications | One per Helm release per cluster: `<cluster prefix>-<release>` (the same name `HelmReleaseObjectName` gives today), in the **tenant namespace** ("Applications in any namespace": `application.namespaces: tenant-*`). |
| Projects | One `AppProject` per tenant (`tenant-<id>`, in `argocd`): `sourceNamespaces: [<tenant ns>]`, `destinations` = only that tenant's cluster Secrets (by name), `sourceRepos` = the chart repositories of the catalog, `clusterResourceWhitelist: *` (charts install CRDs and webhooks), sync windows for the freeze (6.5). |
| Clusters | One Argo cluster Secret per workload cluster (`argocd.argoproj.io/secret-type: cluster`), with `project: tenant-<id>` and `shard: <n>`, written by the controller next to the ClusterAuth. |

**Why not ApplicationSet.** A generator rewrites every cluster's Application at once. The platform needs
each cluster's spec to change only when that cluster's gate admits it (tier, window, freeze, `requires`).
The controller already renders the per-cluster values, so writing the Application is the same step as
writing a Release today.

### 6.2 Credentials

provider-helm uses the kubeconfig that `ClusterAuth` writes: a short-lived token for the tenant role,
which is assumed with the account's External ID. Argo's `awsAuthConfig` (`clusterName`, `roleARN`,
`profile`) does **not** take an External ID. Options:

| Option | How | Trade-off |
|---|---|---|
| A. Token copy | The controller copies the ClusterAuth token into the cluster Secret's `bearerToken` on every refresh. | No new IAM. Expiry races (15-minute tokens) and a write every ~10 minutes per cluster. |
| B. `awsAuthConfig` + profile | Argo's controller role (Pod Identity) and an AWS shared-config file mounted into Argo, with one profile per account (`role_arn` = tenant role, `external_id`, `source_profile` = the Argo role). The controller writes that file as a Secret. | External ID kept, tokens minted by Argo. A Secret holding all External IDs, and the file must be reloaded on change (verify in the prototype). |
| C. Deployer role | A new role per account (CloudFormation template change) trusted only by Argo's role, without an External ID, mapped by an access entry. | Cleanest at runtime. Changes onboarding for every account and weakens the External ID rule. |

**Recommended: B** for the prototype, with A as the fallback if profile reload is unreliable. In every
option the API server holds nothing new: Argo and the controller hold the credentials, as the controller
and provider-helm do today.

### 6.3 Scale and sizing (estimates, to be measured)

| Figure | Value |
|---|---|
| Clusters | 100 |
| Applications | ~9 platform (karpenter, albc, eso, kyverno, 5 istio, capsule if NaaS) + 0–2 tenant ≈ **1,000–1,100** |
| Controller shards | 4 (25 clusters each), `consistent-hashing`, pinned by the `shard` field so a restart does not move clusters. Requests 2 CPU / 6 GiB per shard, limit 12 GiB. `--status-processors 50`, `--operation-processors 25`. |
| Repo-server | 3 replicas, 1 CPU / 2 GiB each, `reposerver.parallelism.limit: 10`, Helm index and OCI cache on an `emptyDir`. Manifests are cached per (chart, version, values, kube version). |
| Cluster cache | Argo watches **every** resource type in each workload cluster, tenant workloads included. `resource.exclusions`: `Event`, `events.k8s.io/Event`, Kyverno `PolicyReport`/`ClusterPolicyReport`/`EphemeralReport`, `Lease`, `EndpointSlice`. Restricting the cluster Secret to platform namespaces (`namespaces:`) would need about 7 × resource-type watches per cluster. That is measured in the load test, not assumed. |
| HA | Controller shards are single-active per shard (a pod restart pauses its clusters' syncs, it does not lose state). Repo-server and server are stateless. Redis is a cache. |

Argo's memory is driven by the number of objects in the workload clusters, not by the number of
Applications. The 25-clusters-per-shard figure is the one to verify against the "large" clusters of the
PROP-02 scale model (6.3).

### 6.4 How the platform drives Argo

```
catalog + release + cluster  ──render──▶  Application spec (chart, version, valuesObject, settings)
   (unchanged: placeholders,              written only when the gate admits it
    fingerprints, moves, tiers)                       │
                                                      ▼
                                   Argo: render, diff, SSA apply, hooks, retry, self-heal, health
                                                      │
                     controller ◀── Application status (sync, health, operationState, comparedTo)
```

| Today (provider-helm) | With Argo |
|---|---|
| `helmRelease()` writes a `Release` | `application()` writes an `Application`: `source.repoURL/chart/targetRevision`, `helm.releaseName` = today's Helm release name, `helm.valuesObject`, `helm.skipCrds`, `destination.name` = the cluster Secret, `destination.namespace`, `ignoreDifferences` and `syncOptions` from the definition. Annotation `cloud.cluster-vending.io/spec-digest`. |
| CRD release `-crds` before the main release | A separate Application, written first and Healthy before the main one is written. There are no cross-Application sync waves. |
| Helm gate: one Release rolling out per cluster | The same gate over Applications: the next Application is written only when the ones it comes after are settled. The rest of `helm_gate.go` (reading Helm state, release records) goes. |
| `sequence.Ready` + `deployed` + workload check | **Settled** = `status.sync.comparedTo.source` equals the spec (Argo has seen this spec), `sync.status: Synced`, `health.status: Healthy`, and `operationState.phase: Succeeded` for this revision. |
| Install timeout judged by the platform | The same timeout. Not settled within it → `Failed` with Argo's message. Argo keeps retrying within its own `retry` limit. |
| Backoff and give-up | `syncPolicy.retry`. After the limit, `operationState.phase: Failed`, so the component is `Failed`. `cluster retry` triggers a new sync (sets `operation`), which replaces "lift the backoff". |
| Error classes | `ComparisonError` (chart not found, render failed, cluster unreachable) → chart or transient. `SyncError` / operation `Failed` → release. `health: Degraded` past the timeout → rollout timed out. |
| Stuck releases | Gone: Argo has no pending state that can wedge. An operation running longer than the timeout is terminated (`argocd app terminate-op` equivalent: clear `operation`) and retried, up to the existing 3 recoveries. |

Status to phase mapping:

| Application | Component phase |
|---|---|
| Not written yet (waits for its turn or a gate) | `Pending` (message as today: "waiting for X") |
| `comparedTo` differs from spec, or `operationState.phase: Running`, or `health: Progressing` | `Installing` / `Upgrading` |
| Settled (above) | `Ready` |
| `Synced`, then later `OutOfSync` with an unchanged spec | `Ready` with a `Drifted` note. Argo self-heals it. Repeated self-heal is counted and shown. |
| Operation `Failed`/`Error` after retries, or not settled within the timeout | `Failed` (`HelmReleaseFailed` becomes `ComponentSyncFailed`) |
| `ComparisonError` for the cluster connection | Transient: not counted, shown |

All of this is a read of Application objects in the management cluster. The controller stops reading
workload Deployments for component readiness.

**Catalog changes, release moves, upgrades** (`catalog_change.go`, `release_move.go`, `upgrade.go`) are
unchanged in what they decide. Their "settled" check becomes the Application check above. The 30-minute
watch and 3 recoveries stay. A recovery is "trigger a sync again", not "rerun Helm".

**Render-environment changes.** Argo re-renders when its cache expires or when the cluster's kube version or
API set changes. A chart whose output depends on them would change without the platform deciding it. The
audit found one such chart (`kyverno-policies`, `podSecurityVersion: latest`), and the platform resolves
that value itself. An Argo upgrade can change rendering fleet-wide (for example the Helm version). It is
done under a fleet freeze (6.5): upgrade, list `OutOfSync` Applications with an unchanged spec, then lift
the freeze by tier.

### 6.5 Freeze and pause

The platform freeze ([Freeze and pause](../architecture.md#freeze-and-pause), one platform-wide freeze) has to stop
Argo too. The controller adds a `deny` sync window to every tenant AppProject (`applications: ['*']`,
`manualSync: false`). It removes the window cluster by cluster when unfreezing (sync windows take cluster
globs). A paused cluster gets a window for its own Applications. Self-heal stops under a deny window, which
is what a freeze means.

### 6.6 Deletion

The stages of `driveDelete` stay. Only how a component is removed changes.

| Stage | With Argo |
|---|---|
| 1. Load balancers | Unchanged (platform, workload API). |
| 2. Tenant components | Delete their Applications, which carry the finalizer `resources-finalizer.argocd.argoproj.io` (cascade). Wait until they are gone. |
| 3. Platform components above sequence 0 | Highest sequence first. Delete the Application. Argo runs the `PreDelete` hooks (kyverno `scale-to-zero` and `rm-webhooks`, capsule pre-delete, gpu cleanup), deletes the resources in reverse wave order, then runs `PostDelete` (capsule dynamic webhooks, NFD prune). The IAM objects are removed after the Application is gone. |
| 4. Node groups | Unchanged: NodePools, NodeClaims, EC2NodeClasses by the platform, **while Karpenter's Application still exists**. |
| 5. Karpenter | Delete its Application, then the queue, rules and node role as today. |
| 6. Add-ons | Unchanged. |
| 7. The rest | The Argo cluster Secret is deleted only after every Application of the cluster is gone (Argo cannot clean up without it). It replaces the Helm ProviderConfig in this stage. |

"Let go" (forced deletion, unreachable cluster, `--teardown-timeout` passed, `DeletionError` from a hook
that keeps failing): remove the Argo finalizer from the Application, so it is deleted without touching
the cluster. This is the same rule as today's Releases without Delete. A `PreDelete` hook that fails is
shown with its message, and `cluster delete --force` skips the wait.

Behaviour change on **catalog removal** (not on cluster deletion): Argo deletes the CRDs that Helm kept
(`resource-policy: keep` on istio-base, kyverno's ConfigMap). The CRs of those CRDs go with them. For a
platform component removed by a catalog change, the controller deletes the Application **without** the
cascade finalizer when the definition sets `install.argocd.keepOnRemove: true` (proposed for istio-base and
kyverno). The objects then stay as Helm left them.

### 6.7 What code goes away and what stays

| Goes | Lines (approx.) |
|---|---|
| `pkg/helmrel/step.go`, `backoff.go`, `classify.go`, `rollout.go` (and tests) | ~530 + tests |
| `pkg/reconcile/cluster/helm_backoff.go`, `platform_recover.go` | ~420 |
| Helm records and storage: `pkg/reconcile/workload.go`, `teardown.go` (Helm parts), `pkg/management/workload.go` (Helm SDK through the kubeconfig) | ~280 |
| `pkg/crossplane/aws/cluster_helm.go` (Helm ProviderConfig), provider-helm in `DefaultProviderRuntime`, the provider package install | ~100 |
| The Helm-state parts of `helm_gate.go`, `delete_platform.go` (`recoverDeleting`), `retry.go` | ~300 |

| Stays (adapted) |
|---|
| Values rendering and placeholders (`pkg/component`), `HelmReleasePlan` (renamed, now the Application source) |
| Ordering by sequence and `requires`, CRD chart first, companions after (the gate, without Helm state) |
| Catalog fingerprints and gated catalog changes, release moves, Kubernetes upgrade steps, tiers, windows, freeze |
| The deletion stages and the "let go" rule |
| The digest (now on the Application, as the "spec changed" signal) |
| The status API, phases and events (renamed where they say Helm) |

### 6.8 Tenant safety

| Risk | Control |
|---|---|
| A tenant changes what is delivered | Tenants have no Kubernetes access to the management cluster. Applications are written only by the controller. The API refuses `spec.version` and validates values against `valuesSchema`, as today. |
| A tenant reaches another tenant's cluster through Argo | Applications live in the tenant namespace. Their AppProject allows only that namespace as source and only that tenant's clusters as destinations. |
| The Argo UI or API | `argocd-server` has no Ingress (ClusterIP), SSO through the platform's OIDC issuer for the platform team only, local `admin` disabled, `policy.default: role:readonly` for the platform group. No tenant user exists. |
| A tenant edits platform objects on their own cluster (they are cluster admins) | Self-heal reverts them. That is a change from today, and the console says so. |
| Values carry secrets | They do not today (role ARNs, names). A definition with a secret value is refused by `controller --check` once the engine is `argocd`, because Application specs are readable by the platform team. |

### 6.9 Migration of existing clusters

A new definition field `install.engine: helm | argocd` (default `helm`) is part of
`component.CatalogFingerprint`. Switching it is a catalog change: it is gated by the channel's rollout
(tiers, `maxInProgress`, soak, windows, halt past `maxFailed`) and applied one entry at a time. That entry
is a **switch**, not a reinstall:

| Step | What happens | Stops (and rolls back) if |
|---|---|---|
| 1 | Preconditions: the component is `Ready` under Helm, at its digest, with no upgrade or move under way. | not met: waits |
| 2 | Pause the Crossplane Release (`crossplane.io/paused`), so provider-helm does nothing more. | |
| 3 | Write the Application with the same chart, version and values, `helm.releaseName` = the existing release name, `destination.namespace` = the release namespace, and **`automated` off**. | |
| 4 | Read the diff (`status.resources`, `sync.status`). Expected: every object is OutOfSync only for Argo's tracking annotation (`argocd.argoproj.io/tracking-id`). Allowed in addition: the `ignoreDifferences` fields of 4.3. | any other field differs |
| 5 | Sync once (SSA). PreSync and PostSync hooks run: capsule's CRD Job, gpu-operator's upgrade-crd Job, kyverno's migrate Job. All are idempotent. No Deployment's pod template changes, so nothing restarts. | the sync fails, or a pod template changed |
| 6 | Settled (6.4) → turn `automated` on. | not settled within the timeout |
| 7 | Let the Crossplane Release go (`managementPolicies` without Delete, as `cleanup.go` does) and delete it. Helm's objects stay. | |
| 8 | After the channel's soak, delete the Helm release Secrets (`sh.helm.release.v1.<release>.v*`) through the workload API. | |

**Rollback** (any step before 8, or `install.engine: helm` again): remove Argo's finalizer and delete the
Application (nothing is removed from the cluster), recreate the Release with full management policies,
and unpause it. Helm upgrades its existing release, and the tracking annotation it leaves is harmless.
After step 8 a rollback is a fresh Helm install over existing objects. Helm adopts them only while they
still carry its `meta.helm.sh/release-*` annotations (Argo leaves them, to be verified in phase 0), and the
revision history is lost. So step 8 waits for the soak.

Both engines run side by side during the migration. The gate treats a settled Release and a settled
Application alike for `requires`. A definition and its CRD chart and companions switch together.

### 6.10 Prototype plan

**First component: `external-secrets`.** It has no hooks, no `lookup` and no generated values, so a
failure is about the topology and not about a chart. It still exercises the hard parts: SSA with two CRDs
over 256 KiB, CRDs in `templates/`, and a cert-controller that patches webhooks and a Secret at runtime.
`aws-load-balancer-controller` comes second, as the test of `ignoreDifferences` + RID and of the
`lookup`/`genCA` case.

Success criteria (on the sandbox account, `docs/architecture.md` End-to-end):

1. A new cluster installs ESO through Argo and reaches `Ready`. The phase shown by the API matches Argo's status.
2. An existing Helm-installed ESO switches (6.9) with **no pod restart** (pod UIDs unchanged) and no diff
   beyond the tracking annotation.
3. Self-heal: a manual edit of the Deployment is reverted within one minute.
4. Zero OutOfSync Applications after 24 h of re-renders (cache expiry, hard refresh).
5. Freeze: a deny window stops self-heal and a value change, and unfreezing applies them.
6. Deletion of the cluster completes through the stages. A forced deletion with an unreachable API completes by finalizer removal.
7. Credentials option B works across a token refresh and an External ID rotation.
8. Load: 100 simulated clusters (kind or vcluster) × 11 Applications on 4 shards. Memory per shard and
   time to settle after a release move of one component across a tier are recorded.

Then the rest, by audit verdict and risk: the Istio members and Karpenter (work as they are, but Karpenter
is sequence 0 and goes last of these) → kyverno, gpu-operator (PreDelete) → kyverno-policies (platform
resolves `podSecurityVersion`) → capsule (hooks, two CRD owners, `skipCrds`).

## 7. Alternatives considered

| Option | For | Against | Verdict |
|---|---|---|---|
| **Keep provider-helm** with the current change (`wait: false`, platform readiness) | No new system or migration. Real Helm semantics: hooks, `lookup` and `IsUpgrade` behave as charts expect. | The platform keeps its delivery engine (3.1). No drift correction, no CRD upgrades. The workload read path for readiness stays. Every Release reconcile is a chart pull and render through one provider. | Fallback if the prototype fails. |
| **Flux in each workload cluster** | Real Helm. Each cluster is self-contained. Scale is spread out. | 100 installs of Flux to bootstrap, upgrade and size, on the platform pool, before any component. Status has to be read from each workload cluster, which needs workload credentials (PROP-02). **Tenants are cluster admins and could edit or suspend the HelmReleases.** | Rejected. |
| **Flux hub** (helm-controller in the management cluster, `kubeConfig.secretRef` per HelmRelease) | Real Helm semantics (all hooks, `lookup`, `genCA` stable through Helm storage). Migration is trivial: a HelmRelease with the same name and namespace upgrades the existing Helm release. Status is local. Remediation and retries are built in. Drift detection is optional. | Helm's release state stays (it can still wedge, though Flux remediates it). No UI or per-object diff. Sharding by label selectors needs several helm-controller instances. EKS auth for remote kubeconfigs has the same External ID question (6.2). | **The strongest alternative.** Its migration risk is lower than Argo's. It is the second candidate if Argo fails criteria 2 or 4. |
| **Sveltos** | Hub model, Helm with real Helm storage, a drift-detection agent, ClusterProfile rollout controls. | Smaller community. It puts an agent in each cluster for drift. ClusterProfiles select clusters by label, so per-cluster gating needs a profile per cluster, which works against its model. | Rejected. |
| **Argo per cluster** | No hub credentials. | The same problems as Flux per cluster, plus a heavier footprint. | Rejected. |
| **Argo hub via an AWS-managed Argo CD (EKS capability)** | No Argo operations. | Version and configuration control (sharding, exclusions, apps in any namespace, sync windows) and cross-account auth with External ID would need checking. | Open question 2. |

## 8. Rollout plan

### Phase 0: Prototype (ESO)
Argo in the dev management cluster, credentials option B, `install.engine` and `install.argocd` in the
definition schema, `application()` rendering, and status mapping for one component. Criteria 1–8 of 6.10.

### Phase 1: Engine in the controller
The gate over Releases and Applications, `install.engine` in the catalog fingerprint, the switch entry
(6.9), freeze through sync windows, deletion stages with Applications, AppProject and cluster Secret per
tenant and cluster. Every definition still on `helm`.

### Phase 2: Migrate by verdict
ESO, ALBC, Istio, Karpenter, Kyverno, gpu-operator, kyverno-policies, Capsule, each as a catalog change on
alpha, then beta and stable after their soak.

### Phase 3: Remove provider-helm
When no definition on any cluster is on `helm`: remove the code in 6.7, the Helm ProviderConfig,
provider-helm and its runtime config, and the `--helm-*` and `--recover-stuck-releases` flags. Update
architecture.md (Helm operations, Deletion, Freeze, Platform components).

## 9. Open questions

1. **Credentials:** option B (shared-config profiles with External ID) or C (a deployer role without
   External ID)? B needs proof that Argo reloads the mounted file.
2. **Hosting:** run Argo ourselves (full control, our upgrades) or AWS-managed Argo CD on EKS, if it allows
   sharding, resource exclusions, Applications in tenant namespaces and cross-account auth?
3. **Cluster cache:** cluster-wide watches with exclusions, or namespace-scoped cluster Secrets (platform
   namespaces only, `clusterResources: true`)? Decided by the phase 0 load test.
4. **Self-heal vs. tenants:** should some objects stay tenant-editable (e.g. Kyverno exceptions)? Today
   nothing reverts them.
5. **`keepOnRemove`:** for istio-base and kyverno CRDs on catalog removal, keep (Helm's behaviour) or delete?
6. **Argo upgrades:** are a fleet freeze, an OutOfSync review and an unfreeze by tier enough, or is a
   render-diff check against the new version needed before the upgrade?
7. **Tenant components:** confirmed on the same path (same AppProject)? This proposal assumes yes.
8. **Capsule's CRD Job vs. Argo:** with `skipCrds: true` and SSA, the Job (`capsule/crd-lifecycle`) and Argo
   never own the same fields. To confirm in phase 2 before Capsule switches.
