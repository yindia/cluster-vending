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
cluster-vending controller --leader-elect=false --aws-management-role-arn <role arn>
cluster-vending serve --store=kubernetes   # the API: writes the objects
```

`controller` takes the global `--kubeconfig`, `--context`, `--log-format`
and `--log-level`, and only these:

| Setting | What it is |
|---|---|
| `--config` | YAML file with any of these settings |
| `--health-probe-addr`, `--metrics-addr` | listen addresses (metrics off by default) |
| `--leader-elect`, `--lease-namespace` | one replica reconciles |
| `--aws-management-role-arn` | required: the IAM role the Crossplane AWS providers run as (IRSA); the management account is taken from it |
| `--install-providers` | create the providers, provider-helm and their runtime config when missing |
| `--provider-credentials-secret` | `<namespace>/<name>[:<key>]`: the providers authenticate with this Secret instead of IRSA (local kind only) |

It installs the CRDs of the three kinds and the Crossplane AWS providers
(unless `--install-providers=false`) and waits for their kinds. The API
server installs nothing; it waits for the kinds the controller installs, so
start the controller first.

How the providers run is the platform's: a drift check every 10 minutes (30
for provider-aws-ec2), at most 10 reconciles per second, 100m CPU and 512Mi
memory requested, 2Gi memory limit (`crossplaneaws.DefaultProviderRuntime`).
There is no setting for these. To run the providers otherwise, set
`--install-providers=false` and manage the `Provider` and
`DeploymentRuntimeConfig` objects yourself.

The controller always brings every resource to this build's renderer. The
former `--render-target` and `--render-rollout` settings are gone; release
objects take their place (docs/design/rollouts.md).

## AWS access

The controller is the only command that looks at AWS: it discovers the VPC
or EKS cluster of an import, checks a new cluster's name against EKS, and
resolves the Kubernetes and add-on versions EKS offers. The API server makes
no AWS call and has no AWS setting or credentials.

There is one rule, for the controller's lookups and for the Crossplane
providers alike:

```
own credentials -> tenant role (always)
```

- **Own credentials.** For the controller, whatever the AWS SDK's default
  chain finds: IRSA in production, keys in its environment for local
  testing. For the providers, IRSA as the management role in production, or
  the keys in `--provider-credentials-secret` where there is no IRSA. They
  are never a flag value or a file of this project.
- **Tenant role**, always: the role stored when the account was bootstrapped
  (the one every account's ProviderConfig assumes). Nothing is read with the
  own credentials, so keys of one account cannot read another, and a role
  that is not a role of the account is refused. A local run therefore needs
  and exercises the same permissions as production.

### Production: IRSA

The controller's service account carries the IAM role annotation
(`eks.amazonaws.com/role-arn`); `config/default/kustomization.yaml` sets it,
with a placeholder to replace. The providers run as the management role
(`--aws-management-role-arn`, on their runtime config). No keys exist
anywhere and `--provider-credentials-secret` is not set. What must be true:

- The roles' trust policies let the service accounts assume them through
  the cluster's OIDC provider (the usual IRSA trust policy).
- They may call `sts:AssumeRole` on the tenant roles.
- Each tenant role's trust policy allows them (`sts:AssumeRole`, principal:
  the role's ARN).

The simplest arrangement is to run the controller as the management role
itself: the tenant roles then trust one role.

### Local run: keys in the environment and in a Secret

Kind has no IRSA, so both the controller and the providers start from your
keys, and both assume the tenant role. The tenant role's trust policy must
allow the IAM user or role the keys belong to.

1. A kind cluster with Crossplane: `make kind-up`.
2. Your keys in the shell, never in a file or a flag:
   ```
   export AWS_ACCESS_KEY_ID=...
   export AWS_SECRET_ACCESS_KEY=...
   export AWS_SESSION_TOKEN=...   # only for temporary keys
   ```
3. The same keys as the providers' Secret, in the AWS shared credentials
   format under the key `credentials` (the command does not print them):
   ```
   kubectl --context kind-cluster-vending -n crossplane-system create secret generic \
     aws-local-credentials --from-file=credentials=/dev/stdin <<EOF
   [default]
   aws_access_key_id = $AWS_ACCESS_KEY_ID
   aws_secret_access_key = $AWS_SECRET_ACCESS_KEY
   EOF
   ```
   An existing Secret works as it is; name it in the next step.
4. In `hack/controller.local.yaml`, set `aws-management-role-arn` (any role
   ARN of the management account; on kind it is only validated) and
   uncomment `provider-credentials-secret: crossplane-system/aws-local-credentials`.
5. `make run-controller` in the shell with the keys.
6. `make run` in another terminal: the API server (`hack/config.local.yaml`,
   the Kubernetes store, no AWS setting).

With the Secret set, every ProviderConfig the platform writes is:

```yaml
spec:
  credentials:
    source: Secret
    secretRef: {namespace: crossplane-system, name: aws-local-credentials, key: credentials}
  assumeRoleChain:
  - roleARN: arn:aws:iam::<tenant account>:role/<tenant role>
```

and without it (production) `credentials: {source: IRSA}` with the same
`assumeRoleChain`. The controller checks once at startup, and before every
account it applies, that the Secret exists and has its key; it reads nothing
else of it. A missing Secret or key shows on each CloudAccount's `Ready`
condition, and nothing is applied until it is fixed. The platform never
writes the Secret.

In a cluster without IRSA that runs the controller as a Deployment, enable
`config/local-credentials` (a Role to read that one Secret, and the
setting) and create the Secret in the controller's namespace.

The server with the database store and the background loop (`serve`,
`--background-loop`) is the reconciler too and follows the same rule with
its own credentials; that mode goes away with the database store.

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
