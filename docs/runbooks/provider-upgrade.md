# Runbook: upgrading the Crossplane providers

How to upgrade the AWS providers (provider-upjet-aws: `provider-aws-ec2`,
`-eks`, `-iam`, `-cloudwatchlogs`, `-kms`, `-sqs`, `-cloudwatchevents` and
their family provider) and `provider-helm` under a running platform, without
a provider acting on a tenant's cloud resources or charts in the middle of it.

Each step says where it comes from:

- **[code]** behaviour read in this repository and held by a test;
- **[upstream]** from Crossplane's documentation, not exercised by this
  repository's tests;
- **[untested]** a reasonable step nobody has run end to end here.

The whole procedure has not been run against a real management cluster yet.

## What the platform freeze does

`cluster-vending admin freeze --reason "provider upgrade" --wait` stores the
cluster-scoped `ReleaseHold/platform-freeze` in the management cluster.
It stops account, network and cluster actions, including deletion,
repair, upgrades and workload scans. Managed resources and Helm Releases
get `crossplane.io/paused: "true"`, so Crossplane also stops reconciling.
It does not cancel AWS operations already submitted.

User writes remain accepted and stored as custom resources. They apply
after unfreeze. `cluster get` and `network get` show `Frozen:`; a
resource let through says so. The events are `PlatformFrozen`,
`PlatformUnfrozen` and `LetThrough`.

`ProviderConfig` and `Usage` objects are not Crossplane managed resources.
The platform does not change or delete account configuration during the
freeze. Workloads already running remain running.

See [platform freeze](../controller.md#admin-scoped-freeze).
There is no separate pause/resume API. Tenant, network and cluster scopes
are supported; channel and tier scopes are not. A network-wide unfreeze
includes its clusters; a cluster-only unfreeze leaves its shared network frozen.

## What the platform freeze does not stop

The controller's own startup work is not a tenant's: on start it creates the
providers, runtime configs and CRDs that are missing, adds kinds to the
activation policy, creates the shipped release and catalog definitions that
are missing, and may move the `alpha` channel to a new shipped release
**[code]**. It never changes an existing provider's package or runtime
config, so it does not undo a provider upgrade. Two cases still want care:

- **Starting the controller while the new providers' kinds are not served
  yet**: it waits up to five minutes for every kind it uses, then exits
  **[code]**. Start it after step 6.
- **A controller build that is behind the providers** may not know a kind
  the new provider version renamed. Deploy the build made for the new
  version (step 5).

So the controller can stay up during the upgrade **[untested]**. Stopping it
(the older procedure) is still a valid, more conservative choice.

## Steps

### 1. Before

```sh
kubectl get providers.pkg.crossplane.io          # all INSTALLED=True HEALTHY=True
kubectl get networks.cloud.cluster-vending.io,clusters.cloud.cluster-vending.io -A
```

Do not start with a network or cluster in `Provisioning`, `Updating` or
`Deleting`: let it finish, or accept that it finishes after the resume.
Note the provider packages in use, for the rollback:

```sh
kubectl get providers.pkg.crossplane.io -o custom-columns=NAME:.metadata.name,PACKAGE:.spec.package
```

### 2. The controller

It can stay up: frozen resources are only observed **[code]**. To be
conservative, stop it instead and start it again in step 7:

```sh
kubectl -n cluster-vending-system scale deployment cluster-vending-controller-manager --replicas=0
```

### 3. Freeze the platform

```sh
cluster-vending admin freeze --reason "provider upgrade"
```

Wait for confirmed `Frozen` before changing the providers. Use
`cluster-vending admin freeze status` to inspect phase, exceptions and counts. Partial failures
are reported; repeat the command to finish pausing managed resources.

### 4. Check that nothing reconciles

Nothing may be left without the annotation:

```sh
kubectl get managed -A -o json \
  | jq -r '.items[] | select(.metadata.annotations["crossplane.io/paused"] != "true")
           | "\(.kind) \(.metadata.namespace)/\(.metadata.name)"'
kubectl get releases.helm.m.crossplane.io -A -o json \
  | jq -r '.items[] | select(.metadata.annotations["crossplane.io/paused"] != "true")
           | "Release \(.metadata.namespace)/\(.metadata.name)"'
```

Both must print nothing (`kubectl get managed` lists the AWS kinds; whether
it also lists provider-helm's `Release` depends on the provider's CRD
categories, hence the second command). Then every paused resource reports
`Synced=False` with the reason `ReconcilePaused` **[upstream]**:

```sh
kubectl get managed -A -o json \
  | jq -r '.items[] | select(([.status.conditions[]? | select(.type=="Synced")][0].reason) != "ReconcilePaused")
           | "\(.kind) \(.metadata.namespace)/\(.metadata.name)"'
```

Give the providers a minute to see the annotation before reading this.

### 5. Upgrade the providers **[upstream]**, **[untested]**

A provider is upgraded by changing its package. The family provider first,
then the service providers, all to the same version:

```sh
V=v2.10.0   # the new provider-upjet-aws version
kubectl get providers.pkg.crossplane.io          # find the family provider's name
kubectl patch providers.pkg.crossplane.io <family provider> --type merge \
  -p "{\"spec\":{\"package\":\"xpkg.crossplane.io/crossplane-contrib/provider-family-aws:$V\"}}"
for p in provider-aws-ec2 provider-aws-eks provider-aws-iam provider-aws-cloudwatchlogs \
         provider-aws-kms provider-aws-sqs provider-aws-cloudwatchevents; do
  kubectl patch providers.pkg.crossplane.io "$p" --type merge \
    -p "{\"spec\":{\"package\":\"xpkg.crossplane.io/crossplane-contrib/$p:$V\"}}"
done
```

`provider-helm` the same way, with its own version
(`xpkg.crossplane.io/crossplane-contrib/provider-helm:<version>`).

Keep every AWS provider on one registry and one family: two family providers
(one from `xpkg.upbound.io`, one from `xpkg.crossplane.io`) fight over the
same `ProviderConfig` CRDs and none becomes healthy (seen on a test cluster).

The platform's build must match the providers:

- `ProviderVersion` in `pkg/crossplane/aws/aws.go` and the Go module
  `github.com/upbound/provider-aws/v2` in `go.mod` are the version the
  controller's types are built from; raise both to `$V` and ship that build
  in step 7.
- The controller's startup install (`--install-providers`) only **creates**
  providers and runtime configs that do not exist. It never changes the
  package or the runtime config of an existing provider, so it does not undo
  this step, with the old build or the new one. The one thing it adds to is
  the activation policy (kinds a newer build needs). **[code]**

### 6. Check the providers

```sh
kubectl get providers.pkg.crossplane.io           # INSTALLED=True HEALTHY=True, the new packages
kubectl get providerrevisions.pkg.crossplane.io   # one Active revision per provider
kubectl get pods -n crossplane-system             # the new provider pods Running
kubectl api-resources --api-group=eks.aws.m.upbound.io >/dev/null && echo "kinds served"
```

Repeat step 4's first command: still nothing unpaused. The new provider
pods must not have acted on anything.

### 7. Resume one cluster first

Deploy the build made for the new provider version (or keep the old one when
only `provider-helm` changed). If you stopped the controller in step 2,
start it:

```sh
kubectl -n cluster-vending-system scale deployment cluster-vending-controller-manager --replicas=1
```

On startup the controller waits (up to five minutes) until every kind it
uses is served, then goes on **[code]**. Resume one cluster of low
importance:

```sh
cluster-vending admin unfreeze --cluster <cluster id>
cluster-vending cluster get <cluster id> --tenant <tenant>
```

It is fine when: the `Frozen` condition shows the cluster is let through, the
cluster is `RUNNING`,
and none of its resources reports an error:

```sh
kubectl get managed -n tenant-<tenant> -l cluster-vending.io/cluster=<cluster id>   # all SYNCED and READY True
```

After a resume a provider observes a resource before it changes it
**[upstream]**. Look at the provider's log for creations it should not make:
upjet has open reports of duplicate or orphaned cloud resources after a
provider restart (crossplane/upjet issues 561, 632 and 304, found in earlier
research for this project and not re-checked for this runbook).

### 8. Unfreeze the rest

```sh
cluster-vending admin unfreeze
kubectl get networks.cloud.cluster-vending.io,clusters.cloud.cluster-vending.io -A
```

Pending automatic changes still follow the rollout tiers and health gates.

## Rollback

While everything is still paused, nothing has changed in the cloud: patch the
providers back to the packages noted in step 1, wait until they are healthy,
then go on from step 7 with the old controller build. After a resume, a
rollback is another upgrade: pause again first.

## What is missing

- **No "pause everything" call**, and no pause of one network: the loop in
  step 3 pauses tenant by tenant.
- **Not run end to end** against real providers.
