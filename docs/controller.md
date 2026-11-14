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
| `--automatic-upgrades` | default `false`: the rollout of each channel's Kubernetes minor is planned and reported on the ReleaseChannel only; no cluster is upgraded (see "Rollout of a channel's Kubernetes minor"). The upgrade step is not built yet |
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
no AWS call and has no AWS setting or credentials.

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
  the role's ARN).

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

## Rollout of a channel's Kubernetes minor

Each ReleaseChannel's release puts one Kubernetes minor on it
(`pkg/catalog/release.yaml`, `channel:` on the minor; `status.kubernetesMinor`).
The rollout controller plans how the channel's clusters with `upgradeMode:
Automatic` move to that minor and writes the plan to the channel's status.
**It does not upgrade any cluster yet**: with `--automatic-upgrades` off (the
default, and for now the only behaviour) `status.rollout.reportOnly` is true
and `wouldStart` lists the clusters an upgrade would start on.

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
  held by a ReleaseHold, not Ready, platform components not Ready, no
  deprecation scan for the target minor or removed APIs in use, outside its
  maintenance window. A cluster blocked by its gates is listed in
  `rollout.blocked` with the first reason and does not hold the rollout back.
- `spec.paused` on the channel: the plan is reported, nothing starts.

```sh
kubectl get releasechannels          # STAGE and WAITING columns
kubectl get releasechannel stable -o jsonpath='{.status.rollout}'
```

A pass runs when the channel, its release, a ReleaseHold or what the rollout
reads of a cluster changes, when a soak ends or a blocking window opens, and
every 10 minutes.

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

The scaffolded manifests under `config/` were adjusted to start
`/cluster-vending controller`, but there is no image build for them yet, and
`config/default` still assumes kubebuilder's authenticated HTTPS metrics,
which this manager does not serve (its metrics are plain HTTP).
