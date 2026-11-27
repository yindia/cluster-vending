# cluster-vending architecture

This is the one design document of the repository. It describes the code on
`main` as it is: where an older document and the code disagreed, the code was
taken as the truth. Things the code does not do are listed in
[7. Known limits](#7-known-limits), not described as planned.

Contents:

1. [Overview](#1-overview)
2. [What we have](#2-what-we-have)
3. [How it works](#3-how-it-works)
4. [CR definitions](#4-cr-definitions)
5. [Configuration](#5-configuration)
6. [Operating it](#6-operating-it)
7. [Known limits](#7-known-limits)

## 1. Overview

cluster-vending creates AWS networks (VPCs) and EKS clusters for tenants by
writing Crossplane managed resources (provider-upjet-aws, namespaced
`*.aws.m.upbound.io` kinds) into a management cluster. It can also import an
existing VPC or EKS cluster read-only and, later, take it over. On top of that
it installs platform components (Karpenter, Kyverno, Istio and others),
installs tenant components on request, rolls Kubernetes minors and platform
releases out through release channels, and scans workload clusters for
deprecated APIs, policy violations and resource recommendations.

The repository builds one binary, `cluster-vending`, with three roles:

| Role | Command | AWS access | What it does |
|---|---|---|---|
| API server | `cluster-vending serve` | Read-only, to a cluster's results bucket, through the tenant role | Serves the Connect API (`cloud.v1`). Validates requests, stores the desired state as custom resources, reads scan results. It runs no reconcile loop and installs nothing. |
| Controller | `cluster-vending controller` | Yes: the lookups, the account access check, and the Crossplane providers (which run as the management role) | Reconciles the custom resources: tenants, accounts, networks, clusters, releases and rollouts, scanners. Installs the CRDs, the providers and the component catalog on start. |
| CLI | `cluster-vending <group> <verb>` | None | Calls the API server (`--server`) and prints tables, JSON or YAML. |

The tenant console (`ui/`) is a Next.js application. It calls the same Connect
API, keeps no state of its own, and has no AWS access.

```mermaid
flowchart LR
  CLI[CLI] -->|Connect RPC| API[serve: API server]
  UI[ui: console] -->|Connect RPC| API
  API -->|writes desired state| CR[(CRs in the management cluster)]
  CTRL[controller] -->|watches and reconciles| CR
  CTRL -->|kubeconfig via ClusterAuth| WL[(workload EKS cluster)]
  CTRL -->|own credentials, then tenant role| AWS[(tenant AWS account)]
  CR --> XP[Crossplane: provider-upjet-aws, provider-helm]
  XP --> AWS
  XP -->|Helm releases| WL
  API -->|GetObject, read-only, tenant role| RES[(results bucket in the tenant account)]
  CTRL -->|writes scan results| RES
```

Who talks to AWS: the controller and the providers make every change in AWS.
The controller also makes read-only lookups (the VPC or EKS cluster of an
import, the EKS name of a new cluster, the versions EKS offers, the account
access check). The API server makes one kind of call: it reads a cluster's
results objects from that cluster's bucket. Every access to a tenant's account
goes through the tenant role stored on the account, never through the
platform's own credentials alone ([AWS access and accounts](#aws-access-and-accounts)).

## 2. What we have

**Accounts and tenants**

- Tenants: a cluster-scoped `Tenant` registers a tenant and owns its namespace
  `tenant-<id>`. Tenant listing, and tenant freezes (see Freeze).
- AWS accounts: `CloudAccount` onboards an account with an external ID per
  account. The API serves three CloudFormation role templates (reader,
  updater, provisioner) and prints the create-stack command.
- Access check: the controller assumes the tenant role in each region the
  account uses and checks the calls the platform makes. It runs on create,
  on change, every 6 hours, and on request (`account verify`). Networks and
  clusters are refused until the account is verified.

**Networks**

- Create with a CIDR layout (carved default subnets, or explicit zonal
  subnets), NAT mode `Single` or `PerZone` (`Regional` is refused), optional
  VPC flow logs and tags.
- Import of an existing VPC (read-only), review of its plan, then takeover.
- Update of display name, tags and tier. Delete in AWS dependency order.

**Clusters**

- EKS clusters: Kubernetes minor (from the channel, or pinned), endpoint
  access, secrets encryption (a platform KMS key or an existing key), EKS
  access (admin principals, access entries, access policies), IRSA, control
  plane log group, EKS add-ons with recommended versions. Node groups are the
  platform's: a system managed node group and a Karpenter platform pool.
- Cluster OIDC sign-in (Microsoft Entra ID, or any OIDC issuer) through an EKS
  identity provider association ([Cluster OIDC](#cluster-oidc)).
- Import of an existing EKS cluster, then takeover, and a kubeconfig download
  (IAM, or OIDC with `kubectl oidc-login`).
- Node groups: exactly two, both the platform's, from its config: a system
  managed node group of a static size (Karpenter and the EKS add-ons) and the
  Karpenter platform pool (everything else). Tenants define none
  ([Node layout](#node-layout)).
- Retry of failed components and automatic upgrades (`cluster retry`).

**Components**

- Platform components, installed with every cluster and never enabled by a
  tenant: Karpenter, AWS Load Balancer Controller,
  External Secrets, Kyverno and the Istio ambient bundle.
- Tenant components, enabled per cluster with validated values: `gpu-operator`
  and `kyverno-policies`.
- The catalog (`ComponentDefinition` objects) is synced from the build by the
  controller. A definition can be disabled or removed without a build.
- Helm reliability: classified errors, backoff, one Helm operation per
  cluster at a time, recovery of stuck releases.

**Releases, channels and upgrades**

- Releases are built from `pkg/catalog/release.yaml`: Kubernetes minors,
  chart pins per minor, the renderer version. Channels: `alpha` (pinned to the
  shipped release), `beta` and `stable` (automatic, after a release soak).
- Kubernetes rollouts by tier (Tier2, then Tier1, then Tier0), with
  `maxInProgress`, `maxFailed`, soak, maintenance windows and per-cluster gates.
  Automatic upgrades are off by default and irreversible when on.
- Platform component moves to a new release, one component at a time, and
  code changes (a new controller build that renders objects differently) with
  the same rollout rules.
- Admin read views: `admin release` and `admin channel`, with the rollout plan
  of each channel.

**Freeze and pause**

- Platform, tenant, network or cluster freezes (`admin freeze`,
  `tenant freeze`). A freeze pauses the Crossplane resources and Helm releases
  of its scope (`crossplane.io/paused`), stops upgrades, repairs, import
  discovery and deletion, and is confirmed by the controller (`--wait`).
- Exceptions let one cluster through a platform freeze. `--until` ends a
  freeze by itself.

**Scans and visibility**

- Deprecated and removed API versions (Pluto's rules, read from the objects'
  last-applied manifests and Helm release records), every 6 hours.
- Resource recommendations from a week of usage (metrics-server and VPA),
  sampled every 5 minutes.
- Policy violations and retained admission events (PolicyReports, Gatekeeper
  constraints, events), collected every 5 minutes.
- Results are kept in an S3 bucket per cluster, in the tenant's account
  ([Results in S3](#results-in-s3)). The custom resource keeps summaries.
- Events: the controller's events on each object, the last ten on the
  object's status, and `cluster events` / `network events`.
- Cluster and network topology views in the console, from the API's
  `architecture` projection of the recorded status.

**Authentication and metrics**

- API authentication with OIDC bearer tokens, off by default. Authentication
  only: there are no roles, and any authenticated user can act on every tenant.
  The CLI has `login`, `logout` and `whoami`.
- Prometheus metrics on all three processes (RPC, auth, store and live-read
  metrics on the API; fleet, rollout, transition and reconcile metrics on the
  controller; reads on the live reader), with alerts and runbooks
  ([Metrics and alerts](#metrics-and-alerts)).

## 3. How it works

### Request flow

A create, from the API to Crossplane:

1. A client calls an RPC (the CLI, the console, or another Connect client).
   With authentication on, the interceptor verifies the bearer token and checks
   the caller's role for the RPC's tenant ([Authentication](#authentication)).
2. The handler validates the request against the contract, reads the target
   account, network or cluster from the store, and resolves the defaults
   (`pkg/network` for the layout, `pkg/cluster` for the node pool and the
   version). A network or cluster in an account that is not verified is
   refused with `FAILED_PRECONDITION` and nothing is stored.
3. The handler writes the custom resource (a `Network` or `Cluster` in the
   tenant's namespace) with the resolved spec and phase `Provisioning` (an
   import: `Imported`, with its import `Discovering`). The call returns the
   object's ID, its etag and the name. Nothing has been written to AWS.
4. The controller's watch sees the change of spec, labels or annotations.
   Changes of status alone do not wake it, so its own writes do not loop. The
   controller calls the reconciler for that object (`Reconcile(tenant, id)`).
5. The reconciler renders the desired objects (`pkg/crossplane/aws`:
   `NetworkSet`, `ClusterSet`), applies them with server-side apply through the
   sequence engine (`pkg/crossplane/sequence`), and only what is ready. What
   waits is held back and applied on a later pass.
6. Crossplane creates the cloud resources and reports conditions on the
   objects. provider-helm installs the platform and tenant components.
7. The reconciler records the status on the object: phase, conditions,
   inventory, recent events. A network becomes `Ready`, a cluster `Running`,
   when every resource is ready and the generation is applied. Failures appear
   in the status (`Failed`, or a message saying it is retrying). A call does
   not fail because AWS did.

Requeue times: 5 s while a generation is not applied in full, 20 s while AWS
works on it, 1 m for an import that is only observed, 30 s after a failed pass,
and a 10 m resync once settled.

### Packages

Each layer has one job. The import rules were checked with `go list -deps`.

| Package | Job | May import |
|---|---|---|
| `idl/cloud/v1` | The `cloud.v1` protos, the contract. Generated Go in `pkg/gen`, TypeScript in `ui/src/gen`. | Nothing in the repository. |
| `pkg/crossplane/aws` | Renders a plan into the provider objects (the upstream provider-upjet-aws and Crossplane types), as lists and as ordered sets. | `pkg/crossplane/sequence`. |
| `pkg/crossplane/sequence` | The engine. `Apply` makes the management cluster match a set: applies what is ready, holds back what waits, builds late values, removes what the set dropped. `Delete` removes an owner's objects tier by tier. | Kubernetes only. Knows nothing about networks or clusters. |
| `pkg/crossplane/managed` | Reads an object's state from its conditions, deletes objects, releases Usages whose user is gone. | Kubernetes only. |
| `pkg/network`, `pkg/cluster`, `pkg/component`, `pkg/rollout`, `pkg/upgrade` | Logic with no API and no storage: defaults, layouts, render plans, import plans, update changes, catalog checks, rollout plans. | No RPC library, no storage, no server. |
| `pkg/reconcile` | `Deps` (the store, the management cluster, the AWS lookups), the bookkeeping every owner shares, the checks a request and a reconciler share, and `AWSAccess` (always through the tenant role). | No RPC library, no `pkg/server`. |
| `pkg/reconcile/network`, `cluster`, `account`, `deprecations`, `recommendations` | A reconciler for each kind. They are the only code that looks at AWS, besides the access and results readers. | As `pkg/reconcile`. |
| `pkg/management` | Prepares the management cluster for the two commands: the manager, the store, and (for the controller) the providers, CRDs and AWS lookups. | No RPC library. |
| `pkg/server/*` | The RPC handlers: validate, read and write rows, map errors to Connect codes. They make no AWS call except reading results. | Everything above. |
| `pkg/auth`, `pkg/server/authn` | Who calls the API: the `auth` settings, OIDC token verification, and the audit log of writes and sensitive reads. No authorization. | No storage. |
| `pkg/database/repository`, `pkg/database/cloudstore`, `pkg/database/model` | The storage interface, its one implementation on the custom resources, and the row types. | `repository` and `model` import nothing else from the repository. |
| `pkg/controller/*`, `pkg/convert`, `pkg/owner`, `api/v1alpha1` | The custom resource types, the controllers, the manager, owner references, and the conversion between the typed spec and the contract. | Types import nothing from `pkg/`. |
| `pkg/results`, `pkg/reconcile/resultsink` | Writes and reads a cluster's results objects in S3, with bounds and ETags. | The S3 client only. |
| `pkg/catalog`, `pkg/release` | The shipped catalog and release, and their checks. | Kubernetes types and `pkg/`-level checks. |
| `pkg/buildserial` | The build's serial, stamped on the CRDs, the shipped definitions and the shipped release, so an older build never writes over a newer one ([Runbook: upgrade and roll back the platform](#runbook-upgrade-and-roll-back-the-platform)). | The standard library only. |
| `pkg/cli`, `pkg/cli/login`, `cmd/` | The CLI commands and the command tree. `cmd/` is thin. | `pkg/cli` imports only the generated client. |

### Storage

- There is no database. The custom resources are the store: `Tenant`,
  `CloudAccount`, `Network`, `Cluster`, `Component`,
  `ComponentDefinition`, `Release`, `ReleaseChannel`, `ReleaseHold`
  ([CR definitions](#4-cr-definitions)). `pkg/database/cloudstore` implements
  the repository interface on a direct (uncached for ownership decisions)
  Kubernetes client.
- Object names: a network's or cluster's name is its object name in the
  tenant's namespace. Its ID is a UUID, stored in the label
  `cloud.cluster-vending.io/id`; the API takes IDs.
- Uniqueness that is not an object name (a create request ID, an imported VPC
  per account and region) is a claim object whose name is derived from the
  value, so of two concurrent creates one wins.
- An update must present the object's etag. Recording status does not change
  the etag.
- Deletes are real deletes. There is no history of deleted rows.
- Inventory: each owner's `status.inventory` lists every Crossplane object it
  rendered, with its external name and creation order. `status.componentInventory`
  does the same for the platform components of a cluster.
- Owner references follow the chain: a network is owned by its account, a
  cluster by its network. A finalizer on each kind keeps an owner until nothing
  depends on it; deleting an owner never cascades.

### AWS access and accounts

One rule holds for the controller's lookups and for the Crossplane providers:

```
own credentials  ->  tenant role (always)
```

- **Own credentials** are whatever the AWS SDK's default chain finds: IRSA in
  production, environment keys for local runs. They are never a flag value or
  a file of the project.
- **The tenant role** is the role the account was onboarded with. Each
  account's Crossplane `ProviderConfig` assumes it (`assumeRoleChain`). Nothing
  is read with the own credentials directly, so the keys of one account cannot
  read another. A role that is not the account's is refused.
- **Source role** (optional, `aws-source-role-arn`): the own credentials assume
  it first; the tenant role is then assumed with its credentials.
- **External ID**: 64 random hex characters, generated when the account is
  first onboarded and kept. It is passed as `sts:ExternalId` on every tenant
  role assumption, and given only to the account's tenant.

Onboarding:

1. `account create -f` (or the `BootstrapTenant` RPC) records the account in
   the tenant's namespace. The tenant and its namespace must exist (`tenant create`
   first). The API prints the template URL, its parameters and the create-stack
   command. Parameters: `RoleName`, `ManagementRoleArn`, `ExternalId`, and
   `ServerPrincipalArn` (the API server's role, which reads results) when set.
   It then prints a second, recommended stack: the account's `reader` role
   (`cluster-vending-reader`, same `ManagementRoleArn` and `ExternalId`, no
   server principal), which each cluster's live-reader access entry and the
   account's live `ProviderConfig` use. Without it the live access entry fails
   (`The specified principalArn is invalid`) and the cluster has no live reads;
   nothing else depends on it. An account whose role is the reader gets no
   second stack.
2. The tenant runs the command with its own AWS credentials. The role's trust
   trusts the management role, and the server principal when given, and both
   only with the external ID.
3. The CloudAccount controller applies the namespace's `ProviderConfig`. In
   the local secret mode it also copies the configured key of the platform
   Secret into the tenant namespace.
4. `account verify --wait` asks for the access check, and waits for the verdict.

The templates are generated by `scripts/generate.py` (troposphere and awacs)
into `pkg/cloudformation/templates`, embedded in the binary and served read-only
at `/cloudformation/`. Three roles, each a superset of the one before:

| Role | Allows | Use |
|---|---|---|
| `reader` | Describe and list of EC2, EKS, IAM, KMS, CloudWatch Logs, SQS and EventBridge | Imports and observation. The access check reports `ec2:CreateVpc` and `ec2:CreateTags` as not allowed; the account is usable for import only. |
| `updater` | `reader`, plus changes to what exists (tags, rules, EKS updates, IAM trust and policy versions, KMS aliases) | Accounts that take over and update existing resources. The `ec2:CreateTags` DryRun passes; `ec2:CreateVpc` does not, so it imports and takes over but creates nothing. |
| `provisioner` | `updater`, plus creating and deleting VPCs, subnets, gateways, EKS clusters and node groups, add-ons, IAM roles and policies | Accounts meant for the platform to create resources in. |

The stack name and role name follow `cluster-vending-<role>` (or
`cluster-vending-<anything>-<role>`, up to 64 characters). Deleting the account
removes the platform's `ProviderConfig`; the role's stack is the tenant's to
delete.

**The management account's roles.** The same script (`scripts/management.py`)
generates two templates for the platform's own AWS account, embedded and served
next to the tenant ones (`/cloudformation/management.yaml`, `/cloudformation/server.yaml`).
They are created once, before any tenant account; `account create --management`
prints both create-stack commands (no manifest, no tenant, no API call: the server
serves the templates without a management role configured).

| Template | Runs as it | Trust | Permissions | Output, and where it goes |
|---|---|---|---|---|
| `cluster-vending-management` | The controller and the Crossplane AWS providers | IRSA: `system:serviceaccount:<PlatformNamespace>:<ServiceAccount>` (the controller, default `cluster-vending-system`/`cluster-vending-controller`, the Helm chart's; `cluster-vending-controller-manager` with `config/default`) and, by `StringLike`, `system:serviceaccount:<CrossplaneNamespace>:provider-aws-*` (Crossplane names a provider's service account after its revision, `provider-aws-ec2-<hash>`); and/or `LocalPrincipalArn` | `sts:AssumeRole` on `arn:<partition>:iam::*:role/<TenantRolePattern>` (default `cluster-vending-*`), nothing else | `ManagementRoleArn`: `aws-management-role-arn` (controller and server), and every tenant stack's `ManagementRoleArn` |
| `cluster-vending-server` | The API server | IRSA: `system:serviceaccount:<PlatformNamespace>:<ServiceAccount>` (default `cluster-vending-server`); and/or `LocalPrincipalArn` | the same | `ServerRoleArn`: `aws-server-role-arn`, which tenant stacks trust as `ServerPrincipalArn` |

Parameters of both: `RoleName`, `OidcProviderArn` (the management EKS cluster's IAM
OIDC provider; the issuer is taken from it), `PlatformNamespace`, `ServiceAccount`,
`LocalPrincipalArn` (an IAM user or role, for local runs), `TenantRolePattern`,
`MaxSessionDuration` (3600 to 43200 seconds, default 3600), and `CrossplaneNamespace`
(management only, default `crossplane-system`). A CloudFormation rule requires
`OidcProviderArn`, `LocalPrincipalArn` or both. The platform makes no other AWS
call with these identities (`sts:GetCallerIdentity` needs no permission) and
passes no session tags, so there is no `sts:TagSession`. They carry their own
version, `cloudformation.ManagementTemplateVersion`, as the same tag and output;
the platform does not read it back.

How each role is created:

| Role | Production | Local (`docs/local-development.md`) |
|---|---|---|
| Management | `account create --management`'s stack with `OidcProviderArn`; its ARN in `aws.managementRoleArn` and the controller's service account annotation | Mode A: none. Mode B: the stack with `LocalPrincipalArn` (your identity) and `MaxSessionDuration=43200`, then a session of it for the controller, the server and the providers' Secret |
| Server | The same, second stack; its ARN in `aws.serverRoleArn` and the server's service account annotation | Optional: the server runs with the controller's identity |
| Tenant provisioner (or updater, reader) | `account create -f`, run by the tenant in its account | The same |
| Tenant reader | `account create -f`'s second stack, recommended | The same |

**The access check** (controller, `pkg/reconcile/account`): for each region the
account's networks use, plus `us-east-1` and any region added with `--region`:

| Call | How | Needed for |
|---|---|---|
| `sts:GetCallerIdentity` after assuming the tenant role | regional STS endpoint | Every call. The account and role of the answer must match. |
| `ec2:DescribeAvailabilityZones` | read | Network layouts and zones |
| `ec2:DescribeVpcs` (one page) | read | Import and observation |
| `ec2:CreateVpc` | dry run: `DryRunOperation` when allowed, `UnauthorizedOperation` when not | Creating networks |
| `eks:ListClusters` (one page) | read | EKS name checks, import, observation |
| `iam:GetRole` of the tenant role | read, once | IAM roles the platform creates and reads |

Usability is per capability, from the per-check results (`pkg/reconcile/account/capability.go`):
`Create` needs every check (`status.access.verified`); `Import` needs the reads
(`DescribeAvailabilityZones`, `DescribeVpcs`, `ListClusters`) in the region; `TakeOver`
needs those and the `ec2:CreateTags` DryRun. CreateCluster and CreateNetwork ask for
`Create`, ImportCluster and ImportNetwork for `Import`, the take-overs for `TakeOver`.
A reader or updater account is not denied: `AccessVerified` is `True` with reason
`VerifiedForImport` and says what it allows and what failed, and a request for more is
refused as `FAILED_PRECONDITION` with that message. A region that failed still refuses all.

Its result is `status.access` (time, principal, each region and call with its
reason) and the condition `AccessVerified`. Reasons: `AssumeRoleDenied`,
`DeniedBySCP`, `NotAllowed`, `WrongAccount`, `RegionNotEnabled`, `Unavailable`
(transient), `NoTenantRole`. A denied account is `Failed`. A transient failure
keeps the last verdict, marked `stale`, and retries after 15 minutes. Reads,
updates, deletes, pauses and resumes never check.

**Template versions.** `cloudformation.TemplateVersion` (`pkg/cloudformation/version.go`) is
bumped whenever a template's permissions or trust change; a test (`version_test.go`) fails when
a template changes without it. Every template writes the version as the tag
`cluster-vending.io/template-version` on the tenant role and as the stack output
`TemplateVersion`. The access check reads the tag (`iam:ListRoleTags`, which every role
grants itself) once per check and keeps it among `status.access.checks` (`iam:ListRoleTags`,
always `ok`, message `template version N`). A role without the tag is version 0. When the
stack is older than the build, the account gets the condition `RoleTemplateCurrent` `False`
(reason `TemplateOutdated`) saying "the account's role template is version N, this platform
needs M; update the stack: ..."; the account is not failed. `account create -f` for such an
account prints `update-stack` instead of `create-stack`. A refusal of take over or create
names the old template. A tag that cannot be read leaves the version unknown and says nothing.

**Bounded AWS calls.** Every SDK operation is bounded by `aws.Config.CallTimeout` (default
20s, retries included; each page of a list separately), whatever context the caller gives; S3 is
left to the HTTP timeouts, since its calls return a body read after the call. The SDK config has
3 attempts with at most 5s of backoff, a 30s HTTP timeout, and dial, TLS and response-header
timeouts. The reconciler's lookups (`resolve`) add a 60s bound, and an access check bounds
a region at 90s. A call that times out is `Unavailable`: retried, never a denial or a failure.

### Create, import and takeover

**Create** (network or cluster). Cluster-specific steps before the first
render: the EKS name is reserved and checked against EKS (no cluster of that
name in the account); the Kubernetes minor and add-on versions are resolved
against EKS for the channel. The result is written to the spec and recorded
with `resolvedGeneration`, so an update that changes neither the version nor
the add-ons does not repeat the lookups. After the apply, the reconciler tags
the network's subnets for the cluster. Replaced node groups are retired after
the new one is ready.

**Import.** `network import` and `cluster import` store the request as an
object in phase `Imported` with its import `Discovering`. They do not look at
AWS. The controller looks the VPC (or EKS cluster) up once, and turns what it
found into a plan, stored in `status.importPlan` before anything is applied.
The plan has one line per resource with a disposition: `Matches`,
`WillUpdate`, `WillCreate`, `NotImported`, `Blocking` or `CarriedOver`.

- A plan with a `Blocking` line imports nothing. The import is `Blocked` with
  that reason. Delete it and import again.
- Otherwise the object is `Imported`. Its managed resources use
  `managementPolicies: [Observe]` (a network) or `Observe` with late
  initialization (a cluster). Crossplane reads them and changes nothing.
- A lookup that did not answer, or credentials that cannot read the resource,
  keep it `Discovering` and are retried. A VPC or cluster that does not exist
  where the import says is final.
- An imported object is read-only. A change made with `kubectl` on its object
  is ignored (`SpecEditIgnored`) and the remembered spec is put back.
- **Carried-over** objects came with the import and no spec field describes them
  (for example an existing flow log). They are marked and never removed on a
  re-render. They are deleted with their owner.

**Takeover** (`takeover <id>`, with `--acknowledge-not-imported` when the plan
has `NotImported` lines). It makes the platform the owner: the set is rendered
again as owned and applied through the engine. Nothing is read and edited in
place, and AWS is not looked up again: the objects discovered by the import are
stored with the object for this. An imported network is never rendered from
its spec, since that would create a second VPC. An import from before its
discovery was stored has nothing to render from, and its takeover fails with a
message to import it again.
A takeover is also refused (`FAILED_PRECONDITION`, listing the differences) when the cluster
changed in AWS after the import: the Observe-only objects report another Kubernetes version,
endpoint access or CIDRs, node group scaling or instance types, add-on version or access
entry groups than the stored plan, which full management would push back. The user imports
it again and reviews the new plan (discovery is not re-run on its own). The takeover also
needs the release of the cluster's channel (stable when it names none) to offer the minor
the cluster runs, unless it skips the platform components.

A taken-over cluster gets the platform components like any other cluster,
unless the takeover asked for `skip_platform_components`. The platform never
deletes or adopts what a cluster came with: a component the cluster already
runs fails with the provider's error, and is installed once that is removed.

### Ordering rules

The engine applies a set in dependency order. These rules are conditions on
creation: once an owner has an object, or an object that waited for the same
condition, it is applied whatever the condition says. While anything waits, the
engine removes nothing.

| Rule | Where it lives |
|---|---|
| NAT gateways are created only once the internet gateway is ready. | A `Ready` condition on the NAT items of `NetworkSet`. |
| Add-ons, access entries, policy associations and pod identity associations wait for the EKS cluster to be `ACTIVE`. A Usage whose user waits waits with it. | A `Field` condition in `ClusterSet`. |
| Values that exist only after another object: the platform's KMS key ARN, the cluster's OIDC issuer (for IRSA), its security group. A new cluster waits for its log group. | Items with a `Build` in `ClusterSet`, filled by `ClusterPlan.FillLive`. |
| During a Kubernetes upgrade, node groups and add-ons stay at the running version until the control plane reports the target. | `upgrade.phase` (`pkg/reconcile/cluster/upgrade.go`), reported as `status.upgrade`. |
| A change EKS cannot make in place on a node group (instance types, capacity type, AMI type, disk size of `nodeGroups.system`, or an earlier build's launch template dropped) creates a new node group. The old one is retired once the new one is ready. | `NodeGroupName` and retirement on a later pass in `pkg/reconcile/cluster`. |
| The platform pool's NodePool and EC2NodeClass are applied only after Karpenter is ready. | `pkg/reconcile/cluster/nodegroups.go`. |

### Deletion

Deletion runs in reverse order of creation. A network with a dependent cluster,
or an account with a network, is not deleted: the condition `DeletionBlocked`
names the dependents.

**Networks** are deleted tier by tier, in the order AWS accepts: flow log, ACL
rules, routes and associations; then log group, route tables and NAT gateways;
then role, policy and Elastic IPs; then subnets and the internet gateway; then
the VPC. A tier starts only when the one before it is gone. A VPC that AWS
refuses to delete for dependencies says so in the message, naming the VPC to look in.

**Clusters** go in seven stages (`driveDelete`, `pkg/reconcile/cluster/delete_stage.go`).
A stage starts only when the one before is fully gone, and the message names the
stage and what it waits for.

1. **Load balancers** in the workload cluster, while the AWS Load Balancer
   Controller still runs: the ALB Ingresses and every Service of type
   `LoadBalancer`. A load balancer left behind blocks the VPC's deletion.
2. **Tenant components**, a component after those that require it.
3. **Platform components above sequence 0**, highest sequence first. Each is
   uninstalled (its Helm releases, then its IAM objects) before the next lower
   sequence starts.
4. **Node groups**: the Karpenter NodePools (users' and any old platform pool),
   then the remaining NodeClaims (Karpenter terminates the nodes), then the
   EC2NodeClasses.
5. **Karpenter** (sequence 0): its chart and IAM role, then its interruption
   queue, its EventBridge rules and targets, and the node role and access entry.
   Before the node role is deleted, it is taken out of the instance profiles
   Karpenter made for this cluster (tagged `karpenter.k8s.aws/ec2nodeclass` or
   `karpenter.sh/managed-by`, and for this cluster), and those profiles are
   deleted, through the tenant role. A forced or timed-out node group stage
   leaves them, and IAM refuses to delete a role a profile still holds. No
   other profile is touched.

   Between the chart and the node role runs the **cloud sweep**
   (`pkg/reconcile/cluster/delete_cloud_sweep.go`, `awscloud.SweepCluster`):
   with the tenant role, in the cluster's region, it finds by the cluster's
   tags what its controllers made and the workload stages did not delete,
   and deletes it:

   | What | Found by | Done |
   |---|---|---|
   | Karpenter's EC2 instances | `karpenter.sh/nodepool` (any value) and `kubernetes.io/cluster/<eks name>=owned` | terminated, then waited for until EC2 says terminated |
   | Karpenter's launch templates | `karpenter.k8s.aws/cluster=<eks name>` | deleted |
   | The AWS Load Balancer Controller's load balancers (ELBv2) | `elbv2.k8s.aws/cluster=<eks name>` | deleted |
   | Its target groups | `elbv2.k8s.aws/cluster=<eks name>` | deleted once no load balancer of the cluster is left |
   | Its security groups | `elbv2.k8s.aws/cluster=<eks name>` | deleted after that; `DependencyViolation` (the load balancer's interfaces linger a few minutes) is retried |

   It runs for every deletion whose EKS cluster the platform deletes, also a
   forced one and after stage 1 or 4 timed out or was skipped; an imported
   cluster whose EKS cluster is only observed or retained is skipped
   (`CloudSweepSkipped`). Nothing without those tags is touched (a managed
   node group's instances have no `karpenter.sh/nodepool`). Each pass is
   bounded (ten pages per call, one minute); its state is `status.cloudSweep`
   (start, result, and the IDs still there in `waiting`). It ends with
   `CloudSweepFinished`, or after `--teardown-timeout` from its own start with
   a `CloudSweepTimedOut` warning naming what is left by ID, and the deletion
   goes on. AWS errors are the stage's message and retried; an account whose
   role template is older than version 2 is told to update its stack.

   The sweep needs, in the provisioner role (template version 2):
   `ec2:DescribeInstances` (the reader's `ec2:Describe*`),
   `ec2:TerminateInstances` on instances tagged `karpenter.sh/nodepool`,
   `ec2:DeleteLaunchTemplate` and `ec2:DeleteSecurityGroup` (already in
   `ProvisionNetwork`), `elasticloadbalancing:DescribeLoadBalancers`,
   `DescribeTargetGroups` and `DescribeTags`, and
   `elasticloadbalancing:DeleteLoadBalancer` and `DeleteTargetGroup` on
   resources tagged `elbv2.k8s.aws/cluster`. Accounts created from an older
   template need `aws cloudformation update-stack` with the template the API
   server serves (`account create` prints the command when the stack is
   outdated).
6. **EKS add-ons** with their pod identity associations, roles and policies.
7. **The rest**, ordered by Usages: the default managed node group, the EKS
   cluster, the cluster's IAM, the results bucket (emptied first, every pass
   until the bucket is gone), unrecorded leftovers (including Helm Releases and
   the Helm ProviderConfig), the subnet tags, and the records. A bucket that
   cannot be emptied holds back only the Bucket and its settings: everything
   else is deleted, and the cluster's status names the bucket problem.

Rules that apply throughout:

- Helm reliability never blocks an uninstall. Deletion lifts the platform's own
  backoff pause on the releases it uninstalls, and recovers a stuck release. A
  pause set by an operator or a freeze is not lifted, and the stage waits and says so.
- The load balancer stage ends after `--teardown-timeout` (default 15 m), and
  the node group stage after the same time from its start: one clock for its
  pools, claims and classes, and for an API that does not answer. Each ends with
  one `TeardownTimedOut` warning naming what is left, and the deletion goes on;
  the cloud sweep then deletes what they left in the account. The node group
  stage's start, and its timed-out finish, are kept in `status.nodeTeardown`, so
  a controller restart neither starts its clock again nor warns again.
- Whether the workload cluster is used is decided by trying, not by the EKS
  status alone: with a kubeconfig Secret and an EKS cluster that is `ACTIVE` or
  `UPDATING` (an upgrade), the API is called, and stages 1 and 4 retry a failing
  API until `--teardown-timeout`. A forced deletion (`cluster delete --force`),
  a cluster with no kubeconfig Secret or whose EKS cluster does not answer
  (`CREATING`, `DELETING`, `FAILED`) skips the workload stages
  (`TeardownSkipped`, with the cause in the message), and its Helm Releases are
  let go without an uninstall.
- An `ACTIVE` or `UPDATING` cluster whose API is down in stages 2 and 3 waits
  `--teardown-timeout` from when it was first seen down (at once when the
  teardown stage already gave up), then its Helm Releases are let go without an
  uninstall (`HelmReleasesLetGo` warning). The stage message names the cause and
  `cluster delete --force`, which skips the wait. The wait restarts with the
  controller. Releases let go stay without Delete.
- The Usage webhook may refuse a deletion while a user exists. That is "waiting
  for its users", not a failure.
- The reconciler asks the API server directly before it forgets: the records and
  the row are removed only when nothing is left in the cloud.
- PersistentVolumeClaims are not deleted. Their volumes block nothing in the VPC,
  and deleting a claim deletes data.
- Cloud accounts: the `ProviderConfig` is removed, then the finalizer. The
  tenant namespace stays.

### Platform components

A `ComponentDefinition` with `management: Platform` is installed on every
cluster of its cloud by the cluster's reconcile, once the cluster is `Running`
(its node group is ready and its kubeconfig exists). Components install in
ascending `sequence` (default 100), and within a sequence in `requires` order.
Each component installs its IAM role first (`cloudResources.iamRole`, IRSA),
then its CRD chart (when it has one), its chart, and its companions.

The install order of a new cluster:

1. The EKS cluster, its node groups and its add-ons (part of reaching `Running`).
2. Karpenter (the `NodeProvisioner` role: sequence 0, the platform's first
   component; everything after it waits for it).
3. The other platform components by sequence (default 100), in `requires`
   order, on the platform pool: AWS Load Balancer Controller, External
   Secrets, Kyverno and the Istio bundle.
4. The tenant components, when the tenant enables them.

Each platform component's version and locked values come from the release the
cluster's channel offered when the components were first installed
(`status.platformRelease`). A release move (see below) changes them. The component states are on the cluster:
`status.platformComponents` (phase, message, Helm state, last error, failures,
recoveries, next retry) and the condition `PlatformComponentsReady`. That
condition does not hold back the cluster's own `Ready`. Platform components are
removed in reverse order when the cluster is deleted.

**Roles.** One definition plays a part of the platform no other does. It
declares it in `spec.role`, and the platform finds it by it, never by name
(`pkg/component/roles.go` is the one place that knows the role and what it
implies). At most one definition per cloud plays the role, and only a Platform
one (`controller --check`, the `Valid` condition):

| Role | Shipped as | What the role implies |
|---|---|---|
| `NodeProvisioner` | `karpenter` | Installs first (sequence 0, whatever `spec.sequence` says; 0 is no other definition's). The only definition on the system group (`${system.*}`), never on the platform pool. Cannot be disabled (CEL). The platform pool is applied once it is `Ready`, and its controller policy is the one the pool's node class needs. Removed last on deletion, after the node groups. |

A definition without a role is an ordinary component: `sequence` and
`requires` order it and `sizing` sizes it, so adding or removing one is a
definition file and a release pin, with no Go change. A shipped definition the
platform team edited before `spec.role` existed is not updated by a build;
until it declares the role, the shipped name (`karpenter`) stands for it, on a
cloud where no definition declares the role.

The shipped platform set (`pkg/catalog/definitions`):

| Component | Chart (pinned in `release.yaml`) | Namespace | Notes |
|---|---|---|---|
| `karpenter` | `karpenter` 1.15.1 (OCI, public ECR) | `kube-system` | `role: NodeProvisioner` (sequence 0). Its IAM role is IRSA. Ships no NodePool or EC2NodeClass. |
| `aws-load-balancer-controller` | 3.6.2 | `kube-system` | IRSA. |
| `external-secrets` | 2.15.0 | `external-secrets` | |
| `kyverno` | 3.10.0 | `kyverno` | Enabled by default. The policies are `kyverno-policies`, a tenant component. |
| `istio` (bundle) | 1.32.0 | `istio-system`, `istio-ingress` | Members are internal: `istio-base`, `istiod`, `istio-cni`, `ztunnel`, `istio-ingress-gateway`, in that `requires` order. Ambient mode, no sidecars. |

Tenant components: `gpu-operator` (v26.7.2, `gpu-operator`), and
`kyverno-policies` (3.10.0, requires `kyverno`). Only these two have a values
schema.

**Catalog sync.** On start the controller creates or updates the shipped
definitions (`--install-catalog`, default true; `pkg/catalog`). It labels what it
ships `cloud.cluster-vending.io/shipped` and records the hash of the spec it
shipped in `cloud.cluster-vending.io/shipped-hash`:

| On the management cluster | What a build does |
|---|---|
| Missing | Creates it, labelled, with the hash. |
| Shipped, spec still equal to the recorded hash, the build's spec differs | Replaces the spec with the build's. `disabled` is kept. |
| Shipped, the spec changed since (the hash differs) | Leaves it: the platform team's edit. Logged. |
| Not labelled, equal to the build's spec | Adopts it (adds the label and hash). |
| Not labelled, different | Leaves it. Logged, with how to take the build's. |
| Shipped, unedited, no longer in the build | Deletes it, unless clusters use it (the finalizer `definition-in-use` holds it). |
| Shipped, edited, no longer in the build, or the platform team's own | Leaves it. |

`spec.disabled` switches a definition off without deleting it: a platform
component is removed from existing clusters by the gated catalog change (see
Code changes), and a tenant component can no longer be enabled. Definitions that
require it are off with it, in the same change.

**Capsule removal.** Capsule left the catalog with sub-tenants, the same way
cluster-autoscaler did ([Node layout](#node-layout)). `catalog.Sync` deletes the
shipped, unedited `capsule` definition; the `definition-in-use` finalizer holds
it while clusters still have it. The controller then clears the
`NamespaceTenancy` role from any definition still declaring it (the CRD refuses
that role on update), and disables one that is not being deleted (a copy the
platform team edited), so it is not installed on every cluster as an ordinary
component. Each cluster removes Capsule as a gated catalog change (code rollout,
tiers, freeze): a Helm uninstall, whose chart post-delete hook Job removes
Capsule's dynamic webhook configurations, then its IAM. The removals run before
the `kyverno` value change (`capsule-system` no longer excluded) in the same
catalog change. Capsule's CRDs and any tenant namespaces stay on the workload
cluster.

**Providers.** Each definition is for one cloud, in `spec.provider` (`aws`,
`gcp` or `azure`, default `aws`). Only `aws` definitions ship. A variant for
another cloud is a separate definition named `<name>-<provider>`.

### Components and the catalog

The tenant's component API (`ComponentService`, `component` commands):

- `catalog` lists the definitions (who manages each, whether it takes values,
  what it requires). With `--cluster`, it adds the version the cluster's release
  gives each one.
- `enable` stores a `Component` object (phase `Pending`) for a cluster. The
  next pass of the cluster installs it, after the platform's components.
- A tenant cannot set a version. `spec.version` is refused by the API, and the
  installer takes the version of the cluster's release.
- Values: the stored values are only the ones that differ from the defaults. A
  key left out of an update goes back to its default. `--validate-only` shows
  what would be stored. Every problem in the values is reported in one answer.
- Platform components cannot be enabled, changed or disabled
  (`PERMISSION_DENIED`). Internal definitions (members of a bundle) cannot be
  enabled alone.

### Helm operations

Every Helm operation on a workload cluster goes through provider-helm, and the
controller manages it around that
([Helm reliability](#helm-operations)). The rules:

- **Rendering.** Releases are rendered with `wait: false`, `waitTimeout` set to
  the definition's install timeout (`install.timeout`, default 10 m; it bounds
  Helm's hooks), `rollbackLimit: 3` and `maxHistory: 5`. Helm applies the chart
  and writes its release record within seconds; it holds no connection or
  provider worker while the pods start. Each Release carries a digest of the
  chart, version, values and these settings
  (`cloud.cluster-vending.io/helm-digest`). A release whose digest is already
  live is not written.
- **Readiness.** The platform judges a rollout itself (`helmGate.rollout` in
  `pkg/reconcile/cluster/helm_rollout.go`, the one notion of Ready). A release
  is Ready when provider-helm acted on its spec, reports it Ready and Helm
  `deployed`, and its workloads are available in the workload cluster: the
  Deployments, StatefulSets and DaemonSets in the release's namespace that carry
  Helm's annotation `meta.helm.sh/release-name: <release>` (and
  `meta.helm.sh/release-namespace`). Available means the controller observed
  the latest spec (`observedGeneration >= generation`) and the desired replicas
  are updated and available; a DaemonSet with 0 desired counts as available. A
  chart without such workloads (CRDs, policies) is Ready once deployed. A
  component is `Ready` when every one of its releases is; until then it is
  `Installing` with what it waits for, e.g. "waiting for
  deployment/kyverno-admission-controller: 1/3 available". Everything that
  waits for a component reads this: the install order, the one operation per
  cluster, a catalog change's fingerprint, the release move, the upgrade steps,
  the rollout gates, the status and the "installed" event.
- **Reading workloads.** Through the workload kubeconfig (the controller's
  `Rollouts` clients), per cluster pass: the client is opened once, each
  namespace is listed once per kind (pages of 250, at most 4), and all reads of
  a pass within 30 s. A rollout that finished is recorded on its Release
  (`cloud.cluster-vending.io/helm-rollout: <digest>/<revision>/ready`), so a
  settled cluster reads no workloads at all, and later pod churn (a node
  coming or going) does not make a component un-Ready.
- **Install timeout.** It runs on the platform's side, from when the platform
  first saw Helm deploy the release's current digest and revision (the same
  annotation, `<digest>/<revision>/<since>`). A release that is not Ready within
  it failed: `Failed`, "rollout timed out after 10m0s waiting for deployment/X",
  a counted release error that goes into the backoff below, as a failed Helm
  wait did. It counts only time the workloads were read and found
  unavailable: while the workload API cannot be read the release is
  `Installing` with the read error ("waiting for the workloads of chart X to be
  read (...)"), transient and never counted, and a timeout already running
  starts again once the workloads are read. Each new attempt gets a full
  timeout: a `cluster retry`, an automatic recovery (release move, catalog
  change) and the end of a backoff pause remove the release's rollout mark, so
  the clock restarts from the next read. There is no automatic rollback: a failed upgrade stays `Failed`
  with its message until it rolls out (it is then Ready again), a new digest
  replaces it, or a person retries.
- **Order and one at a time.** A new release waits until the releases it
  comes after are Ready: the charts before it of its component (the CRD chart,
  the main chart before a companion) and those of the components it requires
  ("waiting for chart kyverno to roll out (...)"). At most one release of a
  cluster installs, upgrades or rolls out at a time, in `sequence`, then
  `requires` order; a rollout past its timeout no longer counts. The others say
  "waiting for Helm release X to settle".
- **Error classes.** *Transient* (the workload API unreachable, TLS or I/O
  timeouts, throttling, Helm's lock): retried quietly, never counted. *Chart*
  (not found, pull failed): retried and reported, never given up on. *Release*
  (a hook failed, invalid values, the platform's rollout timeout): counted.
- **Backoff.** A failed attempt pauses its release (`crossplane.io/paused`, the
  marker `cloud.cluster-vending.io/helm-backoff-until`). The pause starts at 1 m,
  doubles with jitter, and is capped at `--helm-backoff-max` (default 30 m). A new
  digest ends the backoff at once. A pause set by a person or a freeze is never lifted.
- **Giving up.** After `--helm-max-failures` (default 5) counted failures in a
  row, the component is `Failed` with the classified error and a
  `HelmReleaseFailed` event. Its retries stay at the cap until its version or
  values change, or until `cluster retry`.
- **Stuck releases.** An interrupted Helm operation (a dropped connection, a
  restarted provider) can still leave a release in `pending-install`,
  `pending-upgrade`, `pending-rollback` or `uninstalling`. One left there for its
  timeout plus 2 minutes (10 minutes
  for a release an older build wrote) is recovered with the Helm SDK through the
  workload kubeconfig: the newest revision is marked failed and the release is
  rolled back to the last deployed revision, or uninstalled when none was. A
  failed hook Job is deleted first. At most 3 recoveries per component version
  (`StuckReleaseRecovered`, then `RecoveryLimitReached`). `--recover-stuck-releases`
  (default true) turns this off.
- **Retry.** `cluster retry --id <cluster>` restarts the backoff and the count of
  failed components, recovers a stuck revision at once, and restarts a failed or
  timed-out automatic upgrade. It answers per item what happens (`Retry`, `Fix`,
  `Nothing`).

#### Provider runtime

Each provider runs with its own DeploymentRuntimeConfig,
`cluster-vending-<provider without "provider-">` (`cluster-vending-aws-ec2`,
`cluster-vending-aws-eks`, ..., `cluster-vending-helm`), tuned by the settings
file's `providers` section over the platform's defaults
(`crossplaneaws.DefaultProviderRuntime`, the values the shipped files show):
`defaults` apply to every provider, `overrides.<provider>` to one, field by
field (`replicas`, `poll`, `maxReconcileRate`, `resources.requests|limits.cpu|memory`).

| Provider | Poll | Rate | Requests | Limits |
|---|---|---|---|---|
| default (iam) | 10 m | 10/s | 200m, 512Mi | 1, 1Gi |
| `provider-aws-ec2` | 30 m | 10/s | 500m, 1Gi | 2, 2Gi |
| `provider-aws-eks` | 10 m | 10/s | 500m, 1Gi | 2, 2Gi |
| kms, sqs, cloudwatchevents, cloudwatchlogs, s3 | 10 m | 10/s | 50m, 256Mi | 500m, 768Mi |
| `provider-helm` | 10 m | 50/s | 100m, 256Mi | 1, 1Gi |

One replica each, with leader election on, so `replicas: 2` gives a hot
standby. Why: a provider that restarts loses nothing (it observes every
resource again) and nothing waits on it within seconds, and the earlier
1 CPU / 2Gi twice for every provider requested 18 CPUs and 36Gi for providers
that mostly idle; EC2 and EKS hold the most resources and the largest schemas,
KMS, SQS, EventBridge, CloudWatch Logs and S3 a few per cluster. The AWS rate
stays at 10 (AWS throttling punishes the providers' 100); provider-helm reads
only workload clusters, and its poll only catches drift (a spec change
reaches it through its watch; readiness is the platform's). The EC2 poll may
not exceed 30 m: the network's observation bound (`reconcile/network`) is
derived from it.

On every start (`--install-providers`, default true) the controller
server-side applies each runtime config as the platform's field owner
(`cluster-vending`), forcing ownership of the fields earlier builds wrote, so a
new build's tuning or a changed section reaches existing installs. A field
another field manager owns (an operator's `kubectl edit` or own apply: the
replicas, the provider container's args, one resource quantity, the IRSA
annotation) is left out of the apply and kept; the controller logs it. A
missing Provider is created; an existing one only gets its `runtimeConfigRef`
pointed at its own config (unless another manager set it), never a new
package version. The shared `cluster-vending-aws` of earlier builds is
deleted once no Provider uses it. A provider-helm on another runtime config
(for example the `cluster-vending-helm-observation` sample) keeps it.

### Node layout

The platform runs two node groups in a cluster, both its own, from its config
(`nodeGroups`); the API defines no others. Tenants make their own Karpenter
pools in the cluster (see "Tenant pools" below), which the platform does not
manage:

| Pool | Label | Taint | Runs |
|---|---|---|---|
| The system node group (`system`), EKS managed | `node.cluster-vending.io/pool=system` | `node.cluster-vending.io/pool=system:NoSchedule` | Phase 1: the EKS add-on controllers (CoreDNS and the others below) and Karpenter (`${system.*}`), nothing else |
| The platform pool (`platform-pool`, Karpenter) | `node.cluster-vending.io/pool=platform-pool` | `node.cluster-vending.io/pool=platform-pool:NoSchedule` | Phase 2: every other platform component (AWS Load Balancer Controller, External Secrets, Kyverno, istiod, the ingress gateway) and the tenant's components (the GPU operator's controllers), through `${platform.*}` |

DaemonSets (VPC CNI, kube-proxy, the pod identity agent, istio-cni, ztunnel)
tolerate every taint and run everywhere.

The platform places the EKS add-on controllers on the system group through
their `configurationValues`, as `pkg/crossplane/aws/addons.yaml` says: CoreDNS,
metrics-server, kube-state-metrics, snapshot-controller, cert-manager (with its
cainjector and webhook) and the controllers of the EBS, EFS, FSx and Mountpoint
S3 CSI drivers. That file is the one list of the EKS add-ons the platform knows:
per add-on, whether every cluster gets it (`default`), the `configurationValues`
objects that take the `nodeSelector` and `tolerations` and the add-on's own
default tolerations (`placement`), the schema version and source those keys were
confirmed from (`schema`), and the IAM role with Pod Identity it needs (`setup`:
managed policies, or a named Go hook such as `s3-bucket-policy` for the
Mountpoint driver's buckets). Adding or changing an add-on is an edit there; the
package refuses a file with unknown fields, duplicate names, empty paths or
unknown hooks when it loads, and a test fails when the API's add-on names
(`AWSAddon.name`, `conflictsWithAddons`, `AwsAddon.name`) or
`examples/cluster.yaml` disagree with it.
A `nodeSelector` or `tolerations` the tenant sets in an add-on's
`configurationValues` wins. Without this, a cluster would never run those
add-ons: the only other nodes are the tainted platform pool's.

#### Network policy enforcement

Every cluster's VPC CNI enforces NetworkPolicy: `addons.yaml` gives `vpc-cni`
a `configuration` of `enableNetworkPolicy: "true"` and `nodeAgent.enabled: true`
(confirmed with `describe-addon-configuration` from v1.16.0-eksbuild.1 to
v1.23.3-eksbuild.2), on created and imported clusters alike (RenderVersion 12;
an import's plan shows it as a `configurationValues` change). Without it a
tenant's NetworkPolicies are accepted but not enforced. Keys under
`configuration` are written over the tenant's `configurationValues` for that
add-on (objects merge key by key): the platform's win, and every other key the
tenant set, nested ones included, stays. A configuration that is not a JSON or
YAML object goes to EKS as it is, which refuses it on the add-on.

**Install order.** A cluster's components install in two phases, on the pool
they run on (`component.InstallPhase`):

1. **System group.** EKS creates the add-ons with the cluster. Then Karpenter
   installs first (`sequence: 0`), on the system group (`${system.*}`). Karpenter
   is the only catalog definition there, and needs nothing from the platform pool.
2. **Platform pool.** When Karpenter is `Ready`, the controller applies the
   platform pool. A component placed with `${platform.*}` has no placement value
   until the pool's `NodePool` and `EC2NodeClass` are `Ready`. So it waits, and its
   status message names the pool. It never installs pods that would sit
   `Pending`, or a webhook that has no pod behind it. Then the components install
   by sequence and `requires`, one Helm operation at a time. Their pods make
   Karpenter launch the pool's nodes. A component without a pool of its own
   waits for the one it requires (istio-cni for istiod, ztunnel for istio-cni,
   kyverno-policies for kyverno). istio-base, which runs no pod, does not wait.
   Tenant components come after every platform component.

`controller --check` refuses a definition that uses both `${system.*}` and
`${platform.*}`, a Karpenter placed on the platform pool, any other definition
placed on the system group, and a system-group component that requires one on
the pool. An imported cluster keeps the node group it adopted for both
placeholders. If the config has no platform pool,
`${platform.*}` falls back to the system group.

On an existing cluster, the new `${platform.*}` value changes the rendered values
of every phase-2 component. Each one moves to the pool as a catalog change of its
own (PROP-01 4.7), once the pool is `Ready`. The definitions themselves are
the management cluster's `ComponentDefinition` objects, which the controller
syncs from its build at start (`--install-catalog`): a shipped definition that
was not edited takes the build's spec, an edited or unlabelled one is kept as
it is (`kubectl replace -f pkg/catalog/definitions/<name>.yaml` takes the
build's).

**System group.** The default managed node group of every cluster the platform
created, from the platform's `nodeGroups.system` settings. It has one static size, `minSize`
(default 2, the least allowed, since Karpenter runs two replicas): the node
group's min, max and desired size are all that number, and nothing autoscales
it. Karpenter is the only scaler, of the platform pool.
The old `maxSize` is ignored, with a warning at start. Changing `minSize`
applies to existing clusters in place, desired size included (Crossplane keeps
`forProvider.scalingConfig.desiredSize`, so a group that grew is brought back to
it). The platform puts no `k8s.io/cluster-autoscaler/*` tags on it, and refuses
cluster labels with that prefix, which would become the group's tags.
cluster-autoscaler is no longer in the catalog: on a cluster that has it, the
shipped definition is deleted at the next controller start (`catalog.Sync`), the
in-use finalizer keeps it until every cluster has removed it, and each cluster
uninstalls its Helm release and IAM role as a catalog change (PROP-01 4.7),
gated by the channel's code rollout and the freeze like any other.
`instanceTypes` (a list of one architecture and one vCPU and memory class),
`capacityType`, `amiType` and `diskSizeGiB` apply to every cluster the platform
created. EKS cannot change them on a node group, so a change replaces each
cluster's system group: the new group is created first and the old one retired
once the new one is `Ready`. The change reaches clusters as a code change does
(the render's fingerprints differ), in turn by channel. Nothing in the API sets
the group: the spec has no node group fields.

**Platform pool.** `nodeGroups.platform` configures one Karpenter `NodePool` and one
`EC2NodeClass`, both named `platform-pool` (not the legacy `platform`, which the legacy gate removes).
The controller applies them through the workload kubeconfig once Karpenter is
`Ready` and its IAM chain is. The pool is on-demand only (`SPOT` is refused), tainted and labelled
with its own name, `WhenEmpty` consolidation by default, with limits, a disruption budget, `expireAfter`
and `terminationGracePeriod`. It is `Ready` when its two objects are `Ready`, not when it has nodes: pods
create the nodes, so no wait for nodes can deadlock Karpenter. `status.nodeGroups` lists it after `system`
(`platformManaged`). A deletion removes it in the node groups stage, before Karpenter's stage.
`controller --check` sizes what runs on the system group (CPU, memory and pods per node: 17 on a `t3.medium`) against
`nodeGroups.system`, and the components placed on the pool against `nodeGroups.platform.limits` and the shapes the pool allows.

**Tenant node groups of existing clusters.** The API had `spec.nodeGroups`
(tenant Karpenter node groups) and tenant settings of the default group
(`spec.defaultNodePool`, `spec.aws.defaultNodePool`: instance types, capacity
type, disk, AMI type, launch templates, custom AMIs). They are gone: their
field numbers and names are reserved, a binary request that still carries them
is refused (`INVALID_ARGUMENT`), JSON drops them, and an `update_mask` path
under `spec.default_node_pool`, `spec.aws.default_node_pool` or
`spec.node_groups` is refused (`INVALID_ARGUMENT`). A stored cluster that still
has them is read without them and never renders them. The
`NodePool` and `EC2NodeClass` objects an earlier build applied for them
(`cloud.cluster-vending.io/managed-by=platform`, any name but `platform-pool`
and the old `platform`) are handed over, not deleted (`removeNodeGroups`,
`handOverPools`): the controller removes its label once, so it never lists,
changes or deletes them again, and emits one `NodePoolsHandedOver` event naming
them. Nothing is drained: their nodes and pods stay, and they are tenant pools
from then on. A system group a tenant setting had changed (instance types,
a launch template) is replaced by the platform's like any other change of
`nodeGroups.system`. A cluster built before the system group existed has an
old `platform` pool, the platform's own: it is deleted once its components
have moved off it (its pool first, its class once its nodes are gone).

**Tenant pools.** A tenant makes its own `NodePool` and `EC2NodeClass` in its
cluster with kubectl; `examples/tenant-nodepool.yaml` is a minimal one (the
platform does not apply it). Its class names Karpenter's node role in
`spec.role`: `status.aws.karpenterNodeRole` (and `karpenterNodeRoleArn`) in
`cluster get`, `<prefix>-karpenter-node`. Karpenter makes the instance profile
from it; its controller policy (the upstream v1.15 policy,
`pkg/catalog/definitions/karpenter.yaml`) creates, tags and deletes instance
profiles for any `EC2NodeClass` of the cluster, launches instances for any
`NodePool`, and passes only that role to EC2, so a class naming another role
or a ready-made instance profile is denied. Subnets are selected by
`karpenter.sh/discovery: <cluster name>` (or `kubernetes.io/cluster/<cluster
name>: "*"` when another cluster of the network holds the discovery tag; the
platform pool's class shows which), the security group by EKS's
`aws:eks:cluster-name: <cluster name>`. Nodes join through the role's
`EC2_LINUX` access entry. The platform never lists, changes or deletes a
tenant pool while the cluster lives (it acts only on objects with its label),
and an upgrade does not wait for one: the node groups step counts only the
platform's pools (`platform-pool`, and the old `platform` while it exists);
tenant pools whose nodes run another kubelet minor are listed as a warning in
`status.upgrade.tenantPoolsBehind` ("gpu (1.36)"), the tenant's to roll. A
cluster's deletion deletes every `NodePool`, `NodeClaim` and `EC2NodeClass`,
the tenant's included, in its node groups stage: nothing Karpenter launched
outlives the cluster.

**Imported clusters.** An imported cluster keeps the node group it adopted,
read-only: its settings as the import found them are kept in the cluster's
adoption (`Adoption.NodeGroup`), not the spec, and rendered as they are. A
cluster imported before that falls back to the spec its import derived.

**Sizing.** The default group is 2 x `t3.medium`. Only phase 1 counts against it:
the NodeProvisioner (Karpenter), the add-ons and the DaemonSets. What a
definition runs is data, its `spec.sizing`: each Deployment with the
`platformValues` paths of its replicas and per-pod requests, each DaemonSet
with the path of its requests (one pod per node). A definition without
`sizing` runs no workload. The values must set those keys explicitly; a chart
default is never guessed. The pod count is the tight limit. No node is added
when pods are pending there: the size is static. `pkg/catalog/system_group_sizing_test.go`
fails the build when the shipped requests exceed 85 % of the default group's
allocatable CPU or memory, or its pod capacity.

**Status.** `status.nodeGroups` lists the system group first, then the platform
pool (a status an earlier build recorded for a removed tenant group is kept
only until the next pass reaches the cluster): phase (`Pending`, `Ready`, `Failed`, `Deleting`), node counts, and a
message. Node lists are kept in the results bucket (the `nodes` kind, sharded by
group). Reads and applies to a workload cluster are bounded: 500 items a list, 10 s
a call, 45 s a pass, and they never fail the cluster's own pass.

### Releases, channels and rollouts

**Releases.** `pkg/catalog/release.yaml` is the one place a version is set:
the Kubernetes minors on offer, the chart pins of each component per minor, the
values each minor locks, and the three channels of a new management cluster. The
controller turns it into a `Release` object, named after its content and labelled
`cloud.cluster-vending.io/shipped`. A new content gives a new name. A release
offers three to six consecutive Kubernetes minors (shipped: 1.35, 1.36 and 1.37).

**Channels** (`ReleaseChannel`, cluster-scoped, named `alpha`, `beta` and
`stable`):

| Channel | Promotion | Offers | Soak (shipped) |
|---|---|---|---|
| `alpha` | `Pinned` | The release this build ships, or `spec.release` | none |
| `beta` | `Automatic` | The release `alpha` offers, once `alpha` has offered it for the soak | 168 h |
| `stable` | `Automatic` | The release `beta` offers, once `beta` has offered it for the soak | 336 h |

A channel that offers nothing yet takes its upstream's release at once. Clusters
keep the release they have; a new cluster on a channel gets the channel's minor.
Each minor names the channel whose clusters run it (one minor per channel). A
minor that is on no channel stays supported, and its users are warned
(`status.kubernetes.ending`, `KubernetesEnding`). A minor's `upgradeTo` is the
next minor, and `endOfSupport` is shown to users.

**Extended-support minors.** A minor on no channel is an extended-support minor
(EKS extended support). It runs with manual upgrades only:

- A create or update that makes a cluster on such a minor `Automatic` is
  refused. A create gets `INVALID_ARGUMENT` ("set upgradeMode
  UPGRADE_MODE_MANUAL, or choose 1.35 or later"). Switching an existing cluster
  gets `FAILED_PRECONDITION` ("upgrade to 1.35 first (or keep Manual)"). An unset
  mode is `Manual`.
- `spec.aws.upgradePolicy.supportType` left unset is rendered as `EXTENDED` on
  such a minor. This is a render default: the stored spec is not rewritten, and
  the cluster renders as before once it moves to a channel's minor. An explicit
  `STANDARD` is refused, because AWS upgrades the control plane by force when
  standard support ends. Clusters on a channel's minor are unchanged.
- If AWS upgraded a control plane by force anyway (a cluster left on
  `STANDARD`), EKS reports a minor newer than the spec. The controller raises
  the stored spec's `kubernetesVersion` to that minor (a new generation, its
  add-on versions resolved again for it), warns with `ControlPlaneUpgradedByAWS`,
  and the node groups and add-ons follow as in any upgrade. Rendering the older
  minor would be a downgrade EKS refuses. EKS behind the spec (an upgrade under
  way) or on its minor changes nothing.
- An import of an EKS cluster on such a minor is read-only and is not checked.
  Taking it over is checked, because the platform renders the cluster from
  then on. A discovered `STANDARD` (or `Automatic`) is refused with the same
  message, which adds that the setting must be changed in EKS before the
  cluster is imported again.
- The rule applies to API writes and to edits of the `Cluster` object. A
  `kubectl edit` that breaks it is not applied, and `EditRefused` says why. The
  rollout does not take that edit as a request to upgrade automatically. A
  reconcile pass never checks the rule. When a channel moves past a minor, the
  automatic clusters on it are upgraded by their rollout as before (writes that
  leave them as they are, such as an unrelated field, are not refused). Manual
  clusters on it keep running with the ending warning. A rollout's target is
  always its channel's minor. A cluster below that minor still goes up one minor
  at a time, through any minor on no channel in between.

To offer an extended-support minor, list it in `release.yaml` with no `channel`.
Set `endOfSupport` to the EKS extended-support end date, set `upgradeTo` to the
next minor, set a `nodeAmiPin` for the minor, and pin `components` for that minor
where a chart's current version no longer supports it. A release still lists at
most six consecutive minors.

**Cluster settings.** `spec.releaseChannel` (`Alpha`, `Beta` or `Stable`) selects
the channel. `spec.upgradeMode` is `Manual` (the default: the tenant chooses the
versions) or `Automatic` (the platform may move the cluster). `spec.maintenanceWindow`
(days, start in UTC, duration of 1 h to 24 h) limits when an automatic upgrade may
start. A cluster's tier comes from its network (`Network.spec.tier`: `Tier0`,
`Tier1` or `Tier2`, default `Tier0`). All clusters of a network share its tier.

**Rollouts** (`pkg/rollout`, one worker, a pass every 10 minutes and on each change
of the inputs):

- Each channel plans how its clusters move to the channel's minor. A cluster
  moves one minor at a time toward it and is never moved down.
- Tier2 first, then Tier1, then Tier0. Within a tier at most `policy.maxInProgress`
  clusters upgrade at once (default 5; Tier0 always 1). The next tier starts once
  the one before is done and `policy.soak` has passed (default 24 h; `soakPerTier`
  overrides it). More than `policy.maxFailed` failures (default 1) in a tier halt
  the channel (`RolloutHalted`).
- Gates per cluster. The cluster is kept back, with the first reason listed in
  `rollout.blocked`, when it is in `Manual` mode, paused, frozen, not `Ready`,
  has platform or tenant components not ready, has a deprecation scan that finds
  removed APIs for the target minor, or is outside its maintenance window. A
  kept-back cluster does not hold the rollout back.
- `spec.paused` on the channel reports the plan and starts nothing.

**Automatic upgrades** are off by default (`--automatic-upgrades=false`: the plan
is reported, nothing is changed). With `true`, the controller sets
`spec.controlPlane.kubernetesVersion` of each cluster the plan starts, to the
next minor. This is **irreversible**: EKS cannot downgrade a cluster. Each
cluster's record is `status.rollout`, with `result` `Pending`, `Blocked`,
`InProgress`, `Succeeded`, `Failed` or `TimedOut`. `--upgrade-timeout` (default
90 m) is a stall timeout: one step without progress for that long counts as
`TimedOut`. A long upgrade whose steps keep moving is not timed out.

**How a Kubernetes upgrade runs**, one step at a time, each step left only when
AWS and Helm report it settled (`status.upgrade.phase`, with `phaseStartTime`):

1. `ControlPlane`: EKS runs the new minor.
2. `NodeGroups` (and `NodesAndAddons` while both move): the node groups move to
   the new minor. The Karpenter classes' AMI pin moves to the release's
   `nodeAmiPin`, and drift replaces the nodes. The step is settled when every
   node of the platform's groups (the system group, `platform-pool`, the old
   `platform` while it exists) runs the target kubelet minor and none of their
   NodeClaims is still launching. Tenant pools are not waited for:
   `tenantPoolsBehind` lists the ones on another minor.
3. `Addons`: the add-ons take the versions EKS recommends for the new minor, read
   from AWS (`DescribeAddonVersions`), never from `release.yaml`. A pinned add-on
   keeps its pin.
4. `PlatformComponents`: the platform components switch to the new minor's pins.
5. `TenantComponents`: the same for tenant components.

Steps whose components do not change are reported as skipped
(`status.upgrade.skippedSteps`). Each add-on watch is 30 minutes: an add-on that
fails or does not settle is applied again, at most 3 times (`AddonRecovering`).
After that the upgrade halts (`UpgradeHalted`), and `cluster retry` continues it.
Once a day, and after the controller starts, each running cluster with no upgrade
under way asks AWS for the recommended add-on versions. An add-on the tenant has
not pinned moves to the recommended version (`AddonUpgradeScheduled`,
`AddonUpgraded`).

**Release moves.** Once a cluster's Kubernetes minor is current, its platform
components move to the release its channel offers (`status.releaseMove`). The
components move one at a time, in install order, each in the channel's rollout
order (tier, `maxInProgress`, soak, halt past `maxFailed`). A component that fails
or does not settle within 30 minutes is recovered (its chart is tried again), at
most 3 times (`ReleaseMoveRecovering`). Then the move halts until `cluster retry`.
There is no rollback. A move whose target release is gone installs nothing until
it is fixed. The release's tenant components move when the move is done.
Manual clusters do not move. Set `upgradeMode: Automatic` to move one.

**Code changes.** A new controller build can render a cluster's or a network's
objects differently (a field, a tag, a new object). The controller records a
fingerprint per object when it applies, and a pass whose render differs has a
code change pending (`status.codeChange`: `N objects change`). Code changes roll
out like Kubernetes upgrades: per channel, in tier order, with the same
`maxInProgress`, soak, halt and gates. A network goes once the rollout of every
channel of its clusters reaches its tier. A failed apply is tried 3 times, then
the change halts (`CodeChangeHalted`) until a newer build renders it otherwise or
an update applies it. A network's change counts as applied only once its cloud
resources converged on the new render. A resource the provider reports failed
counts as a failed apply (3 recoveries, then halted), and a change still not
converged 90 minutes after it started (the default of `--upgrade-timeout`;
`reconcile.CodeConvergenceTimeout`), say a resource Synced but never Ready,
halts with `did not converge within ...` in its message. The rollout counts a
halted network as failed, so its tier moves on. Objects of an owner are never removed by a code change;
a dropped object's cloud resource stays in place.

The code-change fingerprint covers only the cluster's Crossplane objects
(`ClusterSet`, Helm ProviderConfig included), not the platform components'
Helm releases. **Catalog changes** have their own fingerprint (PROP-01 4.7,
`pkg/reconcile/cluster/catalog_change.go`): each cluster records, in
`status.catalogChange.applied`, one fingerprint per platform definition (its
values with the placeholders resolved, its charts, install timeout, `requires`
and namespace; `component.CatalogFingerprint`) and one for the platform pool
(`nodeGroups.platform`, and the tags its EC2NodeClass gives the nodes: the
cluster's labels and the platform's tenant and cluster tags). Chart versions are the release's pins and are not part
of it: a release move or a Kubernetes upgrade changes them under its own
rollout. Sequence and IAM are not part of it either. A definition whose
fingerprint differs, one added, or one removed (disabled, deleted, or switched
off by the cascade) is a catalog change pending (`status.catalogChange`:
`N catalog entries change`). It is a member of the channel's code rollout with
the code change (tiers, `maxInProgress`, soak, windows, freezes, halt past
`maxFailed`; Manual clusters too). Until it is admitted, what runs stays: the
Release objects of a changed definition are not written, a new definition is
not installed, a removed one is not uninstalled, the platform pool is not
applied. Once admitted, the cluster applies the entries one at a time:
removals first, highest sequence first (dependents before what they require),
then installs and value changes, lowest sequence first, unless the platform
supplies an order (`reconcile.Deps.CatalogOrder`, for PROP-03's placement
moves). An entry's fingerprint is recorded only once its Helm operation settled
(digest written, deployed, Ready, observed); one that fails or does not settle
in 30 minutes is recovered 3 times, then the change halts until `cluster retry`.
A cluster fingerprinted for the first time takes what it renders as applied. A
change to a component that was never Ready, a fingerprint that cannot be
computed yet, and changes that land during a Kubernetes upgrade or a release
move are not gated separately.

**Admin views.** `admin release list` and `get`, and `admin channel list` and
`get` (read-only; platform admins only). `admin channel get` plans the rollout at
the time of the call, by tier and batch, and lists the clusters kept back and why
(`-o wide` lists every cluster).

### Freeze and pause

A freeze stops the controller's cloud work in its scope, and pauses the
Crossplane resources the platform owns so that Crossplane stops too.

- **Scopes.** The platform, a tenant (its accounts, networks and clusters), a
  network (the network and every cluster on it), or one cluster.
- **Commands.** `admin freeze` (platform, tenant, network or cluster, with
  `--reason`, `--until`, `--wait`, `--reset-exceptions`), `admin freeze status`,
  `admin unfreeze`. A tenant admin can freeze and unfreeze its own tenant, network
  or cluster with `tenant freeze`, `tenant freeze status` and `tenant unfreeze`.
- **State.** The freeze is held in the cluster-scoped `ReleaseHold` named
  `platform-freeze`, with `scoped` rules for each scope. The phases are
  `Freezing`, `Frozen`, `Unfreezing`, and unfrozen.
- **Confirmation.** A freeze reports `Freezing` first. The controller drains its
  active reconciles, pauses the resources, and records its acknowledgement in the
  hold's status. Only then is the freeze `Frozen`. `--wait` waits for that.
- **Effect.** Accounts, networks and clusters in scope do not change in the cloud:
  no creates, upgrades, repairs, import discovery or deletions. The API still
  accepts and stores writes, and their cloud changes wait until the unfreeze.
  Resources and Helm releases get `crossplane.io/paused: "true"`. Pauses set by a
  person before the freeze are left as they are.
- **Exceptions.** `admin unfreeze --cluster <id>` lets one cluster through a
  platform or network freeze. A cluster exception does not unpause its shared
  network. Repeating a freeze keeps the exceptions; `--reset-exceptions` clears them.
- **Expiry.** `--until` ends a freeze while the controller runs. The pauses stay
  in force while the controller is stopped.
- Scopes nest: a tenant unfreeze clears the freezes of its networks and clusters, a
  network unfreeze clears its clusters' freezes, and the latest matching action wins.

### Results in S3

Scan results of a cluster (deprecations, recommendations, violations, violation
events and the node list) are kept in an S3 bucket per cluster, not in the
custom resource, so that a large cluster is not limited by the object size. The
custom resource keeps a summary per kind: counts, collection time, completeness,
the key and ETag of the `latest` object, and a digest.

**Bucket** (`pkg/crossplane/aws/results_bucket.go`). Created with the cluster by
Crossplane in the cluster's own account, named `cv-<prefix>-<12 hex>`
(at most 63 characters, unique per account, region and cluster):

- All public access blocked; bucket owner enforced (no ACLs); SSE-S3 encryption;
  a bucket policy that refuses requests that are not over TLS; versioning off.
- Objects under `history/` expire after 30 days; incomplete multipart uploads
  after 1 day.
- The bucket is not force-destroyed. The controller empties it before deleting
  the bucket, every pass until it is gone. Nothing wipes data on its own.

**Objects**:

```
latest/deprecations.json        history/deprecations/<RFC3339>.json
latest/recommendations.json     history/recommendations/<RFC3339>.json
latest/violations.json          history/violations/<RFC3339>.json
latest/violation-events.json    history/violation-events/<RFC3339>.json
latest/nodes/<node group>.json  latest/nodes/index.json   (sharded kind)
```

Each object is the protojson of the kind's contract message, gzip compressed,
and bounded at 16 MiB uncompressed. A write puts the `history` object first,
then `latest`. A reader reads `latest`. A kind that grows with the cluster
(nodes) is sharded: one object per node group, and an index object with the
shard keys, counts and ETags.

**Write rules.** The controller's scanners write. A digest of the collected
content is compared with the last write, so unchanged results are not written
again. History is written at most once per change of the scan's interval (the
recommendation and violation history are hourly). Violation events get no history
object: they are an hour of Kubernetes events, and only `latest` is written
([PROP-02](proposals/PROP-02-cluster-data-live-and-snapshot.md), phase 0). A failed write does not fail the scan: the
caller keeps the summary and a notice that says why.

**Read rules.** The API server reads `latest` objects with `GetObject` through the
tenant role, with a 5-second timeout, the size bound, and a cache keyed by bucket,
key and ETag. Pagination tokens are bound to the object's ETag and the scope. A
missing bucket or object, a denied read, or a timeout is a coverage notice in the
response ("results unavailable: ..."), never a failed call. The server's session is
limited to reading objects in `cv-*` buckets (`results.ReadOnlySessionPolicy`).

**Sorting and the live API shape** (PROP-02 phase 1). The lists have `sort_by` and
`sort_order` (an enum per kind, `SortOrder` shared). Deprecations (`REMOVED_FIRST`,
`NAMESPACE`, `KIND`) and recommendations (`NAMESPACE`, `CPU_CHANGE`,
`MEMORY_CHANGE`) sort in memory after the filters. Their page tokens are bound to
the sort as well as to the object's ETag; a request with the default sort makes and
accepts the tokens it always did. The node, violation and event lists have the same
fields plus `live_at`, `complete` and `notices`. The live reader (`cluster-vending
live`) serves them and `GetNodeResources` (allocatable and requested CPU, memory and
pods per node group and for the cluster) when the API server is configured with it;
without one the lists answer from the stored results with the default order, and
another sort or `GetNodeResources` answers `UNIMPLEMENTED`. `LiveUnavailableDetail` is
the detail of the `UNAVAILABLE` of a cluster the live reader cannot read.

**Targets.** Designed for 100 clusters of up to 1,000 nodes and 10,000 pods each.
This is a design target: it has not been load tested
([Known limits](#7-known-limits)).

### Scanners

The three scanners (policy, deprecated APIs, recommendations) each read
`--scan-workers` clusters at once (default 4). A cluster whose `Reachable`
condition is False is skipped until a probe succeeds: a GET of its API
server's `/version` with a 5-second timeout, at most every 5 minutes per
cluster (shared by the scanners). Without it every scanner spent its full
timeout on each unreachable cluster, every pass.

Workload clients (component rollouts, node groups, Helm recovery) are kept,
one per cluster (`management.WorkloadCache`): each use reads the kubeconfig
Secret, and the client (with its REST mapper and the discovery it learned) is
reused while the kubeconfig is the same; a changed kubeconfig replaces it, an
unreadable Secret (the cluster deleted) drops it, and so do 30 idle minutes;
at most 256, the least recently used going first.

**Cluster status summary** (`pkg/clustersummary`, `pkg/reconcile/cluster/summary.go`). The node pass
(at most once a minute per cluster) sums the nodes it already reads per group and lists the pods once
(`limit` 500 and `continue`, at most 200 pages), adding each page as it arrives; no pod is kept. A pod's
request is the scheduler's: the larger of the containers' sum (plus restartable init containers) and the
init containers' peak, plus `spec.overhead`; pods bound to no node, Succeeded or Failed are excluded; a
missing request is zero. The totals go to the status (`SetSummary`); counts that did not change move their
collected time at most every 10 minutes. The policy pass writes the violation and event counts with the
snapshot. An unreachable pass changes only the `Reachable` condition. A node outside the platform's groups
(not in a Karpenter pool or the default managed group) is not counted.

**Policy violations** (`pkg/controller/policy_controller.go`). Every 5 minutes, for
each cluster that is Running, Updating, Failed or Imported with a kubeconfig, and
not frozen or paused. It reads WG PolicyReports, OpenReports, the Gatekeeper
constraints it discovers, and core events with policy reasons. Limits: a
45-second deadline, 10-second request timeouts, at most 48 list requests, pages of 100
objects, at most 32 Gatekeeper kinds. A collection keeps up to 200 findings and 200
events. A snapshot older than 10 minutes is flagged stale. Events older than one hour
are excluded. The result is in `status.policySnapshot` (bounded) and in the S3 kinds
`violations` and `violation-events`. Violations are failing, warning or
evaluation-error results. A failed background result does not prove an admission
was denied, and the event list is not a durable audit log.

**Deprecated APIs** (`pkg/reconcile/deprecations`, `pkg/deprecation`). Every 6
hours, and 15 minutes after a failed scan, `--scan-workers` clusters at a time. The controller
reads the cluster with the kubeconfig Secret that the ClusterAuth writes, and may
read Secrets in tenant namespaces only (through the `cluster-vending-kubeconfig-reader`
binding). It is read-only: discovery, metadata-only lists of the kinds the rules
name, and the newest deployed Helm release Secrets.

- An object is found when its manifest is kept in the cluster: the
  `kubectl.kubernetes.io/last-applied-configuration` annotation (source `Manifest`),
  or a Helm v3 release Secret (source `HelmRelease`, with `<namespace>/<release>`).
  Objects created by a client that keeps no manifest are not seen.
- The target is the cluster's next minor (the release's `upgradeTo`). Rules come
  from Pluto's data file (`pkg/deprecation/data/versions.yaml`, Pluto v5.24.5,
  Apache-2.0, vendored). Nothing of Pluto is linked in. The newest known removal
  is in 1.36. `make deprecation-rules` refreshes the file.
- The summary is on `status.deprecations` (target, times, `total`, `removed`, the
  error of a failed scan). The findings are in the S3 kind `deprecations`, removed
  first, at most 200 kept on the object. A scan that fails keeps the last one that
  worked.

**Recommendations** (`pkg/reconcile/recommendations`, `pkg/recommend`). Every 5
minutes the controller reads, with the cluster's kubeconfig and read-only:

- The workloads: Deployments, StatefulSets and DaemonSets, with the requests and
  limits of their pod templates. Jobs and bare pods are not recorded.
- Usage: what the metrics API (`metrics.k8s.io`) reports per pod. Without
  metrics-server nothing is recorded.
- The cluster's own VerticalPodAutoscalers, when present (`autoscaling.k8s.io/v1`).

Usage is kept in a ConfigMap `<cluster>-usage-history` in the tenant namespace,
owned by the Cluster object (so it is deleted with it): one bucket per UTC day,
7 days, at most 2,000 containers. The recommendation is in the S3 kind
`recommendations`, and the summary is on `status.recommendations`. A history
object is written at most once an hour.

| Source | When | Recommended request | Recommended memory limit |
|---|---|---|---|
| `vpa` | the VPA recommends for the container | VPA target | VPA upper bound |
| `usage-estimate` | at least a day of usage | peak over the week + 15 %, rounded up (CPU to 10m, memory to 1Mi) | recommended request + 20 % |
| `insufficient-data` | less than a day of usage | none | none |
| `no-data` | no usage and no VPA | none | none |

Floors: 10m CPU and 16Mi memory. No CPU limit is recommended. A change is
recommended only when the difference is a tenth or more, or the workload sets no
request. Recommendations are advice; nothing is applied.

**Events** (`pkg/reconcile/events.go`). Events are decided from the status: when a
pass records a new status, it is compared with the previous one, and the differences
are the events. A restart does not repeat them. The controller writes Kubernetes
`events.k8s.io` objects on the object (`kubectl describe` lists them). Repeats are
folded, and `status.recentEvents` keeps the last ten, with a count. The events
of a Network or Cluster are in the tenant's namespace. Release and ReleaseChannel
events are in `default`.

### EKS name reservations

Two clusters with the same EKS name in one account and region cannot both exist,
and an EKS name is not unique across tenants. The store reserves each
`(partition, canonical account ID, region, exact EKS name)` in a ConfigMap in
`kube-system`, named `cv-eks-name-<hash prefix>`, labelled
`cloud.cluster-vending.io/eks-name-reservation: "true"`. Its data keeps the full
scope key, the tenant, the allocation UUID, the request UUID and the normalised
request hash, so a truncated-hash collision fails closed. Errors returned to a
tenant disclose no other tenant or allocation.

- **Existing registrations** block a competing create, even when they predate the
  reservations. Imports reserve the same identity as creates.
- **Crash recovery.** Retrying the same create request (request UUID and payload)
  recovers the reserved allocation. A different request takes a name only when the
  reservation is abandoned: no Cluster has ever held it (no UID), no Cluster with its
  allocation exists in its tenant, and it was taken more than 10 minutes ago. A
  reservation that a Cluster ever held is never reclaimed this way.
- **Release** is two-phase. After the cluster's inventory is finalised, the
  reservation is marked `releasing` (a durable tombstone), then the Cluster object
  goes, then the reservation. A `releasing` reservation whose Cluster is gone is
  taken over at once. A reservation without the mark whose Cluster vanished (a
  finalizer removed by hand) proves nothing about AWS, and it stays in place until
  an operator checks ([runbook](#runbook-orphaned-eks-name-reservation)).
- Writers of reservations must use an uncached client. Reads that decide
  ownership never come from an informer cache.

Known gap, stated in the code's design notes: Kubernetes has no transaction across
a reservation, a Cluster, an account and a Network. Fences prevent unsafe
application while an object is incomplete, but partial intent and operator
recovery remain possible. An EKS cluster created directly in AWS by an actor that
does not use this protocol is not seen by it.

### Naming

- The EKS cluster takes the cluster's name (its physical name is reserved, above).
- Managed resources of a network or cluster are named `<name>-<hash8>-<suffix>`,
  where `hash8` is the first 8 hex characters of `sha256(tenant_id/id)`. The default
  node group is `<name>-<hash8>-default`.
- Imported resources keep their cloud names.
- IAM policies are checked against IAM's 6,144-character limit at render time, so a
  policy that would be too large is refused before it is attached (the rendering
  bound).
- Platform names of clusters and tenants are DNS labels, checked at the API.

### Authentication

The API server authenticates its callers with OIDC bearer tokens. It is
**authentication only**: there are no roles, rules or grants. Any user whose token
is accepted can call every RPC, `AdminService` included, for every tenant: read,
change and delete any tenant's networks and clusters, create tenants, and freeze the
platform. Without an `auth` section in the settings file the server runs mode `none`,
where every caller is anonymous and has the same full access. The deployments do
not allow that: the Helm chart refuses to render without `auth.mode: required`
(unless the local/dev flag `allowInsecureNoAuth`), and `config/server/auth` is the
kustomize overlay to deploy.

**Modes** (`auth.mode`):

| Mode | A request without a token | A request with a token |
|---|---|---|
| `none` (default of the binary) | Served. The caller is anonymous and audited as `anonymous`. For local development only: the server logs a warning at start, an `INSECURE` one when it listens on an address other than loopback (`:8080`, a pod IP). It still starts. | Served; the token is not checked. |
| `required` | `UNAUTHENTICATED` (HTTP 401, `WWW-Authenticate: Bearer`). | The token must verify: signature (RS256 or ES256, from the issuer's keys), issuer, one accepted audience and expiry. Otherwise `UNAUTHENTICATED`. |

`optional` is removed: a file that sets it stops the server at start, with a message
to set `required` (or `none` locally). `/healthz`, `/readyz` and `/metrics` are not
RPCs and are never authenticated.

`config/server/auth` is the overlay to deploy (`make deploy-manifests` renders it): it
requires tokens, with the issuer and audience to edit before use, and holds public
verification settings only. `config/server/base` alone runs mode `none` and is for
local development only: any pod that reaches its Service could freeze the platform or
delete any tenant's clusters.

**Helm.** With `server.enabled`, the chart fails unless `platform.auth.mode` is
`required`; with `ui.ingress.enabled`, it fails unless `ui.auth.enabled`. The value
`allowInsecureNoAuth: true` lifts both, for local/dev clusters only
(`ci/kind-values.yaml`); NOTES.txt then prints a warning. `networkPolicy.server` is on
by default: only the console's pods and `networkPolicy.server.from` reach the API
(`kubectl port-forward` is not affected).

**Issuers** (`auth.issuers`): `name`, `issuer` (the `iss` of the tokens; `https`, or
`http` for `localhost` only), `audiences` (at least one must be in the token's `aud`),
`entra.tenantId` (a Microsoft Entra ID preset: issuer
`https://login.microsoftonline.com/<tenantId>/v2.0`, optional `acceptV1`), and
`usernameClaim` (default `sub`; `oid` with Entra), the name shown in logs and `whoami`.

**Removed keys.** `auth.rules`, and `rolesClaim` and `tenantClaim` on an issuer, were
the roles. A file that still has them loads; each is ignored with one warning at start.

**Audit.** In both modes, every write (an RPC not marked `NO_SIDE_EFFECTS`) is logged
(`api write`), and so are the sensitive reads (`api sensitive read`):
`GetAccountOnboarding` (the accounts' external IDs) and the live reads
`ListNodeGroupNodes` and `GetNodeResources`. Each line has the procedure, the caller's
subject (`anonymous` in mode `none`), username, issuer, the request's tenant, the
outcome and the caller's address (`peer`). Every refused call is logged
(`api call refused`) with its procedure and address. A token is never logged. `WhoAmI` returns the caller's subject, username and
issuer.

**Microsoft Entra ID.** Two app registrations, one for the API and one for the CLI.
Every user Entra issues a token to for the API's audience can call it, so the API's
app registration should require assignment if only some users may sign in. The CLI's
`login` uses the device-code flow, or the browser flow with PKCE (`--browser`), and
keeps the token in a local file (`pkg/cli/login`). `logout` removes it.

**The UI** signs in with the same issuer and forwards the user's token to the API.

### Cluster OIDC

A cluster can accept the tokens of an OpenID Connect provider next to IAM, so
that users sign in with their company account and `kubectl` works without AWS
credentials. This is the workload cluster's Kubernetes API. The API's own sign-in
is [Authentication](#authentication).

- `spec.access.oidc` has either `entra` (the tenant ID and the application's client
  ID) or the generic form (`name`, `issuerUrl`, `clientId`, `usernameClaim`,
  `usernamePrefix`, `groupsClaim`, `groupsPrefix`, `requiredClaims`). With `entra`,
  the platform fills in the issuer, the user name (`oid`, as `entra:<oid>`) and the
  groups (`entra:<group object id>`).
- The platform renders one EKS identity provider association once the cluster is
  `ACTIVE`, named `<name>-<hash8 of its settings>`. A change is a new association;
  the old one is removed. Between the two, the provider's tokens are refused.
- **No RBAC is created.** A user the provider authenticates has no rights until a
  RoleBinding or ClusterRoleBinding names the user or one of its groups.
- `cluster kubeconfig <id> --oidc` prints a kubeconfig with no credential in it. It
  runs `kubectl oidc-login`, which keeps the tokens on the user's machine. The
  endpoint and CA come from the recorded status.
- EKS limits: associating is a cluster update (several minutes, more in practice);
  the issuer must be reachable from the internet with a certificate that is not
  self-signed; one provider per cluster before Kubernetes 1.32, up to ten after;
  prefixes cannot contain `system:`. An imported cluster keeps the provider it came
  with, and `spec.access.oidc` is refused next to it.

### Sub-tenant removal

Sub-tenants (PROP-05, withdrawn) and Capsule were removed: the `SubTenant` kind,
`SubTenantService`, the `subtenant` commands, the cluster setting `spec.naas`, the
role `NamespaceTenancy` and the config file's `naas` section. On start, once it
leads, the controller (`pkg/controller/subtenant_removal.go`) releases every
`SubTenant` an older build left: it removes the `cloud.cluster-vending.io/subtenant`
finalizer without the old workload-cluster cleanup, so tenants keep the namespaces
and RBAC they have in their clusters. It then deletes the CRD
`subtenants.cloud.cluster-vending.io` and logs what it released. A restart repeats
it with nothing to do. Capsule leaves each cluster as a catalog change
([Capsule removal](#platform-components)).

## 4. CR definitions

Every kind is in the group `cloud.cluster-vending.io`, version `v1alpha1`, and has
a status subresource. The CRD manifests are generated into `config/crd/bases` by
`make controller-apis`. The Go types are in `api/v1alpha1`.

| Kind | Short name | Scope | Written by | Represents |
|---|---|---|---|---|
| `Tenant` | `cvtenant` | Cluster | API (`CreateTenant`); the tenant controller for its namespace | A tenant and its namespace `tenant-<id>` |
| `CloudAccount` | `cvacct`, `cvaccount` | Namespaced | API (`BootstrapTenant`); the account controller | A tenant's AWS account, its `ProviderConfig` and access verdict |
| `Network` | `cvnet`, `cvnetwork` | Namespaced | API (Create, Import, Takeover, Update, Delete); the network controller | A VPC and its subnets, NAT and flow logs, or an imported VPC |
| `Cluster` | `cvc`, `cvcluster` | Namespaced | API (Create, Import, Takeover, Update, Delete); the cluster, rollout and scanner controllers | An EKS cluster, with its node groups, components and status |
| `Component` | `cvcomp` | Namespaced | API (`ComponentService`) | A tenant component enabled on a cluster |
| `ComponentDefinition` | `cvcompdef` | Cluster | The catalog sync (shipped); the platform team | A component's chart, IAM, values and rules |
| `Release` | `cvrel` | Cluster | The release controller, from `release.yaml` | A content-named set of Kubernetes minors and component pins |
| `ReleaseChannel` | `cvch` | Cluster | The release controller creates `alpha`, `beta`, `stable`; the rollout controller writes status | A channel: the release it offers, its policy and its rollout |
| `ReleaseHold` | `cvhold` | Cluster | `AdminService` and `TenantService` freeze RPCs; the freeze controller writes status | A freeze (`platform-freeze`) or a cluster hold |

Shared object conventions:

- **Labels**: `cloud.cluster-vending.io/tenant` (the tenant of a row, on its
  namespace and on each object), `cloud.cluster-vending.io/id` (the row's ID),
  `cloud.cluster-vending.io/network` and `/cluster` (on managed resources, for their
  owners), `cloud.cluster-vending.io/shipped` and `/shipped-hash` (catalog and
  release objects shipped by a build).
- **Finalizers**: `cloud.cluster-vending.io/cloudaccount`, `/network`, `/cluster`,
  `/tenant`, `/component`, `/definition-in-use` (catalog; held while
  a component uses it), `/release-in-use` (a release a channel or hold names).
- **Conditions**: `Ready` and `Synced` on the owned kinds, each with finer ones listed
  under the kinds below. Reasons are CamelCase, the messages are sentences.

### Tenant

`Tenant` is cluster-scoped and has no owner. Its namespace `tenant-<id>` carries the
label `cloud.cluster-vending.io/tenant=<id>`. Deleting the Tenant keeps the namespace.

| Field | Type | Meaning |
|---|---|---|
| `spec.displayName` | string, max 128 | A human-friendly name. Optional. |
| `status.namespace` | string | The tenant's namespace. |
| `status.conditions` | list | `Ready`. |
| `status.freeze` | object | The effective freeze: `frozen`, `scope`, `reason`, `until`, `revision`. |

### CloudAccount

Namespaced in the tenant namespace, finalizer `cloud.cluster-vending.io/cloudaccount`.
Its ProviderConfig is named by `status.providerConfigName`, generated on the first
bootstrap and kept.

| Field | Type | Meaning |
|---|---|---|
| `spec.accountName` | string, max 128 | The AWS account's name. Informational. |
| `spec.aws.accountID` | 12 digits | The account. Immutable. |
| `spec.aws.role` | string | The tenant role's name (no path). The tenant creates it from the template. |
| `status.phase` | enum | `Bootstrapping`, `Ready`, `Deleting`, `Failed`. |
| `status.identity` | string | The role ARN the provider credential assumes. |
| `status.networks` | int | The networks the account owns. The account cannot be deleted while it is not zero. |
| `status.access` | object | The access verdict: `verified`, `checkedAt`, `attemptedAt`, `stale`, `principalARN`, `reason`, `message`, `regions[]`, `checks[]` (each with `action`, `region`, `ok`, `reason`). |
| `status.inventory` | list | The objects of the account itself (its ProviderConfig). |
| `status.conditions` | list | `Ready`, `Synced`, `AccessVerified` (`False` with the first failure's reason, `Unknown` before the first verdict). |

### Network

Namespaced in the tenant namespace, owned by a CloudAccount (`spec.accountRef`),
finalizer `cloud.cluster-vending.io/network`. A network cannot be deleted while it
has clusters (`status.clusters`).

| Field | Type | Meaning |
|---|---|---|
| `spec.accountRef` | `{name}` | The owning CloudAccount, in the same namespace. Required and immutable. |
| `spec.region` | string | The region. Immutable. |
| `spec.tier` | `Tier0`, `Tier1`, `Tier2` | The rollout tier of the network and its clusters. Default `Tier0`. |
| `spec.displayName`, `spec.tags` | string, map | Mutable. A tag change re-tags the resources. |
| `spec.layout` | object | Immutable (create only). `cidrBlock` (/16 to /24, default `10.0.0.0/16`), `subnets[]` (`role` Public or Private, `cidrBlock`, `zoneID`), `availabilityZoneCount` (default 3), `natGatewayMode` (`Single` default, `PerZone`; `Regional` is refused). |
| `spec.flowLogs` | object | `enabled`, `trafficType` (`All` default, `Accept`, `Reject`), `retentionDays` (a CloudWatch period), `aggregationIntervalSeconds` (default 600). |
| `spec.importFrom.aws` | `{vpcID, subnetIDs}` | Imports an existing VPC instead of creating one. |
| `status.phase` | enum | `Provisioning`, `Imported`, `Ready`, `Updating`, `Deleting`, `Failed`. |
| `status.message` | string | Why it is `Failed`, typically. |
| `status.layout` | object | The layout as built, with the defaults resolved. |
| `status.resources` | object | `cloudNetworkID` (the VPC ID), `publicSubnetIDs`, `privateSubnetIDs`, `natGatewayIDs`, `egressIPs`. |
| `status.clusters` | list | Names of the clusters the network owns. |
| `status.importPlan` | object | The import plan, kept after takeover: `phase` (`Discovering`, `Planned`, `Blocked`, `Converging`, `Converged`) and `resources[]` (dispositions). |
| `status.codeChange` | object | A pending change of what the build renders for the network. |
| `status.inventory` | list | Every Crossplane object of the network, with its external name. |
| `status.recentEvents` | list | The last ten events, oldest first. |
| `status.conditions` | list | `Ready`, `Synced`, `Frozen`, `DeletionBlocked`, `Adopted`, `SpecEditIgnored`. |

### Cluster

Namespaced in the tenant namespace, owned by a Network (`spec.network.networkRef`),
finalizer `cloud.cluster-vending.io/cluster`. The spec fields that are immutable
are marked so in the type; the rest can be updated.

| Field | Type | Meaning |
|---|---|---|
| `spec.region` | string | The network's region. Immutable. |
| `spec.network` | object | `networkRef` (the owning Network, immutable), `zoneIDs` (empty: every zone of the network), `serviceCIDR` (default outside the network), `podCIDR` (must be empty on AWS). |
| `spec.controlPlane.kubernetesVersion` | `1.NN` | The minor. Empty on a cluster with a channel: the channel's minor is written into it. Empty on a cluster with no channel: the version it asks for, or EKS's default. The rollout sets it on automatic upgrades. |
| `spec.controlPlane.endpointAccess` | object | `publicAccessCIDRs`, `privateAccess` (default true), `publicAccess` (default true). |
| `spec.controlPlane.secretsEncryption` | object | `enabled` (a KMS key the platform creates and deletes) or `kmsKeyARN` (an existing key, not deleted). |
| `spec.access` | object | `adminPrincipals` (cluster-admin; besides them only the platform's role is admin, and `cluster create` adds the caller's IAM principal unless `--no-self-admin`), `accessEntries` (EKS entries and their policies, `Cluster` or `Namespace` scope), `oidc` ([Cluster OIDC](#cluster-oidc)). |
| `spec.releaseChannel` | `Alpha`, `Beta`, `Stable` | The channel the cluster takes platform releases from. |
| `spec.upgradeMode` | `Manual` (default), `Automatic` | Whether platform releases and Kubernetes upgrades may move the cluster. |
| `spec.maintenanceWindow` | `{days, start, duration}` | When an automatic upgrade may start: days (empty: every day), start in UTC (`HH:MM`), duration 1 h to 24 h. |
| (`naas`) | none | Removed with sub-tenants; field 15 of `ClusterSpec` is reserved. A binary request that still carries it, or an `UpdateCluster` mask naming `spec.naas`, is refused with `INVALID_ARGUMENT` ("spec.naas was removed with sub-tenants: the platform no longer installs Capsule"). JSON drops it. A stored value is ignored on read. A CLI manifest with `naas` is refused, saying it was removed. |
| (node groups) | none | The platform's, not the spec's: the system group and the platform pool ([Node layout](#node-layout)). The old `spec.nodeGroups` and `spec.defaultNodePool` are gone; a stored object that still has them is read without them. |
| `spec.aws` | object | `accessConfig.authenticationMode`, `enabledClusterLogTypes`, `upgradePolicy.supportType`, `addons[]` (name, version, configuration, S3 buckets), `controlPlaneLogGroup`, `irsa`, `additionalSecurityGroupIDs` (immutable). |
| `spec.importFrom.aws` | `{defaultNodeGroupName}` | Imports an existing EKS cluster. The named node group is adopted read-only, as it is; its other node groups are not imported. |
| `status.phase` | enum | `Provisioning`, `Imported`, `Running`, `Updating`, `Deleting`, `Failed`. |
| `status.conditions` | list | `Ready`, `Synced`, `NetworkReady`, `ControlPlaneReady`, `NodePoolReady`, `PlatformComponentsReady`, `Frozen`, `UpgradePaused`, `DeletionBlocked`, `Adopted`, `SpecEditIgnored`. |
| `status.endpoint` | `{url, certificateAuthorityData}` | The API endpoint, once the control plane is ready. |
| `status.kubeconfigSecret` | string | The Secret in this namespace with the key `kubeconfig`. |
| `status.currentKubernetesVersion` | string | The version the control plane runs. |
| `status.aws` | object | `clusterARN`, `clusterSecurityGroupID`, `platformVersion`, `nodeGroupARN`, `karpenterNodeRole` and `karpenterNodeRoleARN` (the role a tenant's `EC2NodeClass` names, [Node layout](#node-layout)), `addons[]` (name, version, ready, message). |
| `status.upgrade` | object | The Kubernetes upgrade in progress: `fromVersion`, `toVersion`, `phase` (`ControlPlane`, `NodesAndAddons`, `NodeGroups`, `Addons`, `PlatformComponents`, `TenantComponents`), `startTime`, `phaseStartTime`, `message`, `nodeGroups[]`, `addonsBehind`, `componentsBehind`, `recoveries`, `halted`, `skippedSteps`, `tenantPoolsBehind` (a warning: tenant pools on another kubelet minor, never waited for). |
| `status.rollout` | object | The cluster's last automatic upgrade: `channel`, `fromVersion`, `toVersion`, `startTime`, `finishTime`, `result` (`Pending`, `Blocked`, `InProgress`, `Succeeded`, `Failed`, `TimedOut`), `message`, `tier`. |
| `status.releaseMove` | object | The component move to the channel's release: `current`, `target`, `phase` (`Idle`, `Pending`, `Components`, `Halted`), `component`, `step`, `steps`, `message`. |
| `status.kubernetes` | object | `channel`, `channelMinor`, `available` (the next minor), `ending`, `message`. |
| `status.nodes`, `status.nodeGroups[]` (usage), `status.violations`, `status.violationEvents` | objects | The cluster status summary (PROP-02 5.2), a few KiB at most. `nodes`: total, ready, not ready, `nodesCollectedAt`. Each node group entry also has `ready`, `notReady`, allocatable CPU (millicores), memory (bytes) and pods, requested CPU and memory, `podCount` and `podsWithoutRequests`; at most 16 entries, the groups after the 15th summed into one `other`. `violations`: total and counts by severity (8 values, the rest in `other`). `violationEvents`: last-hour total and counts by reason. `Reachable` (a condition) is False with `CLUSTER_UNREACHABLE` or `CREDENTIALS_REJECTED` when the pass cannot read the cluster; the counts of the last good pass stay and their collected times show their age. |
| `status.platformComponents` | list | Per platform component: `name`, `version`, `phase` (`Pending`, `Installing`, `Ready`, `Failed`), `message`, `chart`, `helmState`, `lastError`, `failures`, `since`, `recoveries`, `chartVersion`, `revision`, `digest`, `attempts`, `countedFailures`, `nextRetryAt`, `lastErrorClass` (`transient`, `chart`, `release`), `lastRecovery`, `unchanged`. |
| `status.platformRelease` | string | The release the platform components' versions come from. |
| `status.componentInventory` | list | The Crossplane objects of the platform components (IAM roles, Helm releases). |
| `status.nodeGroups` | list | The system group, then the platform pool: `name`, `platformManaged`, `phase`, `message`, `nodes`, `nodesNotice`. Tenant pools are not listed. |
| `status.deprecations` | object | The last deprecation scan's summary (target, times, counts, error). |
| `status.recommendations` | object | The last sample's summary (times, workloads, containers, `metricsAvailable`, `vpaAvailable`, error, message). |
| `status.results` | list | One reference per result kind in S3: `kind`, `bucket`, `key`, `etag`, `digest`, `collectedAt`, `attemptedAt`, `total`, `removed`, `incomplete`, `truncated`. |
| `status.policySnapshot` | string | The bounded policy snapshot (protojson). |
| `status.codeChange` | object | A pending code change (`phase`: `Pending`, `Applying`, `Done`, `Halted`; `summary`; `objects`). |
| `status.catalogChange` | object | A pending catalog change (`phase` as above; `summary`; `pending[]`, in order; `component`, `step`, `steps`; `message`; times). The applied fingerprints are in `contractStatus`. |
| `status.inventory` | list | Every Crossplane object of the cluster, with its external name. |
| `status.network` | object | The VPC, subnets, NAT gateways and egress IPs of the cluster's network. |
| `status.importPlan` | object | As for a network. |
| `status.recentEvents` | list | The last ten events. |

Note: the cluster's tier is not in `spec`. It is shown in `status.tier`, from the network.

### ComponentDefinition

Cluster-scoped. Shipped by the build (`cloud.cluster-vending.io/shipped`) or written by
the platform team. Its finalizer `cloud.cluster-vending.io/definition-in-use` holds it
while a component uses it.

| Field | Type | Meaning |
|---|---|---|
| `spec.description` | string, 1 to 1024 | Shown to tenants. |
| `spec.management` | `Platform`, `Tenant` (default) | Who controls it: platform components are installed with every cluster. |
| `spec.provider` | `aws`, `gcp`, `azure` | The cloud it is for. Immutable. Default `aws`. |
| `spec.disabled` | bool | Switches it off without deleting it. |
| `spec.internal` | bool | Only installed as a member of a bundle (`includes`). |
| `spec.install.helm` | object | `crds` (a chart holding only the CRDs, as templates), `companions` (charts installed after the main one is ready), and the chart source (`repository`, `chart`, `credentials`, `insecureSkipTLSVerify`, `plainHTTP`). |
| `spec.install.timeout` | duration, 1 m to 60 m | How long each chart's workloads may take to become available after Helm deployed it (judged by the platform), and Helm's hook timeout. |
| `spec.namespace` | string | Namespace in the workload cluster. Required with `install`. |
| `spec.includes` | list | Makes the definition a bundle of other definitions. |
| `spec.requires` | list, up to 16 | Components that must be ready first. |
| `spec.role` | `NodeProvisioner` | The part of the platform it plays ([Roles](#platform-components)). Platform only, one per cloud. Unset: an ordinary component. |
| `spec.sequence` | int32, 0 to 10000, default 100 | Platform components install in ascending sequence. The `NodeProvisioner` is 0 by its role (CEL: it is not disabled), and 0 is no other definition's; every other shipped definition uses the default. |
| `spec.sizing` | object | What a Platform component runs, for the sizing rule: `deployments` (`name`, `replicas` and `requests`: `platformValues` paths) and `daemonSets` (`name`, `requests`). Unset: no workload. |
| `spec.versionGroup` | string | Components that are versions of one product, upgraded together (`istio`, `kyverno`). |
| `spec.upgradeOneMinorAtATime` | bool | Supports in-place upgrades of one minor at a time (Istio). |
| `spec.conflictsWithAddons` | list | EKS add-ons the cluster must not have while this is enabled. |
| `spec.cloudResources.iamRole` | object | The IAM role for the component's service accounts: `access` (`IRSA`, default, or `PodIdentity`), `serviceAccounts`, `policy` (a JSON document with placeholders), `managedPolicyArns`. |
| `spec.platformValues` | object | Values set by the platform team, locked: a tenant cannot change them. |
| `spec.userValues` | string | The starting values file that tenants pull and edit. |
| `spec.valuesSchema` | JSON Schema | What a tenant may override. `userValues` must validate against it. |
| `status.conditions` | list | `Valid`: the definition's checks hold (bundles, requirements, cloud and chart rules). |

### Component

Namespaced in the tenant namespace. Its finalizer is `cloud.cluster-vending.io/component`.
Its ID is the label `cloud.cluster-vending.io/id`.

| Field | Type | Meaning |
|---|---|---|
| `spec.clusterID` | UUID | The cluster it is installed into. The object is owned by that Cluster. Immutable. |
| `spec.component` | string | The `ComponentDefinition` name, for example `kyverno-policies`. |
| `spec.version` | string | Not set by the API: it is refused, and the installer takes the cluster's release version. |
| `spec.values` | JSON object, at most 64 KiB | The tenant's overrides only: the keys that differ from the defaults. |
| `status.phase` | enum | `Pending`, `Installing`, `Ready`, `Degraded`, `Failed`, `Deleting`. |
| `status.installedVersion`, `status.targetVersion`, `status.release` | string | What runs, what it moves to, and the release it came from. |
| `status.effectiveValues` | object | The merged values applied, with platform-computed keys and no secrets. |
| `status.message`, `status.since`, `status.chart`, `status.helmState`, `status.lastError`, `status.failures`, `status.recoveries` | | Why it is not ready (see Helm operations). |
| `status.inventory` | list | The objects the platform has for this component. |
| `status.conditions` | list | `Valid` (the request's checks hold now), `Synced` (the objects are applied), `Ready`. |

### Release

Cluster-scoped. Written by the release controller from `release.yaml`, and named after
its content. Its content is immutable: a new set of minors is a new release. Its
finalizer `cloud.cluster-vending.io/release-in-use` holds it while a channel or hold names it.

| Field | Type | Meaning |
|---|---|---|
| `spec.content.renderVersion` | int | The renderer version clusters are brought to. |
| `spec.content.kubernetes[]` | list, 3 to 6 consecutive | Per Kubernetes minor: `minor` (`1.NN`), `default`, `channel` (`alpha`, `beta` or `stable`), `upgradeTo`, `endOfSupport`, `nodeAmiPin`, `components[]` (`name`, `version`, `values`: the pins and locked values of that minor). |
| `spec.serial` | int64, at least 1 | The release order: a higher serial is a newer release, whatever the creation times, so a re-created release keeps its place. Immutable once set (CEL). Unset on a release that predates it: such a release is older than any with a serial, and is ordered by creation time among its kind. `admin release list` shows it; the list is newest first by serial, and a release move compares serials. `controller --export` numbers the releases that have none, in creation order (ties by name). |
| `spec.notes` | string, max 4096 | Notes for operators and tenants. |
| `spec.withdrawn` | bool | No channel may offer it again. Cannot be undone. |
| `status.conditions` | list | `Valid` (the release passes its checks), `InUse`. |
| `status.channels` | list | The channels offering it now. |
| `status.holds` | list | The holds naming it. |

### ReleaseChannel

Cluster-scoped, named `alpha`, `beta` or `stable`. Its status is written by the
rollout controller. Edits to the spec come from operators (the API has no RPC for them).

| Field | Type | Meaning |
|---|---|---|
| `spec.promotion` | `Automatic`, `Manual`, `Pinned` | `Automatic` takes each release the upstream channel completed; `Manual` and `Pinned` offer `spec.release`. |
| `spec.release` | string | The release a `Manual` or `Pinned` channel offers. |
| `spec.policy` | object | `maxInProgress` (default 5; Tier0 is 1), `maxFailed` (default 1), `soak` (default 24 h), `soakPerTier` (per tier), `releaseSoak` (how long the upstream must have offered a release). |
| `spec.paused` | bool | No cluster starts a new release. Clusters in progress finish. |
| `status.release` | string | The release the channel offers now. |
| `status.kubernetesMinor` | string | The minor that release puts on this channel. |
| `status.pending` | object | The release waiting for its soak to end, with `since` and `until`. |
| `status.rollout` | object | The Kubernetes rollout: `targetMinor`, `reportOnly`, `waiting`, `halted`, `done`, `wouldStart[]`, `members[]` (per tier and state), `blocked[]`. |
| `status.tiers` | list | Per tier: `total`, `done`, `inProgress`, `failed`, `held`, `completedAt`. |
| `status.releaseMoves`, `status.codeChanges` | object | The rollouts of the release (component moves) and of code changes: `release`, `admitted[]`, `waiting`, `halted`, `done`, `tier`, `members[]`, `blocked[]`. |
| `status.history` | list, at most 20 | The releases offered, newest first: `release`, `from`, `to`, `reason`. |
| `status.conditions` | list | `Offering`. |

### ReleaseHold

Cluster-scoped. The platform freeze is the object named `platform-freeze`. Scoped
freezes are rules in it (`spec.scoped: true`, `spec.rules[]`). A hold with `clusterID`
keeps one cluster on a release (`spec.release`, a lock) or ends by itself (`spec.until`).

| Field | Type | Meaning |
|---|---|---|
| `spec.platform` | bool | Identifies the platform freeze. Unless `scoped`, it freezes everything. |
| `spec.scoped` | bool | The freeze is made of `rules`. |
| `spec.rules[]` | list | Each rule: `tenantID`, `networkID`, `clusterID` (its scope), `frozen`, `by`, `reason`, `since`, `until`. |
| `spec.revision` | string | The policy revision. A confirmation is bound to it. |
| `spec.except[]` | list | Legacy resource exceptions. New requests use `rules`. |
| `spec.clusterID` | UUID | The cluster a hold applies to. |
| `spec.release` | string | Keeps the cluster on exactly this release. Unset: it stays on what it has. |
| `spec.reason` | string, required | Shown on the status of what it holds. |
| `spec.until` | time | Ends the hold by itself. |
| `spec.by` | string | Who asked for it. |
| `status.freezePhase` | string | The phase of the confirmation (`Freezing`, `Frozen`, `Unfreezing`, ...). |
| `status.freezeRevision`, `status.observedGeneration` | | The revision the status describes. |
| `status.drainedRevision`, `status.drainedGeneration` | | The controller drained its reconciles for this exact policy. |
| `status.resources`, `status.pausedResources`, `status.newlyPausedResources` | int | Matched and paused managed resources. |
| `status.clusters` | int | How many clusters the hold matches now. |
| `status.namespace`, `status.name` | string | The Cluster object its ID resolved to (for people). |
| `status.conditions` | list | `Resolved`. |

### Shared types

Used inside the kinds above:

| Type | Where | Meaning |
|---|---|---|
| `InventoryEntry` | `status.inventory` | `apiVersion`, `kind`, `name`, `namespace`, `externalName` (the cloud identifier), `carriedOver`, `recordedAt`, `goneAt`. The external name is the recorded cloud identity that a repair uses. |
| `ImportedResource` | `status.importPlan.resources` | `kind`, `name`, `externalName`, `specPath`, `disposition` (`Matches`, `WillUpdate`, `WillCreate`, `NotImported`, `Blocking`, `CarriedOver`), `detail`, `requiresReplacement`. |
| `RecentEvent` | `status.recentEvents` | `time`, `type` (`Normal` or `Warning`), `reason`, `message`, `count`. |
| `NetworkResources` | `status.network`, `status.resources` | `cloudNetworkID`, subnet IDs, NAT gateway IDs, egress IPs. |
| `ClusterResultRef` | `status.results[]` | An S3 result: `kind` (`deprecations`, `recommendations`, `violations`, `violation-events`, `nodes`), `bucket`, `key`, `etag`, `digest`, counts, flags. |
| `DeprecationFinding` | the S3 kind `deprecations` | `namespace`, `kind`, `name`, `apiVersion`, `component`, `deprecatedIn`, `removedIn`, `replacementAPI`, `replacementAvailableIn`, `source` (`Manifest` or `HelmRelease`), `helmRelease`, `removed`. |

### Field mapping to AWS

This is the mapping of the `cloud.cluster-vending.io` contract (`idl/cloud/v1`)
to the Crossplane resources the platform renders. Paths are relative to the
Crossplane resource. The node pool is always a separate resource, the AWS
`NodeGroup`. The GCP and Azure columns of the contract are the intended mapping
for providers that do not exist yet, and are left out here.

| Contract field | AWS resource and field |
|---|---|
| `spec.region` | `region` on every resource |
| `control_plane.kubernetes_version` | `Cluster.version`; `NodeGroup.version` follows it |
| `control_plane.secrets_encryption.enabled` | a KMS `Key` and `Alias`; `Cluster.encryptionConfig` |
| `control_plane.secrets_encryption.kms_key_arn` | an existing key, in `Cluster.encryptionConfig` only |
| `endpoint_access.endpoint_private_access` / `endpoint_public_access` | `Cluster.vpcConfig.endpointPrivateAccess` / `endpointPublicAccess` |
| `endpoint_access.public_access_cidrs` | `vpcConfig.publicAccessCidrs` |
| `NetworkSpec.layout.cidr_block` | `VPC.cidrBlock` |
| layout `subnets[].cidr_block`, `role`, `zone_id` | `Subnet.cidrBlock`; `PUBLIC` or `PRIVATE`; `availabilityZoneId` (required) |
| layout `nat_gateway_mode` | `NATGateway` and `EIP`: one, or one per zone (`Regional` is refused) |
| `NetworkSpec.flow_logs` | `FlowLog` on the VPC; cloudwatchlogs `Group`; iam `Role`, `Policy` and `RolePolicyAttachment` |
| `network.network_id` | the private `Subnet`s: `vpcConfig.subnetIds` and `NodeGroup.subnetIds` |
| `network.zone_ids` | the private subnets of those zones only |
| `network.service_cidr` | `kubernetesNetworkConfig.serviceIpv4Cidr` |
| `nodeGroups.system` (platform config) `instanceTypes`, `capacityType`, `amiType`, `diskSizeGiB` | `NodeGroup.instanceTypes`, `capacityType`, `amiType`, `diskSize` |
| `nodeGroups.system.minSize` | `NodeGroup.scalingConfig.minSize`, `maxSize`, `desiredSize` |
| (fixed) | `NodeGroup.labels`, `taint[]`: `node.cluster-vending.io/pool=system` |
| `labels` (as cloud tags) | `tags` |
| `spec.access.admin_principals` | `AccessEntry` and an `AccessPolicyAssociation` (`AmazonEKSClusterAdminPolicy`) |
| `spec.access.access_entries` | `AccessEntry`, and one `AccessPolicyAssociation` per policy |
| `spec.access.oidc` | `IdentityProviderConfig` (`oidc`), created once the cluster is `ACTIVE`. Name `<name>-<hash8>`. Entra: issuer `https://login.microsoftonline.com/<tenant_id>/v2.0`, user claim `oid`, groups claim `groups` |
| `spec.aws.control_plane_log_group` | cloudwatchlogs `Group` `/aws/eks/<name>/cluster`, created before the `Cluster` |
| `spec.aws.irsa` | iam `OpenIDConnectProvider` of the cluster's issuer |
| `spec.aws.additional_security_group_ids` | `Cluster.vpcConfig.securityGroupIds` |
| `status.network.egress_ips` | `EIP.atProvider.publicIp` |

Created for every AWS cluster without a spec field: the cluster and node IAM
roles with their policies, the default add-ons, the EBS CSI driver's role and pod
identity association, the `ClusterAuth` object behind `status.kubeconfigSecret`,
and `Usage` objects that order deletion.

**Names** the server derives so that they fit the provider limits:

| Object | Name |
|---|---|
| Namespace | `tenant-<tenant_id>` |
| Managed resources of a network or cluster | `<name>-<hash8>-<suffix>` (`hash8`: first 8 hex of `sha256(tenant_id/id)`) |
| EKS cluster | `Cluster.name` |
| Default node group | `<name>-<hash8>-default`, and `<name>-<hash8>-default-<hash6>` for a replacement |
| Imported resources | their cloud names, unchanged |

### The API

The API is `cloud.v1` in `idl/cloud/v1`, served over Connect (and gRPC and gRPC-Web),
with JSON or binary bodies. Its services:

| Service | RPCs |
|---|---|
| `TenantService` | `ListTenants`, `CreateTenant`, `DeleteTenant`, `Freeze`, `Unfreeze`, `GetFreeze`, `ListReleases`, `GetRelease`, `ListReleaseChannels`, `GetReleaseChannel`, `BootstrapTenant`, `GetCloudAccount`, `ListCloudAccounts`, `VerifyCloudAccount`, `DeleteCloudAccount`, `GetAccountOnboarding`, `WhoAmI` |
| `NetworkService` | `CreateNetwork`, `ImportNetwork`, `TakeOverNetwork`, `GetNetwork`, `ListNetworks`, `UpdateNetwork`, `DeleteNetwork`, `ListNetworkEvents` |
| `ClusterService` | `CreateCluster`, `ImportCluster`, `TakeOverCluster`, `GetCluster`, `ListClusters`, `UpdateCluster`, `DeleteCluster`, `RetryCluster`, `ListClusterEvents`, `ListClusterDeprecations`, `ListClusterRecommendations`, `ListClusterViolations`, `ListClusterViolationEvents`, `ListClusterNodeGroupManifests`, `ListNodeGroupNodes`, `GetNodeResources` |
| `ComponentService` | `ListComponentDefinitions`, `GetComponentValues`, `EnableComponent`, `UpdateComponent`, `DisableComponent`, `GetComponent`, `ListComponents` |
| `AdminService` | `Freeze`, `Unfreeze`, `GetFreeze`, `ListReleases`, `GetRelease`, `ListReleaseChannels`, `GetReleaseChannel` |

Conventions:

- Tenant-scoped requests carry `tenant_id`. The server checks it against the caller
  ([Authentication](#authentication)).
- Updates present an etag, and the update mask names the fields to change.
- `FAILED_PRECONDITION` for a network or cluster in an account that is not verified,
  and for deleting an account that still has networks.
- Each RPC is marked `idempotency_level` (`NO_SIDE_EFFECTS` for reads). An RPC
  that carries no `tenant_id` and is not on the open list is refused, and a test
  fails when an RPC is added without a classification.
- Changes to the contract are additive only: fields and enum values are added,
  never renumbered or removed. `make breaking` checks this against `main`.
- A `Cluster` update cannot change `spec.aws.access_config.authentication_mode`
  (`UNIMPLEMENTED`; it can be set at creation). The `tier` of a cluster is not a
  field: it is the network's.

Manifests (CLI `-f`) are the messages as JSON or YAML, with `apiVersion:
cluster-vending.io/v1` and a kind: `CloudAccount`, `Network`, `NetworkImport`,
`Cluster`, `ClusterImport` (see `examples/`). They are the API's messages, not the
custom resources.

## 5. Configuration

Every setting is a flag, an environment variable `CV_*`, and a key of the config
file (`--config`, or `CV_CONFIG`). The precedence is: flag, then environment
variable, then the file, then the default. The config file is YAML with the
settings keyed by flag name, plus two structured sections: `auth` (server only,
[Authentication](#authentication)) and `nodeGroups`
([Node groups configuration](#node-groups-configuration)). Unknown keys are errors, and
every problem is reported at startup, together. A file that still has the top-level
`naas` section is refused at start by both processes ("the naas section was removed
with sub-tenants ... delete it"); the chart's values schema refuses `platform.naas`.

### Global settings

Accepted by every command.

| Setting | Flag | Environment | Default | Meaning |
|---|---|---|---|---|
| Kubeconfig of the management cluster | `--kubeconfig` | `CV_KUBECONFIG` | empty: `$KUBECONFIG`, `~/.kube/config`, then in-cluster | |
| Kubeconfig context | `--context` | `CV_KUBE_CONTEXT` | the current context | |
| Log level | `--log-level` | `CV_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| Log format | `--log-format` | `CV_LOG_FORMAT` | `json` | `json` or `text` |
| Config file | `--config` | `CV_CONFIG` | none | YAML with any of these settings |

### Server settings (`serve`)

| Setting | Flag | Environment | Default | Meaning |
|---|---|---|---|---|
| Listen address | `--http-addr` | `CV_HTTP_ADDR` | `:8080` | The Connect API, with `/healthz`, `/readyz`, `/metrics` and `/cloudformation/` |
| Shutdown timeout | `--shutdown-timeout` | `CV_SHUTDOWN_TIMEOUT` | `30s` | How long in-flight requests get to finish |
| Management role | `--aws-management-role-arn` | `CV_AWS_MANAGEMENT_ROLE_ARN` | empty | The principal the account templates trust. Printed by `account create`. Empty: `account create` refuses. The server makes no AWS call with it. |
| Server role | `--aws-server-role-arn` | `CV_AWS_SERVER_ROLE_ARN` | empty | The API server's own role (its IRSA role). Trusted by the templates, to read results. Not needed with a source role. |
| Default AWS region | `--aws-region` | `CV_AWS_REGION` | the SDK's | Region of the SDK, for the role assumptions |
| Source role | `--aws-source-role-arn` | `CV_AWS_SOURCE_ROLE_ARN` | empty | Assumed first with the own credentials |
| External ID | `--aws-external-id` | `CV_AWS_EXTERNAL_ID` | empty | `sts:ExternalId` for a tenant role whose account has none of its own |
| Node group layout | see below | see below | see below | Creation rules for node groups |

### Controller settings (`controller`)

| Setting | Flag | Environment | Default | Meaning |
|---|---|---|---|---|
| Metrics address | `--metrics-addr` | `CV_METRICS_ADDR` | `0` (off) | Prometheus metrics, plain HTTP |
| Probe address | `--health-probe-addr` | `CV_HEALTH_PROBE_ADDR` | `:8081` | `/healthz` and `/readyz`; `0` turns them off |
| Leader election | `--leader-elect` | `CV_LEADER_ELECT` | `true` | One replica reconciles |
| Lease namespace | `--lease-namespace` | `CV_LEASE_NAMESPACE` | `crossplane-system` | Namespace of the Lease in the management cluster |
| Service account | `--service-account` | `CV_SERVICE_ACCOUNT` | empty | The controller's own service account. Each tenant namespace binds it to the kubeconfig reader role. Set by the Deployment; empty for a local run. |
| Management role | `--aws-management-role-arn` | `CV_AWS_MANAGEMENT_ROLE_ARN` | required | The IAM role the Crossplane providers run as (IRSA). The management account is taken from it. |
| Install providers | `--install-providers` | `CV_INSTALL_PROVIDERS` | `true` | On every start, bring the providers and their runtime configs to this build and the `providers` section ([provider runtime](#provider-runtime)); fields set by another field manager are kept, existing providers keep their package version |
| Install catalog | `--install-catalog` | `CV_INSTALL_CATALOG` | `true` | On start, create or update the shipped `ComponentDefinition`s ([catalog sync](#platform-components)) |
| Recover stuck releases | `--recover-stuck-releases` | `CV_RECOVER_STUCK_RELEASES` | `true` | Roll back or uninstall a stuck Helm release, at most 3 times per component version |
| Helm backoff cap | `--helm-backoff-max` | `CV_HELM_BACKOFF_MAX` | `30m` | Longest pause between attempts at a failed release (at least 1 m) |
| Helm failure limit | `--helm-max-failures` | `CV_HELM_MAX_FAILURES` | `5` | Counted failures in a row before a component is `Failed` |
| Recommendations | `--recommendations` | `CV_RECOMMENDATIONS` | `true` | Sample running clusters every 5 minutes. `false` records nothing. |
| Reconcile workers | `--reconcile-workers` | `CV_RECONCILE_WORKERS` | `8` | Accounts, networks and clusters reconciled at once, per kind. One object is never reconciled twice at once. |
| Scan workers | `--scan-workers` | `CV_SCAN_WORKERS` | `4` | Clusters each scanner (deprecated APIs, policy, recommendations) reads at once (at least 1). |
| Management client rate | `--kube-qps`, `--kube-burst` | `CV_KUBE_QPS`, `CV_KUBE_BURST` | `50`, `100` | Client-side rate limits of the controller's clients of the management cluster (burst at least the rate). Each workload cluster's client has its own: 20, burst 40. |
| Automatic upgrades | `--automatic-upgrades` | `CV_AUTOMATIC_UPGRADES` | `false` | `true` upgrades `Automatic` clusters, tier by tier. **Irreversible.** `false` plans and reports only. |
| Upgrade timeout | `--upgrade-timeout` | `CV_UPGRADE_TIMEOUT` | `90m` | Stall timeout of one upgrade step, then `TimedOut` |
| Teardown timeout | `--teardown-timeout` | `CV_TEARDOWN_TIMEOUT` | `15m` | How long a cluster's deletion waits for its load balancers and Karpenter nodes |
| Provider credentials Secret | `--provider-credentials-secret` | `CV_PROVIDER_CREDENTIALS_SECRET` | empty | `<namespace>/<name>[:<key>]` (key default `credentials`): the providers and accounts use this shared credentials file instead of IRSA. Local kind only. |
| Skip the tenant role | `--skip-tenant-role` | `CV_SKIP_TENANT_ROLE` | `false` | Local testing only, with the Secret: act with its keys, without assuming tenant roles |
| Provider runtime | the `providers` section | | the platform's | Replicas, poll, reconcile rate and resources per provider ([provider runtime](#provider-runtime)). `--install-providers=false` to manage the providers yourself. |

**Catalog modes of `controller`.** Two read-only modes run no controller
([PROP-01](proposals/PROP-01-admin-configuration.md), phase 0):

| Flag | Meaning |
|---|---|
| `--export` | Reads the management cluster (`--kubeconfig`) and writes its `ComponentDefinition`s, `Release`s and `ReleaseChannel`s as clean manifests (no status, uid, resourceVersion, managed fields, finalizers or shipped label and hash; the `cloud.cluster-vending.io/catalog-schema: "1"` annotation added; releases without a serial numbered in creation order). One multi-document stream on stdout, or with `--export-dir <dir>` the layout `definitions/`, `releases/`, `channels/` with one file per object and a `kustomization.yaml`. On stderr it lists how the live definitions differ from this build's embedded catalog. |
| `--check -f <path\|->` | Reads rendered manifests (`kustomize build ... \| cluster-vending controller --config platform.yaml --check -f -`), makes no cluster call, prints every problem and exits 1 if there is any: the three channels once each, requirements that exist and do not cycle, a requirement's sequence not above its dependent's, one definition per role per cloud, the `NodeProvisioner` on and alone at sequence 0 and on the system group, unique serials, consecutive minors and one minor per channel, a pin for every enabled chart (and companion) on every minor, channels naming releases of the set, each definition's `Valid` checks, the schema annotation, and the sizing of the system group, of the bootstrap set (CPU, memory and pods per node) and of the platform pool against `nodeGroups.system` and `nodeGroups.platform` of `--config` (the defaults without it). Disabling a component switches its dependents off with it: that is not a problem. |


The controller's `--aws-region`, `--aws-source-role-arn` and `--aws-external-id` are
the same as the server's.

### CLI settings

| Setting | Flag | Environment | Default | Meaning |
|---|---|---|---|---|
| API server | `--server` | `CV_SERVER` | `http://127.0.0.1:8080` | Base URL of the API |
| Request timeout | `--request-timeout` | `CV_REQUEST_TIMEOUT` | `2m` | Per call. `--wait` is not bounded by it. |
| Tenant | `--tenant` | `CV_TENANT` | none | The tenant ID. A manifest's `metadata.tenant` wins over the variable. |

### Node groups configuration

The platform admin sets the layout in the `nodeGroups` section of the config file,
with the same shape and struct for the controller and the server. The controller
renders, and the server validates, so the two must get the same file or values.
Lists are real YAML lists in the file, and comma-separated in flags and variables.

There is one file for both processes. `config/node-groups` is a kustomize component:
its `configMapGenerator` makes the ConfigMap `platform-config` from `platform.yaml`, with the
content hash in its name, so a change of the file rolls both Deployments. The server and the
controller mount it at `/etc/cluster-vending` and pass `--config=/etc/cluster-vending/platform.yaml`.
They load it with the same code, so an invalid file stops both with the same problems.
The manifests set no `CV_SYSTEM_*`, `CV_PLATFORM_*` or `CV_NODE_GROUP_*` node group variable. A variable still
wins over the file (flag, then variable, then file), so one that is set anywhere logs a
warning at start of either command. Locally, `make run` and `make run-controller` both pass
`hack/config.local.yaml`. `config/server/auth` carries the same `nodeGroups` plus the `auth`
section for the server's one `--config` (a config test keeps the two equal).

```yaml
nodeGroups:
  system:                      # the default managed node group of every cluster: Karpenter and the EKS add-ons
    instanceTypes: [t3.medium] # one architecture, one vCPU/memory class
    minSize: 2                 # at least 2 (Karpenter's two replicas); the fixed size, changed in place
    capacityType: ON_DEMAND
    amiType: AL2023_x86_64_STANDARD  # of the instance types' architecture
    diskSizeGiB: 0             # 0 is EKS's default
  platform:                    # the Karpenter pool `platform-pool`: everything else
    instanceFamilies: [m7i, m6i]
    instanceSizes: [large, xlarge, 2xlarge]
    architectures: [AMD64]     # exactly one
    capacityTypes: [ON_DEMAND] # SPOT is refused
    limits: {cpu: 128, memory: 512Gi}
    consolidationPolicy: WhenEmpty   # or WhenEmptyOrUnderutilized
    consolidateAfter: 5m
    expireAfter: 336h
    terminationGracePeriod: 1h
    disruptionBudget: 1        # nodes disrupted at once
    maxPods: 0                 # 0: Karpenter's default; at most 110
```

A change of `system.instanceTypes`, `capacityType`, `amiType` or `diskSizeGiB`
replaces the system group of every cluster the platform created (new group
first, the old one retired once the new one is `Ready`), rolled out like a code
change. Tenants define no node groups.

| Key | Flag | Environment | Default |
|---|---|---|---|
| `system.instanceTypes` | `--system-instance-types` | `CV_SYSTEM_INSTANCE_TYPES` | `t3.medium` |
| `system.instanceType` | `--system-node-group-instance-type` | `CV_SYSTEM_NODE_GROUP_INSTANCE_TYPE` | none: deprecated, a list of one, warned about (not together with `instanceTypes`) |
| `system.minSize` | `--system-node-group-min-size` | `CV_SYSTEM_NODE_GROUP_MIN_SIZE` | `2` (minimum 2): the static size (min = max = desired) |
| `system.maxSize` | `--system-node-group-max-size` | `CV_SYSTEM_NODE_GROUP_MAX_SIZE` | ignored, warned about at start (accepted so old configs still start) |
| `system.size` | `--system-node-group-size` | `CV_SYSTEM_NODE_GROUP_SIZE` | none: the old fixed-size setting, an alias of `minSize` (two different values are an error) |
| `system.capacityType` | `--system-node-group-capacity-type` | `CV_SYSTEM_NODE_GROUP_CAPACITY_TYPE` | `ON_DEMAND` |
| `system.amiType` | `--system-node-group-ami-type` | `CV_SYSTEM_NODE_GROUP_AMI_TYPE` | `AL2023_x86_64_STANDARD`; or `AL2023_ARM_64_STANDARD`, `BOTTLEROCKET_x86_64`, `BOTTLEROCKET_ARM_64`, matching the instance types' architecture |
| `system.diskSizeGiB` | `--system-node-group-disk-size-gib` | `CV_SYSTEM_NODE_GROUP_DISK_SIZE_GIB` | `0` (EKS default) |
| `platform.instanceFamilies`, `.instanceSizes`, `.architectures`, `.capacityTypes` | `--platform-instance-families`, `-sizes`, `-architectures`, `-capacity-types` | `CV_PLATFORM_INSTANCE_FAMILIES`, ... | `m7i,m6i`; `large,xlarge,2xlarge`; `AMD64`; `ON_DEMAND` |
| `platform.limits.cpu`, `.memory` | `--platform-limit-cpu`, `--platform-limit-memory` | `CV_PLATFORM_LIMIT_CPU`, `CV_PLATFORM_LIMIT_MEMORY` | `128`, `512Gi` |
| `platform.consolidationPolicy`, `.consolidateAfter`, `.expireAfter`, `.terminationGracePeriod` | `--platform-consolidation-policy`, `-consolidate-after`, `-expire-after`, `-termination-grace-period` | `CV_PLATFORM_*` | `WhenEmpty`, `5m`, `336h`, `1h` |
| `platform.disruptionBudget`, `.maxPods` | `--platform-disruption-budget`, `--platform-max-pods` | `CV_PLATFORM_DISRUPTION_BUDGET`, `CV_PLATFORM_MAX_PODS` | `1`, `0` |
| `karpenter.*` (`instanceFamilies`, `instanceTypes`, `defaults.*`) | `--node-group-instance-families`, `-instance-types`, `--node-group-default-architectures`, `-capacity-types` | `CV_NODE_GROUP_*` | ignored, warned about at start (the old tenant node group allowlist; accepted so old configs still start) |

Startup fails with every problem listed: a `minSize` below 2, an empty `system.instanceTypes` or one that mixes architectures, a platform
family or size not shaped like `m7i` or `xlarge`, a platform pool with more than one
architecture, a family of another architecture than the pool's, `SPOT` in the platform pool,
non-positive limits, a disruption budget below 1, `maxPods` above 110, an instance type not like `t3.large`, a family not like `t3`, a capacity
type other than `ON_DEMAND` or `SPOT`, an architecture other than `AMD64` or `ARM64`, an `amiType` not in the list or not of the instance types' architecture.
The two allowlist flags are the names that shipped before the section, and they
still work.

### AWS credentials

The platform has four ways to get AWS credentials, and never a flag value for a key.

| Mode | Where | How |
|---|---|---|
| IRSA | Production | The controller's service account and the providers run with the management role through the cluster's OIDC provider, the API server's with the server role. Both roles come from the management account's templates (`account create --management`, [AWS access and accounts](#aws-access-and-accounts)), which trust exactly those service accounts. The Helm chart (`controller.serviceAccount.annotations`, `server.serviceAccount.annotations`) or `config/default/kustomization.yaml` sets the annotation. |
| Source role | Either process | `aws-source-role-arn`: the own credentials assume it first, then the tenant role. |
| Environment keys | Local runs | `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_SESSION_TOKEN` in the shell. The SDK's default chain reads them for the controller and the server. To run as the management role (mode B), create it from its template with `LocalPrincipalArn` and export a session of it. |
| Secret for the providers | Local kind | `--provider-credentials-secret`. The providers and the accounts use the Secret's shared credentials file. The controller copies the key into each tenant namespace as `<providerConfig>-credentials`. |

In every mode the tenant role is assumed for the tenant's account. The
`skip-tenant-role` setting exists for an account whose policies forbid assuming roles,
for local testing only.

## 6. Operating it

### Run locally

The step-by-step guide, including another management cluster or AWS account
and troubleshooting, is [local-development.md](local-development.md).

The controller does the AWS work and is the only process that makes changes in AWS.
The API server validates and stores. Three terminals, against a kind cluster:

```sh
make kind-up                     # kind with Crossplane (hack/start.sh)
export AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=...   # in the shell only

# Once: the same keys as the providers' Secret (kind has no IRSA)
kubectl --context kind-cluster-vending -n crossplane-system create secret generic \
  aws-local-credentials --from-file=credentials=/dev/stdin <<EOF
[default]
aws_access_key_id = $AWS_ACCESS_KEY_ID
aws_secret_access_key = $AWS_SECRET_ACCESS_KEY
EOF
# then, in hack/config.local.yaml: uncomment provider-credentials-secret and set aws-management-role-arn

make run-controller              # terminal 1: the controller (hack/config.local.yaml, the same file)
make run                         # terminal 2: the API server (hack/config.local.yaml), after the CRDs and providers exist
make run-ui                      # terminal 3: the console at localhost:3000
```

`hack/config.local.yaml` points the server at the kind cluster. It has no AWS
setting, and its state is custom resources (there is no database). Restart the
server with the same keys exported to read results buckets. Without keys, a
cluster's results are answered with a "results unavailable" notice.

Login is off locally. To turn it on, run `make keycloak` (it starts and configures
a local Keycloak; it does not start the API or the UI), uncomment the complete
`auth` block in `hack/config.local.yaml`, and restart the API and the UI.
Commenting the block out again turns it off.

Make targets:

| Target | What it does |
|---|---|
| `make build` | Builds `bin/cluster-vending`. |
| `make check` | Lint (protos and Go), `go vet` and the unit tests: the gate before a commit. |
| `make test` | Unit tests with the race detector. They use a fake API server; no cluster needed. |
| `make cover` | Unit tests with a coverage report in `bin/coverage.html`. |
| `make e2e`, `make e2e-lint` | The end-to-end tests on a sandbox AWS account (below; real resources, hours), and their vet and lint (nothing is run). |
| `make generate` | Regenerates Go in `pkg/gen` and TypeScript in `ui/src/gen` from `idl/` (buf, remote plugins). |
| `make controller-apis` | Regenerates the deepcopy code, the CRDs and the RBAC of `api/`. |
| `make mocks` | Regenerates the repository mocks. |
| `make run`, `make run-controller`, `make run-ui`, `make keycloak` | The local processes above. |
| `make kind-up`, `make kind-down`, `make managed` | Creates, removes, and lists the managed resources of the kind cluster. |
| `make deprecation-rules PLUTO_VERSION=...` | Refreshes the deprecated-API rules from Pluto. |
| `make cf-requirements`, `make cf-generate`, `make cf-lint`, `make cf-format-check` | The account role templates: install the Python tools, generate, lint, format-check. |
| `make docker-build`, `make deploy-manifests` | The container image (nothing is pushed), and the rendered manifests of the controller and the server (nothing is applied). |
| `make docker-build-ui`, `make helm-lint`, `make helm-template`, `make helm-sync` | The console image; lint and render the Helm chart; copy `config/` RBAC into it ([Install with Helm](#install-with-helm)). |
| `make kind-load`, `make helm-install-kind` | Load both images into the kind cluster `HELM_KIND_CLUSTER` (default `cv-helm`) and install the chart there. |

No test reads a file under `docs/`. Two tests name a section of this document in a comment.

### Install with Helm

The chart [`charts/cluster-vending`](../charts/cluster-vending/README.md) deploys the
same objects as `config/` (the kustomize bases stay the reference), with each
workload on or off from one values file: the API server, the controller, the live
reader and the console. Crossplane must be installed first (as `hack/start.sh` does).

```sh
# my-auth-values.yaml: platform.auth (mode: required, your issuers), as
# charts/cluster-vending/ci/auth-values.yaml. The chart refuses an API without it.
helm install cluster-vending charts/cluster-vending \
  -n cluster-vending-system --create-namespace \
  -f my-auth-values.yaml \
  --set aws.managementRoleArn=arn:aws:iam::111122223333:role/crossplane-provider-aws \
  --set controller.serviceAccount.annotations."eks\.amazonaws\.com/role-arn"=arn:aws:iam::111122223333:role/crossplane-provider-aws
```

- **Login.** The render fails without `platform.auth.mode: required` (with
  `server.enabled`), and when `ui.ingress.enabled` is set without `ui.auth.enabled`.
  `allowInsecureNoAuth: true` lifts both, on local/dev clusters only
  (`ci/kind-values.yaml`), and NOTES.txt warns. See [Authentication](#authentication).
- **Network policies.** `networkPolicy.server` is on by default: the API accepts the
  console's pods and `networkPolicy.server.from` only (add an ingress controller,
  in-cluster CLI users, Prometheus scraping `/metrics`). A `kubectl port-forward`
  enters the pod's network namespace and is not blocked.

- **Settings.** The `platform` value is the shared settings file: one ConfigMap,
  mounted at `/etc/cluster-vending/platform.yaml` into the server and the controller,
  with a checksum annotation that restarts both on a change. Its default is
  `config/node-groups/platform.yaml`. `aws.*` values are added to it as flat keys.
  Secrets are referenced by name only.
- **CRDs.** The chart ships none: the controller installs and updates them on start
  from its binary. The API server's startup probe waits for them.
- **Identities.** The controller's service account carries the management role
  (IRSA). The server's may carry its own server role (`aws.serverRoleArn`), never the
  management role. The live reader's (`cluster-vending-live`, a fixed name the
  controller binds in tenant namespaces) has none, and the chart refuses one. The API
  server gets no workload-cluster credentials. The RBAC rules are copies of
  `config/` (`make helm-sync`), and `config/helm_chart_test.go` fails when they drift.
- **Live reader.** One replica, strategy `Recreate`, reachable from the API server's
  pods only (`networkPolicy.live`, on by default); its metrics-only port (8091) from
  `networkPolicy.metricsFrom`.
- **Console.** The browser calls only the console's origin (`/api/rpc/...`). The
  console's server side forwards each call to the API's Service (`CV_API_BASE_URL`).
  One port-forward is enough:
  `kubectl -n cluster-vending-system port-forward svc/cluster-vending-ui 3000:3000`.
  For the CLI: `kubectl -n cluster-vending-system port-forward svc/cluster-vending-server 8080:8080`,
  then `cluster-vending --server http://localhost:8080 ...`. With login off, the
  RPC route accepts an `Origin` only for a loopback host the browser addressed
  (`localhost`, `*.localhost`, `127.0.0.1`, `[::1]`: a port-forward) or exactly
  `CV_PUBLIC_ORIGIN`, which the chart sets from the first Ingress host. Any other
  host is refused (DNS rebinding), and forwarded headers are never consulted. With web
  login on, it accepts the `AUTH_URL` origin only, and the console needs HTTPS URLs:
  use `ui.ingress` with TLS.
- **One release per management cluster**: the names the controller binds in tenant
  namespaces are fixed.

Images: `make docker-build` (the binary) and `make docker-build-ui` (the console,
`ui/Dockerfile`, Next.js standalone, non-root). The chart's appVersion is the default
tag of both.

### CLI

The command tree is `cluster-vending [--server ...] <group> <verb>`. The groups
`tenant`, `account`, `network`, `cluster`, `component`, `admin` and the
commands `login`, `logout`, `whoami`, `serve`, `controller`, `live` (the live reader,
started for the API server; not for people) are the complete set.
`--help` on each lists the flags. `-o table|wide|yaml|json` applies to every read.

| Group | Verbs | Notes |
|---|---|---|
| `tenant` | `create <id>`, `list`, `delete <id> [--wait]`, `freeze [--reason --until --wait]`, `freeze status`, `unfreeze [--wait]` | `list` needs no `--tenant`. `delete` is refused, naming them, while the tenant has accounts, networks, clusters or components; its namespace is unregistered and kept. Freezes are the tenant's own scope. |
| `account` | `create -f`, `get --id`, `list`, `verify --id [--region ... --wait]`, `delete --id [--wait]` | `create` prints the role template's steps (or the trust, for a custom role). |
| `network` | `create -f [--wait]`, `get <id> [--live]`, `list`, `update <id> -f [--plan]`, `delete <id> [--wait]`, `import -f`, `takeover <id> [--wait]`, `events <id> [--only-warnings]` | `--plan` shows the changes without applying them. `--live` reads the status from the management cluster. |
| `cluster` | `create -f [--no-self-admin] [--wait]`, `get <id> [--live]`, `list`, `update <id> -f [--plan]`, `delete <id> [--force] [--wait]`, `import -f`, `takeover <id> [--skip-platform-components] [--wait]`, `kubeconfig <id> [--print \| --oidc]`, `deprecations <id> [--only-removed --sort --order]`, `recommendations <id> [--namespace --only-changes --sort --order]`, `nodes <id> [--group --ready \| --not-ready --name --sort --order]`, `nodes <id> --manifests [--group]`, `versions [--channel]`, `violations <id> [--namespace --engine --sort --order]`, `violations <id> --events`, `events <id> [--only-warnings]`, `retry <id> [--component ... --upgrade]` | `--force` tears down without the API. `retry` answers per item. `get` shows the control plane's CloudWatch log group and its console link when control plane logging is on. `nodes` reads every group the cluster reports unless `--group`, and ends with each group's CPU and memory allocatable vs requested, read live (GetNodeResources); `--manifests` prints the platform pool's NodePool and EC2NodeClass YAML. `violations --events` is the policy and admission events of the last hour. `versions` lists the Kubernetes minors the channels' releases offer (default, upgrade target, end of support; a minor on no channel is extended support, Manual clusters only). `--sort` takes the list's keys in lower case (`cpu-requested-percent`, `cpu-change`); `--order` is `asc` or `desc`. |
| `component` | `catalog [--cluster]`, `template <name>`, `enable <name> --cluster [-f] [--validate-only] [--wait]`, `update <id> -f [--validate-only] [--etag]`, `disable <id> [--etag]`, `get <id> [--values]`, `list [--cluster] [--details]` | `template` prints the values file to start from. The version is not a flag. `catalog` and `get` show the IAM role the platform creates in the tenant account for a component (access, service accounts, policy). |
| `admin` | `freeze [--platform \| --tenant \| --network \| --cluster] --reason [--until --wait --reset-exceptions]`, `freeze status`, `unfreeze [scope] [--wait]`, `release list`, `release get --id`, `channel list`, `channel get --id` | Operator commands; they take no `--tenant`. Releases and channels are read-only. |

Manifests are the API's messages (`apiVersion: cluster-vending.io/v1`, kinds
`CloudAccount`, `Network`, `NetworkImport`, `Cluster`, `ClusterImport`). `update -f`
compares the manifest with the stored object, prints each change (for example
`spec.release_channel: Beta → Alpha`), and applies only those. A field the manifest
leaves out is left as it is. `--wait` follows an operation to its end.

### Events and conditions

Events are kept as Kubernetes `events.k8s.io` objects on the object (about an hour),
and the last ten are on the object's `status.recentEvents`. The API reads them with
`ListClusterEvents` and `ListNetworkEvents`. Every event is emitted when a state
changes, never on a pass that finds things as they were. The main reasons (the full
list is in `pkg/reconcile/events.go`):

| Object | Reason | Type | When |
|---|---|---|---|
| Network, Cluster | `Provisioning`, `Updating`, `Running`, `Ready` | Normal | The state of the resources was reached. |
| Network, Cluster | `Failed` | Warning | A resource failed or vanished, or the pass could not apply the change. |
| Network, Cluster | `ImportDiscovered`, `ImportBlocked`, `TakenOver` | Normal, Warning, Normal | The import's plan, and the takeover. |
| Network, Cluster | `PlatformFrozen`, `PlatformUnfrozen`, `LetThrough` | Normal | The freeze holds the object, ended, or lets it through. |
| Network, Cluster | `Repaired` | Normal | An object deleted in the management cluster was created again. |
| Network, Cluster | `CodeChangeStarted`, `CodeChangeFinished`, `CodeChangeHalted` | Normal, Normal, Warning | A code change rolls out, is applied, or halts. |
| Cluster | `ControlPlaneActive`, `NodeGroupReady`, `AddonsReady` | Normal | Steps of creation. |
| Cluster | `UpgradeStarted`, `UpgradeNodes`, `UpgradeStep`, `UpgradeFinished`, `UpgradeHalted` | Normal, Warning for halt | Steps of a Kubernetes upgrade. |
| Cluster | `ControlPlaneUpgradedByAWS` | Warning | EKS runs a newer minor than the spec (AWS upgraded it by force at the end of support); the spec follows it. |
| Cluster | `UpgradePaused`, `UpgradeResumed`, `UpgradeRetried` | Normal | An upgrade paused by a freeze or a paused resource, resumed, or retried. |
| Cluster | `AddonRecovering`, `AddonUpgradeScheduled` | Warning, Normal | Add-ons that failed or did not settle, and the daily version check. |
| Cluster | `AutomaticUpgradeStarted`, `AutomaticUpgradeSucceeded`, `AutomaticUpgradeFailed` | Normal, Normal, Warning | The rollout set a version, it finished, or it failed or stalled. |
| Cluster | `PlatformComponentInstalling`, `PlatformComponentReady`, `PlatformComponentFailed`, `PlatformComponentRemoved`, `PlatformComponentsReady` | Normal, Warning for failed | Platform component states. |
| Cluster | `StuckReleaseRecovered`, `RecoveryLimitReached`, `HelmReleaseFailed` | Warning | Helm recovery and giving up. |
| Cluster | `NodePoolsHandedOver` | Normal | Karpenter pools an earlier build made for the tenant are the tenant's now. |
| Cluster | `ComponentRetried`, `UpgradeRetried` | Normal | `cluster retry`. |
| Cluster | `ReleaseMoveStarted`, `ComponentUpgraded`, `ReleaseMoveRecovering`, `ReleaseMoveHalted`, `ReleaseMoveFinished` | Normal, Warning for recovery and halt | The component move to a release. |
| Cluster | `TeardownStarted`, `TeardownFinished`, `TeardownTimedOut`, `TeardownSkipped` | Normal, Warning | The deletion's first stage. |
| Cluster | `DeprecatedAPIs`, `DeprecationScanFailed`, `DeprecationScanClean`, `KubernetesEnding` | Warning, Warning, Normal, Warning | Scan results, and a minor on no channel. |
| CloudAccount | `Bootstrapped`, `ProviderConfigFailed`, `AccessVerified`, `AccessDenied`, `Deleted` | | The account's setup and access. |
| Release | `ReleaseValid`, `ReleaseInvalid` | | A release's checks. |
| ReleaseChannel | `ChannelOffers`, `ChannelCannotMove`, `PromotionWaiting`, `RolloutTier`, `RolloutHalted` | | A channel's release, its soak, its rollout's tier and halt. |

Conditions to look at first: `Ready` and `Synced` on every owned kind; `Frozen` on
the frozen objects; `DeletionBlocked` (with the dependents named); `AccessVerified`
on an account; `PlatformComponentsReady`, `UpgradePaused` and `SpecEditIgnored`
on a cluster; `DeprecationScan` for scans; and `Offering` on a channel.

### Metrics and alerts

Every process serves Prometheus metrics as plain HTTP, without authentication, to
scrape through the management network:

| Process | Where | Registry |
|---|---|---|
| API server | `/metrics` on its HTTP listener (`--http-addr`, 8080) | controller-runtime's (`client-go` and cache metrics too) |
| controller | `/metrics` on `--metrics-addr` (command default `0`, off; the chart and `config/default` set `:8080`) | controller-runtime's (`controller_runtime_*`, `workqueue_*`, `rest_client_*`) |
| live reader | `/metrics` on `--live-addr` (8090), and on `--live-metricsAddr` (8091 in the chart and `config/live`), a listener that serves only `/metrics` and the probes | its own |

No label carries a request ID, an error message, a user, a value or a tenant or
cluster name: labels come from fixed vocabularies (phases, codes, kinds, channels,
tiers) or from bounded sets checked when the series is made (catalog components,
Kubernetes minors, managed resource kinds, gate codes, event reasons). A value
outside the set, or past its bound, is `other`.

**API server.**

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `cluster_vending_rpc_requests_total` | Counter | `rpc_service`, `rpc_method`, `rpc_type`, `code` | Completed calls, including refused ones; unknown procedures are `unknown`. |
| `cluster_vending_rpc_duration_seconds` | Histogram | `rpc_service`, `rpc_method`, `rpc_type` | Lifetime of a call (a stream's whole life). |
| `cluster_vending_rpc_in_flight` | Gauge | `rpc_service`, `rpc_method`, `rpc_type` | Calls executing now. |
| `cluster_vending_auth_failures_total` | Counter | `reason` | Calls refused by authentication: `missing_token`, `malformed_header`, `rejected` (signature, issuer, audience, expiry), `issuer_unavailable`. |
| `cluster_vending_store_requests_total` | Counter | `operation`, `kind`, `outcome` | The store's calls to the Kubernetes API (the platform's custom resources): `get`, `list`, `create`, `update`, `patch`, `delete`, `update_status`, `patch_status`; outcome `ok`, `not_found`, `conflict`, `already_exists`, `invalid`, `forbidden`, `timeout`, `error`. The controller's store has them too. |
| `cluster_vending_store_request_duration_seconds` | Histogram | `operation`, `kind` | Their duration. |
| `cluster_vending_live_client_requests_total` | Counter | `method`, `outcome` | Calls to the live reader: `ok`, `timeout`, `unreachable`, or the reader's own code (`unavailable`, `resource_exhausted`, ...). |
| `cluster_vending_live_client_request_duration_seconds` | Histogram | `method` | Their duration. |
| `cluster_vending_live_client_in_flight` | Gauge | | Live calls executing now. |

**Controller: the fleet.** One collector refreshes a snapshot of the informer cache
every 30 s on the elected leader (no API call on a scrape; standbys publish only the
refresh health). A failed refresh keeps the previous snapshot.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `cluster_vending_fleet_resources` | Gauge | `kind`, `phase` | Accounts, networks, clusters and tenant components by phase. |
| `cluster_vending_fleet_conditions` | Gauge | `kind`, `condition`, `status` | `ready` and `synced` on every kind, `reachable` on clusters, `accessverified` on accounts. |
| `cluster_vending_fleet_cluster_configuration` | Gauge | `channel`, `tier`, `mode` | Clusters by channel, tier and upgrade mode. |
| `cluster_vending_fleet_clusters_by_version` | Gauge | `minor`, `channel` | Clusters by the Kubernetes minor they run. |
| `cluster_vending_fleet_clusters_extended_support` | Gauge | `minor` | Clusters on a minor no channel offers (extended support, Manual upgrades only). |
| `cluster_vending_fleet_generation_pending` | Gauge | `kind` | Owners whose status has not observed the latest generation. |
| `cluster_vending_fleet_oldest_provisioning_seconds` | Gauge | `kind` | Age of the oldest owner still provisioning. |
| `cluster_vending_fleet_deletions` | Gauge | `kind`, `stage` | Owners being deleted; a cluster's stage is `teardown` (its load balancers and nodes) or `resources`. |
| `cluster_vending_fleet_oldest_deleting_seconds` | Gauge | `kind` | Time since the deletion request of the oldest owner still deleting. |
| `cluster_vending_fleet_frozen_resources` | Gauge | `kind` | Owners whose `Frozen` condition is true. |
| `cluster_vending_fleet_inventory_resources` | Gauge | `owner_kind` | Recorded inventory references. |
| `cluster_vending_fleet_managed_resources_not_ready` | Gauge | `owner_kind`, `kind`, `state` | Crossplane managed resources of networks and clusters not READY (`creating`, `waiting`, `failed`, `deleting`, `missing`, `updating`), by resource kind. |
| `cluster_vending_fleet_upgrade_clusters` | Gauge | `channel`, `tier`, `phase` | Kubernetes upgrades in progress by step. |
| `cluster_vending_fleet_upgrade_oldest_phase_seconds` | Gauge | `phase` | Longest time in the current upgrade step. |
| `cluster_vending_fleet_upgrade_halted` | Gauge | `channel` | Upgrades halted (add-ons did not settle after the recoveries). |
| `cluster_vending_fleet_addons_behind` | Gauge | | Add-ons not on their new version during upgrades. |
| `cluster_vending_fleet_components` | Gauge | `scope`, `phase` | Platform and tenant component instances by phase. |
| `cluster_vending_fleet_component_phases` | Gauge | `scope`, `component`, `phase` | The same by catalog component (a name the catalog does not define is `other`). |
| `cluster_vending_fleet_component_oldest_not_ready_seconds` | Gauge | `scope`, `component` | Longest time an instance has not been Ready. |
| `cluster_vending_fleet_helm_releases` | Gauge | `state` | Platform component Helm Releases: `deployed`, `backoff` (paused by the platform's backoff), `gave_up`, `stuck` (pending-* or uninstalling), `failed`, `other`. |
| `cluster_vending_fleet_release_moves`, `cluster_vending_fleet_code_changes` | Gauge | `phase`; `kind`, `phase` | Component moves to a release, and code changes (`cluster`, `network`, `catalog`) by phase. |
| `cluster_vending_rollout_clusters` | Gauge | `channel`, `rollout`, `tier`, `state` | A channel's rollouts (`kubernetes`, `release`, `code`): clusters `pending`, `in_progress`, `succeeded`, `failed`, `blocked` per tier. |
| `cluster_vending_rollout_halted` | Gauge | `channel`, `rollout` | 1 when the rollout is halted. |
| `cluster_vending_rollout_current_tier` | Gauge | `channel`, `rollout`, `tier` | 1 for the tier the rollout is at. |
| `cluster_vending_rollout_blocked_clusters` | Gauge | `channel`, `gate` | Clusters the Kubernetes rollout passes over, by first gate code (of the at most 20 the channel lists). |
| `cluster_vending_fleet_scan_oldest_attempt_seconds` | Gauge | `kind` | Longest time since a running cluster was scanned or sampled: `deprecations`, `recommendations`, `policy` (never: since its creation). |
| `cluster_vending_freeze_policy_present`, `cluster_vending_freeze_phase`, `cluster_vending_freeze_resources` | Gauge | `phase`; `state` | The platform freeze: present, its confirmation phase, matched and paused resources. |
| `cluster_vending_fleet_metrics_active`, `..._refresh_success`, `..._last_success_timestamp_seconds`, `..._refresh_duration_seconds`, `..._refresh_errors_total` | Gauge, Counter | | The snapshot's health. |

**Controller: transitions.** Counted and timed where the reconcilers make them.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `cluster_vending_events_total` | Counter | `kind`, `type`, `reason` | Every event the controller emits ([Events and conditions](#events-and-conditions)). Each transition is one: `UpgradeStarted`, `UpgradeFinished`, `UpgradeHalted`, `AutomaticUpgradeFailed` (a rollout's upgrade failed or timed out), `StuckReleaseRecovered`, `RecoveryLimitReached`, `HelmReleaseFailed` (gave up), `TeardownTimedOut`, `Deleted`, `RolloutHalted`, `CodeChangeHalted`, `ReleaseMoveHalted`, ... |
| `cluster_vending_cluster_provisioning_seconds` | Histogram | | A cluster's creation to its first RUNNING (imports excluded). |
| `cluster_vending_upgrade_duration_seconds` | Histogram | | A finished Kubernetes upgrade, from first seen in progress. |
| `cluster_vending_component_install_seconds` | Histogram | `component` | A platform component's INSTALLING to READY. |
| `cluster_vending_results_write_failures_total` | Counter | `kind` | Scan results not written to the cluster's results bucket. |

controller-runtime adds `controller_runtime_reconcile_total`,
`controller_runtime_reconcile_errors_total`, `controller_runtime_reconcile_time_seconds`
(per `controller`) and `workqueue_*` (per `name`).

**Live reader.**

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `cluster_vending_live_rpc_requests_total`, `cluster_vending_live_rpc_duration_seconds`, `cluster_vending_live_rpc_in_flight` | Counter, Histogram, Gauge | `rpc_service`, `rpc_method`, `rpc_type` (`code`) | Live reads served, by outcome code; their duration; those in flight. |
| `cluster_vending_live_builds_total` | Counter | `kind`, `outcome` | Builds of a live view, by outcome. |
| `cluster_vending_live_build_seconds` | Histogram | `kind` | Their duration. |
| `cluster_vending_live_refused_total` | Counter | `why` | Reads answered from a cooldown or refused busy. |
| `cluster_vending_live_view_bytes` | Gauge | | Bytes the views keep. |

**Scraping.** `config/prometheus` (ServiceMonitors for the three processes and the
PrometheusRule; uncomment it in `config/default`) or the chart's
`metrics.serviceMonitor.enabled` and `metrics.prometheusRule.enabled`. Each target
gets `cluster_vending_role` (`server`, `controller`, `live`), which the alerts select
on. The live reader's metrics port and the controller's are reached from namespaces
labelled `metrics=enabled` (`networkPolicy.metricsFrom`); the API server's `/metrics`
shares its API port, so Prometheus goes in `networkPolicy.server.from`.
`config/grafana/cluster-vending-fleet.json` is a fleet overview dashboard (clusters
by phase and minor, components not ready, Helm releases, rollouts, transitions,
deletions, API latency and errors, live reads, reconcile errors).

**Alerts** (`config/prometheus/rules.yaml`; `make prom-rules-check` runs promtool on
them). The fleet alerts fire only on a fresh snapshot of the publishing leader
(`fleet_metrics_active == 1`, last success under 120 s ago); a stale snapshot is its
own alert. Each alert's `runbook_url` is its section below.

#### ClusterVendingControllerDown

No controller answers its scrape: nothing reconciles, rolls out or deletes. Check
the controller Deployment (`kubectl -n cluster-vending-system get pods`), its logs
and the leader election Lease. The API keeps serving reads of the stored state.

#### ClusterVendingMetricsTargetDown

A target does not answer. Check the pod, then the scrape path: the metrics port, the
Service, `networkPolicy.metricsFrom` (live reader, controller) or
`networkPolicy.server.from` (API server).

#### ClusterVendingReconcileErrors

More than 20% of a controller's reconciles fail for 15 minutes. The controller's logs
name the object and the error: an AWS API error, a provider that is down, a
Kubernetes API that refuses. `cluster_vending_store_requests_total{outcome!="ok"}`
shows store errors.

#### ClusterVendingReconcileStuck

A worker has run one reconcile for over 15 minutes: a slow AWS or workload-cluster
call holds it, and that controller's queue waits behind it
(`workqueue_depth{name=...}`). Take a goroutine dump (`SIGQUIT`) if it does not move.

#### ClusterVendingRPCServerErrors

More than 5% of API calls fail with `internal`, `unknown`, `unavailable` or
`deadline_exceeded`. Look at `cluster_vending_rpc_requests_total` by `rpc_method`,
the store's outcomes and the API server's logs.

#### ClusterVendingLiveReaderUnreachable

Over half of the live reads time out or never reach the live reader: node lists and
`GetNodeResources` fail. Check the live reader pod, its Service and
`networkPolicy.live`; `cluster_vending_live_refused_total` shows a busy reader.

#### ClusterVendingFleetMetricsStale

The leader's fleet snapshot failed to refresh (`fleet_metrics_refresh_errors_total`
rises): the counts are old. Check cache sync and RBAC in the controller's logs before
trusting the fleet alerts.

#### ClusterVendingNoFleetPublisher

No controller publishes fleet metrics: none is leader, or none runs. Check leader
election and the controller pods.

#### ClusterVendingResourcesFailed

Accounts, networks, clusters or tenant components are FAILED for 15 minutes.
`kubectl get cvc -A` (or the CLI) lists them; their status message and recent events
say why. Fix the cause, then `cluster retry` where it applies.

#### ClusterVendingComponentsFailed

Component instances are FAILED for 15 minutes. The cluster's
`status.platformComponents` has the chart, Helm state and last error;
`cluster retry` after the fix.

#### ClusterVendingComponentNotReady

A component instance has not been Ready for 30 minutes. Check its workloads in the
workload cluster (the platform waits for its Deployments, StatefulSets and
DaemonSets), its Helm state and `nextRetryAt` (backoff).

#### ClusterVendingHelmReleaseGaveUp

The platform gave up on a release after `--helm-max-failures` counted failures: it is
tried again only at the backoff cap. Fix the chart, values or workload, then
`cluster retry`. See [Helm operations](#helm-operations).

#### ClusterVendingHelmReleaseStuck

A Helm release is pending-* or uninstalling for 30 minutes. The platform recovers a
stuck release up to 3 times (`StuckReleaseRecovered` events); past that, see
`ClusterVendingStuckReleaseRecoveryLimit`.

#### ClusterVendingStuckReleaseRecoveryLimit

A stuck release was recovered as often as allowed and the component stays FAILED.
Look at the chart's release in the workload cluster (`helm -n <ns> history`) and fix
it by hand, then `cluster retry`.

#### ClusterVendingRolloutHalted

A channel's rollout (Kubernetes, release or code) halted: more clusters of a tier
failed than its policy's `maxFailed`. `kubectl get cvch <channel> -o yaml` lists the
members; fix or retry the failed clusters.

#### ClusterVendingUpgradeHalted

A cluster's Kubernetes upgrade halted after the add-on recoveries. Its
`status.upgrade.message` names what did not settle; `cluster retry` resumes it.

#### ClusterVendingChangeHalted

A cluster's code or catalog change, or its component move to a release, halted on a
failure. The cluster's events (`CodeChangeHalted`, `ReleaseMoveHalted`) and status say
which step.

#### ClusterVendingDeletionStuck

An owner has been deleting for 2 hours. Its `DeletionBlocked` condition names the
dependents it waits for; its status message lists the cloud resources left. See the
deletion order in [Deletion](#deletion).

#### ClusterVendingTeardownTimedOut

A cluster's teardown (its load balancers and Karpenter's nodes) passed
`--teardown-timeout` with objects left. The deletion went on; their cloud resources
may be left in the account: the event names them, delete them in AWS.

#### ClusterVendingAccountAccessLost

An account's access check fails (`AccessVerified` False): the tenant role cannot be
used in a region or lacks a permission. `account verify` names the check; fix the
role's stack.

#### ClusterVendingClustersUnreachable

The controller cannot read a workload cluster (`Reachable` False:
`CLUSTER_UNREACHABLE` or `CREDENTIALS_REJECTED`). Scans, component readiness and node
counts are stale for it. Check the endpoint, the access entries and the kubeconfig
Secret.

#### ClusterVendingScanLagging

A running cluster was not scanned for two intervals (deprecations 13 h,
recommendations and policy 1 h). Each scanner has one worker: check
`controller_runtime_reconcile_time_seconds` of that controller and unreachable
clusters.

#### ClusterVendingResultsWriteFailing

Scan results do not reach the clusters' results buckets: a bucket missing or not
Ready, or a tenant role that cannot write it. The Cluster keeps the counts and a
notice; the controller logs the cause. See [Results in S3](#results-in-s3).

#### ClusterVendingProvisioningSlow

A network or cluster is provisioning for over an hour. Check its conditions, the
provider's queue, cloud quotas and the freeze (frozen time counts).

#### ClusterVendingUpgradePhaseSlow

An upgrade step has lasted 30 minutes: node drains, add-ons or components that do not
settle. Tune the threshold to your workloads.

#### ClusterVendingFreezeNotConfirmed

The platform freeze is not confirmed after 10 minutes: some resources could not be
paused. `admin freeze status` shows them.

### Runbook: provider upgrade

Upgrading the Crossplane AWS providers (`provider-aws-*`, the family provider) and
`provider-helm` under a running platform, without a provider acting on a tenant's
resources in the middle of it. Not run end to end against a real management
cluster.

1. Check first: all providers `INSTALLED=True HEALTHY=True`; no network or cluster in
   `Provisioning`, `Updating` or `Deleting`. Note the current packages (for a rollback).
2. The controller may stay up. To be conservative, scale it to 0.
3. `cluster-vending admin freeze --reason "provider upgrade" --wait`. Wait for
   `Frozen`. `admin freeze status` shows partial failures.
4. Check that nothing reconciles: every managed resource and every Helm `Release` has
   `crossplane.io/paused: "true"`, and each paused resource reports `Synced=False`
   with reason `ReconcilePaused`.
5. Upgrade the family provider first, then the service providers, all to one version,
   by patching `spec.package`. Keep every AWS provider on one registry and family:
   two family providers fight over the same `ProviderConfig` CRDs.
6. Check the providers: `INSTALLED=True HEALTHY=True`, one active revision each, the
   new pods running. Step 4's checks still show nothing unpaused.
7. Deploy the controller build made for the new version (`ProviderVersion` in
   `pkg/crossplane/aws/aws.go` and the module in `go.mod`). Start the controller. On
   start it waits up to 5 minutes for every kind it uses. Then
   `admin unfreeze --cluster <id>` for one cluster of low importance, and check it:
   `RUNNING`, and its managed resources all synced and ready. Read the provider's log
   for creations it should not make.
8. `admin unfreeze` for the rest.

Rollback: while everything is paused, nothing has changed in the cloud. Patch the
packages back, wait until they are healthy, then continue from step 7 with the old
build. After a resume, a rollback is another upgrade: pause again first.

### Runbook: upgrade and roll back the platform

Every build has a serial (`buildserial.Serial`). It is stamped
(`cloud.cluster-vending.io/build-serial`) on the CRDs it installs, the component
definitions it ships and its shipped release. A build never writes over a newer
serial: a CRD of a newer build is not applied (its older schema would prune the
newer fields of every stored object), a newer definition is neither replaced nor
deleted, and channel `alpha` is not moved off a newer build's release. Each refusal
is logged as an error, and the controller's `/readyz` fails (`build-serial`), so a
rollout to an older build stops at its first pod. Builds of one serial are
interchangeable.

Upgrade:

1. `cluster-vending admin freeze --reason "platform upgrade" --wait`.
2. Deploy the new build. Its controller installs the CRDs and the catalog on start,
   before leader election. Wait until every replica is ready.
3. Watch: `admin channel get` for each channel, and the code and catalog changes
   (`status.codeChange`, `status.catalogChange`) of a low-importance cluster.
4. `admin unfreeze --cluster <id>` for one cluster, check it, then `admin unfreeze`
   for the rest. Code and catalog changes then roll out per channel and tier.

Roll back:

- To a build of the same serial (a code fix, no CRD, definition or release change):
  safe. Deploy it as an upgrade.
- To an older serial: not supported. Its pods refuse to start ready, and while one
  runs it reads newer objects with older types, so a write of it can drop fields it
  does not know. Roll forward with a fix instead. If the older build must run, scale
  the controller to 0, restore the CRDs, definitions and channels from a backup of
  that build, and accept that status fields only the newer build knows are lost.

Serial 2 removed sub-tenants and Capsule (CRDs, definitions and `release.yaml`
changed; RenderVersion stays 12). Delete the `naas` section from `platform.yaml`
before deploying it: both processes refuse to start with it. Its controller
releases the old `SubTenant` objects and deletes their CRD
([Sub-tenant removal](#sub-tenant-removal)), and Capsule leaves each cluster as a
catalog change ([Capsule removal](#platform-components)).

### Runbook: orphaned EKS name reservation

A reservation (`cv-eks-name-*` ConfigMap in `kube-system`) is orphaned when its
Cluster is gone but the reservation stays, for instance after a crash or a removed
finalizer. A new create of that name then fails closed. Do not reclaim it by age, do
not delete all reservations, and do not adopt an EKS cluster because its name matches.

1. Read the reservation: its complete scope (partition, account, region, exact name),
   its tenant and its allocation UUID.
2. Read the management Cluster and its inventory in that tenant, if any.
3. Read the actual EKS identity in AWS: `aws eks describe-cluster` in that account and
   region, with the tenant role.
4. If the EKS cluster exists, it belongs to the registration or it is someone else's.
   Restore the original registration and let the normal finalization complete. Do not
   adopt it on the reservation's behalf.
5. If AWS shows no cluster and every managed resource is cleaned up, delete that
   specific reservation, with its UID and resourceVersion as preconditions.

### Local login

With the `auth` block of `hack/config.local.yaml` commented out (the default), the API
accepts every caller, and the console runs without login. With it set and
`make keycloak` running, both the API and the console require a token from the local
Keycloak. [Authentication](#authentication) describes the settings.

### Checks and tests

- `make check` runs `lint` (buf lint of the protos, golangci-lint), `vet` and `test`.
- `make test` runs the unit tests with the race detector, against fake API servers.
  The store has a behaviour suite (`pkg/database/storetest`) that each store test
  runs. The catalog tests check the system group's sizing, and that the release and
  the definitions agree on the components.
- The console has its own checks, described in the Checks section of
  [`ui/README.md`](../ui/README.md#checks).

### End-to-end on a sandbox account

`test/e2e` drives a running API server and controller against a real AWS account,
with the same Connect clients as the CLI. It is behind the `e2e` build tag and off
unless `CV_E2E=1`, so `make check` and `go test ./...` never build or run it.
`make e2e-lint` vets and lints it without running anything. It creates real
resources and costs money: use an account you can empty.

**Prerequisites**

- A sandbox AWS account that holds nothing you need. The harness creates a VPC with one
  NAT gateway, an EKS cluster with a two-node system group, Karpenter nodes, an S3 bucket
  and IAM roles, and deletes them.
- The tenant role's stack (`cluster-vending-<role>`) applied in that account, from the
  template of the build under test (`GET /cloudformation/`): a stale template fails the
  access check or, worse, a deletion. The role is `provisioner` for the full run; with
  `reader` or `updater` the harness asserts the verdict and the refusal to create, and runs
  only the import scenario.
- An API server and a controller running (`make run`, `make run-controller`, or a
  deployment), with `aws-management-role-arn` set, against a management cluster with
  Crossplane, its providers and room for the platform's pods: 12 GB of memory at least.
- Optional, each adds checks: `CV_E2E_KUBECONTEXT` (the management cluster: node group
  `atProvider`, the workload kubeconfig for the Karpenter nodes, EKS name reservations,
  leftover managed resources), the `aws` CLI with credentials of the sandbox account
  (node group, bucket, tags, instance profiles, the tag-based leak check) and `kubectl`.
  A check whose tool is missing is logged as skipped, not failed.

**Variables**

| Variable | Default | Meaning |
|---|---|---|
| `CV_E2E` | | `1` turns the harness on. |
| `CV_E2E_SERVER` | `http://127.0.0.1:8080` | The API server. `CV_E2E_TOKEN` is sent as a bearer token when set. |
| `CV_E2E_TENANT` | required | The tenant, created when missing. A tenant cannot be deleted through the API and stays. |
| `CV_E2E_ACCOUNT_ID`, `CV_E2E_REGION` | required | The sandbox account and its region. |
| `CV_E2E_ROLE`, `CV_E2E_ROLE_NAME` | `provisioner`, `cluster-vending-<role>` | `provisioner`, `reader` or `updater`, and the IAM role's name. |
| `CV_E2E_KUBECONTEXT` | | The management cluster's kubectl context. |
| `CV_E2E_CIDR`, `CV_E2E_CHANNEL`, `CV_E2E_K8S_FROM` | `10.77.0.0/16`, `RELEASE_CHANNEL_ALPHA`, `1.35` | The network, and the channel and Kubernetes minor of the cluster. The upgrade scenario moves it to `status.kubernetes.available`, so the minor must be behind what the channel offers. |
| `CV_E2E_IMPORT_CLUSTER`, `_VPC`, `_SUBNETS`, `_NODE_GROUP` | | An existing EKS cluster, its VPC, the VPC's subnets (comma separated) and the cluster's default node group (`default`). Without the cluster, scenario 8 is skipped. |
| `CV_E2E_IMPORT_TAKEOVER=1` | off | Also take the import over. The platform then owns the cluster and the VPC, and the cleanup deletes them from AWS. |
| `CV_E2E_IMPORT_DRIFT=1` | off | Change the imported cluster's endpoint access in AWS, assert the takeover is refused, restore it and import again. Needs `aws` and the management cluster. |
| `CV_E2E_IMPORT_PLATFORM=1` | off | Install the platform components on a taken-over cluster (default: skipped). |
| `CV_E2E_STRICT_STAGES=1` | off | Require all seven deletion stages to be seen. By default stages 4 to 7 are required: stages 1 to 3 can pass within one reconcile on a cluster with nothing in them, between two polls. |
| `CV_E2E_RECREATE_RUNNING=1` | off | Wait for the re-created cluster to be RUNNING (default: until its resources are rendered). |
| `CV_E2E_POLL`, `_REQUEST_TIMEOUT` | `10s`, `60s` | Polling interval and the bound of one API call. |
| `CV_E2E_NETWORK_TIMEOUT`, `_CLUSTER_TIMEOUT`, `_COMPONENTS_TIMEOUT`, `_UPDATE_TIMEOUT`, `_UPGRADE_TIMEOUT`, `_DELETE_TIMEOUT`, `_SCAN_TIMEOUT` | `30m`, `60m`, `45m`, `30m`, `120m`, `60m`, `20m` | Deadline of each wait. A Go duration or a number of minutes. |

```sh
export CV_E2E_TENANT=e2e CV_E2E_ACCOUNT_ID=111122223333 CV_E2E_REGION=us-west-2 \
       CV_E2E_KUBECONTEXT=mgmt
make e2e                                           # everything
CV_E2E=1 go test -tags e2e ./test/e2e/ -v -run 'TestE2E/0[12]'   # scenarios 1 and 2
```

The scenarios are subtests of `TestE2E`, in order, and share what they create: `01_account_onboarding`
(onboarding, the access verdict of the role, what a reader or updater may not do), `02_network_create`,
`03_cluster_create` (install order, platform components, system node group (static size, no autoscaler tags), results bucket,
node lists), `04_cluster_update` (labels in place; a user Karpenter group `t3` with real nodes; its removal),
`05_cluster_upgrade` (one minor, manually: steps in order, components skipped when unchanged),
`06_scanners`, `07_cluster_delete` (seven stages, name reservation released, the same name again, deleted
again), `08_import_takeover`, `09_network_delete`. A scenario whose predecessor failed is skipped. A failing
wait logs the cluster's status, conditions, resources that are not ready, components and events.

**Cost and time.** A full run takes 3 to 4 hours: the network about 10 minutes, the cluster 15 to 20 and its
components 15 to 25 more, the upgrade 45 to 90, each deletion 20 to 30. While it lives it costs roughly what an
EKS control plane, two `t3.medium` nodes, a NAT gateway and a `t3` Karpenter node cost per hour, a few US
dollars in all; a run that is killed leaves the money running.

**Cleanup.** Each scenario that creates something registers its deletion on `TestE2E`. When the scenarios are done
(or the test is stopped by a failure or `-timeout`, but not when the process is killed) the cleanups run, last created
first, and each verifies the deletion completed. Then the leak check lists what the run left: Crossplane managed
resources named after the run id on the management cluster, and, with credentials, AWS resources tagged with the
run's label (`cv-e2e=<run id>`, which every resource of the run carries as a tag) or with the ids of its networks and
clusters, and an EKS cluster by name. A leak fails the run, and is logged as `LEAK:` with what to delete. After a
killed run, delete the cluster and the network through the API (`cluster delete --force` if stuck), then look for the
`cv-e2e-*` names and the `cv-e2e` tag.

**What it checks that unit tests cannot.** The AWS behaviours the platform assumes:

- **The `ec2:CreateTags` DryRun on a VPC that does not exist** tells an updater from a reader: scenario 1
  asserts the verdict of the configured role (updater: CreateTags passes, CreateVpc does not, condition
  `AccessVerified` True with `VerifiedForImport`; reader: both fail; provisioner: verified).
- **Node group `status.atProvider` has the labels, taints, tags and scaling**, which the platform reads back:
  scenario 3 reads the system group from the managed resource and from EKS and asserts the pool label and
  taint, no `k8s.io/cluster-autoscaler/*` tags (on any group) and `min = desired = max` with `min >= 2`.
- **EKS accepts a node group update where min is above desired.** Only the shape is asserted on a new group;
  changing `nodeGroups.system.minSize` of the controller is not driven by the harness. Do it by hand between
  scenarios 3 and 4 and watch the system group stay `Ready` in `status.nodeGroups`.
- **provider-helm resumes a paused Release.** Install (scenario 3) and uninstall (scenario 7, stages 2 and 3, which
  lift the platform's own pause) go through it; a component that needed a retry is logged with its failures.
- **EventBridge rules carry an `eventBusName` that is read back on rules that already exist**: scenario 3
  asserts Karpenter's rules are READY and at the generation applied, and stage 5 of scenario 7 deletes them.
- **The tenant role may delete Karpenter's instance profiles**: scenario 4 makes nodes, so Karpenter makes
  profiles; scenario 7 asserts the deletion does not stall at the node role and leaves no profile tagged with
  the cluster.

## 7. Known limits

What is not verified, or not built. Anything not on this list works as described above.

- **Not run against real AWS, EKS or Crossplane.** Every test runs against a fake API
  server. The provider upgrade runbook is not run end to end.
- **Ordered deletion through Usages is untested**: the fake server has no Usage
  webhook, so the refusals and waits of the webhook are not exercised.
- **CEL rules** in the CRDs are not loaded into a real API server.
- **Authentication** has run only against a fake OIDC issuer. No Entra tenant, Keycloak
  or Okta has issued a token the server verified.
- **Helm reliability and the scans** are tested against fakes. Their behaviour on a live
  workload cluster is not proven.
- **Design targets** are not load tested: 100 clusters, 1,000 nodes and 10,000 pods
  per cluster ([Results in S3](#results-in-s3)).
- **Only AWS.** `gcp` and `azure` are valid enum values, and they are refused by every
  RPC. No GCP or Azure definition ships.
- **Not implemented, refused with `UNIMPLEMENTED`**: NAT gateway mode `Regional`; updating
  `spec.aws.accessConfig.authenticationMode` (it can be set at creation).
- **Tenant component versions cannot be set.** The `spec.version` field of `Component` is
  refused by the API. Installs use the cluster's release.
- **Helm chart CRDs** in a chart's own `crds/` directory are not upgraded by Helm during a
  release move. A chart with a separate CRD chart (`install.helm.crds`) gets both in one pass.
- **Automatic upgrades have no rollback.** EKS cannot downgrade a cluster. A failed
  component move halts until `cluster retry`.
- **The deletion's cloud sweep finds only tagged resources.** It deletes Karpenter's
  instances and launch templates and the AWS Load Balancer Controller's load balancers,
  target groups and security groups by the cluster's tags. A Classic Load Balancer of the
  in-tree cloud provider (a Service without the controller), an instance or load balancer
  made by hand, or one whose tags were removed is left, and the network's deletion then
  waits on it (`DependencyViolation`, named in the network's message). A load balancer with
  deletion protection is not deleted: the sweep reports it and times out. The sweep has
  not run against real AWS.
- **Changing a cluster's labels rolls its platform pool.** The labels are tags of the
  EC2NodeClass `platform-pool`, and Karpenter treats a change of its tags as drift: it
  replaces the pool's nodes within the pool's disruption budget. The tags are part of
  the pool's catalog fingerprint, so a label change, and the build that adds the tags,
  is a catalog change: it waits for the cluster's turn in its channel's code rollout
  (tiers, windows, freezes), and the pool keeps its old tags until then. A new cluster
  gets them at once. The roll itself is not avoided.
- **PersistentVolumeClaims are not deleted** with a cluster. The EBS volumes of a claim
  with a `Delete` reclaim policy are left in AWS, as an EKS deletion leaves them.
- **A release's `default` minor is not applied.** A cluster on no channel keeps the
  version it asks for, or EKS's default, although `release.yaml` marks one minor as
  `default`.
- **Sub-tenant leftovers stay on workload clusters.** Releasing an older build's
  `SubTenant` objects and removing Capsule leaves Capsule's CRDs, the sub-tenants'
  namespaces and their RBAC in place ([Sub-tenant removal](#sub-tenant-removal)).
  Deleting them is the tenant's.
- **No authorization.** The API authenticates callers and nothing more: any
  authenticated user (and, with `auth.mode: none`, any caller) can act on every tenant
  and on the platform. Who may sign in is decided at the identity provider.
- **Recommendations** do not model HPA effects: a workload scaled by an HPA spreads its
  load over its replicas, so the per-pod usage is lower than the total.
- **Policy events** are not a durable audit log: they expire with Kubernetes' retention, and
  an engine can omit emission. Native ValidatingAdmissionPolicy findings need a report producer.
- **Quota planning** (AWS service quotas, Elastic IP and VPC limits) is not built.
- **EKS name reservations** do not span independent management clusters, and an EKS
  cluster created in AWS by an actor outside this protocol is not detected by it. There is
  no atomic AWS and Kubernetes ownership transaction.
- **No cost visibility.** The platform reports no spend per tenant or cluster. The nodes,
  volumes and load balancers it and the cluster's controllers create carry the tags
  `cluster-vending.io/tenant`, `cluster-vending.io/cluster` and
  `cluster-vending.io/cluster-name`; activated as cost allocation tags in the tenant's
  payer account, they let AWS Cost Explorer filter and group the spend by tenant and
  cluster. The control plane's CloudWatch log group (`cluster get` shows it and a console
  link when control plane logging is on) is billed to the tenant account as well.
