# Controller

The platform's controller, laid out as a kubebuilder v4 project at the root
of the repository's one Go module. It holds the API of the platform's own custom
resources and the controllers that reconcile them. The controllers have no
reconcile logic of their own: each turns a watch event into a call of a
reconciler (`pkg/reconcile/account`, `network`, `cluster`), on the store
that keeps its rows as these objects
(`pkg/database/cloudstore`). None of it has run against a real API server
or AWS; it is tested on a fake API server only.

## API

Group `cloud.cluster-vending.io`, version `v1alpha1`. Every kind is
namespaced; the tenant is the namespace.

| Kind | Short name | Owned by | Owns |
|---|---|---|---|
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
the only store): a tenant is its namespace, and every account, network and
cluster the API knows is an object here.

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
| `--recover-stuck-releases` | default `true`: recover a platform component whose Helm release is stuck (uninstalling or pending for 10 minutes) by removing the Helm record of the stuck revision in the workload cluster, at most 3 times per component version ([catalog.md](catalog.md), "When a component fails"); `false` leaves it failed for a person |
| `--recommendations` | default `true`: every five minutes, sample each running cluster for resource recommendations (read-only: workloads, metrics-server usage, the cluster's own Vertical Pod Autoscalers) and record a week of usage in the tenant namespace ([recommendations.md](recommendations.md)); `false` records nothing |
| `--reconcile-workers` | default `8`: how many cloud accounts, networks and clusters of each kind are reconciled at once; one object is never reconciled twice at the same time. The release, catalog, deprecation and recommendation controllers keep one worker |
| `--automatic-upgrades` | default `false`: the rollout of each channel's Kubernetes minor is planned and reported on the ReleaseChannel and each cluster (`status.rollout`) only; no cluster is upgraded. `true` upgrades clusters with `upgradeMode: Automatic`, tier by tier; **irreversible** (EKS cannot downgrade). See "Rollout of a channel's Kubernetes minor" |
| `--upgrade-timeout` | default `90m`: how long one step of an automatic upgrade (control plane, node groups, add-ons, platform components, tenant components) may go without progress before the upgrade counts as failed (`TimedOut`, against the tier's `maxFailed`). A long upgrade that keeps moving is not timed out |
| `--teardown-timeout` | default `15m`: how long a cluster's deletion waits for the load balancers and Karpenter nodes its controllers made to be deleted before it goes on (see Deletion) |
| `--provider-credentials-secret` | `<namespace>/<name>[:<key>]`: the providers authenticate with this Secret instead of IRSA (local kind only) |
| `--skip-tenant-role` | local testing only, with `--provider-credentials-secret`: act with the Secret's keys directly instead of assuming each account's tenant role |

It installs the CRDs of the three kinds and the Crossplane AWS providers
(unless `--install-providers=false`) and waits for their kinds. The API
server installs nothing; it waits for the kinds the controller installs, so
start the controller first.

How the providers run is the platform's, sized for ~100 clusters of ~80
managed resources each: a drift check every 10 minutes (30 for
provider-aws-ec2), at most 10 reconciles per second, 1 CPU and 2Gi memory
requested, 4 CPU and 4Gi limits, and 2 replicas with leader election (one
reconciles, the other is a hot standby) (`crossplaneaws.DefaultProviderRuntime`).
The AWS providers run with the `DeploymentRuntimeConfig`s
`cluster-vending-aws` and `cluster-vending-aws-ec2`, provider-helm with
`cluster-vending-helm`. There is no setting for these. To run the providers
otherwise, set `--install-providers=false` and manage the `Provider` and
`DeploymentRuntimeConfig` objects yourself.

The controller creates these objects only when they are missing: a
management cluster installed by an older build keeps its runtime configs
(and provider-helm keeps running without one) until they are deleted, so
the controller recreates them on its next start, or edited by hand.

The controller always brings every resource to this build's renderer. The
former `--render-target` and `--render-rollout` settings are gone; release
objects take their place (docs/design/rollouts.md).

## AWS access

The controller is the only command that looks at AWS: it discovers the VPC
or EKS cluster of an import, checks a new cluster's name against EKS,
resolves the Kubernetes and add-on versions EKS offers, and checks that it
can reach and act in each account (the access check, below). The API server makes
no AWS call and has no AWS credentials; its one AWS setting,
`aws-management-role-arn`, is only handed to the CLI creating the role
(`account create`, [onboarding.md](onboarding.md)).

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
   the Kubernetes store, no AWS setting).

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
  its upgrades paused (`admin pause --only upgrades`), not Ready, platform components not Ready, no
  deprecation scan for the target minor or removed APIs in use, outside its
  maintenance window. A cluster blocked by its gates is listed in
  `rollout.blocked` with the first reason and does not hold the rollout back.
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
cluster-vending admin rollout get --channel alpha   # every cluster of the channel, tier by tier
```

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
cluster-vending admin release list                    # NAME SHIPPED VALID CHANNELS PINNED AGE (PINNED: pauses of upgrades that keep a cluster on it)
cluster-vending admin release get --id shipped-1aed928c [-o wide]   # each minor: channel, upgrade target, end of support, ending; its components (wide: locked values)
cluster-vending admin channel list [-o wide]          # NAME PROMOTION RELEASE KUBERNETES ROLLING TIER STATE WAITING PAUSED AGE (wide: TIER2 TIER1 TIER0 progress, policy)
cluster-vending admin channel get --id stable         # policy, release soak, next release, rollout, the releases it offered, conditions
cluster-vending admin rollout get --channel stable [--tier 1] [-o wide]
```

`channel list` shows each channel's rollout as the controller last wrote
it to the channel's status; a rollout with nothing left to do is at
tier `none (done)` (`none (nothing to upgrade)` when no cluster needed
one). `rollout get` plans the channel's rollout again at the time of the
call with the controller's rules (`pkg/rollout`), and shows its plan per
tier and batch:

```text
State:          Rolling tier 1, batch 2 of 3

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

### Admin: pause and resume

One command pauses, one resumes. The target is a tenant (`--tenant`), one of
its networks or clusters (`--tenant` with `--network` or `--cluster`), or the
clusters of a release channel, of every tenant (`--channel`, with `--tier`
for one tier). What stops is everything, or with `--only upgrades` the
automatic upgrades only:

| | `admin pause …` | `admin pause … --only upgrades` |
|---|---|---|
| Stops | everything: the platform and the Crossplane providers do not act on it | only automatic upgrades: the rollouts skip the clusters |
| The clusters | keep running in AWS; nothing is created, changed, repaired or deleted; what is asked for meanwhile is done after the resume | are reconciled, repaired and updated as usual |
| Use it for | a provider upgrade, an incident, a fix by hand | a freeze ("not this quarter"), a team testing on the version it runs |
| Networks | `--network` or `--tenant` | not for a network: it has no upgrades |
| By channel or tier | pauses the clusters on it now; one that joins later is not paused | covers the clusters that join later too |

```sh
cluster-vending admin pause --tenant acme --reason "provider-aws upgrade"
cluster-vending admin pause --channel beta --tier 0 --only upgrades --reason "quarter-end freeze" --until 2026-12-31
cluster-vending admin pause --tenant acme --cluster <id> --only upgrades --reason "app team testing 1.36"
cluster-vending admin pause list        # TARGET WHAT BY SINCE UNTIL CLUSTERS REASON
cluster-vending admin resume --channel beta --tier 0 --only upgrades
```

- `--until` (RFC 3339, or a date: its start in UTC) ends a pause by itself:
  the controller lifts it then and the providers act again. Without it a
  pause lasts until `admin resume`.
- Pauses add up: a resume removes the pause it names (same target, same
  `--only`) and nothing else. A cluster paused on its own stays paused when
  its channel's tier or its tenant is resumed; a tenant's resume leaves a
  network or cluster paused on its own.
- `cluster get` shows each pause on its `Paused:` line: `everything: …` or
  `upgrades only: …`, with why, the scope it was asked at, who, since and
  until when.

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
- **Cluster teardown, first.** A cluster's controllers make cloud
  resources the platform never recorded: the AWS Load Balancer Controller
  makes a load balancer (and its security groups and network interfaces)
  for every Service of type LoadBalancer and ALB Ingress, Karpenter makes
  EC2 instances, launch templates and instance profiles. Uninstalling those
  controllers first would leave them in the account, and a left load
  balancer keeps the network's VPC from being deleted (AWS:
  `DependencyViolation`). So a cluster's deletion starts, before any
  component is uninstalled, by deleting in the workload cluster (with its
  kubeconfig): the ALB Ingresses (the `alb` class, any IngressClass of
  `ingress.k8s.aws/alb`, the old `kubernetes.io/ingress.class: alb`
  annotation) and every Service of type LoadBalancer; once they are gone,
  Karpenter's NodePools and NodeClaims; once those are gone, its
  EC2NodeClasses. Each stage waits until the controllers have removed what
  they made (the objects are gone). Meanwhile the cluster's message says
  what it waits for, e.g. `deleting; deleting 1 load balancer (Service
  istio-system/istio-ingressgateway) before the platform components are
  uninstalled`, and `status.teardown` records it.
  - **Timeout:** after `--teardown-timeout` (default 15m) the deletion goes
    on, with a `TeardownTimedOut` warning listing what is left.
  - **Skipped**, with a `TeardownSkipped` warning, when the deletion is
    forced (`cluster delete --force`) or the cluster's API cannot be
    reached (its EKS cluster is not ACTIVE): the load balancers and nodes
    may then be left, and the network's deletion waits on them.
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
