# Architecture

This document describes the code as it is on `main`, then, in a separate
section, what is decided but not built. The two are kept apart on purpose.

## Layers

Each layer has one job and a rule about what it may import. The rules in the
last column were checked with `go list -deps`.

| Package | Job | Import rule |
|---|---|---|
| `idl/cloud/v1` | The contract: the `cloud.v1` protos of the API. Generated Go is in `pkg/gen`. | Depends on nothing in the repository. |
| `pkg/crossplane/aws` | Renders a plan into the exact provider objects (the upstream Go types of provider-upjet-aws and Crossplane), as plain lists (`NetworkResources`, `ClusterResources`) and as ordered sets (`NetworkSet`, `ClusterSet`). | No RPC library, no storage. Uses `pkg/crossplane/sequence`. |
| `pkg/crossplane/sequence` | The engine. `Apply` makes the management cluster match a set: it applies what is ready, holds back what waits, builds late values, and removes what the set dropped. `Delete` deletes an owner's objects tier by tier. | Knows nothing about networks, clusters or storage. The owner's objects are given to it as an `Inventory`. |
| `pkg/crossplane/managed` | Reads an object's state from its conditions, deletes objects, releases Usages whose user is gone. | Kubernetes only. |
| `pkg/network`, `pkg/cluster` | Logic that needs neither the API nor storage: defaults, layout, render plan, import planning, update changes. | No RPC library, no `pkg/database`, no `pkg/server`. |
| `pkg/fault` | An error with a kind and a message for the caller, for code that is not an RPC handler. | Imports nothing from the repository. |
| `pkg/reconcile` | What reconciling needs: `Deps` (store, management cluster, the AWS lookups) and the bookkeeping every owner shares (record, observe, prune, the store-backed `Inventory`), the checks a request and a reconciler share (`Validate`, `CheckAWSLabels`), and how a tenant's account is reached (`AWSAccess`: always through its tenant role). | No RPC library, no `pkg/server`. Failures a caller can act on are faults. |
| `pkg/reconcile/network`, `pkg/reconcile/cluster`, `pkg/reconcile/account` | A `Reconciler` each: moves one stored network, cluster or account towards its spec (apply, staging, delete, takeover, status). They also build the row a create or an import stores (`NewRow`, `NewImportRow`), and they are the only code that looks at AWS: the discovery of an import, the EKS name check and the version resolution. | Same as `pkg/reconcile`. |
| `pkg/management` | Prepares the management cluster for the two commands: the manager whose cache both read from, the store on the platform's kinds, and, for what reconciles only, the Crossplane providers, the CRDs and the AWS lookups (`WithAWS`). | No RPC library, no `pkg/server`. |
| `pkg/server/network`, `pkg/server/cluster`, `pkg/server/tenant` | RPC handlers only: validate, read and write rows, map errors to Connect codes. They use the reconcilers to check a request against the store and to read. They make no AWS call, and the network and cluster handlers write nothing to the management cluster (the tenant handler still applies an account's namespace and ProviderConfig itself). | May import everything above. |
| `pkg/server/service` | What handlers share: request validation, `ToConnect` (repository errors and faults to Connect codes), the `Faults` interceptor. | Imports the RPC library; nothing below the handlers does. |
| `pkg/auth` | Who may call the API: the `auth` section of the server's settings, the verification of an OIDC bearer token (go-oidc), the caller it stands for, and what each RPC needs (read or write from its `idempotency_level`, the tenant from its `tenant_id`). See [authentication.md](authentication.md). | No RPC library, no storage. `authtest` is its fake issuer for tests. |
| `pkg/server/authn` | The server interceptor that applies `pkg/auth` to requests: reads the bearer token, has it verified, refuses what the caller's roles do not allow, logs writes. Handlers read the caller from the context. | Imports the RPC library and `pkg/auth`. |
| `pkg/database` | `repository` is the storage interface, `model` its rows. `cloudstore` is the one store, on the controller's kinds in the management cluster. `storetest` is the behaviour suite it runs, and the store fixture of every test that needs one. | `repository` and `model` import nothing else from the repository. |
| `pkg/cloud/aws` | Read-only AWS lookups for imports and checks, always as the tenant role of the account they read. Called by the reconcilers only. | Imports only `pkg/constant/aws`. |
| `pkg/kube` | Client, scheme and server-side apply. | No storage, no RPC library. |
| `api/`, `pkg/controller`, `pkg/convert`, `pkg/owner`, `config/` | The kubebuilder project (`PROJECT` at the root): API types (`api/v1alpha1`), their conversion to the contract, the controllers and the manager (`pkg/controller/manager`), CRDs and deploy manifests for the controller and the server. | The types import nothing from `pkg/`; the controllers and the manager import `pkg/reconcile`, `pkg/database/cloudstore` and `pkg/management`. Nothing in them imports `pkg/server`. |
| `pkg/cli`, `cmd/` | The CLI commands and the command tree. `cmd/` is thin and has no tests. | `pkg/cli` imports only the generated client. |
| `pkg/cli/login` | The CLI's login at an OIDC issuer (device code, or the browser with PKCE), the token file, and the token source that refreshes it. | Imports `pkg/auth` for the Entra issuer URL only. |

What the table would otherwise hide:

- `pkg/kube` registers the controller's kinds (`api/v1alpha1`) in
  its scheme, so `pkg/crossplane/sequence` and everything above it depend on
  that package through `pkg/kube`.
- The server packages construct no AWS client and call none, but they still
  link the AWS SDK: `pkg/reconcile` (and `pkg/network`, `pkg/cluster`) use
  the types of `pkg/cloud/aws` (what a lookup returns, the lookup
  interfaces), and that package holds the SDK-backed implementation too.

## Who talks to AWS

Only what reconciles: the controller (`cluster-vending controller`). The API
server (`cluster-vending serve`) validates a request against the store and
the management cluster, stores the desired state, and returns. It has no AWS
setting, no AWS credentials and no IAM role.

| What needs the cloud | Where it runs | What the client sees when it fails |
|---|---|---|
| Looking up the VPC of an imported network | `pkg/reconcile/network` (`discoverImport`), once | The network is `FAILED` with the reason. A VPC that does not exist, or an ID AWS refuses, ends the import (`BLOCKED`): it is not looked up again, delete it and import again. Credentials that cannot read it, and a lookup that did not answer, keep it `DISCOVERING` and are retried. |
| Looking up the EKS cluster of an imported cluster | `pkg/reconcile/cluster` (`discoverImport`), once | The same, on the cluster. |
| Is the EKS name of a new cluster taken by a cluster made elsewhere | `pkg/reconcile/cluster` (`resolve`), before anything is created | The cluster is `FAILED`: "account ... already has an EKS cluster named ...". |
| Which Kubernetes and add-on versions EKS offers | `pkg/reconcile/cluster` (`resolve`), for a new cluster and after an update of the version or the add-ons, before anything is rendered | The cluster is `FAILED` with what EKS refused; nothing is applied. |
| Installing the Crossplane AWS providers | `pkg/management`, at the controller's start | The controller does not start. |

The controller's lookups have one way into a tenant's account
(`reconcile.Deps.AWSAccess`, `pkg/cloud/aws`): its own credentials, as the AWS
SDK's default chain finds them (IRSA in production, keys in the environment
for local testing), then optionally a source role, then always the tenant
role stored at the account's bootstrap. Nothing is read with the base
credentials themselves.

The API server has no AWS setting, credentials or role: it only stores the
desired state.

## How a create flows

### Network

1. **Handler** (`pkg/server/network`, `CreateNetwork`). It returns the
   existing network for a repeated request ID and builds the row
   (`pkg/reconcile/network` `NewRow`): the request is validated, the labels
   are checked as AWS tags, and the account and the layout are resolved
   (`pkg/network`: defaults, subnets inside the CIDR, zones of the region).
2. **Store.** The row is inserted with the resolved spec, `PROVISIONING` and
   not applied; the call returns. Nothing was written to the management
   cluster.
3. **Reconciler** (`pkg/reconcile/network`, `Apply`). It renders the plan
   into a set with `crossplaneaws.NetworkSet` and gives it to the engine with
   the network's inventory.
4. **Engine** (`sequence.Engine.Apply`). It records the objects that are
   ready, applies them with server-side apply, and reports what waits.
5. **Crossplane** creates the cloud resources and reports conditions on the
   objects.
6. **Status.** The controller calls `Reconcile` while the network is not
   settled: it applies again while anything was held back, records the
   status that `GetNetwork` returns, and stores `READY` once every resource
   is ready and the provider has seen the latest change. What goes wrong
   applying is the network's status (`FAILED`, or "retrying"), not an error
   of the call, and nothing is rolled back.

### Cluster

The shape is the same (`pkg/server/cluster` `CreateCluster`,
`pkg/reconcile/cluster` `Apply`, `crossplaneaws.ClusterSet`), with more
before and after:

- The row (`pkg/reconcile/cluster` `NewRow`) resolves the target: the
  account, the network and its private subnets in at least two zones. It
  checks the name against the platform's own clusters in the store and
  fills the spec's defaults (`pkg/cluster`). It does not look at the cloud.
- Before the first render the reconciler makes the cloud lookups of the
  generation (`resolve`): that EKS has no cluster of the name, and the
  Kubernetes and add-on versions, which it writes to the stored spec. The
  row's `ResolvedGeneration` says which generation had them made; an update
  that changes neither the version nor the add-ons needs none.
- After the apply the reconciler tags the network's subnets for the
  cluster.
- The controller keeps applying until the set is complete, retires replaced node
  groups, and stores `RUNNING`.

### Ordering rules

| Rule | Where it lives |
|---|---|
| NAT gateways are created only once the internet gateway is Ready. | A `Ready` condition on the NAT items in `crossplaneaws.NetworkSet`. |
| Add-ons, access entries, access policy associations and pod identity associations are created only once the EKS cluster is `ACTIVE`; a Usage whose user is one of them waits with it. | A `Field` condition in `crossplaneaws.ClusterSet`. |
| Values that exist only after another object does: the ARN of the encryption key the platform creates, the cluster's OIDC issuer (for IRSA) and its security group; a new cluster waits for its log group. | Items with a `Build` in `ClusterSet`; the rule is `ClusterPlan.FillLive` in `pkg/crossplane/aws`. |
| During a Kubernetes upgrade, node groups and add-ons stay at the running version until the control plane reports the target. | `upgrade.phase` in `pkg/reconcile/cluster/upgrade.go`: one rule, read by `GateUpgrade` (what is applied) and reported as `status.upgrade` (`ClusterUpgrade`: phase, from and to version, start, message). It is the one ordering rule still outside the engine: the engine has no result for "applied, but at a held version", which is what keeps the cluster being applied again. |
| A change EKS cannot make in place on a node group (instance types, capacity type, AMI type, disk size, launch template) creates a new node group; the old one is retired once the new one is Ready. | `pkg/reconcile/cluster` (`NodeGroupName`, retirement on a later pass). |

A condition gates creation only. Once the owner has the object, or has any
object that waited for the same condition, it is applied whatever the
condition says. While anything waits, the engine removes nothing.

## Import and takeover

An import is read-only, and the call that starts it does not look at the
cloud: it stores the request as a network or cluster that is `IMPORTED` with
its import `DISCOVERING` (`NewImportRow`). The reconciler looks the existing
VPC or EKS cluster up in AWS once (`pkg/cloud/aws`, `discoverImport`), and
`pkg/network` or `pkg/cluster` turns what it found into a plan, which is
stored with the row before anything is applied, so no later pass looks the
cloud up again:

- A spec derived from what exists.
- Objects that only observe: `managementPolicies` is `Observe` for a
  network's objects, and `Observe` with `LateInitialize` for a cluster's.
  Each carries the cloud resource's ID as its external name.
- One line per resource in `status.import`, with a disposition: `MATCHES`,
  `WILL_UPDATE`, `WILL_CREATE`, `NOT_IMPORTED`, `BLOCKING` or `CARRIED_OVER`.

A plan with a `BLOCKING` line imports nothing. Otherwise the network or
cluster is `IMPORTED`: Crossplane reads the resources and changes nothing.

An import whose lookup says it cannot succeed stops the same way: when the
VPC or EKS cluster does not exist where the import says it is (a typo,
another region or account), or AWS refuses what was asked for, the import is
stored as `BLOCKED` with that reason as its one plan line, and the cloud is
not asked again. Delete it and import again. A lookup that did not answer,
or credentials that cannot read the resource, are retried: neither is final.

An imported network or cluster is read-only until its takeover. The API
refuses to change it, and a change made on its object behind the API
(`kubectl edit`) is not kept: the reconciler remembers the spec, display name
and labels as the import left them (with the row), puts them back on the next
pass, and reports it (`SpecEditIgnored` condition, the status message, one
line in the controller's log). A takeover does the same before it stores
anything, so what is applied is the plan that was reviewed. A resource
imported before this was remembered is remembered on its next pass, as it is
then.

A **carried-over** object is one that came with the import and that no spec
field describes (for example a VPC's existing flow log). It is marked, never
removed when the owner is rendered again, and deleted with its owner.

**Takeover** makes the platform the owner, and it is the same operation as
the import with other management policies: the set the import rendered is
rendered again, as owned, and applied through the sequence engine. Nothing
is read and edited in place, and AWS is not looked up again; what the import
discovered is kept with the row for this (a network's discovered resources
in its stored input, a cluster's planned objects in its stored adoption).
The row leaves the imported state first, so a takeover that stops half-way
is finished by the controller.

A taken-over cluster then gets the platform components like any cluster,
unless the takeover asked to keep them off (`skip_platform_components`); see
[catalog.md](catalog.md), "Taken-over clusters".

- **Network**: `crossplaneaws.OwnedNetworkResources` renders every imported
  object with full management, or without the right to delete for a log
  group or role that other VPCs also use. Subnets get the load balancer
  role tag, and the network's labels become tags on top of the tags the
  import had. A later label update is the same render with other labels:
  a label that goes is no tag any more, and a tag the import had under
  that key is back. An imported network is never rendered from its spec,
  also after takeover: that would create a second VPC.
- **Cluster**: the normal apply renders the spec with the adopted names;
  the carried-over objects are added to the same set from the import's
  stored objects, owned (`crossplaneaws.Own`).

A network or cluster that was imported before its import was kept with it
has nothing to render from. Its takeover fails with a status that says to
import it again; it is never rendered from something else.

## Deletion

- **Networks** are deleted tier by tier, the order AWS accepts
  (`crossplaneaws.NetworkDeleteOrder`): flow log, ACL rules, routes and
  associations; then log group, route tables and NAT gateways; then role,
  policy and Elastic IPs; then subnets and the internet gateway; then the
  VPC. The next tier starts only when the previous one is gone.
- **Clusters** have no tiers (`crossplaneaws.ClusterDeleteOrder` is empty).
  Every object is deleted at once and the rendered Crossplane Usages order
  the cloud deletion: the Usage webhook holds an object until what uses it
  is gone. The reconciler also sweeps objects that carry the cluster's label
  but were never recorded, and removes the cluster's tags from the subnets.
- **Before forgetting.** When the cache shows nothing left, the reconciler
  asks the API server directly. Only then are the records and the row
  removed, so a cache that lost its objects cannot make the platform forget
  cloud resources.

## Storage

`pkg/database/repository` is the interface the server and the reconcilers
use: tenants and cloud accounts, networks, clusters, and the record of which
objects belong to which owner. An update must present the row's etag;
recording a status does not change it.

There is one store and no database: rows are CloudAccount, Network and
Cluster objects of the `cloud.cluster-vending.io` kinds in the tenant's
namespace (`pkg/database/cloudstore`), with the typed spec converted to and
from the contract (`pkg/convert`).

- Names are unique per tenant: a network's or cluster's name is its
  object's name in the tenant's namespace.
- Deletes are real deletes: there is no history of deleted rows.
- Uniqueness that is not an object name (a create request ID, an imported
  VPC per account and region) is a claim object whose name is derived from
  the value, so of two creates one wins.
- Its one "transaction" (`WithinTransaction`, used by the tenant bootstrap)
  takes back the rows it created when it fails; it is not atomic.

It runs the behaviour suite `pkg/database/storetest`, which also gives every
test that needs a store its own fake API server (`storetest.NewStore`).

## The contract

The API is `cloud.v1` in `idl/cloud/v1`; [idl/README.md](../idl/README.md)
maps its fields to AWS. Changes to it are additive only: fields and enum
values are added, never renumbered or removed, and new booleans that need
"unset" are `optional`. `make breaking` checks this against `main`.

`ClusterSpec` has two fields for rollouts that are stored and returned and
that nothing acts on yet:

- `release_channel`: alpha, beta or stable. A release goes to the channels
  in that order.
- `tier`: 0, 1 or 2, tier 0 being the most sensitive. Within a channel a
  release reaches tier 2 first, then tier 1, then tier 0. A cluster created
  without a tier is stored as tier 0.

Neither can be changed after creation today: the update mask does not accept
them.

## Direction

Everything in this section is decided. None of it works yet; where part of
it exists, that is said.

- **A controller reconciles the `cloud.cluster-vending.io` kinds.** Built,
  not verified against a real API server: the three controllers in
  `pkg/controller` turn a watch event into a call of the
  existing reconcilers in `pkg/reconcile` (on `pkg/database/cloudstore`),
  adopt Networks and Clusters created with kubectl, apply spec edits, and
  requeue until a resource is settled. It is the only thing that
  reconciles: the server's background loop is removed. See
  `docs/controller.md`.
- **Ownership.** A CloudAccount owns its Networks and a Network owns its
  Clusters; one account has many of each. The references are required and
  immutable in the types, and the store and the controllers set the owner
  references. An owner is not deleted while it has dependents; there is no
  cascade: the controllers hold the finalizer and say what blocks the
  deletion.
- **API handlers write desired state only, and the API server needs no AWS
  access.** Built for networks and clusters: the handlers validate, store
  and return, make no AWS call, and a failed apply or lookup is a status on
  the resource, not an error on the call. Still to do: BootstrapTenant
  applies the account's namespace and ProviderConfig itself, and the
  validate-only update plan needs to be allowed a dry-run apply.
- **Storage moves onto the controller's kinds, and the database is
  removed.** Done: the store on the controller's kinds is the only one, the
  GORM backend, its migrations and the `--store` and `--database-*` settings
  are gone, and so is the server's background loop. No existing state was
  migrated.
- **Moving upgrade gating onto the engine.** The upgrade's step is now a
  status field (`status.upgrade`) computed by the same rule that gates the
  apply; the rule itself still lives in the reconciler.
- **Rollouts by channel and tier.** Only the two fields exist. See
  [design/rollouts.md](design/rollouts.md).

## What is not verified

- Authentication has only run against a fake OIDC issuer: no real
  Microsoft Entra ID tenant, Keycloak or Okta has issued a token the
  server verified, and the CLI's login has not been used against one.
- Nothing has run against a real AWS account or real Crossplane providers.
  In particular the unit tests' API server has no Usage webhook, so ordered
  cloud deletion through Usages is untested.
- The Kubernetes-backed store has only run against controller-runtime's
  fake client.
- The validation rules in the controller's CRDs (CEL) have not been loaded
  into a real API server.
