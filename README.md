# cluster-vending

cluster-vending creates AWS networks (VPCs) and EKS clusters for tenants by
writing Crossplane managed resources (provider-upjet-aws, namespaced
`*.aws.m.upbound.io` kinds) into a management cluster. It can also import a
network or EKS cluster that something else built, read-only at first, and
then take it over.

The repository holds one binary with three roles: an API server (`serve`), a
CLI that calls it (`bootstrap`, `network`, `cluster`, `admin`), and a
controller manager (`controller`) that reconciles the same resources when
they are kept as custom resources.

Nothing here has run against a real AWS account. See
[docs/architecture.md](docs/architecture.md) for what is built, what is
decided but not built, and what is not verified.

## Prerequisites

- Go 1.27 (see `go.mod`).
- `buf` for the protos (`make generate`, `make lint`).
- For a local management cluster: `docker`, `kind`, `kubectl`, `helm`.
- `golangci-lint`; `make lint` uses the one on `PATH` or `bin/golangci-lint`.

## Build and check

| Command | What it does |
|---|---|
| `make build` | Builds `bin/cluster-vending`. |
| `make check` | Lint (protos and Go), `go vet` and the unit tests. This is the gate before every commit. |
| `make test` | Unit tests with the race detector. They use SQLite and a fake API server; no cluster is needed. |
| `make test-postgres` | The database tests against a throwaway Postgres in docker. |
| `make generate` | Regenerates `pkg/gen` from `idl/`. |
| `make controller-apis` | Regenerate deepcopy code, CRDs and RBAC for the kinds in `api/`. |
| `make mocks` | Regenerates the repository mocks. |
| `make help` | Lists every target. |

## Run locally

```sh
make kind-up   # hack/start.sh: a kind cluster with Crossplane, nothing else
make run       # the server against it, with hack/config.local.yaml
```

`hack/start.sh` creates a kind cluster named `cluster-vending` and installs
Crossplane from its Helm chart. It installs no provider: on startup what
reconciles installs the Crossplane AWS providers it needs and activates only
the managed resource kinds it uses. That is the controller; with the
database store of `hack/config.local.yaml` the server runs the background
loop and is the reconciler itself.

`hack/config.local.yaml` points the server at the `kind-cluster-vending`
context, listens on `127.0.0.1:8080`, keeps state in a local SQLite file
(`bin/cluster-vending.db`) and turns leader election off. Every setting is
also a flag and a `CV_*` environment variable; a flag wins over the variable,
which wins over the file. `bin/cluster-vending serve --help` lists them.

The server keeps its state in a database by default. `--store=kubernetes`
keeps it in custom resources in the management cluster instead (see the
architecture document).

The controller manager is a separate command:

```sh
bin/cluster-vending controller --leader-elect=false \
  --aws-management-account-id <account> --aws-management-role-arn <role arn>
bin/cluster-vending serve --http-addr 127.0.0.1:8080 --store=kubernetes
```

With `--store=kubernetes` the server only writes the
`cloud.cluster-vending.io` objects and the controller reconciles them (the
server's background loop is off; `--background-loop=true` runs the server
without the controller). In this mode the server needs nothing of AWS: no
`--aws-*` setting, no credentials. The controller is the only command that
looks at AWS and the one that installs the providers; it needs
`--aws-management-account-id`, `--aws-management-role-arn` and AWS
credentials (for local testing, `AWS_ACCESS_KEY_ID` and
`AWS_SECRET_ACCESS_KEY` in its environment; every lookup still assumes the
tenant role of the account it reads). Start the controller first: the
server waits for the kinds it installs. See
[docs/controller.md](docs/controller.md).

## CLI walkthrough

The CLI talks to the server at `--server` (default
`http://127.0.0.1:8080`). Manifests are YAML or JSON; the examples in
`examples/` are one chain for the tenant `acme`. A server-assigned id that a
later manifest needs is a placeholder in it, named in the file's header:
`BOOTSTRAP_ID`, `NETWORK_ID`, `IMPORTED_NETWORK_ID`. Replace the account ID
`111122223333`, the role and the VPC and subnet IDs with your own.

```sh
# 1. Onboard acme's AWS account. The answer has the account's id
#    (BOOTSTRAP_ID, the bootstrap id that networks refer to).
bin/cluster-vending bootstrap create -f examples/cloudaccount.yaml
bin/cluster-vending bootstrap list --tenant acme

# 2. Create a network in it (set spec.bootstrapId to BOOTSTRAP_ID first).
#    The answer has its id: NETWORK_ID.
bin/cluster-vending network create -f examples/network.yaml --wait
bin/cluster-vending network get NETWORK_ID --tenant acme      # status last recorded
bin/cluster-vending network status NETWORK_ID --tenant acme   # status read live

# 3. Create a cluster in the network (set spec.network.networkId to
#    NETWORK_ID first). The answer has its id: CLUSTER_ID.
bin/cluster-vending cluster create -f examples/cluster.yaml --wait

# 4. Or import an existing VPC read-only (set spec.bootstrapId and the VPC
#    and subnet IDs first), review the plan in status.import, then take
#    it over. The answer has its id: IMPORTED_NETWORK_ID.
bin/cluster-vending network import -f examples/network-import.yaml
bin/cluster-vending network takeover IMPORTED_NETWORK_ID --tenant acme --wait

# 5. Delete, in order: clusters before their network.
bin/cluster-vending cluster delete CLUSTER_ID --tenant acme --wait
bin/cluster-vending network delete NETWORK_ID --tenant acme --wait
```

`cluster import` and `cluster takeover` work the same way for an existing
EKS cluster in an imported network (`examples/cluster-import.yaml`).
`update` changes the fields named in `--update-mask`, `watch` streams state
and resource events, and `list` lists. Each command has `--help`. The
controller's samples (`config/samples`) are the same account, network and
cluster as objects of its kinds.

## Where to read more

- [docs/architecture.md](docs/architecture.md): the packages and their
  rules, how a create, an import and a delete flow, storage, and the
  direction the project is moving in.
- [idl/README.md](idl/README.md): the `cloud.v1` contract and how its fields
  map to AWS.
- [docs/controller.md](docs/controller.md): the kubebuilder project and
  its kinds.
- [docs/design/rollouts.md](docs/design/rollouts.md): the design for
  rollouts by release channel and tier (design only).
