# controller

The platform's controller, laid out as a kubebuilder v4 project inside the
repository's one Go module. It holds the API of the platform's own custom
resources. The reconcilers are not implemented: nothing here is connected to
the server, the background loop or the database yet.

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
owner reference along the chain (`owner`), with
`blockOwnerDeletion`.

Deletion rule: an owner is not deleted while it has dependents. Each kind
has a finalizer; an owner's finalizer is released only once no dependent is
left. Deleting an account never deletes its networks and clusters by
cascade.

The specs mirror the `cloud.v1` contract in `idl/cloud/v1`, with Kubernetes
conventions (camelCase, string enums, `metav1.Condition`). `convert`
translates between the two, both ways without loss.

With `--store=kubernetes` the API server keeps its state in these kinds
(`pkg/database/cloudstore`): a tenant is its namespace, and every account,
network and cluster the API knows is an object here, written through the
same repository interface the database store has.

## Layout

```
PROJECT                 kubebuilder project file
api/v1alpha1/           the types (edit these), generated deepcopy
internal/controller/    the three reconcilers: empty stubs
owner/                  the ownership chain as owner references and labels
manager.go              Run: the manager with the reconcilers and probes
config/crd/bases/       generated CRDs
config/rbac/            generated role, and the scaffolded ones
config/samples/         an account, a network and a cluster of one chain
config/{default,manager,prometheus,network-policy}/   scaffolded manifests
```

kubebuilder scaffolds a `Makefile`, a `cmd/main.go` and a `Dockerfile` next
to `PROJECT`. This project has none of them: there is one Makefile and one
binary for the repository. `kubebuilder create api` still works from this
directory for adding kinds, but its generated `cmd/main.go` edits must be
moved into `manager.go`, and its `make` hints replaced by the targets below.

## Generate

From the repository root, after changing the types or the RBAC markers:

```
make controller-apis
```

It runs `go tool controller-gen` (deepcopy, CRDs, RBAC role). `make check`
covers this directory like any other package.

## Run

```
kubectl apply -f controller/config/crd/bases
cluster-vending controller --leader-elect=false
```

`controller` takes the global `--kubeconfig` and `--context`, plus
`--metrics-addr` (off by default), `--health-probe-addr`, `--leader-elect`
and `--lease-namespace`. It runs an idle manager: it watches the three kinds
and changes nothing.

The scaffolded manifests under `config/` were adjusted to start
`/cluster-vending controller`, but there is no image build for them yet, and
`config/default` still assumes kubebuilder's authenticated HTTPS metrics,
which this manager does not serve (its metrics are plain HTTP).
