# cluster-vending

cluster-vending creates AWS networks (VPCs) and EKS clusters for tenants by
writing Crossplane managed resources (provider-upjet-aws, namespaced
`*.aws.m.upbound.io` kinds) into a management cluster. It can also import a
network or EKS cluster that something else built, read-only at first, and
then take it over.

The repository holds one binary with three roles: an API server (`serve`), a
CLI that calls it (`tenant`, `account`, `network`, `cluster`, `admin`, and `login`,
`logout`, `whoami` where the server authenticates), and a controller manager
(`controller`) that reconciles the same resources when they are kept as
custom resources.

Nothing here has run against a real AWS account. See
[docs/architecture.md](docs/architecture.md) for what is built, what is
decided but not built, and what is not verified.

The tenant web dashboard lives in [`ui/`](ui/README.md), with its own Next.js
build and Dockerfile. It uses the same generated Connect RPC contracts as the CLI.

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
| `make test` | Unit tests with the race detector. They use a fake API server; no cluster is needed. |
| `make generate` | Regenerates Go in `pkg/gen` and TypeScript in `ui/src/gen` from `idl/`. |
| `make controller-apis` | Regenerate deepcopy code, CRDs and RBAC for the kinds in `api/`. |
| `make mocks` | Regenerates the repository mocks. |
| `make help` | Lists every target. |

## Run locally

Three terminals, against a kind cluster. The controller does the work and is
the only command that talks to AWS; the API server validates and stores.

```sh
# Once: a kind cluster with Crossplane, nothing else (hack/start.sh).
make kind-up

# Your AWS keys, in the shell only (never in a file or flag).
export AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=...

# Once: the same keys as a Secret the Crossplane providers authenticate
# with (kind has no IRSA). They never appear on the command line.
kubectl --context kind-cluster-vending -n crossplane-system create secret generic \
  aws-local-credentials --from-file=credentials=/dev/stdin <<EOF
[default]
aws_access_key_id = $AWS_ACCESS_KEY_ID
aws_secret_access_key = $AWS_SECRET_ACCESS_KEY
EOF
# then uncomment provider-credentials-secret in hack/controller.local.yaml
# and set aws-management-role-arn there.

# Terminal 1, in the shell with the keys: the controller.
make run-controller    # hack/controller.local.yaml

# Terminal 2: the API server, once the controller has installed the CRDs
# and providers (it waits for them).
make run               # hack/config.local.yaml

# Terminal 3: the CLI walkthrough below.
```

Both the controller's lookups and the providers assume each account's
tenant role, whatever their own credentials are, so the tenant role's trust
policy must allow the IAM user or role the keys belong to.

The API server also reads the scan results of a cluster from its results
bucket in the tenant's account (read-only, through the tenant role). Start it
(terminal 2) in a shell that has the same `AWS_ACCESS_KEY_ID` and
`AWS_SECRET_ACCESS_KEY` exported, and let the tenant role trust the same IAM
user or role (the stack's `ServerPrincipalArn` parameter). Without keys it
still runs: a cluster whose results are in a bucket is answered with a
"results unavailable" notice. Keys are never settings; `aws-region`,
`aws-source-role-arn` and `aws-external-id` are optional.

Local API and UI login is disabled by default. Run `make run-ui` for the UI.
For optional login, run `make keycloak` separately, uncomment the `auth` block
in `hack/config.local.yaml`, then restart the API and UI. Comment it out again
to disable login. See [local Keycloak setup](docs/local-keycloak.md).

`hack/config.local.yaml` points the server at the kind cluster and needs
no AWS setting; its state is custom resources there (there is no database).
`hack/controller.local.yaml` turns leader election off and names the
providers' credentials Secret. Every setting is also a flag and a `CV_*`
environment variable; a flag wins over the variable, which wins over the
file. `bin/cluster-vending serve --help` and `controller --help` list them.
In production there is no Secret and no keys: the controller and the
providers run with IRSA. See [docs/controller.md](docs/controller.md).

## CLI walkthrough

The CLI talks to the server at `--server` (default
`http://127.0.0.1:8080`). Manifests are YAML or JSON; the examples in
`examples/` are one chain for the tenant `acme`. A server-assigned id that a
later manifest needs is a placeholder in it, named in the file's header:
`BOOTSTRAP_ID`, `NETWORK_ID`, `IMPORTED_NETWORK_ID`. Replace the account ID
`111122223333`, the role and the VPC and subnet IDs with your own.

```sh
# Register the tenant and create its namespace before adding accounts.
bin/cluster-vending tenant create acme

# 1. Onboard acme's AWS account (docs/onboarding.md). It onboards the
#    account and prints how to create the platform's IAM role there: the
#    template this server serves (/cloudformation/provisioner.yaml), its
#    parameters and the aws cloudformation create-stack command to run with
#    your AWS credentials of the account. The answer has the account's id
#    (BOOTSTRAP_ID, the bootstrap id that networks refer to).
bin/cluster-vending account create -f examples/cloudaccount.yaml
bin/cluster-vending account list --tenant acme

# 2. Once the stack is CREATE_COMPLETE, have the controller check the
#    account's access (--region adds a region no network uses yet).
#    Networks and clusters are refused until ACCESS is Verified.
bin/cluster-vending account verify --id BOOTSTRAP_ID --tenant acme --region us-east-1 --wait
bin/cluster-vending account get --id BOOTSTRAP_ID --tenant acme   # its Checks

# 3. Create a network in it (set spec.bootstrapId to BOOTSTRAP_ID first).
#    The answer has its id: NETWORK_ID.
bin/cluster-vending network create -f examples/network.yaml --wait
bin/cluster-vending network get NETWORK_ID --tenant acme      # status last recorded
bin/cluster-vending network get NETWORK_ID --tenant acme --live   # status read live

# 4. Create a cluster in the network (set spec.network.networkId to
#    NETWORK_ID first). The answer has its id: CLUSTER_ID.
bin/cluster-vending cluster create -f examples/cluster.yaml --wait

# 5. Or import an existing VPC read-only (set spec.bootstrapId and the VPC
#    and subnet IDs first), review the plan in status.import, then take
#    it over. The answer has its id: IMPORTED_NETWORK_ID.
bin/cluster-vending network import -f examples/network-import.yaml
bin/cluster-vending network takeover IMPORTED_NETWORK_ID --tenant acme --wait

# 6. Delete, in order: clusters before their network.
bin/cluster-vending cluster delete CLUSTER_ID --tenant acme --wait
bin/cluster-vending network delete NETWORK_ID --tenant acme --wait
```

`cluster import` and `cluster takeover` work the same way for an existing
EKS cluster in an imported network (`examples/cluster-import.yaml`).
`update -f` applies what the manifest changes: it compares the manifest with
the object, prints each change (`spec.release_channel: Beta → Alpha`) and
applies only those, refused if the object changed meanwhile; `--plan` shows
them without applying. A field the manifest leaves out is left as it is. `--wait` follows
a change until it is done, `events` lists what happened, and `list` lists. Each command has `--help`. The
controller's samples (`config/samples`) are the same account, network and
cluster as objects of its kinds.

### Output

Commands print tables in the style of kubectl. A list prints one row per
object; `-o wide` adds columns, such as a network's VPC, CIDR, zones and NAT,
or a cluster's network, node pool and release. `get` prints the object, then
its managed resources (external name, state, synced, ready), its conditions
and its recent events. `-o yaml` and `-o json` print the full API object.

```sh
bin/cluster-vending cluster list --tenant acme -o wide
bin/cluster-vending cluster get CLUSTER_ID --tenant acme -o yaml
```

## Where to read more

- [docs/architecture.md](docs/architecture.md): the packages and their
  rules, how a create, an import and a delete flow, storage, and the
  direction the project is moving in.
- [idl/README.md](idl/README.md): the `cloud.v1` contract and how its fields
  map to AWS.
- [docs/onboarding.md](docs/onboarding.md): onboarding an AWS account: the
  CloudFormation templates of the platform's role (reader, updater,
  provisioner), which the API server serves at `/cloudformation/`, the steps
  `account create` prints to create the stack, and the `make cf-*` targets.
- [docs/components.md](docs/components.md): the `component` command: the
  catalog, enabling a component on a cluster, its values, and what is not
  installed yet.
- [docs/oidc.md](docs/oidc.md): signing in to a cluster's Kubernetes API
  through an OIDC identity provider (Microsoft Entra ID or any other), and
  the kubeconfig for it.
- [docs/authentication.md](docs/authentication.md): OIDC authentication of
  the API (off by default), roles per tenant, Microsoft Entra ID setup and
  the CLI's `login`.
- [docs/runbooks/provider-upgrade.md](docs/runbooks/provider-upgrade.md):
  upgrading the Crossplane providers under a running platform: pause,
  upgrade, check, resume.
- [docs/recommendations.md](docs/recommendations.md): CPU and memory
  requests recommended from a week of recorded usage, without installing
  anything in the workload cluster.
- [docs/deprecations.md](docs/deprecations.md): how deprecated and removed
  API versions in workload clusters are found and shown.
- [docs/cluster-policy-visibility.md](docs/cluster-policy-visibility.md): cluster policy
  violations, retained admission events, and collection coverage.
- [docs/events.md](docs/events.md): the events the controller emits about
  accounts, networks, clusters and releases, their reasons, and how to read
  them (`cluster events`, `network events`, `status.recentEvents`).
- [docs/controller.md](docs/controller.md): the kubebuilder project and
  its kinds.
- [docs/design/upgrades.md](docs/design/upgrades.md): the plan for moving
  running clusters from one release to the next (not built yet).
- [docs/design/rollouts.md](docs/design/rollouts.md): the design for
  rollouts by release channel and tier (design only).
