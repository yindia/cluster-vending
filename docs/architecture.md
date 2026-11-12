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
| `pkg/reconcile` | What reconciling needs: `Deps` (store, management cluster, the AWS lookups) and the bookkeeping every owner shares (record, observe, prune, the database-backed `Inventory`), the checks a request and a reconciler share (`Validate`, `CheckAWSLabels`), and how a tenant's account is reached (`AWSAccess`: always through its tenant role). | No RPC library, no `pkg/server`. Failures a caller can act on are faults. |
| `pkg/reconcile/network`, `pkg/reconcile/cluster`, `pkg/reconcile/account` | A `Reconciler` each: moves one stored network, cluster or account towards its spec (apply, staging, delete, takeover, status). They also build the row a create or an import stores (`NewRow`, `NewImportRow`), and they are the only code that looks at AWS: the discovery of an import, the EKS name check and the version resolution. | Same as `pkg/reconcile`. |
| `pkg/management` | Prepares the management cluster for the two commands: the manager whose cache both read from, the store on the platform's kinds, and, for what reconciles only, the Crossplane providers, the CRDs and the AWS lookups (`WithAWS`). | No RPC library, no `pkg/server`. |
| `pkg/server/network`, `pkg/server/cluster`, `pkg/server/tenant` | RPC handlers only: validate, read and write rows, map errors to Connect codes. They use the reconcilers to check a request against the store and to read. They make no AWS call, and the network and cluster handlers write nothing to the management cluster (the tenant handler still applies an account's namespace and ProviderConfig itself). | May import everything above. |
| `pkg/server/service` | What handlers share: request validation, `ToConnect` (repository errors and faults to Connect codes), the `Faults` interceptor. | The only shared package that imports the RPC library. |
| `pkg/database` | `repository` is the storage interface. `implementation` is the GORM backend (SQLite, Postgres). `cloudstore` is the Kubernetes-backed backend, on the controller's kinds. `storetest` is the behaviour suite both run. | `repository` and `model` import nothing else from the repository. |
| `pkg/loop` | The background loop: every interval it calls `Reconcile` for rows that are not settled. | Imports only `pkg/database`. It is handed the reconcilers. |
| `pkg/cloud/aws` | Read-only AWS lookups for imports and checks, always as the tenant role of the account they read. Called by the reconcilers only. | Imports only `pkg/constant/aws`. |
| `pkg/kube` | Client, scheme and server-side apply. | No storage, no RPC library. |
| `api/`, `pkg/controller`, `pkg/convert`, `pkg/owner`, `config/` | The kubebuilder project (`PROJECT` at the root): API types (`api/v1alpha1`), their conversion to the contract, the controllers and the manager (`pkg/controller/manager`), CRDs and deploy manifests for the controller and the server. | The types import nothing from `pkg/`; the controllers and the manager import `pkg/reconcile`, `pkg/database/cloudstore` and `pkg/management`. Nothing in them imports `pkg/server`. |
| `pkg/cli`, `cmd/` | The CLI commands and the command tree. `cmd/` is thin and has no tests. | `pkg/cli` imports only the generated client. |

Two things the table would otherwise hide:

- `pkg/database/model` uses GORM for its hooks, so every package that imports
  the models (`pkg/reconcile` and above) links GORM even when no database is
  opened.
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
| Looking up the VPC of an imported network | `pkg/reconcile/network` (`discoverImport`), once | The network is `FAILED` with the reason, its import still `DISCOVERING`; a lookup that did not answer is retried. |
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

While the database store exists, `serve` with the background loop is the
reconciler too (there is no controller for that store): only then does it
take the controller's AWS settings, install the providers and look the
cloud up, through the same reconcilers.

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
6. **Status.** Whatever reconciles (the controller, or the background loop
   with the database store) calls `Reconcile` while the network is not
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
- The loop keeps applying until the set is complete, retires replaced node
  groups, and stores `RUNNING`.

### Ordering rules

| Rule | Where it lives |
|---|---|
| NAT gateways are created only once the internet gateway is Ready. | A `Ready` condition on the NAT items in `crossplaneaws.NetworkSet`. |
| Add-ons, access entries, access policy associations and pod identity associations are created only once the EKS cluster is `ACTIVE`; a Usage whose user is one of them waits with it. | A `Field` condition in `crossplaneaws.ClusterSet`. |
| Values that exist only after another object does: the ARN of the encryption key the platform creates, the cluster's OIDC issuer (for IRSA) and its security group; a new cluster waits for its log group. | Items with a `Build` in `ClusterSet`; the rule is `ClusterPlan.FillLive` in `pkg/crossplane/aws`. |
| During a Kubernetes upgrade, node groups and add-ons stay at the running version until the control plane reports the target. | `upgrade.phase` in `pkg/reconcile/cluster/upgrade.go`: one rule, read by `GateUpgrade` (what is applied) and reported as `status.upgrade` (`ClusterUpgrade`: phase, from and to version, start, message). It is the one ordering rule still outside the engine: the engine has no result for "applied, but at a held version", which is what keeps the cluster being applied again. |
| A change EKS cannot make in place on a node group (instance types, capacity type, AMI type, disk size, launch template) creates a new node group; the old one is retired once the new one is Ready. | `pkg/reconcile/cluster` (`NodeGroupName`, retirement in the loop). |

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
is finished by the loop.

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
use: tenants and cloud accounts, networks, clusters, node pools, and the
record of which objects belong to which owner. An update must present the
row's etag; recording a status does not change it.

| Backend | Selected with | Notes |
|---|---|---|
| Database (GORM: SQLite or Postgres) | `--store=database`, the default | Deletes are soft deletes. Uniqueness is enforced by partial unique indexes. |
| Kubernetes custom resources | `--store=kubernetes` | Rows are CloudAccount, Network and Cluster objects of the `cloud.cluster-vending.io` kinds in the tenant's namespace (`pkg/database/cloudstore`), with the typed spec converted to and from the contract (`pkg/convert`). Names are unique per tenant. Deletes are real deletes. Its one "transaction" undoes created rows on failure and is not atomic. There are no node pools. |

Both run the same behaviour suite (`pkg/database/storetest`).

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
  requeue until a resource is settled. With `--store=kubernetes` the server
  leaves reconciling to it (`--background-loop`). See `docs/controller.md`.
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
  removed.** The current Kubernetes-backed store uses separate storage kinds
  and is a first step. No existing state is migrated.
- **Moving upgrade gating onto the engine.** The upgrade's step is now a
  status field (`status.upgrade`) computed by the same rule that gates the
  apply; the rule itself still lives in the reconciler.
- **Rollouts by channel and tier.** Only the two fields exist. See
  [design/rollouts.md](design/rollouts.md).

## What is not verified

- Nothing has run against a real AWS account or real Crossplane providers.
  In particular the unit tests' API server has no Usage webhook, so ordered
  cloud deletion through Usages is untested.
- The Kubernetes-backed store has only run against controller-runtime's
  fake client.
- The validation rules in the controller's CRDs (CEL) have not been loaded
  into a real API server.
