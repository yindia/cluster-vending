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

## What a pause is

`AdminService.PauseReconcile` (CLI: `cluster-vending admin pause --tenant <t>
[--network <id> | --cluster <id>] [--reason "..."] [--until <time>]`, or
`--channel <c> [--tier <n>]` for a channel's clusters; docs/controller.md,
"Admin: pause and resume") does two things **[code]**:

1. **It records the pause on the platform's own objects**: the tenant's
   `CloudAccount`s, `Network`s and `Cluster`s (only the `Cluster` with
   `--cluster`) and the tenant's namespace get the annotation
   `cloud.cluster-vending.io/paused`, a JSON value with who paused (the
   caller's name, `anonymous` without authentication), since when, and the
   reason. The platform's controller does not act on what carries it, and
   what is created in a paused tenant carries it from the start.
2. **It pauses the providers**: every managed resource of the scope, and
   provider-helm's `Release`s of its platform components, get
   `crossplane.io/paused: "true"`; the provider of a paused resource stops
   reconciling it until the annotation is removed **[upstream]**:
   "Manually applying the `crossplane.io/paused` annotation causes the
   Provider to stop reconciling the managed resource", and "Kubernetes and
   Crossplane can't delete resources with a `paused` annotation"
   (docs.crossplane.io, Managed Resources, "Paused").

`ResumeReconcile` removes both, in the same order.

| | |
|---|---|
| Unit | One tenant (its accounts, networks and clusters), one network (`--network <id>`: the network and its own managed resources, not its clusters), or one cluster (`--cluster <id>`). Resuming one network or cluster of a paused tenant leaves the rest paused. There is no single call for every tenant. **[code]** |
| Network scope | A network's pause is the network's alone: its clusters keep running. What of a cluster needs the network to change (new subnet tags, say) waits until the network is resumed; deleting one of its clusters is not held. **[code]** |
| Scopes | A row records each scope it was paused at. A tenant's resume removes the tenant's scope only: a network or cluster also paused on its own stays paused, with the providers' pause still on its managed resources. Its own resume ends it, whatever else holds it. A pause written by an older build records no scope, and any resume ends it. **[code]** |
| Covers | Our objects as above; every managed resource kind the platform renders for the AWS providers, and provider-helm's `Release`. **[code]** |
| Does not cover | `ProviderConfig`s and `Usage`s: they are not managed resources. Crossplane v2.4.3's Usage controller has no pause handling, and a Usage does nothing to the cloud: it only blocks deletes. **[code]**, **[upstream]** source read |
| Stored as | The two annotations. Who, when and why are in `status.pause` of a paused account, network or cluster and in its `Paused` condition, and in the message of a network or cluster. A paused account's own `CloudAccount` object has the `Paused` condition too. **[code]** |
| Needs | The `PlatformAdmin` role where the API authenticates. **[code]** |
| Repeatable | Yes: a paused object keeps who paused it first, and pausing or resuming twice returns the same count. **[code]** |

What the platform's controller does with a paused object **[code]**:

| Loop | While paused |
|---|---|
| Cloud account | Its `ProviderConfig`, credentials copy and kubeconfig-reader binding are not applied; a delete is held. |
| Network | Nothing applied, repaired or deleted; import discovery (an AWS lookup) does not run. Status recorded. |
| Cluster | Nothing applied, repaired or deleted; no subnet tagging, no import discovery, no EKS name or version lookup, no node group retirement. Status recorded. |
| Platform components | Not applied; no stuck-release recovery (nothing is removed in the workload cluster). |
| Deprecation scan | Does not run: it reads the workload cluster. |
| Spec update, delete | Accepted by the API and stored; done by the first pass after the resume, once. |
| New cluster or network in a paused tenant | Stored, paused from the start; `PROVISIONING` with "paused by …" until the resume. |

The status of a paused network or cluster **[code]**:

- `status.pause` says who, since when and why, and the condition `Paused=True`
  says it too; the message ends with `paused by <who> since <time> (<reason>)`;
- a paused resource is never a failure: one that was ready stays `READY`,
  one that was not is `WAITING` with the reason `ReconcilePaused`, however
  long the pause lasts. A running cluster stays `RUNNING`;
- the events `Paused` and `Resumed` are emitted when it starts and ends
  (`cluster events <id>`).

## What a tenant pause does not stop

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

It can stay up: paused tenants are only observed **[code]**. To be
conservative, stop it instead and start it again in step 7:

```sh
kubectl -n cluster-vending-system scale deployment cluster-vending-controller-manager --replicas=0
```

### 3. Pause every tenant **[code]**

A tenant's namespace is `tenant-<tenant id>`.

```sh
for t in $(kubectl get namespaces -o name | sed -n 's|^namespace/tenant-||p'); do
  cluster-vending admin pause --tenant "$t" --reason "provider upgrade to $V"
done
```

Each call prints how many resources are paused now. A tenant created after
this loop is not paused: run the loop again just before step 5.

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
cluster-vending admin resume --tenant <tenant> --cluster <cluster id>
cluster-vending cluster get <cluster id> --tenant <tenant>
```

It is fine when: the `Paused` condition and `status.pause` are gone, the
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

### 8. Resume the rest

One tenant, check, then all:

```sh
cluster-vending admin resume --tenant <tenant>
for t in $(kubectl get namespaces -o name | sed -n 's|^namespace/tenant-||p'); do
  cluster-vending admin resume --tenant "$t"
done
kubectl get networks.cloud.cluster-vending.io,clusters.cloud.cluster-vending.io -A
```

## Rollback

While everything is still paused, nothing has changed in the cloud: patch the
providers back to the packages noted in step 1, wait until they are healthy,
then go on from step 7 with the old controller build. After a resume, a
rollback is another upgrade: pause again first.

## What is missing

- **No "pause everything" call**, and no pause of one network: the loop in
  step 3 pauses tenant by tenant.
- **Not run end to end** against real providers.
