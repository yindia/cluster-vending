# Local development

How to run cluster-vending from your machine: the controller, the API server and
the console, against a management cluster of your choice and an AWS account of
your choice. The README's quickstart is the short version. This guide covers
setting it up from scratch, pointing it at a different management cluster or AWS
account, and the problems people hit.

## What runs where

| Process | Command | Talks to |
|---|---|---|
| Controller | `make run-controller` | The management cluster (Crossplane objects, CRDs) and AWS. It is the only process that changes AWS. |
| API server | `make run` | The management cluster (custom resources are its storage; there is no database). It reads results buckets in S3. |
| Console | `make run-ui` | The API server, at `http://localhost:3000`. |
| Live reader (optional) | `make run-live` | The workload clusters' API servers, for node lists and totals. |
| CLI | `bin/cluster-vending` | The API server. |

Crossplane runs **inside** the management cluster and calls AWS on the
controller's behalf. All processes read one settings file, `hack/config.local.yaml`
by default (`LOCAL_CONFIG=...` picks another).

## Prerequisites

- Go (version in `go.mod`), Node.js for `ui/`, `buf`, `golangci-lint`
- `docker` (Docker Desktop or OrbStack), `kind`, `kubectl`, `helm`
- The AWS CLI, with credentials for the account clusters are created in
- `make build` once, to check the toolchain

## 1. Pick a management cluster

### a) A local kind cluster (default)

```sh
make kind-up                         # kind "cluster-vending" + Crossplane (hack/start.sh)
KIND_CLUSTER=dev2 make kind-up       # or a second, separate one: context kind-dev2
```

### b) Any other Kubernetes cluster

Any cluster your kubeconfig can reach works, as long as it can run Crossplane:
a shared dev cluster, a second kind, or EKS. Install Crossplane the way
`hack/start.sh` does, keeping the default activations off so only the kinds the
platform uses are created:

```sh
CTX=my-dev-cluster
helm repo add crossplane-stable https://charts.crossplane.io/stable --force-update
helm upgrade --install crossplane crossplane-stable/crossplane \
  --kube-context "$CTX" --namespace crossplane-system --create-namespace \
  --version 2.4.3 --set provider.defaultActivations=null --wait
```

Then point the settings file at it (step 3: `context: my-dev-cluster`). On start,
the controller creates the platform's CRDs, the Crossplane AWS providers,
provider-helm and the component catalog wherever they are missing
(`--install-providers`, `--install-catalog`, both on by default). If the cluster
already runs Crossplane providers you manage yourself, set
`install-providers: false`.

One controller per management cluster. Two controllers (yours and a teammate's)
on the same cluster fight over the same objects. Use separate clusters, or keep
`leader-elect: true` so only one is active.

## 2. Pick an AWS account

The platform reaches an AWS account through an IAM role in that account,
`cluster-vending-provisioner`. It is created from a CloudFormation template the
CLI prints.

1. **The management identity.** Locally, this is the IAM user or role whose keys
   you export (kind has no IRSA). Note its ARN: `aws sts get-caller-identity`.
2. **The provisioner role in the target account.** Create the tenant and the
   account, and `account create` prints the exact `aws cloudformation
   create-stack` command (template, `ManagementRoleArn`, external ID):

   ```sh
   bin/cluster-vending tenant create acme
   # edit examples/cloudaccount.yaml: spec.aws.accountId = your account
   bin/cluster-vending account create -f examples/cloudaccount.yaml
   # run the printed create-stack with credentials of the target account, then:
   bin/cluster-vending account verify --id <BOOTSTRAP_ID> --tenant acme --wait
   ```

3. **Recommended: the reader role** (`cluster-vending-reader`). `account create`
   prints its create-stack command right after the provisioner's, with the same
   `ManagementRoleArn` and external ID; run it in the same account. Without it,
   each cluster's live-reader access entry fails with
   `The specified principalArn is invalid` and the cluster has no live reads.
   Nothing else depends on it, and the cluster still becomes Ready.

## 3. The settings file

Copy the local file for your setup so it isn't overwritten by others' changes:

```sh
cp hack/config.local.yaml hack/config.mine.yaml
export LOCAL_CONFIG=hack/config.mine.yaml     # every make run* target reads it
```

The keys that differ per setup:

```yaml
context: kind-cluster-vending          # kubectl context of the management cluster (step 1)
aws-management-role-arn: arn:aws:iam::<ACCOUNT>:role/cluster-vending-provisioner
provider-credentials-secret: <namespace>/<name>   # the Crossplane providers' keys (kind has no IRSA)
skip-tenant-role: true                 # mode A (own keys); false for mode B (assume roles); see "AWS credentials" below
http-addr: 127.0.0.1:8080
leader-elect: false                    # one local controller
automatic-upgrades: true               # false: plan upgrades but never start one
nodeGroups:                            # the platform's node groups (docs/architecture.md#node-groups-configuration)
  system: {instanceTypes: [t3.medium], minSize: 2, capacityType: ON_DEMAND, amiType: AL2023_x86_64_STANDARD}
  platform: {instanceFamilies: [t3], instanceSizes: [large, xlarge, 2xlarge], architectures: [AMD64]}
```

The providers' credentials Secret holds the same keys you export for the
controller, as a shared-credentials file:

```sh
kubectl --context <CTX> -n crossplane-system create secret generic aws-local-credentials \
  --from-file=credentials=/dev/stdin <<EOF
[default]
aws_access_key_id = $AWS_ACCESS_KEY_ID
aws_secret_access_key = $AWS_SECRET_ACCESS_KEY
EOF
# then: provider-credentials-secret: crossplane-system/aws-local-credentials
```

Keys never go in the settings file or in flags. The controller and the server
take them from the AWS SDK's default chain: exported variables first, then
`~/.aws/credentials`.

## AWS credentials: two ways to run locally

Two identities need credentials:
- **The controller and the API server**, which use the AWS SDK's default chain: exported variables, then `~/.aws/credentials`.
- **The Crossplane AWS providers.** Locally they use the `provider-credentials-secret` Secret; in production they use IRSA.

The providers assume the account's provisioner (tenant) role in **one hop** from
the Secret's identity. That role trusts only the role named by
`aws-management-role-arn` (the `ManagementRoleArn` stack parameter must be a
role ARN). So a plain IAM user's keys, or root keys, cannot assume it. You either
skip the role (mode A) or give everything a session of the management role
(mode B).

### Mode A: your own keys, no role assumption

Simplest. Your keys act directly in the account, as one identity for every
account. Use it for a single sandbox account where your keys already have the
permissions. It isn't production-like: the provisioner role's permissions are
never exercised.

1. Export your keys in the controller's and the server's shells, and check the account:
   ```sh
   export AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=...
   aws sts get-caller-identity          # must be the account clusters go into
   ```
2. Put the same keys in the providers' Secret:
   ```sh
   kubectl --context <CTX> -n crossplane-system create secret generic aws-local-credentials \
     --from-file=credentials=/dev/stdin <<EOF
   [default]
   aws_access_key_id = $AWS_ACCESS_KEY_ID
   aws_secret_access_key = $AWS_SECRET_ACCESS_KEY
   EOF
   ```
3. In your settings file:
   ```yaml
   aws-management-role-arn: arn:aws:iam::<ACCOUNT>:role/cluster-vending-provisioner  # required; names the stack's trusted role
   provider-credentials-secret: crossplane-system/aws-local-credentials
   skip-tenant-role: true            # act with the Secret's keys directly
   ```
4. Create the provisioner stack as `account create` prints it (the platform still
   checks the account through it), then `account verify`, and run as in section 4.

### Mode B: assume roles, like production

Everything runs as a session of a management role, and that session assumes
each account's provisioner role, as IRSA does in production. Use it to test the
real permissions, a second account, or the external ID.

1. **Create the management role** in the management account from its template
   (`cluster-vending-management`, the one production uses with IRSA). Locally it
   trusts your own identity (`LocalPrincipalArn`: an IAM user, or the role of an
   SSO session, not its `assumed-role` ARN), allows 12-hour sessions, and may only
   assume the tenant roles (`cluster-vending-*`). With the API server running
   (`make run`), `bin/cluster-vending account create --management` prints the
   commands; or, with the server not up yet, from the repository's copy:
   ```sh
   aws sts get-caller-identity --query Arn --output text   # your ARN, e.g. arn:aws:iam::<MGMT_ACCOUNT>:user/<you>
   aws cloudformation create-stack --stack-name cluster-vending-management \
     --template-body file://pkg/cloudformation/templates/cluster-vending-management-role.template.yaml \
     --capabilities CAPABILITY_NAMED_IAM \
     --parameters ParameterKey=LocalPrincipalArn,ParameterValue=arn:aws:iam::<MGMT_ACCOUNT>:user/<you> \
       ParameterKey=MaxSessionDuration,ParameterValue=43200 \
     --region us-east-1
   aws cloudformation wait stack-create-complete --stack-name cluster-vending-management --region us-east-1
   aws cloudformation describe-stacks --stack-name cluster-vending-management --region us-east-1 \
     --query "Stacks[0].Outputs[?OutputKey=='ManagementRoleArn'].OutputValue" --output text
   ```
   The server role (`cluster-vending-server`, printed second) is optional locally:
   the server runs with the same session as the controller.
2. **Settings file** (the stack's `ManagementRoleArn` output):
   ```yaml
   aws-management-role-arn: arn:aws:iam::<MGMT_ACCOUNT>:role/cluster-vending-management
   provider-credentials-secret: crossplane-system/aws-local-credentials
   skip-tenant-role: false           # assume each account's provisioner role (the default)
   ```
3. **Create or update the provisioner stack** (and the reader stack) in each target account.
   `account create` prints them with `ManagementRoleArn` set to that management role, and with the account's external ID.
   An account created under mode A needs `update-stack` with the new `ManagementRoleArn`.
4. **Start a session of the management role,** for up to 12 hours (the stack's `MaxSessionDuration=43200`), and export it:
   ```sh
   eval "$(aws sts assume-role --role-arn arn:aws:iam::<MGMT_ACCOUNT>:role/cluster-vending-management \
     --role-session-name cv-local --duration-seconds 43200 \
     --query 'Credentials.[AccessKeyId,SecretAccessKey,SessionToken]' --output text |
     awk '{print "export AWS_ACCESS_KEY_ID="$1" AWS_SECRET_ACCESS_KEY="$2" AWS_SESSION_TOKEN="$3}')"
   aws sts get-caller-identity       # arn:aws:sts::<MGMT_ACCOUNT>:assumed-role/cluster-vending-management/cv-local
   ```
5. **Put the session in the providers' Secret,** including the token:
   ```sh
   kubectl --context <CTX> -n crossplane-system delete secret aws-local-credentials --ignore-not-found
   kubectl --context <CTX> -n crossplane-system create secret generic aws-local-credentials \
     --from-file=credentials=/dev/stdin <<EOF
   [default]
   aws_access_key_id = $AWS_ACCESS_KEY_ID
   aws_secret_access_key = $AWS_SECRET_ACCESS_KEY
   aws_session_token = $AWS_SESSION_TOKEN
   EOF
   ```
6. **Start the controller and the server** from that shell, as in section 4, then run `account verify --wait`. It shows the role assumption working, or the exact denial.
7. **When the session expires** (`ExpiredToken`): repeat steps 4 and 5, then restart the controller and the server.

To keep only the controller and the server from expiring, run them with your own
long-lived keys and `aws-source-role-arn: <management role ARN>`. They then
assume the management role themselves and renew it. The providers still need a
session in the Secret (step 5): they don't take a source role.

In both modes, export the **same** identity in every process's shell. A server
running with another account's keys fails with `... is not authorized to perform:
sts:AssumeRole on ... cluster-vending-provisioner`.

## 4. Run it

Use one shell per process, and export **the same AWS keys** in the controller's
and the server's shells:

```sh
export AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=...   # AWS_SESSION_TOKEN too for temporary keys
aws sts get-caller-identity                              # check: the account you expect

make run-controller     # shell 1; wait until it has installed the CRDs and providers
make run                # shell 2: API server on 127.0.0.1:8080
make run-ui             # shell 3: console on http://localhost:3000
make run-live           # shell 4, optional: live node lists
```

Then create things with the CLI (each command has `--help`). Edit the examples'
account ID, region, network ID and bootstrap ID first:

```sh
bin/cluster-vending network create -f examples/network.yaml --wait
bin/cluster-vending cluster create -f examples/cluster.yaml --wait
bin/cluster-vending cluster get --tenant acme --id <CLUSTER_ID>
```

Login is off locally. To turn it on, run `make keycloak`, uncomment the `auth`
block in your settings file, and restart the API and the console
(docs/architecture.md#run-locally).

## 5. After you change code

- **Rebuild and restart.** `make run-controller` and `make run` build first, but a
  running process keeps the old binary. Stop it and start it again.
- **Catalog changes** (`pkg/catalog/definitions/*.yaml`, `pkg/catalog/release.yaml`)
  are synced into the management cluster when the controller starts. A definition
  someone edited in the cluster is kept and never updated. To take this build's
  copy, delete that `ComponentDefinition` object and restart.
- **A change to what is rendered** (add-ons, node groups, placement) reaches
  running clusters only if `RenderVersion` in `pkg/crossplane/aws/aws.go` is
  bumped. There is a test that fails when an add-on render changes without a bump.
- Run `make check` before committing (buf, lint, vet and all tests, ~10 minutes).
  `pkg/live` `TestLoadSyntheticFleet` is a timing test that can fail under load.
  It passes when run alone.

## Look at a workload cluster

```sh
aws eks update-kubeconfig --name <cluster> --region <region> --kubeconfig /tmp/<cluster>.kubeconfig
KUBECONFIG=/tmp/<cluster>.kubeconfig kubectl get nodes -L node.cluster-vending.io/pool
```

Use a separate kubeconfig file per cluster. A deleted and recreated cluster with
the same name has a new endpoint, and an old context entry for it fails with
`no such host`.

The management side: `make managed` lists every Crossplane managed resource,
`kubectl --context <CTX> -n tenant-<tenant> get release.helm.m.crossplane.io`
lists the components' Helm releases, and
`kubectl --context <CTX> get componentdefinitions,releases,releasechannels`
lists the catalog.

## Try the Helm chart on a second kind cluster

To run everything in-cluster from the chart, use a separate kind cluster so the
development one stays untouched: `make kind-up KIND_CLUSTER=cv-helm`, then
`make docker-build docker-build-ui kind-load helm-install-kind TAG=dev`,
then `kubectl -n cluster-vending-system port-forward svc/cluster-vending-ui 3000:3000`
([Install with Helm](architecture.md#install-with-helm)). Remove it with
`make kind-down KIND_CLUSTER=cv-helm`.

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| `AccessDenied ... is not authorized to perform: sts:AssumeRole on ... cluster-vending-provisioner`, from the server or controller | That process runs with keys of another account (`User: arn:aws:iam::<other>...`). Check `aws sts get-caller-identity` in its shell, export the right keys (or `unset` stray ones), and restart it. |
| A cluster's live access entry is `Failed: The specified principalArn is invalid` | The reader role isn't deployed in that account (step 2.3). It doesn't affect readiness. |
| A component says `reconciliation is paused (crossplane.io/paused)` or `retrying after a transient error, next try at ...` | The platform's own retry backoff after a failed Helm install. It resumes on its own at the time shown. `cluster retry --component <name>` retries now. |
| Large charts (capsule, kyverno) fail with `failed to record the release ... connection reset by peer` | provider-helm's write of the release record from a local management cluster to EKS was cut off. The platform retries. A management cluster running in AWS doesn't see this. |
| A component's pods stay `Pending` with `untolerated taint` | It isn't placed on a pool: check its definition uses `${platform.nodeSelector}` and `${platform.tolerations}`, and that the stored `ComponentDefinition` is this build's (see section 5). |
| Changes in a definition file never reach the cluster | The stored definition counts as edited, so sync leaves it alone. Delete it and restart the controller. |
| `kubectl` says `no such host` for an EKS cluster | The cluster was recreated and your context is stale. Run `aws eks update-kubeconfig` again. |

## Clean up

```sh
bin/cluster-vending cluster delete <CLUSTER_ID> --tenant acme --wait   # deletes cloud resources in order
bin/cluster-vending network delete <NETWORK_ID> --tenant acme --wait
make kind-down                                                         # removes the local kind cluster
```

Delete clusters and networks through the platform before removing the
management cluster. Deleting kind first leaves the AWS resources behind, and you
then have to remove them by hand.
