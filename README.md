# cluster-vending

cluster-vending creates AWS networks and EKS clusters for tenants by writing
Crossplane managed resources into a management cluster. It can also import an
existing VPC or EKS cluster read-only and take it over later.

One binary runs three roles: the API server (`serve`), the controller
(`controller`), and the CLI that calls the API. A tenant console lives in
[`ui/`](ui/README.md). Nothing here has run against a real AWS account.

The design, the CRDs, configuration, operations and known limits are in
**[docs/architecture.md](docs/architecture.md)**.

## Prerequisites

- Go (see `go.mod`).
- `buf` for the protos (`make generate`, `make lint`).
- For a local management cluster: `docker`, `kind`, `kubectl`, `helm`.
- `golangci-lint`; `make lint` uses the one on `PATH` or `bin/golangci-lint`.

## Build and check

| Command | What it does |
|---|---|
| `make build` | Builds `bin/cluster-vending`. |
| `make check` | Lint, `go vet` and the unit tests. Run before every commit. |
| `make test` | Unit tests with the race detector. No cluster needed. |
| `make help` | Lists every target. |

## Quickstart

Three terminals against a kind cluster. The controller is the only process that
talks to AWS; the API server validates and stores.

```sh
make kind-up                # kind with Crossplane (hack/start.sh)
export AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=...   # in the shell only

make run-controller         # terminal 1: hack/controller.local.yaml
make run                    # terminal 2: API server, after the controller installed the CRDs
```

Then the CLI (terminal 3). Manifests are in `examples/`; each command has `--help`:

```sh
bin/cluster-vending tenant create acme
bin/cluster-vending account create -f examples/cloudaccount.yaml   # prints the role stack to create
bin/cluster-vending account verify --id BOOTSTRAP_ID --tenant acme --wait
bin/cluster-vending network create -f examples/network.yaml --wait
bin/cluster-vending cluster create -f examples/cluster.yaml --wait
```

The local setup, the AWS credential modes, login and the console are described in
[docs/architecture.md#run-locally](docs/architecture.md#run-locally) and
[docs/architecture.md#aws-credentials](docs/architecture.md#aws-credentials).

## Where to read more

- [docs/architecture.md](docs/architecture.md): what exists, how it works, the
  custom resources, configuration, operations and known limits.
- [idl/README.md](idl/README.md): the `cloud.v1` contract.
- [ui/README.md](ui/README.md): the tenant console.
