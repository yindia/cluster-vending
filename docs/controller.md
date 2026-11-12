# Controller

The platform's controller, laid out as a kubebuilder v4 project at the root
of the repository's one Go module. It holds the API of the platform's own custom
resources and the controllers that reconcile them. The controllers have no
reconcile logic of their own: each turns a watch event into a call of the
reconciler the API server's background loop also runs (`pkg/reconcile/account`,
`network`, `cluster`), on the store that keeps its rows as these objects
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

With `--store=kubernetes` the API server keeps its state in these kinds
(`pkg/database/cloudstore`): a tenant is its namespace, and every account,
network and cluster the API knows is an object here, written through the
same repository interface the database store has.

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

## Run

```
cluster-vending serve --store=kubernetes        # the API: writes the objects
cluster-vending controller --leader-elect=false # reconciles them
```

`controller` takes the global `--kubeconfig` and `--context`, plus
`--metrics-addr` (off by default), `--health-probe-addr`, `--leader-elect`,
`--lease-namespace`, `--render-target` and `--render-rollout`. It installs
the CRDs of the three kinds itself and waits for the Crossplane AWS
providers' kinds; it does not install the providers (`serve` does, or an
operator).

Who reconciles: one reconciler per resource. With `--store=kubernetes` the
server's background loop is off by default (`--background-loop=auto`) and
this controller reconciles; `serve --store=kubernetes --background-loop=true`
runs the server alone, without the controller. Do not run both on the same
objects. With the database store the loop reconciles and the controller has
nothing to watch.

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
  failed pass, and a 10m resync when settled. A change of a provider object
  wakes its owner at once (by the `cluster-vending.io/network` or
  `/cluster` label).
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
