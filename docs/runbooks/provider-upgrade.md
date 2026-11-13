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

`AdminService.PauseReconcile` (CLI: `cluster-vending admin pause`) puts the
annotation `crossplane.io/paused: "true"` on managed resources; the provider
of a paused resource stops reconciling it until the annotation is removed
**[upstream]**: "Manually applying the `crossplane.io/paused` annotation
causes the Provider to stop reconciling the managed resource", and
"Kubernetes and Crossplane can't delete resources with a `paused`
annotation" (docs.crossplane.io, Managed Resources, "Paused").

| | |
|---|---|
| Unit | One tenant (all its networks and clusters), or one cluster (`--cluster <id>`). There is no single call for every tenant, and none for one network. **[code]** |
| Covers | Every managed resource kind the platform renders for the AWS providers, and provider-helm's `Release` (a platform component's chart). **[code]** |
| Does not cover | `ProviderConfig`s and `Usage`s (not managed resources), and objects created after the pause (see "The controller during a pause"). **[code]** |
| Stored as | Only the annotation on each object. Who paused and when is in the API server's log line `reconciliation paused state set`, nowhere else. **[code]** |
| Needs | The `PlatformAdmin` role where the API authenticates. **[code]** |
| Repeatable | Yes: pausing or resuming twice changes nothing and returns the same count. **[code]** |

While resources are paused **[code]**:

- the network or cluster carries the condition `Paused=True`
  (`N of M resources are paused`), and each paused resource has
  `paused: true` in `status.managedResources`;
- a paused resource is never a failure: one that was ready stays `READY`,
  one that was not is `WAITING` with the reason `ReconcilePaused`, however
  long the pause lasts. A running cluster stays `RUNNING`;
- a deleted object of the cluster is not created again (the repair is off
  while anything of the owner is paused);
- a stuck Helm release is not recovered: nothing is removed in the workload
  cluster for a paused `Release`;
- a delete requested during the pause is held, not lost: the objects get
  their deletion timestamp, the providers do not act **[upstream]**, and the
  delete continues after the resume;
- a spec update is accepted and stored. Its objects are applied to the
  management cluster, and the providers act on them after the resume.

## The controller during a pause

The pause stops the **providers**. The platform's own controller does not
know about it **[code]**: it keeps applying objects to the management cluster.
For objects that already exist and are paused that changes nothing in the
cloud. But an object the controller creates during the pause is not paused:
a cluster created now, a platform component's chart installed now, a tenant's
`ProviderConfig`. So for a provider upgrade, **stop the controller** for the
duration (step 2). The API server can stay up: it only stores what is asked.

Stopping the controller also holds what else it does by itself: applying
clusters again after a renderer change, tagging subnets, discovering
imports, the deprecation scan, moving `alpha` to a new shipped release.

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

### 2. Stop the controller **[untested]**

```sh
kubectl -n cluster-vending-system scale deployment cluster-vending-controller-manager --replicas=0
```

(A controller run from a terminal: stop it.)

### 3. Pause every tenant **[code]**

A tenant's namespace is `tenant-<tenant id>`.

```sh
for t in $(kubectl get namespaces -o name | sed -n 's|^namespace/tenant-||p'); do
  cluster-vending admin pause --tenant "$t"
done
```

Each call prints how many resources are paused now.

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

### 7. Start the controller, resume one cluster first

Deploy the build made for the new provider version (or scale the old one
back up when only `provider-helm` changed):

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

It is fine when: the `Paused` condition is gone, the cluster is `RUNNING`,
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

- **The controller does not honour a pause.** A pause should be recorded on
  the tenant (and on the cluster for `--cluster`), and the controller should
  then stamp the annotation on every object it creates for that scope, skip
  what it does in the cloud and in workload clusters by itself, and say who
  paused and since when. Until then: stop the controller (step 2).
- **No "pause everything" call**, and no pause of one network.
- **Not run end to end** against real providers.
