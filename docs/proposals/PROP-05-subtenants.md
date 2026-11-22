# PROP-05: Sub-tenants that people can use: automatic qualification, owner access, usage and scale

| | |
|---|---|
| Status | Accepted; phase 0 implemented |
| Author | `<author>` |
| Date | 2026-11-22 |
| Reviewers | `<reviewers>` |
| Related | [architecture.md](../architecture.md): Sub-tenants and NaaS, SubTenant (4), 7. Known limits; [PROP-01](PROP-01-admin-configuration.md) (gated catalog changes, config file); [PROP-02](PROP-02-cluster-data-live-and-snapshot.md) (live reader, status summary); [PROP-03](PROP-03-component-placement.md) (Capsule on the bootstrap group); [PROP-04](PROP-04-api-authentication.md) (no authorization in the API) |

## 1. Summary

A `SubTenant` is an allocation of namespaces on a cluster that runs Capsule (`spec.naas: true`). The
provisioning path is built: the controller writes one Capsule `Tenant`, and shared Capsule
`GlobalTenantResource`s generate the namespaces, the network and mesh policies, the limit ranges, the
service accounts and, once access is open, the RoleBindings for the owners' OIDC groups. Deletion is
guarded and never deletes user content.

What is missing is everything after "provisioned". In practice nobody can use a sub-tenant today:

- **Access closes every hour.** Human access opens only on a qualification record that an operator
  writes by hand into the workload cluster, valid for at most one hour (`VerifyAccessQualification`).
  When it expires, the generated RoleBindings are removed.
- **The API is not a boundary any more** (PROP-04). Any authenticated caller can set `adminGroups` on any
  sub-tenant. The hand-written qualification is the only thing between an API call and namespace admin
  rights for an arbitrary group.
- **Owners have no way in**: no kubeconfig, no usage, no quota consumption, no way to remove a namespace.
- **It does not scale.** Every sub-tenant is reconciled every 20 seconds, and each pass makes about 20
  requests to the workload cluster, including an unfiltered list of every namespace.

This proposal keeps the provisioning design and fixes those four things, in this order:

| # | Change | Section |
|---|---|---|
| 1 | Qualification runs automatically, per cluster and template revision, not per allocation per hour | 6.1 |
| 2 | An operator-set group allowlist per cluster, in the config file, decides which OIDC groups a sub-tenant may name | 6.2 |
| 3 | Reconcile on change, with filtered reads and a per-cluster bootstrap check; Ready sub-tenants resync every 10 minutes | 6.3 |
| 4 | Owner access: `subtenant kubeconfig`, an `editorGroups` role, and the console's "Connect" panel | 6.4 |
| 5 | Usage and quota consumption on the status, live detail through the PROP-02 live reader | 6.5 |
| 6 | Per-cluster caps and an overcommit warning | 6.6 |
| 7 | Removing an empty namespace key | 6.7 |
| 8 | Capsule and template upgrades through PROP-01's gated catalog changes, with automatic re-qualification | 6.8 |

Out of scope: adopting existing namespaces, sub-tenants spanning clusters, new policy profiles, and
authorization in the API (6.11).

## 2. How it works today

Checked against `main` (commit 58df553).

**Objects and API.**

- `SubTenant` (`api/v1alpha1/subtenant_types.go`, CRD `config/crd/bases/cloud.cluster-vending.io_subtenants.yaml`)
  is namespaced in `tenant-<id>`, with finalizer `cloud.cluster-vending.io/subtenant`. Spec:
  - `tenantRef` and `clusterRef` carry UIDs, so a replacement with the same name is never adopted;
  - `name` (DNS label, at most 35 characters, immutable);
  - `deletionProtection` (nil means protected);
  - `namespaceKeys` (1 to 64, each at most 27 characters, append-only by CEL);
  - `quota`: `namespaceCount` (1 to 64) and `hard`, 12 allowed keys, a budget shared by the whole
    sub-tenant;
  - `policyProfile`, only `restricted-ambient-v1`;
  - `access`: `bindings` (`identityRef` and `workload-editor`), `adminGroups` and `viewerGroups` (exact
    group-claim values, at most 32 each).
- `SubTenantService` (`idl/cloud/v1/subtenant_service.proto`, `pkg/server/subtenant/`) has Create, Get,
  List (paged), Update and Delete. Mutations acknowledge persistence, not readiness.
- Names are unique per workload cluster through a claim ConfigMap in the tenant namespace, keyed by the
  cluster UID and the name (`name_claim.go`). Since PROP-04 the service checks only the request's shape
  (`service.go`, `validation.go`).
- The console (`ui/src/components/console/subtenants-view.tsx`, `subtenant-detail.tsx`,
  `subtenant-editor.tsx`, `cluster-subtenants.tsx`, `lib/workflows/subtenants.ts`) and the CLI
  (`pkg/cli/subtenant.go`, `examples/subtenant.yaml`) use the same manifest.

**Workload side.**

- **Capsule** is a platform component installed only when `spec.naas` is true
  (`pkg/catalog/definitions/capsule.yaml`, 0.15.2, `capsule-system`, cert-manager from the cluster).
  Per PROP-03 it runs on the bootstrap group.
- **Shared bootstrap.** `pkg/naas/assets/bootstrap.yaml` holds 16 objects. They are delivered as the
  Capsule chart's `extraManifests` in new release pins (`CapsuleBootstrapValues`, `pkg/catalog/bootstrap.go`),
  and `Revision()` is their SHA-256. The objects:
  - three `GlobalTenantResource`s:
    - `naas-namespace-bootstrap` creates the namespaces `<name>-<key>`, with the PSS restricted labels
      pinned to the cluster's minor and the ambient label;
    - `naas-isolation-baseline` creates, per namespace, a default-deny NetworkPolicy, HBONE within the
      sub-tenant, DNS, a strict mTLS PeerAuthentication, an AuthorizationPolicy for the owned namespaces,
      the `naas-container-defaults` LimitRange and the service accounts;
    - `naas-developer-access` creates the RoleBindings to the owners' groups, only while the Tenant has
      `access-ready=true`;
  - the `naas-system/provisioner` ServiceAccount, the only Capsule owner;
  - the `naas-workload-editor`, `naas-workload-admin` and `naas-workload-viewer` ClusterRoles.
- **Controller** (`pkg/controller/subtenant_controller.go`, `subtenant_access.go`; `pkg/naas/tenant.go`,
  `access.go`, `tenant_observe.go`). It writes exactly one Capsule `Tenant`, named `spec.name`:
  - owners are only the provisioner;
  - `forceTenantPrefix` and `preventDeletion` are set from `deletionProtection`;
  - the budget quota, ClusterIP-only Services and no external IPs;
  - the namespace keys go into `spec.data`.

  The controller then observes the namespaces and their baseline and reports `ProvisioningReady`. Every
  pass ends with `RequeueAfter: 20s` (`pendingDelay`), whatever the state (`subTenantReport`).
- **Access** (`reconcileSubTenantAccess`) opens only when all of these hold:
  - the cluster has `spec.access.oidc` and an ACTIVE EKS association (`ObserveAccessOIDC`);
  - at least one group is requested;
  - a qualification record exists, the ConfigMap `naas-system/naas-qualified-<hash>`. The operator writes
    it after running ten checks by hand (`RequiredQualificationChecks`: namespace RBAC, cross-namespace
    RBAC, admission guards, ambient mTLS, cross-sub-tenant traffic, DNS, egress deny, restricted PSS,
    shared quota, group delegation). `pkg/naas/cmd/access-qualification` only prints the binding to test;
  - the record binds every UID, the spec hash, the bootstrap revision, the OIDC revision and the
    association UID;
  - the record's validity is at most one hour.

  When the record is missing, expired or no longer matches, the Tenant's `access-ready` label goes back to
  `false`. The developer RoleBindings use `deletion: Remove`, so they disappear.
- **`identityRef` bindings** are refused at render ("no resolver is configured"): the field exists but
  cannot be used.
- **Deletion** (`deleteSubTenant`, `pkg/naas/namespace_cleanup.go`, `tenant_delete.go`) runs in order:
  1. close access;
  2. withdraw namespace generation;
  3. wait for Capsule to acknowledge;
  4. scan every namespace with full discovery;
  5. report any user content as `deletionBlockers`, and delete nothing of it;
  6. delete only empty owned namespaces, under a cordon;
  7. delete the Tenant;
  8. release the finalizer.
- **Cluster safety.** Disabling `naas` or deleting a cluster with sub-tenants is held
  (`pkg/reconcile/cluster/subtenant_safety.go`, `pkg/controller/subtenant_safety.go`, `pkg/naas/dependents.go`).
- **Freeze.** A freeze stops the controller's workload writes. It does not stop Capsule's own replication
  (bootstrap header).

**Docs and examples.**

- `examples/subtenant.yaml` still says "workload provisioning remains pending until the workload
  controller is implemented". That is stale: provisioning is built.
- architecture.md 7 lists sub-tenant isolation and access qualification as lab procedures never run on a
  live cluster.

## 3. Problems

| # | Problem | Where it comes from | Effect |
|---|---|---|---|
| P1 | Access lasts one hour, then closes | `VerifyAccessQualification`: `ExpiresAt - IssuedAt <= 1h` | Owners lose their RoleBindings every hour unless an operator re-runs and re-records the tests. Access cannot be operated. |
| P2 | Qualification is manual and per allocation | `RequiredQualificationChecks`; the record is keyed by allocation UID and spec hash | At 2,200 sub-tenants (section 5), and every spec edit invalidating its record, nobody can keep up. |
| P3 | Any API caller can grant namespace admin to any group | PROP-04 removed authorization; `adminGroups` are free text | An authenticated user names a group they belong to on someone else's sub-tenant. Only the manual qualification stops it, and P1/P2 push operators to make it automatic. |
| P4 | Every sub-tenant is reconciled every 20 seconds, Ready or not | `subTenantReport` returns `RequeueAfter: pendingDelay` | About 110 reconciles a second across the fleet (5.2). |
| P5 | Each pass lists every namespace of the workload cluster, unfiltered, and re-reads the 16 bootstrap objects | `ObserveTenantNamespaces`, `VerifyTenantBootstrap` | About 20 workload requests per pass; about 100 a second against one large cluster's apiserver (5.2). |
| P6 | Owners have no kubeconfig and no instructions | No `kubeconfig` verb for sub-tenants; the console has no connect panel | Even with access open, owners must build an OIDC kubeconfig by hand. |
| P7 | No usage | Status has namespaces and conditions only | "How much of my quota is used?" has no answer in the console or CLI. |
| P8 | No per-cluster caps | No limit on sub-tenants or namespaces per cluster; quotas are not compared with capacity | One tenant can create hundreds of allocations, or quotas far above the cluster's size. |
| P9 | Namespaces cannot be removed | CEL: `namespaceKeys` are append-only | A namespace added by mistake stays until the whole sub-tenant is deleted. |
| P10 | `identityRef` bindings exist but always fail | `RenderTenant` refuses them | The only way to grant `workload-editor` is unusable; groups get only admin or viewer. |
| P11 | Template and Capsule upgrades are not gated per cluster | Bootstrap rides the Capsule release pin; PROP-01 phase 5 not built | A bootstrap change reaches every NaaS cluster with its release move, and invalidates every qualification record at once (closing all access). |
| P12 | Stale docs | `examples/subtenant.yaml` header | Says provisioning is not built. |

## 4. Goals and non-goals

### Goals

1. Access, once qualified, stays open until something that matters changes, with no human step per
   sub-tenant.
2. The groups a sub-tenant may name are decided by the platform operator, not by whoever calls the API.
3. Reconcile cost follows changes, not the number of sub-tenants.
4. An owner goes from "Ready" to `kubectl get pods` with one command.
5. Usage against quota is visible, cheaply, for every sub-tenant.
6. Bounded: caps per cluster, refused with the numbers.
7. A namespace can be removed when it is empty, with the same guards as deletion.
8. Template and Capsule changes roll out by tier, like other catalog changes.

### Non-goals

- Authorization in the API (PROP-04 stays). 6.2 is a platform config rule about group names, not a
  permission system for API users.
- Adopting existing namespaces into a sub-tenant (6.11).
- Sub-tenants across clusters, or moving one between clusters.
- More policy profiles than `restricted-ambient-v1`.
- Per-sub-tenant node pools or dedicated nodes.
- Cost reporting.

## 5. Scale model

### 5.1 Assumptions (estimates)

| Item | Small cluster | Medium | Large |
|---|---|---|---|
| Clusters in the fleet | 60 | 30 | 10 |
| Sub-tenants per NaaS cluster | 5 | 30 | 100 |
| Namespaces per sub-tenant (average) | 3 | 3 | 3 |
| Namespaces per cluster from sub-tenants | 15 | 90 | 300 |
| All namespaces in the cluster | about 40 | about 150 | about 400 |

Fleet: about 2,200 sub-tenants and 6,600 sub-tenant namespaces, if every cluster runs NaaS (upper
bound).

### 5.2 Controller and workload load

| Quantity | Today | After 6.3 |
|---|---|---|
| Reconciles a second, fleet | 2,200 / 20 s = about 110 | About 4 (2,200 / 600 s) at rest, plus one per change |
| Workload requests per pass | about 20: Tenant get, 16 bootstrap gets, provisioner get, a list of all namespaces | about 3: Tenant get and a namespace list filtered by `capsule.clastix.io/tenant=<name>`. The bootstrap check runs once per cluster per revision (or every 10 minutes), not per sub-tenant. |
| Requests a second against one large cluster | 100 / 20 s x 20 = about 100, plus 5 full namespace lists a second | about 2.7 at rest, measured (100 / 600 s x 16, see phase 0) |
| Bytes per namespace list, large cluster | about 1 MB (400 namespaces), 5 times a second | about 6 KB (3 namespaces) |

### 5.3 Usage collection (6.5)

| Quantity | Value |
|---|---|
| ResourceQuota objects per sub-tenant | one per namespace (Capsule projects the shared budget) |
| Reads per usage pass | one list of ResourceQuotas filtered by the sub-tenant label per sub-tenant, at most every 10 minutes |
| Fleet, at rest | about 4 lists a second |
| Status bytes per sub-tenant | under 2 KiB (12 quota keys, used and hard) |

## 6. Design

### 6.1 Automatic qualification, per cluster and revision (P1, P2)

**What is qualified.** The ten checks test the shared templates on a given cluster: the bootstrap
revision, the Kubernetes minor, the CNI, the Istio version and the cluster's OIDC configuration. They do
not depend on which sub-tenant they run in. So qualification moves from per allocation to per cluster:

- **Key**: (cluster UID, bootstrap revision, Kubernetes minor, Capsule version, Istio version, OIDC
  revision).
- **Runner**: a platform Job, `naas-qualifier`, in `naas-system`. It is the same binary as
  `pkg/naas/cmd/access-qualification` extended to run the checks rather than only print them. It creates
  two throwaway sub-tenants with one namespace each (`qual-a`, `qual-b`) through the normal path. Then:
  - it runs the ten checks as test pods and test identities (impersonation of the two test groups);
  - it writes the record, and deletes the two sub-tenants.
- **Trigger**: the controller starts the Job when a NaaS cluster has no current record for its key. That
  happens at first NaaS enable, after a release move that changes the bootstrap revision or Capsule, after
  a Kubernetes upgrade, and after an OIDC change.
- **Record**: the ConfigMap `naas-system/naas-qualified-cluster`. It holds the key, the result of each
  check, the Job's name and UID as evidence, and the time. The namespace is protected as today: no Capsule
  owner, no sub-tenant role reaches it. Only the qualifier's ServiceAccount and the controller can write
  it.
- **Validity**: until the key changes, with a 30-day maximum. After 30 days the Job re-runs. The old
  record stays valid while the new run is in flight, so access does not close. A failed run closes access
  on that cluster, with reason `QualificationFailed` and the failed checks named.
- **Per-allocation check** is static and cheap. It verifies:
  - the Tenant's spec hash equals the render;
  - every namespace exists with the expected labels and UIDs;
  - the baseline objects are present (the existing `BaselineReady`).

  Nothing is recorded per allocation.
- **Operator override**: an operator can still write a per-cluster record by hand (the current format,
  keyed as above). That covers clusters where the Job cannot run, such as egress-restricted test
  environments.

Files:
- `pkg/naas/access_qualification.go`: the per-cluster key and record, a 30-day validity, overlapping
  renewal.
- `pkg/naas/cmd/access-qualification/main.go`: becomes the runner. It runs the checks; the `--print` mode
  stays.
- New `pkg/naas/qualifier_job.go`: renders the Job, its ServiceAccount and its Role.
- `pkg/controller/subtenant_access.go`: uses the cluster record, and no longer binds the per-allocation
  spec hash.
- `pkg/naas/assets/bootstrap.yaml`: the qualifier's ServiceAccount and Role, which changes the revision.
- New `pkg/controller/naas_qualification_controller.go`, one per NaaS cluster: starts the Job, records the
  result, and sets the cluster condition `NaaSQualified`.
- The cluster status and the console's cluster page show the condition.

### 6.2 Group allowlist per cluster, from the config file (P3)

PROP-04 makes the API open to every authenticated caller, so the API cannot decide who may grant what.
The platform operator decides which group names a sub-tenant may use, in the PROP-01 config file:

```yaml
naas:
  # Groups a sub-tenant may name in adminGroups, editorGroups and viewerGroups. Exact values or a
  # prefix ending in "*". Empty: no group may be named, and access stays closed.
  allowedGroups:
    - "cv-subtenant-*"
  # Groups that may never be named, checked first.
  deniedGroups:
    - "cv-platform-operators"
```

- **Checked at the API** (`pkg/server/subtenant/validation.go`): a group outside the list is
  `INVALID_ARGUMENT`, with the allowed patterns in the message.
- **Checked again by the controller** before it renders the access data (`pkg/naas/access.go`
  `ResolveAccess`). A group that left the list closes access with reason `GroupNotAllowed`; no RoleBinding
  is generated for it.
- **Naming convention.** Each group's owners are managed at the identity provider. A convention such as
  `cv-subtenant-<tenant>-<name>-admin` makes the mapping visible. This is documented, not enforced.
- **Honest limit.** This bounds the groups, not the caller. Any API user can still put an allowed group on
  any sub-tenant. Isolation between API users is a non-goal of PROP-04. What this prevents is naming
  platform or cluster-wide groups (`system:*` is already refused).

Files:
- `pkg/config` (a new `naas` section and its loader, both processes);
- `pkg/server/subtenant/validation.go`;
- `pkg/naas/access.go`;
- `config/node-groups/platform.yaml`, `hack/config.local.yaml` and `config/server/auth/platform.yaml`
  (examples).

### 6.3 Reconcile on change, filtered reads (P4, P5)

- **Requeue** (`subTenantReport`, `pkg/controller/subtenant_controller.go`):
  - `Ready` resyncs every 10 minutes;
  - `Provisioning` and `Pending` every 20 seconds, as today, but with backoff to 2 minutes after 10
    unchanged passes;
  - `Deleting` every 20 seconds;
  - `Failed` every 5 minutes.
- **Change-driven.** Today the controller watches `Cluster`, `Tenant` and `ReleaseHold`. Watches on the
  workload cluster are not added, because that would mean watch streams in 100 clusters (PROP-02 5.7(b)).
  Remote drift is caught by the resync.
- **Filtered namespace reads** (`ObserveTenantNamespaces`, `pkg/naas/tenant_observe.go`): list with the
  label selector `capsule.clastix.io/tenant=<name>`. Deletion keeps its full, unfiltered scan, because
  completeness matters there (`PreflightNamespaceCleanup`).
- **Bootstrap check once per cluster** (`VerifyTenantBootstrap`): the result is cached in the controller
  per (cluster UID, revision) for 10 minutes. A cache miss or a mismatch falls back to the full check.
- **Per-cluster concurrency**: at most 4 sub-tenant reconciles of one cluster at a time, so one large
  cluster cannot take the whole worker pool (`pkg/controller/subtenant_controller.go` `SetupWithManager`).

### 6.4 Owner access (P6, P10)

- **Roles by group.** Add `editorGroups` (role `naas-workload-editor`, which exists in the bootstrap)
  next to `adminGroups` and `viewerGroups`, with the same CEL rules (exact, unique across the three lists,
  at most 32). Deprecate `bindings` and `identityRef`: refused on create, ignored with a status warning on
  existing objects, and removed in the next API version. Files: `api/v1alpha1/subtenant_types.go`,
  `idl/cloud/v1/subtenant.proto`, `pkg/naas/access.go` (`BindingData`),
  `pkg/naas/assets/bootstrap.yaml` (the `naas-developer-access` template), `pkg/server/subtenant/validation.go`,
  the console editor and `pkg/cli`.
- **`subtenant kubeconfig <id> --cluster <cluster>`** (CLI) prints a kubeconfig for the cluster's OIDC
  issuer. It uses the `kubectl oidc-login` exec plugin with the issuer and client ID of
  `spec.access.oidc`, sets the default namespace to the sub-tenant's first namespace, and lists the other
  namespaces in a comment.
  - It contains no secret, so it is safe to share.
  - It refuses with the reason when access is not Ready.
  - Files: `pkg/cli/subtenant.go`, `cmd/subtenant.go`, and `pkg/cli/kubeconfig.go` (reusing the cluster
    `--oidc` kubeconfig code).
- **Console "Connect" panel** on the sub-tenant page. It shows:
  - the access state and its reason (OIDC pending, not qualified, group not allowed);
  - the groups and their roles;
  - the namespaces;
  - a "Download kubeconfig" button (the same output as the CLI) and the `kubectl` commands to try.

  Files: `ui/src/components/console/subtenant-detail.tsx` and `ui/src/lib/workflows/subtenants.ts`.
- **What owners can do** is the three ClusterRoles of the bootstrap, unchanged. `naas-workload-admin`
  does not include RoleBindings (no owner promotion: `allowOwnerPromotion: false`).

### 6.5 Usage and quota consumption (P7)

- **On the status** (`SubTenantStatus.usage`, new): per quota key, `used` (summed over the sub-tenant's
  namespaces' ResourceQuota `status.used`) and `hard`; plus pods running and namespaces ready. The
  controller writes it at most every 10 minutes, from one filtered ResourceQuota list (5.3). Files:
  `api/v1alpha1/subtenant_types.go`, a new `pkg/naas/usage.go`, `pkg/controller/subtenant_controller.go`,
  `idl/cloud/v1/subtenant.proto`, and `pkg/server/subtenant/convert.go`.
- **Shown** in `subtenant list` (a `QUOTA` column: the highest used/hard percentage and its key), in
  `subtenant get`, and on the console page as bars per key. A key above 90 % is marked.
- **Live detail** (pods per namespace, requests against quota now) comes from the PROP-02 live reader,
  through a new live kind, `SubTenantUsage`: a view keyed by (tenant, cluster, sub-tenant), built from the
  namespaces' pods and ResourceQuotas only. It follows the caps of PROP-02 5.9. This is phase 4 and
  depends on PROP-02 phase 3.

### 6.6 Per-cluster caps and overcommit (P8)

The caps live in the config file, the same section as 6.2:

```yaml
naas:
  maxSubTenantsPerCluster: 250
  maxNamespacesPerCluster: 2000     # sum of namespaceCount over the cluster's sub-tenants
```

- The API refuses a create or update over a cap with `RESOURCE_EXHAUSTED` and the numbers. The count
  comes from the management cluster's SubTenant list for that cluster. Files:
  `pkg/server/subtenant/crud.go` and `pkg/config`.
- **Overcommit.** The sum of `requests.cpu` and `requests.memory` quotas over a cluster's sub-tenants is
  compared with the cluster's allocatable in the PROP-02 summary. Above 100 %, the cluster gets the
  condition `NaaSOvercommitted` (a warning, not a refusal: quotas are budgets, and tenant pools scale).
  The console shows it on the cluster's sub-tenant list. Files: `pkg/controller/subtenant_controller.go`
  (sum), `pkg/clustersummary` (allocatable).

### 6.7 Removing an empty namespace (P9)

- The CEL rule changes from "append-only" to "a key may be removed only when
  `status.namespaces[key].removable` is true" (a transition rule on `oldSelf`).
- The controller sets `removable` from the deletion code's per-namespace scan (`PreflightNamespaceCleanup`
  on that namespace only): no user content and no foreign ownership. It runs only on request:
  - `subtenant update` with the key removed first gets `FAILED_PRECONDITION`, "run `subtenant update
    --check-remove <key>`";
  - that command sets the annotation `cloud.cluster-vending.io/remove-check=<key>`;
  - the controller scans and reports.
- Removal then withdraws the key from the Tenant's data. Capsule stops generating the namespace (`deletion:
  Orphan` keeps it), and the controller deletes the empty namespace under the cordon, as offboarding does.
- Files: `api/v1alpha1/subtenant_types.go` (CEL, `removable`), `pkg/naas/namespace_cleanup.go`
  (per-namespace entry), `pkg/controller/subtenant_controller.go`, `pkg/cli/subtenant.go` and the console
  editor.

### 6.8 Capsule and template upgrades (P11)

- The bootstrap stays in the Capsule release pin (`CapsuleBootstrapValues`). A new revision reaches a
  cluster only through its release move, gated per definition by PROP-01 phase 5. That gives tiers, soak,
  windows, the freeze and a halt on failures.
- A new revision changes the qualification key (6.1). The old record stays valid until the new Job
  finishes, so access does not flap during an upgrade.
- If the new run fails, access closes on that cluster and its rollout halts at the Capsule step, with
  reason `QualificationFailed`. That is the PROP-01 halt rule, with qualification as Capsule's readiness
  check.
- Files: `pkg/reconcile/cluster/upgrade_components.go` (Capsule's readiness includes `NaaSQualified`),
  `pkg/naas/access_qualification.go`.

### 6.9 Status and conditions

Conditions after this proposal (on `SubTenant`):

| Condition | True when |
|---|---|
| `ProvisioningReady` | As today: the current Tenant and every namespace's baseline are observed. |
| `GroupsAllowed` (new) | Every named group is inside `naas.allowedGroups` and outside `deniedGroups`. |
| `OIDCReady` | As today: an ACTIVE EKS association matches the cluster's OIDC. |
| `ClusterQualified` (replaces `AccessQualified`) | The cluster's current qualification record matches its key and has passed. |
| `AccessReady` | All of the above, and at least one group is named. |
| `Ready` | `ProvisioningReady`, and `AccessReady` when groups are named. |

A Ready sub-tenant whose access closes goes to `Degraded` with the reason, not `Pending`.

### 6.10 Docs and examples

- `examples/subtenant.yaml`:
  - the stale "pending until the workload controller is implemented" note is removed;
  - the commented `bindings` block becomes `adminGroups`, `editorGroups` and `viewerGroups`, with a note
    on `naas.allowedGroups`.
- `docs/architecture.md`:
  - the access section describes the per-cluster qualification;
  - the caps, usage and kubeconfig;
  - known limit 7 keeps "not run on a live cluster" until the phase 1 sandbox run.

### 6.11 Out of scope

| Item | Why |
|---|---|
| Adopting existing namespaces | It needs ownership transfer of user content into Capsule and a review of every object. That is a separate proposal once usage exists. |
| Sub-tenants across clusters | A sub-tenant is bound to one cluster's UID throughout. |
| More policy profiles | `restricted-ambient-v1` is the only one qualified. A new profile is a new bootstrap and qualification suite. |
| Freeze pausing Capsule replication | Cordoning every Tenant during a freeze would block tenants' own workloads. A freeze stops the platform's writes only, as documented. |
| Authorization of API users | PROP-04. |

## 7. Security

- **Boundary.** The workload cluster is the boundary: Capsule, the generated policies, PSS restricted,
  ambient mTLS, and Kubernetes RBAC for OIDC groups. The API is not a boundary (PROP-04).
- **Who decides groups.** The operator, through `naas.allowedGroups` (6.2). Group membership is decided at
  the identity provider.
- **Qualification evidence.** The record is written only by the qualifier's ServiceAccount or an operator,
  in a namespace no sub-tenant role reaches (as today). The controller never trusts a sub-tenant's status
  or annotations for it.
- **Failure closes access.** A failed or stale run closes access; an in-flight renewal keeps the previous
  record. Revocation removes the RoleBindings, as today (`deletion: Remove`).
- **Test identities.** The qualifier impersonates two test groups only (`cv-naas-qual-a`, `cv-naas-qual-b`),
  through a Role limited to impersonating those groups. The test sub-tenants are deleted after the run;
  a left-over test sub-tenant is reported.
- **Kubeconfig.** It holds no credential: the exec plugin gets tokens from the issuer.
- **Deletion.** It never deletes user content. Unchanged.

## 8. Alternatives considered

| Alternative | Why not |
|---|---|
| Keep per-allocation, one-hour records, and automate writing them | A test per sub-tenant every hour, about 53,000 runs a day at fleet scale, to test the same templates again. |
| No qualification: open access on `ProvisioningReady` | Static matching of templates does not prove that the CNI and mesh enforce them on this cluster. The checks exist because of that. |
| Authorization in the API for `adminGroups` | Reverses PROP-04. The allowlist bounds the groups without per-user permissions. |
| Watches on every workload cluster | About 2 streams per NaaS cluster, constant traffic whether or not anything changes. The 10-minute resync is enough for drift. |
| Free-form RBAC per sub-tenant | Replaces three reviewed ClusterRoles with tenant-written ones, which needs its own policy engine. |
| Quota usage only through the live reader | The console's list needs it for every sub-tenant at once. The status covers that cheaply. |

## 9. Rollout plan

Each phase ships alone and leaves the system consistent.

### Phase 0: Scale fix and docs (no behaviour change for users)

- 6.3 (requeue by state, filtered namespace list, bootstrap cache, per-cluster concurrency); 6.10 (stale
  example note).
- **Implemented** (2026-11-23): `pkg/controller/subtenant_pace.go`, `naas.ObserveSelectedNamespaces`.
  Measured on the controller fixture with access open (`TestSubTenantSettledPassIsCheapAndSlow`): 33
  workload reads on a cold pass, 16 with the cluster's bootstrap cached. That is about 2.7 reads a second
  for 100 settled sub-tenants of one cluster, against about 165 before (33 every 20 seconds). The 16 are
  mostly the per-namespace isolation baseline reads, which stay uncached on purpose. A Ready sub-tenant
  with open access is looked at again no later than its qualification's expiry, so access still closes on
  time.
- **Accepted when**: a fixture of 100 Ready sub-tenants on one cluster makes at most 4 workload requests a
  second at rest, against about 165 before. A changed sub-tenant is reconciled within one pass of the
  change. Deletion still runs its full scan.
- **Tests**: a counting fake workload client over 100 sub-tenants for 10 minutes of fake time; the
  label-selected list; the bootstrap cache hit, miss and mismatch; the requeue per state.

### Phase 1: Automatic qualification and the group allowlist

- 6.1 and 6.2; conditions of 6.9. The per-allocation hourly record is retired. The per-cluster record and
  the manual override format are accepted.
- **Accepted when**:
  - On a sandbox NaaS cluster, the Job runs at NaaS enable and passes all ten checks.
  - A sub-tenant with an allowed group gets `AccessReady` without a human step, and keeps it for more
    than an hour.
  - A group outside the list is refused at the API, and closes access when the list changes.
  - A failed check closes access on that cluster, naming the check.
  - The renewal overlaps with no access gap.
- **Tests**:
  - the record key and validity (match, key change, 30 days, overlap);
  - the allowlist (exact, prefix, deny-first, empty list);
  - the controller's access state machine on a fake;
  - the sandbox run, opt-in, as the e2e harness of `830afe6`.

### Phase 2: Owner access

- 6.4: `editorGroups`, deprecated `bindings`, `subtenant kubeconfig`, the console Connect panel.
- **Accepted when**:
  - An owner in an editor group runs `subtenant kubeconfig`, logs in through the issuer, creates a
    Deployment in the sub-tenant's namespace, and cannot read the next sub-tenant's namespace.
  - The console download gives the same file.
- **Tests**:
  - CEL for the three group lists;
  - the binding data per role;
  - the kubeconfig golden output;
  - the console component tests;
  - the sandbox run for the cross-sub-tenant denial.

### Phase 3: Usage and caps

- 6.5 (status usage, CLI and console display) and 6.6 (caps, overcommit).
- **Accepted when**:
  - A sub-tenant with 3 namespaces shows used/hard per key equal to the sum of its ResourceQuotas.
  - A create over `maxSubTenantsPerCluster` is `RESOURCE_EXHAUSTED` with the numbers.
  - A cluster whose quotas exceed its allocatable shows `NaaSOvercommitted`.
- **Tests**:
  - the usage sum (missing quotas, partial namespaces);
  - the caps at the boundary;
  - the overcommit rule against a summary fixture;
  - the CLI table and the console bars.

### Phase 4: Namespace removal and live usage

- 6.7, and the live `SubTenantUsage` view of 6.5 (after PROP-02 phase 3).
- **Accepted when**:
  - An empty namespace key is removed and its namespace deleted.
  - A non-empty one is refused with its blockers.
  - The live view returns pods per namespace within PROP-02's caps.
- **Tests**:
  - the CEL transition rule;
  - the per-namespace scan;
  - the remove flow on a fake, including a namespace that gains content between the check and the
    removal (refused);
  - the live view build on a paginating fake.

### Phase 5: Gated template upgrades

- 6.8, after PROP-01 phase 5.
- **Accepted when**:
  - A new bootstrap revision rolls out by tier.
  - Each cluster re-qualifies before access moves to the new key, with no access gap.
  - A failing qualification halts the rollout at Capsule.
- **Tests**: the planner with the Capsule step and `NaaSQualified`, the halt, and the overlap.

## 10. Decisions (2026-11-23)

All seven questions were closed with the recommendation:

1. **Qualification validity**: 30 days, with overlapping renewal; any change to what it checked ends it
   sooner.
2. **Test identities**: the qualifier impersonates two test groups, limited by a Role to the two throwaway
   sub-tenants.
3. **Default `allowedGroups`**: empty. Access stays closed until an operator sets the list. Ships with
   phase 1.
4. **Caps**: 250 sub-tenants and 2,000 namespaces per cluster, then measured on the sandbox and raised.
5. **`bindings`**: dropped. Refused on create now, removed in the next API version.
6. **Overcommit**: a warning only (`NaaSOvercommitted`), because tenant pools scale.
7. **Capsule webhooks**: an eleventh qualification check, admission available during a platform-pool
   outage.

### Questions as asked

1. **Qualification validity.** 30 days, with overlapping renewal. *Recommendation*: 30 days. Shorter
   gives no extra safety, because every change that matters already changes the key.
2. **Test identities for the qualifier.** Impersonate two test groups (no identity provider change), or
   use real test users at the issuer? *Recommendation*: impersonation, limited by a Role to those two
   groups. Real users need credentials in the cluster.
3. **Default `allowedGroups`.** Empty (access closed until the operator sets it), or a default prefix such
   as `cv-subtenant-*`? *Recommendation*: empty. A default prefix would open access on every install whose
   identity provider happens to have such groups.
4. **Caps.** 250 sub-tenants and 2,000 namespaces per cluster. *Recommendation*: start with these, and
   measure Capsule's replication time at the large class in the phase 1 sandbox run (GTR resync is 60
   seconds over every Tenant).
5. **`bindings` removal.** Refuse on create now and drop in the next API version, or keep for a resolver
   later? *Recommendation*: drop. Groups cover the need, and no resolver is planned.
6. **Overcommit.** Warning only, or refuse quotas above the cluster's allocatable? *Recommendation*:
   warning. Tenant pools scale with Karpenter, so allocatable is not a fixed ceiling.
7. **Capsule webhooks.** PROP-03 keeps Capsule on the bootstrap group, with fail-closed `namespaces` and
   `replications` webhooks. Confirm that the qualification suite should add an "admission available during
   a platform-pool outage" check. *Recommendation*: yes, as an eleventh check.
