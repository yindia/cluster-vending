# cluster-vending

cluster-vending is a platform for vending AWS networks and EKS clusters to
tenants. Tenants ask for a network or a cluster through an API, a CLI or a web
console. The platform creates it with Crossplane from a management cluster,
installs and upgrades the platform's components on it, and keeps it current
through release channels rolled out tier by tier. It can also import an
existing VPC or EKS cluster read-only and take it over later.

Status: run end to end against a real AWS account from a local management
cluster; not yet run in production.

## What it does

| Area | What the platform does |
|---|---|
| **Accounts** | Onboards a tenant's AWS account through an IAM role it creates with a CloudFormation stack the CLI prints, then verifies access. |
| **Networks** | VPC, subnets, gateways, routes and flow logs, from a layout; import an existing VPC. |
| **Clusters** | EKS with encryption, logging, access entries, OIDC, IRSA and EKS add-ons. Node groups are the platform's: a fixed-size system group (Karpenter and the EKS add-ons) and Karpenter's `platform-pool` (everything else). |
| **Components** | A catalog of Helm charts (Karpenter, the AWS Load Balancer Controller, Istio, Kyverno, External Secrets, the GPU Operator, …) installed in order and kept at the versions their release pins. Tenants enable the tenant ones; the platform installs its own. |
| **Releases and upgrades** | Channels (alpha, beta, stable) offer releases; changes reach clusters tier by tier, with soak, maintenance windows and freezes. Kubernetes upgrades are automatic on a channel. Older minors are offered as extended support, upgraded by hand. |
| **Insights** | Live node lists, deprecated-API scans, resource recommendations from a week of usage, and policy violations. |
| **Import and takeover** | Observe an existing cluster read-only, then take it over without recreating it. |

## How it runs

One binary, `cluster-vending`, runs every process. The management cluster's
custom resources are its only storage; there is no database.

| Process | Command | Does |
|---|---|---|
| API server | `serve` | Validates and stores requests; serves the API (Connect/gRPC). Holds no AWS or workload-cluster credentials. |
| Controller | `controller` | Reconciles everything: renders Crossplane resources, installs components, rolls out upgrades, deletes in order. The only process that changes AWS. |
| Live reader | `live` | Reads node lists and totals from workload clusters, read-only, for the API. |
| CLI | `cluster-vending <group> <verb>` | Calls the API: `tenant`, `account`, `network`, `cluster`, `component`, `admin`. |
| Console | [`ui/`](ui/README.md) | The tenant web console (Next.js), at port 3000. |

## Quickstart (local)

Prerequisites: Go (see `go.mod`), Node.js, `buf`, `golangci-lint`, `docker`,
`kind`, `kubectl`, `helm`, the AWS CLI.

```sh
make kind-up                                   # kind + Crossplane
export AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=...   # in each process's shell, never in a file

make run-controller     # shell 1: installs CRDs, providers and the catalog on start
make run                # shell 2: API server on :8080
make run-ui             # shell 3: console on http://localhost:3000
```

Then, with the CLI (each command has `--help`; edit the IDs in `examples/` first):

```sh
bin/cluster-vending tenant create acme
bin/cluster-vending account create -f examples/cloudaccount.yaml   # prints the role stacks to create
bin/cluster-vending account verify --id <BOOTSTRAP_ID> --tenant acme --wait
bin/cluster-vending network create -f examples/network.yaml --wait
bin/cluster-vending cluster create -f examples/cluster.yaml --wait   # adds you (your AWS identity) as a cluster admin
```

The full setup, both AWS credential modes (your own keys, or assumed roles like
production), another management cluster or account, and troubleshooting are in
**[docs/local-development.md](docs/local-development.md)**.

## Deploy

The Helm chart [`charts/cluster-vending`](charts/cluster-vending/README.md)
runs the API server, controller, live reader and console in a management
cluster that has Crossplane:

```sh
# Login is required: put your OIDC issuer and audience in a copy of
# charts/cluster-vending/ci/auth-values.yaml (platform.auth.mode: required).
# On a local/dev cluster only, `--set allowInsecureNoAuth=true` runs without it.
helm install cluster-vending charts/cluster-vending -n cluster-vending-system --create-namespace \
  -f my-auth-values.yaml \
  --set aws.managementRoleArn=arn:aws:iam::<ACCOUNT>:role/cluster-vending-management
kubectl -n cluster-vending-system port-forward svc/cluster-vending-ui 3000:3000
```

The management account's IAM roles come from templates the CLI prints:
`bin/cluster-vending account create --management`. See
[docs/architecture.md#install-with-helm](docs/architecture.md#install-with-helm).

## Build and check

| Command | What it does |
|---|---|
| `make build` | Builds `bin/cluster-vending`. |
| `make check` | buf, lint, vet and every unit test. Run before every commit. |
| `make test` | Unit tests with the race detector; no cluster needed. |
| `make generate`, `make controller-apis` | Regenerate the API code and the CRDs. |
| `make docker-build`, `make docker-build-ui` | The images. |
| `make help` | Every target. |

## Read more

- [docs/architecture.md](docs/architecture.md): how it works, the custom
  resources, configuration, operations and known limits.
- [docs/local-development.md](docs/local-development.md): local setup and
  troubleshooting.
- [docs/proposals/](docs/proposals): design proposals.
- [idl/README.md](idl/README.md): the `cloud.v1` API.
- [ui/README.md](ui/README.md): the console.
- [CONTRIBUTING.md](CONTRIBUTING.md): how to make and check a change.
