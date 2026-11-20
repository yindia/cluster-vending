# Controller

The platform's controller, laid out as a kubebuilder v4 project at the root
of the repository's one Go module. It holds the API of the platform's own custom
resources and the controllers that reconcile them. The controllers have no
reconcile logic of their own: each turns a watch event into a call of a
reconciler (`pkg/reconcile/account`, `network`, `cluster`), on the store
that keeps its rows as these objects
(`pkg/database/cloudstore`). None of it has run against a real API server
or AWS; it is tested on a fake API server only.

See [Provider adapters, inventory and update plans](providers-and-plans.md) for
the Kubernetes inventory fields, resource deletion previews and the extension
points for a future GCP backend.

See [Prometheus metrics](metrics.md) for RPC and controller fleet metrics,
scrape configuration, scaling queries and example alert rules.

## API

Group `cloud.cluster-vending.io`, version `v1alpha1`. `Tenant` is cluster
scoped and registers a tenant. Its controller creates and maintains the
`tenant-<id>` namespace; accounts, networks and clusters live there.

| Kind | Short name | Owned by | Owns |
|---|---|---|---|
| `Tenant` | `cvtenant` | (nothing) | namespace registration, without a garbage-collection owner reference |
| `CloudAccount` | `cvacct` | (nothing) | its `Network`s |
| `Network` | `cvnet` | one `CloudAccount` (`spec.accountRef`) | its `Cluster`s |
| `Cluster` | `cvc` | one `Network` (`spec.network.networkRef`) | (nothing) |

One account has many networks; one network has many clusters. Both
references are required and immutable. The controller sets a controller
owner reference along the chain (`pkg/owner`), with
`blockOwnerDeletion`.

Deletion rule: an owner is not deleted while it has dependents. Each kind
has a finalizer; an owner's finalizer is released only once no dependent is
left. Deleting an account never deletes its networks and clusters by
cascade.

The specs mirror the `cloud.v1` contract in `idl/cloud/v1`, with Kubernetes
conventions (camelCase, string enums, `metav1.Condition`). `pkg/convert`
translates between the two, both ways without loss.

The API server keeps its state in these kinds (`pkg/database/cloudstore`,
the only store). Cloud-account creation requires an existing `Tenant` and
its correctly labeled, active namespace; it never creates either implicitly.
Deleting tenant registration retains the namespace. Existing labeled tenant
namespaces are migrated by the tenant controller.

`ReleaseHold` remains the freeze policy authority. The tenant controller
reflects effective platform or tenant freezes in `Tenant.status.freeze` and
the `crossplane.io/paused` annotation, preserving independently applied pauses.

```sh
cluster-vending tenant create acme
cluster-vending tenant list
cluster-vending tenant freeze --tenant acme --reason maintenance --wait
cluster-vending tenant freeze status --tenant acme
cluster-vending tenant unfreeze --tenant acme --wait
```

Tenant commands use `TenantService` and accept only the caller's permitted
tenant, network or cluster targets. Platform operations remain in the operator
`admin` commands through `AdminService`.

## Layout

```
PROJECT                  kubebuilder project file
api/v1alpha1/            the types (edit these), generated deepcopy
pkg/controller/          the three controllers
pkg/controller/manager/  Run: the manager with the reconcilers and probes
pkg/owner/               the ownership chain as owner references and labels
pkg/convert/             typed spec to and from the contract
config/crd/bases/        generated CRDs
config/rbac/             generated role, and the hand-written ones
config/samples/          an account, a network and a cluster of one chain
config/{default,manager,metrics,prometheus,network-policy}/   the controller's manifests
config/server/           the API server's manifests (base and overlays)
```

kubebuilder scaffolds a `Makefile`, a `cmd/main.go` and a `Dockerfile` next
to `PROJECT`. This project has none of them: there is one Makefile and one
binary for the repository. `kubebuilder create api` still works from the
repository root for adding kinds, but its generated `cmd/main.go` edits must be
moved into `pkg/controller/manager`, its controllers from `internal/controller`
into `pkg/controller`, and its `make` hints replaced by the targets below.

## Generate

From the repository root, after changing the types or the RBAC markers:

```
make controller-apis
```

It runs `go tool controller-gen` (deepcopy, CRDs, RBAC role). `make check`
covers these packages like any other.

The controller's roles have no rule on Secrets. The scan for deprecated
API versions reads a workload cluster with the kubeconfig Secret its
ClusterAuth writes in the tenant's namespace
([deprecations](deprecations.md)), and RBAC cannot select Secrets by a
name suffix, so the narrowest scope is the tenant namespace:

- `config/rbac/kubeconfig_reader_role.yaml` is a ClusterRole with `get` on
  Secrets (deployed as `cluster-vending-kubeconfig-reader`). Nothing binds
  it cluster-wide.
- When a cloud account reconciles, the controller applies a RoleBinding of
  that name in the tenant namespace, to its own service account
  (`--service-account`). It is applied again on every account reconcile,
  so existing tenants get it too. It belongs to the namespace, which the
  tenant's accounts share: it is not deleted with an account.
- To make that binding without holding the permission itself, the
  generated role has `create` and `patch` on RoleBindings and the `bind`
  verb on that one ClusterRole name, nothing else on RBAC kinds.

So the controller can get Secrets in tenant namespaces only, and cannot
list, watch or write them. Outside the cluster (`--service-account`
unset, a local run with your kubeconfig) no binding is made.

## Run

```
cluster-vending controller --leader-elect=false --aws-management-role-arn <role arn>
cluster-vending serve                      # the API: writes the objects
```

`controller` takes the global `--kubeconfig`, `--context`, `--log-format`
and `--log-level`, and only these:

| Setting | What it is |
|---|---|
| `--config` | YAML file with any of these settings |
| `--health-probe-addr`, `--metrics-addr` | listen addresses (metrics off by default) |
| `--leader-elect`, `--lease-namespace` | one replica reconciles |
| `--service-account` | name of the controller's own service account, in `--lease-namespace`; bound in each tenant namespace to the kubeconfig reader role. Set by the Deployment, empty outside the cluster |
| `--aws-management-role-arn` | required: the IAM role the Crossplane AWS providers run as (IRSA); the management account is taken from it |
| `--install-providers` | create the providers, provider-helm and their runtime config when missing |
| `--install-catalog` | create the shipped component definitions (`pkg/catalog/definitions`, [catalog.md](catalog.md)) that are missing; existing ones are never changed |
| `--recover-stuck-releases` | default `true`: recover a platform or tenant component whose Helm release is stuck (pending or uninstalling for its chart's install timeout plus 2 minutes): with Helm, in the workload cluster, roll the release back to its last deployed revision (uninstall it when none was deployed) and delete a failed hook Job it left behind, at most 3 times per component version ([catalog.md](catalog.md), "When a component fails"); `false` leaves it failed for a person |
| `--helm-backoff-max` | default `30m`, at least `1m`: the longest pause between two attempts at a Helm release that failed to install or upgrade. The pause starts at 1m, doubles, and has a little jitter; the Release is paused with `crossplane.io/paused` and the platform's marker meanwhile |
| `--helm-max-failures` | default `5`: how many release failures in a row (a hook failed, the rollout timed out, invalid values; not transient API errors, not chart-fetch errors) make a component Failed, with a `HelmReleaseFailed` event; its retries then stay at `--helm-backoff-max` until its version or values change or `cluster retry --component` |
| `--recommendations` | default `true`: every five minutes, sample each running cluster for resource recommendations (read-only: workloads, metrics-server usage, the cluster's own Vertical Pod Autoscalers) and record a week of usage in the tenant namespace ([recommendations.md](recommendations.md)); `false` records nothing |
| `--reconcile-workers` | default `8`: how many cloud accounts, networks and clusters of each kind are reconciled at once; one object is never reconciled twice at the same time. The release, catalog, deprecation and recommendation controllers keep one worker |
| `--automatic-upgrades` | default `false`: the rollout of each channel's Kubernetes minor is planned and reported on the ReleaseChannel and each cluster (`status.rollout`) only; no cluster is upgraded. `true` upgrades clusters with `upgradeMode: Automatic`, tier by tier; **irreversible** (EKS cannot downgrade). See "Rollout of a channel's Kubernetes minor" |
| `--upgrade-timeout` | default `90m`: how long one step of an automatic upgrade (control plane, node groups, add-ons, platform components, tenant components) may go without progress before the upgrade counts as failed (`TimedOut`, against the tier's `maxFailed`). A long upgrade that keeps moving is not timed out |
| `--teardown-timeout` | default `15m`: how long a cluster's deletion waits for the load balancers, and for Karpenter to terminate its nodes, before it goes on (see Deletion) |
| `--provider-credentials-secret` | `<namespace>/<name>[:<key>]`: the providers authenticate with this Secret instead of IRSA (local kind only) |
| `--skip-tenant-role` | local testing only, with `--provider-credentials-secret`: act with the Secret's keys directly instead of assuming each account's tenant role |
| `--aws-region`, `--aws-source-role-arn`, `--aws-external-id` | how the controller reaches a tenant's account, the same three settings as the API server's: the AWS SDK's default region, an optional role to assume with the controller's own credentials before the tenant role (own credentials, source role, tenant role), and an optional external ID for a tenant role whose account has none of its own. The controller's own credentials are never a setting (IRSA in production; `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` in the environment locally). The same access writes each cluster's scan results to its S3 results bucket (deprecations, recommendations, policy violations and events) and empties that bucket when the cluster is deleted ([design/cluster-results-s3.md](design/cluster-results-s3.md)) |

It installs the CRDs of the three kinds and the Crossplane AWS providers
(unless `--install-providers=false`) and waits for their kinds. The API
server installs nothing; it waits for the kinds the controller installs, so
start the controller first.

How the providers run is the platform's, sized for ~100 clusters of ~80
managed resources each: an AWS drift check every 10 minutes (30 for
provider-aws-ec2), a separate Helm readiness/drift check every minute,
at most 10 reconciles per second, 1 CPU and 2Gi memory
requested, 4 CPU and 4Gi limits, and 2 replicas with leader election (one
reconciles, the other is a hot standby) (`crossplaneaws.DefaultProviderRuntime`).
The AWS providers run with the `DeploymentRuntimeConfig`s
`cluster-vending-aws` and `cluster-vending-aws-ec2`, provider-helm with
`cluster-vending-helm`. There is no setting for these. To run the providers
otherwise, set `--install-providers=false` and manage the `Provider` and
`DeploymentRuntimeConfig` objects yourself.

Helm's shorter interval lets release readiness catch up after workloads become
healthy without increasing AWS resource polling. In code, `ProviderRuntime.HelmPoll`
can override it; zero falls back to one minute and negative intervals are rejected.

The controller creates these objects only when they are missing: a
management cluster installed by an older build keeps its runtime configs
(and provider-helm keeps running without one) until they are deleted, so
the controller recreates them on its next start, or edited by hand.
In particular, an existing `cluster-vending-helm` using `--poll=10m0s` keeps that
interval after upgrading; the one-minute default applies only to newly created
runtime configs. An operator must explicitly update an existing config to adopt it.
Existing `Provider` runtime references are also preserved. A provider-helm using
the default runtime (no explicit reference) does not adopt `cluster-vending-helm`
on upgrade; its provider default remains in effect until an operator explicitly
changes that reference. A dedicated observation-only runtime config can change
the poll argument without adopting the platform's replica/resource profile; see
`config/samples/provider_helm_observation_runtime.yaml`.

The controller always brings every resource to this build's renderer. The
former `--render-target` and `--render-rollout` settings are gone; release
objects take their place (docs/design/rollouts.md).

## AWS access

The controller is the only command that looks at AWS: it discovers the VPC
or EKS cluster of an import, checks a new cluster's name against EKS,
resolves the Kubernetes and add-on versions EKS offers, and checks that it
can reach and act in each account (the access check, below). The API server
makes one kind of AWS call, read-only: it reads the scan results and the node
lists of the node groups from each cluster's results bucket, through the
tenant role, with the AWS SDK's default credentials (IRSA in production) and
a session limited to reading `cv-*` buckets
([design/cluster-results-s3.md](design/cluster-results-s3.md)). It creates,
changes and deletes nothing in AWS. Its settings are `aws-region`,
`aws-source-role-arn` and `aws-external-id` (the same three as the
controller's), and `aws-management-role-arn`, which is handed to the tenant
creating the role (`account create` prints it, [onboarding.md](onboarding.md)).

There is one rule, for the controller's lookups and for the Crossplane
providers alike:

```
own credentials -> tenant role (always)
```

- **Own credentials.** For the controller, whatever the AWS SDK's default
  chain finds: IRSA in production, keys in its environment for local
  testing. For the providers, IRSA as the management role in production, or
  the keys in `--provider-credentials-secret` where there is no IRSA. They
  are never a flag value or a file of this project.
- **Tenant role**, always: the role stored when the account was bootstrapped
  (the one every account's ProviderConfig assumes). Nothing is read with the
  own credentials, so keys of one account cannot read another, and a role
  that is not a role of the account is refused. A local run therefore needs
  and exercises the same permissions as production.

### Production: IRSA

The controller's service account carries the IAM role annotation
(`eks.amazonaws.com/role-arn`); `config/default/kustomization.yaml` sets it,
with a placeholder to replace. The providers run as the management role
(`--aws-management-role-arn`, on their runtime config). No keys exist
anywhere and `--provider-credentials-secret` is not set. What must be true:

- The roles' trust policies let the service accounts assume them through
  the cluster's OIDC provider (the usual IRSA trust policy).
- They may call `sts:AssumeRole` on the tenant roles.
- Each tenant role's trust policy allows them (`sts:AssumeRole`, principal:
  the role's ARN). The role's CloudFormation template does this
  (`cluster-vending account create`, [onboarding.md](onboarding.md)).

The simplest arrangement is to run the controller as the management role
itself: the tenant roles then trust one role.

### Local run: keys in the environment and in a Secret

Kind has no IRSA, so both the controller and the providers start from your
keys, and both assume the tenant role. The tenant role's trust policy must
allow the IAM user or role the keys belong to.

1. A kind cluster with Crossplane: `make kind-up`.
2. Your keys in the shell, never in a file or a flag:
   ```
   export AWS_ACCESS_KEY_ID=...
   export AWS_SECRET_ACCESS_KEY=...
   export AWS_SESSION_TOKEN=...   # only for temporary keys
   ```
3. The same keys as the providers' Secret, in the AWS shared credentials
   format under the key `credentials` (the command does not print them):
   ```
   kubectl --context kind-cluster-vending -n crossplane-system create secret generic \
     aws-local-credentials --from-file=credentials=/dev/stdin <<EOF
   [default]
   aws_access_key_id = $AWS_ACCESS_KEY_ID
   aws_secret_access_key = $AWS_SECRET_ACCESS_KEY
   EOF
   ```
   An existing Secret works as it is; name it in the next step.
4. In `hack/controller.local.yaml`, set `aws-management-role-arn` (any role
   ARN of the management account; on kind it is only validated) and
   uncomment `provider-credentials-secret: crossplane-system/aws-local-credentials`.
5. `make run-controller` in the shell with the keys.
6. `make run` in another terminal: the API server (`hack/config.local.yaml`,
   the Kubernetes store; with the same AWS keys in its environment it reads
   results buckets, without them it answers with a "results unavailable"
   notice).

With the Secret set, every ProviderConfig the platform writes is:

```yaml
spec:
  credentials:
    source: Secret
    secretRef: {namespace: tenant-<tenant>, name: <providerConfig>-credentials, key: credentials}
  assumeRoleChain:
  - roleARN: arn:aws:iam::<tenant account>:role/<tenant role>
```

and without it (production) `credentials: {source: IRSA}` with the same
`assumeRoleChain`. A namespaced ProviderConfig reads its Secret from its own
namespace, whatever namespace its `secretRef` names, so the controller copies
the configured key of your Secret into the tenant namespace as
`<providerConfig>-credentials` on every account it applies (a rotated key is
copied again on the next reconcile), and deletes the copy with the account.
It never logs the keys. A missing Secret or key shows on each CloudAccount's
`Ready` condition, and nothing is applied until it is fixed. You create only
the source Secret; the copies are the platform's.

If the account's own policies forbid assuming roles (for example an AWS
Organizations service control policy that denies `sts:AssumeRole` in the
regions you use, as some free or sandbox accounts have), add
`skip-tenant-role: true`. The ProviderConfigs then have no
`assumeRoleChain`, and the controller's own lookups use its keys as they
are: every account is reached as that one identity, so the tenant role and
its permissions are not exercised. Local testing only; it is refused
without `--provider-credentials-secret`.

In a cluster without IRSA that runs the controller as a Deployment, enable
`config/local-credentials` (read on that one Secret, and write on Secrets in
the tenant namespaces for the copies) and create the Secret in the
controller's namespace.

### The access check

A bootstrapped account is not proof that the platform can work in it: an
AWS Organizations SCP can deny `sts:AssumeRole` in every region but one, a
trust policy can name the wrong principal, a region can be disabled. So the
CloudAccount controller checks each account, after its ProviderConfig is
applied, as the providers will reach it: the tenant role assumed from the
controller's own credentials (with `skip-tenant-role`, the keys as they
are), in every region of the account's networks (a cluster is in its
network's region) and in `us-east-1`. Nothing is created or changed:

| Call | How | Needed for |
|------|-----|------------|
| `sts:GetCallerIdentity` | after assuming the tenant role, in the region's STS endpoint (no permission needed); the account and the role of the answer must be the account's | every call: the providers assume the same role in the same regions |
| `ec2:DescribeAvailabilityZones` | read | network layouts and their zones |
| `ec2:DescribeVpcs` | read (one page) | network import and the providers' observation |
| `ec2:CreateVpc` | `DryRun=true`: AWS answers `DryRunOperation` when allowed, `UnauthorizedOperation` when not | creating networks |
| `eks:ListClusters` | read (one page) | EKS name checks, import and the providers' clusters |
| `iam:GetRole` | of the tenant role itself, once (IAM is global) | the IAM roles the platform creates and reads |

`iam:SimulatePrincipalPolicy` is not used: it does not see SCPs, which are
the usual cause. A region whose identity call fails runs none of its other
calls. Throttling and network errors are retried twice with backoff; a
check that still cannot finish is no verdict: the last one stays, marked
stale, and it is tried again in 15 minutes.

When: on create and on every change of the account, when its networks use a
region not checked yet, every 6 hours, and on request
(`cluster-vending account verify --id <id> --tenant <t>`, the
`VerifyCloudAccount` RPC, which only records the request; `account get`
shows the result once the controller has run it). `--region` (repeatable;
`VerifyCloudAccountRequest.regions`) adds regions no network uses yet: they
are kept on the account (the annotation
`cloud.cluster-vending.io/access-regions`) and checked with the others from then
on; a later verify without `--region` keeps them. A paused account is not
checked.

The API server acts on the result. `CreateNetwork`, `ImportNetwork`,
`TakeOverNetwork`, `CreateCluster`, `ImportCluster` and `TakeOverCluster`
(a cluster: the account of its network) refuse an account the last check
did not verify, with `FAILED_PRECONDITION`, and store nothing:

| The account | The refusal |
|-------------|-------------|
| never checked (or no verdict yet) | `account <name> (<account id>) has not been verified yet; the controller checks it shortly after it is added, or run: cluster-vending account verify --id <id>` |
| denied | `account <name> (<account id>) cannot be used: <reason>: <message>; fix the account and run: cluster-vending account verify --id <id>` |
| the region failed | `region <region> of account <name> (<account id>) cannot be used: <reason>: <message>; fix the account and run: cluster-vending account verify --id <id>` |
| the region not checked | `region <region> of account <name> (<account id>) has not been verified yet; run: cluster-vending account verify --id <id> --region <region>` |

A verdict that is stale (later checks could not finish) but verified is
used, and the server logs a warning. Reads, updates, deletes, pause and
resume do not check: what exists can always be looked at, changed and
removed. There is no switch to skip it; a local run with
`skip-tenant-role` is checked with its keys like any other. A cluster is
also created only once its network is Ready (or being updated).

The result is the account's `status.access` (when, as whom, each region
and call with its reason) and the condition `AccessVerified`: `True`,
`False` with the first failure's reason, or `Unknown` before the first
verdict. Reasons: `AssumeRoleDenied` (the trust policy, or no permission to
assume), `DeniedBySCP` (an explicit deny in a service control policy),
`NotAllowed` (the role's policies), `WrongAccount` (the role answered as
another account or role), `RegionNotEnabled`, `Unavailable` (transient),
`NoTenantRole` (none stored). An account whose access is denied is
`Failed`, and the change is one event, `AccessVerified` or `AccessDenied`
(docs/events.md). `account list` shows it in the ACCESS column, `account
get` in its Checks section.

IAM the check needs: the controller's own role may `sts:AssumeRole` on the
tenant role (as for its other lookups), and the tenant role allows
`ec2:DescribeAvailabilityZones`, `ec2:DescribeVpcs`, `ec2:CreateVpc`,
`eks:ListClusters` and `iam:GetRole` on itself, in every region it is used
in. Each is a permission the providers need anyway; an SCP must allow them
in those regions too.

Who reconciles: this controller, and nothing else. The API server only
stores the desired state; it has no background loop.

## Releases through the channels: the release soak

alpha is Pinned: it offers the release this build ships (or `spec.release`).
beta and stable are Automatic: beta takes alpha's release, stable beta's,
but only once the channel before has offered it for the channel's
**release soak** (`spec.policy.releaseSoak`; shipped: beta 168h, stable
336h; unset on a channel made before: the same 7 and 14 days). Until then
the channel keeps its release, `status.pending` names the one it takes and
when, its Offering condition says so, and a `PromotionWaiting` event is
emitted once. A channel that offers nothing yet (a new management cluster)
takes its upstream's release at once. A soak of `0s` makes a channel follow
at once.

A release a channel offers is what new clusters on it install; clusters
that have one keep it (their move to a new release is its own step).

## Rollout of a channel's Kubernetes minor

Each ReleaseChannel's release puts one Kubernetes minor on it
(`pkg/catalog/release.yaml`, `channel:` on the minor; `status.kubernetesMinor`).
The rollout controller plans how the channel's clusters with `upgradeMode:
Automatic` move to that minor, writes the plan to the channel's status and
each member's place in it to the cluster's `status.rollout`. With
`--automatic-upgrades` off (the default) that is all: `status.rollout.reportOnly`
is true and `wouldStart` lists the clusters an upgrade would start on.

The plan (`pkg/upgrade`):

- A cluster moves one minor at a time toward its channel's minor and is
  never moved down; a cluster ahead of its channel is not part of the rollout.
- Tier2 clusters go first, then Tier1, then Tier0 (an unset tier is Tier0).
  Within a tier at most `spec.policy.maxInProgress` upgrade at once (Tier0:
  one); the next tier starts once the tier before is done and
  `spec.policy.soak` (or `soakPerTier[<tier>]`) has passed since its last
  upgrade ended. More than `spec.policy.maxFailed` failures in a tier halt the
  channel (`rollout.halted`, a `RolloutHalted` event).
- Before a cluster starts, its own gates are checked: Manual mode, paused,
  frozen (`admin freeze`), not Ready (every resource, the EKS add-ons
  included), platform components not Ready, tenant components not Ready
  (`TENANT_COMPONENTS_NOT_READY`; one being removed does not count), no
  deprecation scan for the target minor or removed APIs in use, outside its
  maintenance window. A cluster blocked by its gates is listed in
  `rollout.blocked` with the first reason and does not hold the rollout back.
  The controller checks readiness again against the stored cluster right
  before it writes the new version, so a plan read from a cache that is
  behind cannot start an upgrade on a cluster that is not ready.
- `spec.paused` on the channel: the plan is reported, nothing starts.

```sh
kubectl get releasechannels          # ROLLING TIER and WAITING columns
kubectl get releasechannel stable -o jsonpath='{.status.rollout}'
```

A pass runs when the channel, its release, a ReleaseHold or what the rollout
reads of a cluster changes, when a soak ends or a blocking window opens, and
every 10 minutes.

### Automatic upgrades (`--automatic-upgrades=true`)

**Irreversible**: EKS cannot downgrade a cluster. Turn it on only when the
channels' minors and the clusters' tiers, modes and windows are what you
want.

For each cluster the plan starts, the controller reads it again and starts
the hop only if it is still Automatic, on the channel, asks for the minor the
plan saw, and is not paused or being deleted. It then sets
`spec.controlPlane.kubernetesVersion` to the next minor through the store
(the etag-guarded update an API call would make; a write that loses to
another change is skipped and planned again next pass), records the upgrade
and emits `AutomaticUpgradeStarted` on the cluster and the channel. The
cluster's own reconcile runs the upgrade as for any version change (see
"How a Kubernetes upgrade runs" below).

The record (`status.rollout` on the Cluster, `rollout` in `GetCluster`):

| `result` | Meaning |
|---|---|
| `Pending` | not started: it waits its turn, its tier, a soak, or automatic upgrades are off (`message` says which) |
| `Blocked` | kept back by its own check: manual mode, a maintenance window, the deprecation scan, health, a pause |
| `InProgress` | the rollout set the new version; counts against `maxInProgress` before the cluster reports the upgrade |
| `Succeeded` | the cluster runs the new minor and reports no upgrade; `finishTime` starts the tier's soak |
| `Failed` | the cluster failed while upgrading (`message`); counts against `maxFailed` until the cluster runs the minor |
| `TimedOut` | one step made no progress for `--upgrade-timeout`; counted as failed |

`Pending` and `Blocked` are written only when they change, and never over an
upgrade in progress or one that failed. A version a person changes
(`cluster update`) writes no record and is not counted as the rollout's. A
`Pending` or `Blocked` record whose target the cluster already runs (a person
upgraded it, or it was upgraded before the record was written) is closed as
`Succeeded`, "reached by an update, not started by the rollout", with no
event; `cluster get` never shows such a record as waiting: it says
`Rollout: up to date with channel alpha (1.37)`.

`--upgrade-timeout` is kept, as a stall timeout: it bounds one step without
progress, not the whole upgrade, so a two-hour upgrade whose steps keep
finishing is fine. The add-ons' step has its own watch (below), which fails
the record as soon as it halts.

### How a Kubernetes upgrade runs

An upgrade runs in the order a cluster is created, one step at a time, each
left only when it is settled as AWS and Helm report it, read by a pass after
the one that applied it (the cache may still hold the old state):

1. **Control plane**: EKS runs the new minor (`atProvider.version`).
2. **Node groups**: each runs the new minor, Ready, provider caught up. Until
   then the node groups render at the version the control plane reports.
   Once the control plane is done the Karpenter EC2NodeClasses' AMI pin
   moves to the target minor's (`nodeAmiPin` of the release); Karpenter
   drift replaces the nodes. The step is settled when every NodeClaim has a
   node and every Karpenter node runs the target kubelet minor (read through
   the workload kubeconfig; a read that fails or is cut short keeps the step
   where it is). The timeout and observation rules are the step's own.
3. **Add-ons**: the add-ons take the versions EKS recommends for the new
   minor (always read from AWS, `DescribeAddonVersions`, never from
   `release.yaml`; a pinned add-on keeps its pin). Until this step they keep
   the versions they run. Skipped in effect when no version changes.
4. **Platform components**: they switch to the new minor's pins of the
   cluster's release (when a version or values change).
5. **Tenant components**: the same, for the tenant's components.

`status.upgrade.phase` is the step (`ControlPlane`, `NodeGroups`, `Addons`,
`PlatformComponents`, `TenantComponents`); `phaseStartTime` when it started.
`cluster get` shows every step in order:

```
Upgrade:       1.36 → 1.37: control plane ✓, node groups ✓, add-ons (coredns, vpc-cni) …, platform components, tenant components
```

When the last step settles the upgrade is cleared at once (`UpgradeFinished`),
the rollout record turns `Succeeded`, and an upgrading cluster is looked at
every 30 seconds, so the rollout plans the next hop right away
(1.35 → 1.36 → 1.37). A release move and a Kubernetes upgrade never run at
the same time: an upgrade does not start while a move is under way, and a
move waits for the upgrade.

**Add-on versions between upgrades:** once a day (and once after the
controller starts) a Running cluster with no upgrade or release move under
way asks AWS for the versions EKS recommends for its minor. An add-on not
pinned by the tenant whose recommended version changed moves to it, applied
by the same pass (`AddonUpgraded` event). A check AWS does not answer is
logged and tried again in an hour. `pkg/catalog/release.yaml` holds no
add-on versions.

**The add-ons' watch:** add-ons that fail (the provider's error, or AWS's
`CREATE_FAILED`, `UPDATE_FAILED`, `DEGRADED`) or do not settle within 30
minutes are recovered: applied again now (the provider updates them with
`resolveConflicts: OVERWRITE`), the watch starting over, up to 3 times
(`AddonRecovering` events). Still not settled, the upgrade halts
(`status.upgrade.halted`, `UpgradeHalted`, the AWS reason in the message) and
the rollout record is `Failed`; `cluster retry --id <cluster>` tries again.

To follow it:

```sh
cluster-vending cluster get --id <cluster>          # Rollout: 1.36 → 1.37 via channel alpha (tier 2): in progress since 12m
cluster-vending cluster events --id <cluster>       # what happened, newest first
cluster-vending admin channel get --id alpha        # the channel's rollout plan, tier by tier (-o wide: every cluster)
```

### Helm operations

Every Helm operation on a workload cluster goes through provider-helm and is
managed by the controller around it ([design/helm-reliability.md](design/helm-reliability.md)):

- Releases are rendered with `wait: true`, the definition's
  `install.timeout` and `maxHistory: 5`, and carry a digest of chart,
  version and values (`cloud.cluster-vending.io/helm-digest`); one whose
  digest is the live one is not written. A Kubernetes upgrade where no
  component changes reports the component steps as skipped
  (`status.upgrade.skipped_steps`).
- At most one Release of a cluster installs or upgrades at a time, in
  `sequence`, then `requires` order. The controller lists a cluster's own
  Releases once per pass (label selector); its work per pass is the
  cluster's releases, never the fleet's.
- A failed attempt pauses its Release and lifts the pause after an
  exponential backoff (`--helm-backoff-max`); transient errors are retried
  quietly and never counted; after `--helm-max-failures` counted failures the
  component is Failed (`HelmReleaseFailed`). A cluster is looked at again when
  a platform component's backoff ends.
- A stuck release is rolled back or uninstalled with the Helm SDK through the
  workload kubeconfig (`--recover-stuck-releases`), each time an event and
  counted.

### Component upgrades

Component upgrades are automatic; there is no setting. A cluster with
`upgradeMode: Automatic` moves its platform components to the release its
channel offers:

- **One component at a time, in install order** (requirements first). A
  component takes the new release's version only when every changed
  component before it runs its own and is Ready, as read by a pass after the
  one that applied it. Components whose version and values do not change are
  not a step. A component only in the new release is installed at its turn;
  one only in the old release is removed when the move is done. Tenant
  components take the new release when the move is done.
- **When** (the channel's rollout of its release, `status.releaseMoves` on the
  ReleaseChannel): tier 2 first, at most `maxInProgress` moving at once per
  tier (tier 0: one), `soak` between tiers, halted past `maxFailed`. A cluster
  starts only when it is Running, its components are Ready, it is not paused
  (nor its upgrades), inside its maintenance window, and no Kubernetes upgrade
  of it is running; one kept back does not hold its tier back. A started move
  finishes on the release it started for, even if the channel moves on.
- **Watch, recover, halt:** the component being moved has 30 minutes to run
  its new version and be Ready. One that fails (FAILED at its new version) or
  does not settle in that time is recovered: its chart is tried again now (a
  stuck Helm revision removed, provider-helm told to act), the 30 minutes
  starting over, up to 3 times (`ReleaseMoveRecovering` events; a failure
  after a recovery counts only after 5 minutes). Still failing, the move
  halts, saying why. Nothing moves until `cluster retry --id <cluster>`;
  there is no rollback. A move whose target
  release is gone or not valid any more installs nothing until it is fixed,
  so no moved component is taken back.
- **Manual clusters** do not move: `cluster get` says the new release is
  available. Set `upgradeMode: Automatic` (`cluster update -f`) to move one.
- **Status:** `status.release_move` (`releaseMove` on the kind), events
  `ReleaseMoveStarted`, `ComponentUpgraded`, `ReleaseMoveRecovering`,
  `ReleaseMoveHalted`, `ReleaseMoveFinished`. `cluster get` shows
  `Release: shipped-a → shipped-b: karpenter (3 of 9), since 4m`.

Not done yet: a chart whose CRDs are in its own `crds/` directory (Helm does
not upgrade them) moves with its old CRDs; a chart with a separate CRD chart
(`install.helm.crds`) gets both in the same pass, the CRD chart first.

### Admin: releases, channels and rollouts

The same views through the API (AdminService, platform admin role only),
without access to the management cluster:

```sh
cluster-vending admin release list                    # NAME SHIPPED VALID CHANNELS PINNED AGE (PINNED: holds that keep a cluster on it)
cluster-vending admin release get --id shipped-1aed928c [-o wide]   # each minor: channel, upgrade target, end of support, ending; its components (wide: locked values)
cluster-vending admin channel list [-o wide]          # NAME PROMOTION RELEASE KUBERNETES ROLLING TIER STATE WAITING PAUSED AGE (wide: TIER2 TIER1 TIER0 progress, policy)
cluster-vending admin channel get --id stable [-o wide]   # policy, release soak, next release, the rollout plan, release moves, the releases it offered, conditions
```

`channel list` shows each channel's rollout as the controller last wrote
it to the channel's status; a rollout with nothing left to do is at
tier `none (done)` (`none (nothing to upgrade)` when no cluster needed
one). `channel get` plans the channel's rollout again at the time of the
call with the controller's rules (`pkg/rollout`), and shows its plan per
tier and batch, then its release moves (`Release Moves:` the release its
clusters' components move to, how far, why the rest waits, and the
clusters kept back with their gate):

```text
Rollout:          Rolling tier 1, batch 2 of 3
Release Moves:    2026-11-a: Rolling, 3/7 done; waiting: tier 1 soaks; 1 kept back

Plan:
  TIER 2  done 2/2, failed 0/1, finished 2026-11-12 11:00 UTC, soak ended 2026-11-13 11:00 UTC
    batch 1  ✓ acme/dev  ✓ acme/qa
  TIER 1  rolling: done 2/7, failed 0/1, 2 at a time
    batch 1  ✓ acme/a  ✓ acme/b
    batch 2  ▶ acme/payments (control plane, 12m)  · acme/search
    batch 3  · globex/api  · globex/core
    waits    ⏸ acme/batch: the maintenance window opens Sat 02:00 UTC
  TIER 0  not started: after tier 1 and its 48h soak, one at a time
    batch 1  · acme/prod
    batch 2  · globex/prod
```

- Tiers go 2, 1, 0; a tier's batches hold at most `maxInProgress` clusters
  (tier 0: one). `failed n/m` is the tier's failures against `maxFailed`:
  one more halts the channel (`halted: …`).
- ✓ succeeded, ✗ failed, ▶ upgrading (the step and for how long),
  · queued. The batch with ▶ or the first · is the one starting: its queued
  clusters are exactly those the controller starts next (Would Start).
- Finished batches are laid out by when each cluster finished: a
  reconstruction, not a record of how they ran.
- `waits` are clusters blocked by their own checks (maintenance window,
  manual upgrades, deprecations, health, a pause): they are in no batch and
  do not hold the tier back; one joins the batch being filled once its
  check passes.

`-o wide` adds the table of every cluster: tier, tenant, the version it
runs, the minor it moves to next, its state, since when, why it is blocked,
its ID and the gate code. All of them are read-only and support
`-o yaml|json` (the plan is the `plan` field).

### Network tiers and cluster channels

A Network owns `spec.tier` (`Tier0`, `Tier1`, `Tier2`; default `Tier0`).
All clusters on that network inherit the same tier, reported in their
`status.tier`. Clusters retain their own `spec.releaseChannel`: two clusters
sharing a network may follow Alpha and Stable while sharing Tier0.
The rollout planner reads the network tier directly, including before
starting an upgrade, so a stale cluster status cannot choose its tier.
Network tier edits replan the rollout and refresh cluster status. Kubernetes
upgrades, release moves and code changes recheck the live parent tier before
starting. An orphaned cluster blocks its own channel, not unrelated channels.
Networks without channel-bound clusters use their own tier and wait for all
configured platform code rollouts to reach it; missing rollout plans hold them.

For API manifests use `spec.tier: TIER_0` on a Network. Cluster `spec.tier`
is deprecated and rejected on create/update; move it to the network.
Existing networks without a tier conservatively inherit Tier0. If previous
cluster tiers differed, choose the network tier explicitly before rollout.

### Admin: scoped freeze

A freeze request first reports `Freezing`. The controller drains active
reconciliations, pauses held managed resources and records its acknowledgement
of that request in the Kubernetes ReleaseHold status. Only then does the
status become `Frozen`. Use `--wait` before a risky deployment; a successful
request alone is not confirmation. If the controller is stopped, confirmation
waits until it returns. A freeze changed during confirmation is checked again.
Confirmation assumes one elected controller manager (or one local manager).
It does not cancel AWS operations the providers already submitted.

Freeze can target the platform, a tenant, a network or one cluster. A tenant
scope covers its accounts, networks and clusters; a network scope covers the
network and every cluster on it. A cluster scope covers only that cluster.

A freeze stops cloud account, network and cluster actions in its scope,
including upgrades, repairs, import discovery and deletion. The controller
still records status. Platform-managed AWS resources and Helm Releases get
`crossplane.io/paused: "true"`, so Crossplane also stops reconciling them.
Existing workloads keep running; AWS operations already submitted cannot
be cancelled by freezing.

```sh
cluster-vending admin freeze --platform --reason "controller rollout" --wait
cluster-vending admin freeze status
cluster-vending admin freeze --reset-exceptions --reason "refreeze canaries" --wait
cluster-vending admin freeze --reason "maintenance" --until 2026-12-31
cluster-vending admin freeze --network <id> --reason "network maintenance" --wait
cluster-vending admin freeze --cluster <id> --reason "cluster maintenance" --wait
cluster-vending admin freeze --tenant acme --reason "tenant maintenance" --wait
cluster-vending admin unfreeze --cluster <id> --wait # shared network stays frozen
cluster-vending admin unfreeze --tenant acme # includes its networks and clusters
cluster-vending admin unfreeze --network <id> # includes its clusters
cluster-vending admin unfreeze --platform    # the whole platform
```

`admin freeze status` lists phase, reason, expiry, exceptions and provider
pause totals. Repeating freeze preserves exceptions; `--reset-exceptions`
clears them without unfreezing the platform. Unfreeze is asynchronous too:
status remains `Unfreezing` while freeze-owned provider pauses remain.
Use `admin unfreeze --cluster/--network/--tenant <id> --wait` to wait for
that scope independently of other freezes. `admin freeze status` accepts
the same scope flags. Confirmation is bound to the requested policy revision;
a concurrent freeze change aborts the wait rather than confirming another request.

The latest matching scoped action wins. A tenant-wide unfreeze clears
nested network and cluster freezes in that tenant; network-wide unfreeze
clears nested cluster freezes. Platform unfreeze clears all scopes. A later
specific freeze can hold one resource again. There are no channel/tier
freeze flags and no separate pause/resume commands.

A cluster exception does not unpause its shared network. Required network
tag changes or tag cleanup wait for network unfreeze. Automatic changes
still follow their channel, inherited tier, health gates and maintenance
window after being let through.

Creates, updates and deletes are accepted and stored as Kubernetes custom
resources during a freeze. Their cloud changes wait until unfreeze.
`--until` ends the freeze automatically while the controller is running.
Crossplane annotations stay in force when the controller is stopped, so
expiry waits for it to return.

`cluster get` and `network get` show `Frozen:`, including exceptions;
`admin channel get` shows the freeze above the channel. Unfreeze removes
freeze-owned pause annotations; resources paused manually beforehand stay
paused. The RPCs share a `FreezeTarget` (one of platform, tenant, network, cluster)
and a `FreezeStatus` with typed phase, echoed target, revision and progress.
CLI JSON/YAML output uses this same status shape for freeze, unfreeze and status.
Legacy flat RPC fields and target-less platform commands remain supported;
new automation should use explicit targets. Scoped freeze waits and counts
cover the selected scope independently of unrelated provider pause failures.

State is `ReleaseHold/platform-freeze` in the management cluster,
with no database.

### Freeze and code changes

A new controller build may render a cluster's or a network's objects
differently (a field, a tag, a new object). That is a **code change**, and
it is not applied at once: it rolls out like a Kubernetes upgrade.

- **Seen by fingerprint.** Every full apply records a short hash per object
  (`sequence.Fingerprints`) with the row. A pass whose render differs from
  what was recorded (or whose renderer version is behind the target) has a
  code change pending: `N objects change (a added, r removed)`. Objects
  made from live values (a Build item) count by identity only. A row
  applied by a build without fingerprints takes the first render as
  applied.
- **Rolled out in turn.** Each channel plans its code rollout
  (`status.codeChanges`) with the Kubernetes rollout's rules: tier 2, then
  1, then 0, `maxInProgress` at once, the soak between tiers, halted past
  `maxFailed`, maintenance windows, health, pauses and freezes. Manual
  clusters are members too: the change is the platform's, not a version.
  A cluster on no channel goes when nothing freezes it.
- **Networks first.** A network uses its own tier, shared by all its
  clusters, and goes when the code rollout of each
  of its clusters' channels reaches that tier; its clusters of that tier
  wait for it. Earlier tiers go before it.
- **Applied, recovered, halted.** An admitted change is applied
  (`CodeChangeStarted`); once what is applied is what the build renders it
  is done (`CodeChangeFinished`). A failed apply is tried again 3 times,
  then it halts (`CodeChangeHalted`) until a newer build renders it
  otherwise or an update applies it (a `cluster retry` of other failures
  tries it again too).
- **Frozen.** `admin freeze` (platform, tenant, channel or tier) keeps every
  code change from starting, like Kubernetes upgrades, release moves and the
  daily add-on upgrade; `admin unfreeze --cluster <id>` or `--network <id>`
  lets one through (a cluster let through takes its network along).
- **Where to see it.** `cluster get` has a `Code Change:` line,
  `network get` a `Rollout:` line, and `admin channel get` a `Code Changes:`
  line with the clusters it keeps back and why.
- **Safety net.** Stateful objects (VPC, subnets, NAT and internet
  gateways, EIPs, route tables, the EKS cluster and node groups, KMS keys,
  IAM roles and policies, the OIDC provider, log groups) are managed
  without Delete while their owner exists. Only the owner's own deletion,
  or an update that drops an object, switches it to full management first,
  so the cloud resource goes with it. An object a code change drops, or one
  deleted by hand, leaves its cloud resource in place.

## What a controller does

- **Event to reconciler.** The object's labels (`cloud.cluster-vending.io/tenant`,
  `/id`) name the tenant and the row; the controller calls the reconciler's
  `Reconcile(tenant, id)`. It acts on changes of the spec, labels and
  annotations (the store keeps a row's state there) and on a deletion, not
  on status changes, so its own writes do not wake it.
- **Created with kubectl.** A Network or Cluster without those labels is
  adopted: it gets the row the API's create would have stored, after the
  same checks. A Network keeps its spec as written (the layout is resolved
  again whenever it is rendered); a Cluster gets its resolved spec written
  back (zones, default add-ons, tier). What the API would have refused is
  the condition `Adopted=False` with the reason. Imports (`spec.importFrom`)
  and CloudAccounts are not adopted: they are made through the API, and say
  so in the same condition.
- **Spec edits.** When `metadata.generation` is ahead of
  `status.observedGeneration` the row's generation is moved on and the
  reconciler applies the edit. The edit is applied as written: only the
  kinds' own validation rules check it, not the API's update checks.
- **Karpenter node groups.** After the platform components, and only while
  the Karpenter component is READY, the cluster pass server-side applies
  (field manager `cluster-vending`, forced) the EC2NodeClass and NodePool
  `platform` and one pair per `spec.node_groups` into the workload cluster
  (`Deps.NodeGroupClients`, the kubeconfig the teardown uses), labelled
  `cloud.cluster-vending.io/managed-by=platform`. Every pass puts them back;
  a group removed from the spec is deleted (only labelled objects, pool
  before class). The NodePools' `Ready` condition and node counts go into
  `status.node_groups` (platform first). Reads and applies are bounded (10s
  per call, 45s per pass, lists of at most 500) and never fail the pass: an
  error is the group's message, the last good phase stays, missing Karpenter
  CRDs are PENDING. The other platform components wait for the platform node
  group to be READY (`sequence`, docs/catalog.md).
- **Requeue.** 5s while a generation is not applied in full, 20s while the
  cloud works on it, 1m for an import that is only observed, 30s after a
  failed pass (and while a cluster's recorded object is missing), and a 10m
  resync when settled. A change of a provider object wakes its owner at
  once (by the `cluster-vending.io/network` or `/cluster` label); a cluster
  is also woken by its Helm ProviderConfig and Releases.
- **Repair.** An object of a network or cluster that someone deleted from
  the management cluster is created again by the next pass, although the
  generation is applied (`Deps.Repairable`, logged as "cluster repaired:
  recreated <kind> <name>"). Only kinds that cannot become a second cloud
  resource are created again (`crossplaneaws.Recreatable`): objects that
  are no cloud resource (the cluster's Helm ProviderConfig, Usages, the
  ClusterAuth) and resources AWS identifies by a name the platform chose
  (IAM roles, policies and attachments, log groups, key aliases, node
  groups, add-ons, access entries, the OIDC identity provider
  configuration), where the provider takes a survivor over or AWS refuses
  the duplicate. Deleting such an object deletes its cloud resource first,
  so a node group created again is a new node group.

  Every other kind (the encryption key, VPCs, subnets, gateways, addresses,
  route tables, network ACLs, flow logs, security groups and rules, launch
  templates, pod identity associations, the IAM OIDC provider) has an ID
  AWS assigns, so a new object would be a second resource. Such an object
  is created again only with its **recorded cloud identity**: the external
  name the platform stored when it last saw the object
  (`ManagedResource.ExternalName`). It is created with that name in its
  first write (`sequence.Item.ExternalName`), before anything else is
  applied to it, so its provider takes the existing resource over. One
  whose identity was never recorded (deleted before its provider created
  the resource) is not created again, and while it is missing nothing is
  repaired and the owner reports it. The EKS cluster is never created again.

  Imported networks and clusters are repaired too: the object comes back
  from what the import discovered, with its cloud identity and the same
  read-only `Observe` policies. Taken-over ones are repaired from the set
  their import discovered, like any other. Not repaired: blocked imports,
  anything being deleted, a node group being retired, and an owner with a
  paused object.
- **Deletion.** With dependents (a network of an account, a cluster of a
  network) nothing is deleted and the condition `DeletionBlocked=True` names
  them. Without, the row is marked DELETING and the reconciler deletes the
  cloud resources pass by pass; when it removes the row, the store drops the
  finalizer and the object goes. A CloudAccount loses its ProviderConfig
  (the tenant namespace stays) and then its finalizer.
- **Cluster deletion runs in the reverse of its creation**, in seven stages
  (`driveDelete`, `delete_stage.go`); a stage starts only when the one before
  it is fully gone, and the cluster's message names the current stage and
  what it waits for, e.g. `deleting; stage 3 of 7, platform components:
  uninstalling sequence 100; base waits for the components that require it;
  mesh: waiting for Release prod-1a2b3c4d-mesh to be uninstalled (Helm state
  uninstalling); next sequence 10`.
  1. **Load balancers** in the workload cluster, while the AWS Load Balancer
     Controller still runs: the ALB Ingresses (the `alb` class, any
     IngressClass of `ingress.k8s.aws/alb`, the old
     `kubernetes.io/ingress.class: alb` annotation) and every Service of type
     LoadBalancer. A load balancer left behind keeps the network's VPC from
     being deleted (AWS: `DependencyViolation`). `status.teardown` records
     this stage.
  2. **Tenant components**, a component after those that require it.
  3. **Platform components above sequence 0**, highest sequence first, and
     within a sequence a component after those that require it; each is
     fully uninstalled (its Helm Releases gone, then its IAM objects gone)
     before the next lower sequence starts.
  4. **Node groups**: Karpenter's user NodePools, then the platform NodePool,
     then the NodeClaims left (Karpenter terminates the nodes), then the
     EC2NodeClasses (Karpenter removes their instance profiles). Reads are
     one page of 500 per kind and bounded by a deadline per pass.
  5. **Karpenter** (sequence 0): its chart and IAM role, then its
     interruption queue, queue policy, EventBridge rules and targets, and the
     `karpenter-node` role with its policy attachments and access entry. The
     Usages that keep the node role until the EKS cluster is gone are
     released: its nodes are gone.
  6. **EKS add-ons** with their pod identity associations, roles and
     policies.
  7. **The rest**, ordered by the Usages as before: the default managed node
     group, the EKS cluster, the cluster's IAM, the results bucket (emptied
     first, every pass until the Bucket is gone), then the unrecorded
     leftovers, the subnet tags and the records.
  - **Helm reliability never blocks an uninstall.** Deletion lifts the
    platform's own backoff pause (and the give-up mark) on a Release it
    uninstalls; the one-operation gate only governs writes of a Release's
    spec and plays no part. A Release paused by anyone else (an operator, a
    freeze) is not touched: the message says `Release X is paused
    (crossplane.io/paused) by an operator or a freeze, so it is not
    uninstalled; unpause it to go on`, and the stage waits. A Release stuck
    in `uninstalling` or a `pending-*` state for its timeout plus two
    minutes is recovered like a stuck install (`--recover-stuck-releases`,
    counted in `recoveries`).
  - **Timeout:** the load balancer stage ends after `--teardown-timeout`
    (default 15m) from its start, the node group stage after the same time
    from the request to delete (or from the first time the API could not be
    reached; a restart counts again), each with a `TeardownTimedOut` warning
    listing what is left; the deletion goes on.
  - **Skipped**, with a `TeardownSkipped` warning, when the deletion is
    forced (`cluster delete --force`) or the cluster's API cannot be
    reached (its EKS cluster is not ACTIVE, no kubeconfig): the load
    balancers and nodes may then be left, and the network's deletion waits on
    them. The Helm Releases are then let go without an uninstall and the
    other stages still run in order.
  - **Not deleted: PersistentVolumeClaims.** Their EBS volumes block
    nothing (they are not in the VPC), and deleting a claim deletes data;
    volumes of a `Delete` reclaim policy are left as an EKS deletion leaves
    them.
  - A network whose VPC AWS refuses to delete for dependencies says so in
    its message, with the VPC to look in: delete what is left there (a
    load balancer, its network interfaces and security groups) and the
    deletion finishes by itself.

The scaffolded manifests under `config/` were adjusted to start
`/cluster-vending controller`, but there is no image build for them yet, and
`config/default` still assumes kubebuilder's authenticated HTTPS metrics,
which this manager does not serve (its metrics are plain HTTP).
