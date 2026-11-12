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
| `pkg/reconcile` | What reconciling needs: `Deps` (store, management cluster, AWS access) and the bookkeeping every owner shares (record, observe, prune, the database-backed `Inventory`). | No RPC library, no `pkg/server`. Failures a caller can act on are faults. |
| `pkg/reconcile/network`, `pkg/reconcile/cluster` | A `Reconciler` each: moves one stored network or cluster towards its spec (apply, staging, delete, takeover, status). | Same as `pkg/reconcile`. |
| `pkg/server/network`, `pkg/server/cluster` | RPC handlers only: validate, read and write rows, call the reconciler, map errors to Connect codes. | May import everything above. |
| `pkg/server/tenant` | Handlers and the account bootstrap logic together; it has not been split yet. | As the other server packages. |
| `pkg/server/service` | What handlers share: request validation, `ToConnect` (repository errors and faults to Connect codes), the `Faults` interceptor. | The only shared package that imports the RPC library. |
| `pkg/database` | `repository` is the storage interface. `implementation` is the GORM backend (SQLite, Postgres). `cloudstore` is the Kubernetes-backed backend, on the controller's kinds. `storetest` is the behaviour suite both run. | `repository` and `model` import nothing else from the repository. |
| `pkg/loop` | The background loop: every interval it calls `Reconcile` for rows that are not settled. | Imports only `pkg/database`. It is handed the reconcilers. |
| `pkg/cloud/aws` | Read-only AWS lookups for imports and checks. | Imports only `pkg/constant/aws`. |
| `pkg/kube` | Client, scheme and server-side apply. | No storage, no RPC library. |
| `controller/` | The kubebuilder project: API types (`controller/api/v1alpha1`), stub reconcilers, CRDs, the manager. | Imports nothing from `pkg/`. |
| `pkg/cli`, `cmd/` | The CLI commands and the command tree. `cmd/` is thin and has no tests. | `pkg/cli` imports only the generated client. |

Two things the table would otherwise hide:

- `pkg/database/model` uses GORM for its hooks, so every package that imports
  the models (`pkg/reconcile` and above) links GORM even when no database is
  opened.
- `pkg/kube` registers the controller's kinds (`controller/api/v1alpha1`) in
  its scheme, so `pkg/crossplane/sequence` and everything above it depend on
  that package through `pkg/kube`.

## How a create flows

### Network

1. **Handler** (`pkg/server/network`, `CreateNetwork`). It validates the
   request, returns the existing network for a repeated request ID, checks
   the labels as AWS tags, and resolves the account and the layout
   (`pkg/network`: defaults, subnets inside the CIDR, zones of the region).
2. **Store.** The row is inserted with the resolved spec, so later renders
   do not depend on defaults.
3. **Reconciler** (`pkg/reconcile/network`, `Apply`). It renders the plan
   into a set with `crossplaneaws.NetworkSet` and gives it to the engine with
   the network's inventory.
4. **Engine** (`sequence.Engine.Apply`). It records the objects that are
   ready, applies them with server-side apply, and reports what waits.
5. **Crossplane** creates the cloud resources and reports conditions on the
   objects.
6. **Status.** The handler answers with the network as it is now. From then
   on the background loop calls `Reconcile` every interval while the network
   is not settled: it applies again while anything was held back, records
   the status that `GetNetwork` returns, and stores `READY` once every
   resource is ready and the provider has seen the latest change.

The handler applies inline today. If that apply fails, the create is rolled
back and the call fails.

### Cluster

The shape is the same (`pkg/server/cluster` `CreateCluster`,
`pkg/reconcile/cluster` `Apply`, `crossplaneaws.ClusterSet`), with more
before and after:

- The handler resolves the target: the account, the network and its private
  subnets in at least two zones. It checks the name against the store and
  against EKS, and fills the spec's defaults (`pkg/cluster`).
- After the apply it tags the network's subnets for the cluster.
- The loop keeps applying until the set is complete, retires replaced node
  groups, and stores `RUNNING`.

### Ordering rules

| Rule | Where it lives |
|---|---|
| NAT gateways are created only once the internet gateway is Ready. | A `Ready` condition on the NAT items in `crossplaneaws.NetworkSet`. |
| Add-ons, access entries, access policy associations and pod identity associations are created only once the EKS cluster is `ACTIVE`; a Usage whose user is one of them waits with it. | A `Field` condition in `crossplaneaws.ClusterSet`. |
| Values that exist only after another object does: the ARN of the encryption key the platform creates, the cluster's OIDC issuer (for IRSA) and its security group; a new cluster waits for its log group. | Items with a `Build` in `ClusterSet`; the rule is `ClusterPlan.FillLive` in `pkg/crossplane/aws`. |
| During a Kubernetes upgrade, node groups and add-ons stay at the running version until the control plane reports the target. | `GateUpgrade` in `pkg/reconcile/cluster`. This is the one ordering rule still outside the engine. |
| A change EKS cannot make in place on a node group (instance types, capacity type, AMI type, disk size, launch template) creates a new node group; the old one is retired once the new one is Ready. | `pkg/reconcile/cluster` (`NodeGroupName`, retirement in the loop). |

A condition gates creation only. Once the owner has the object, or has any
object that waited for the same condition, it is applied whatever the
condition says. While anything waits, the engine removes nothing.

## Import and takeover

An import is read-only. The handler looks the existing VPC or EKS cluster up
in AWS once (`pkg/cloud/aws`), and `pkg/network` or `pkg/cluster` turns what
it found into a plan:

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

**Takeover** makes the platform the owner. The row leaves the imported state
first; then the objects are switched to full management. For a network each
recorded object is updated in place; a log group or role that other VPCs
also use is owned without the right to delete it. For a cluster the normal
apply renders the spec with the adopted names and takes ownership of the
carried-over objects. A takeover that stops half-way is finished by the
loop. An imported network is never rendered from its spec, also after
takeover: that would create a second VPC.

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
| Kubernetes custom resources | `--store=kubernetes` | Rows are CloudAccount, Network and Cluster objects of the `cloud.cluster-vending.io` kinds in the tenant's namespace (`pkg/database/cloudstore`), with the typed spec converted to and from the contract (`controller/convert`). Names are unique per tenant. Deletes are real deletes. Its one "transaction" undoes created rows on failure and is not atomic. There are no node pools. |

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

- **A controller reconciles the `cloud.cluster-vending.io` kinds.** The
  kinds (CloudAccount, Network, Cluster), their CRDs and a manager exist in
  `controller/`; the reconcilers are stubs. They will call the existing
  reconcilers in `pkg/reconcile`, not a second implementation.
- **Ownership.** A CloudAccount owns its Networks and a Network owns its
  Clusters; one account has many of each. The references are required and
  immutable in the types, and a helper sets the owner references. An owner
  is not deleted while it has dependents; there is no cascade. The finalizer
  logic for this is not written.
- **API handlers write desired state only.** Today they apply objects
  inline. They will validate, store and return; a failed apply becomes a
  status on the resource, not an error on the call.
- **Storage moves onto the controller's kinds, and the database is
  removed.** The current Kubernetes-backed store uses separate storage kinds
  and is a first step. No existing state is migrated.
- **Upgrade gating becomes a status field** on the Cluster, instead of a
  rule in the reconciler.
- **Takeover becomes a re-apply** of the imported set without its
  Observe-only policies. This needs what was discovered to be kept in full,
  which it is not today.
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
