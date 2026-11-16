# The component catalog

The components this build ships, as `ComponentDefinition` files in
`pkg/catalog/definitions/` (one per definition). How tenants enable theirs is in
[design/component-usage.md](design/component-usage.md).

## Who controls a component

| `management` | Components | How they reach a cluster |
|---|---|---|
| `Platform` | `aws-load-balancer-controller`, `cluster-autoscaler`, `external-secrets`, `karpenter`, `istio` (a bundle of five charts); `capsule` only when cluster `spec.naas: true`; `kyverno` ships disabled (turn it on with `spec.disabled: false`; `kyverno-policies` is off until then) | Installed automatically on applicable clusters, with fixed values. There is no `Component` object for them, they are not in `ListComponents`, and the Component API refuses their names (`PERMISSION_DENIED`, "installed with the cluster"). |
| `Tenant` (default) | `gpu-operator`, `kyverno-policies` | A tenant enables and disables them on a cluster (a `Component`), and may set the values the schema allows. |

**Platform components are part of the cluster.** Once a cluster is
RUNNING (its node group is ready and its kubeconfig exists), its reconcile
installs them: for each component its IAM role, then its CRD chart, its
chart and its companions; components in `requires` order
(`component.PlatformComponents`), versions from the release the cluster's
channel offered at that moment (`status.platformRelease`). Their state is
on the Cluster: `status.platformComponents` (name, version, phase, message)
and the condition `PlatformComponentsReady`; it does not hold back the
cluster's own Ready. They are removed, in reverse, before the cluster is
deleted. Nothing is installed on an imported cluster before its takeover. The details are in
[design/component-usage.md](design/component-usage.md), section 7.2.

**Taken-over clusters.** An imported cluster is only observed, so it gets
no platform components. Once it is taken over and RUNNING it gets them like
any cluster.

- *It may already run some of them itself* (its own load balancer
  controller, Karpenter, mesh, ...). The platform never deletes or adopts
  what a cluster came with. Its own install of the same component then
  fails next to the existing one (a Helm release or objects of that name
  exist): the component is `Failed` with the provider's error, followed by
  a note that the cluster was taken over. Uninstall the cluster's own and
  the platform's is installed on the next try.
- *To keep the platform components off such a cluster*, take it over with
  `skip_platform_components` (`cluster takeover <id> --skip-platform-components`).
  It is decided at the takeover and stays; there is no switch afterwards
  yet.
- *IRSA.* A taken-over cluster keeps the IAM OIDC provider it was imported
  with, as a carried-over object, while `spec.aws.irsa` stays off (it cannot
  be turned on over that provider). It counts as having IRSA: components
  with an IAM role trust that provider. A cluster with no provider at all
  reports "IRSA is off" for them, as any cluster does.

See what a cluster has, through the API or on the management cluster:

```sh
cluster-vending cluster components <cluster-id> --tenant acme        # -o json for every field
kubectl get cluster prod-eks -n tenant-acme \
  -o jsonpath='{range .status.platformComponents[*]}{.name}{"\t"}{.version}{"\t"}{.phase}{"\t"}{.message}{"\n"}{end}'
```

### When a component fails

A component that is not Ready says why, on the cluster's status
(`GetCluster`, `status.platform_components`) and in `cluster components`:

| Field | What it is |
|---|---|
| `message` | what it waits for, or why it failed, in full |
| `chart` | the Helm release the phase is about: the component's chart, its CRD chart or a companion |
| `helmState` | Helm's state of that release (provider-helm's `status.atProvider.state`): `deployed`, `failed`, `pending-install`, `pending-upgrade`, `pending-rollback`, `uninstalling` |
| `lastError` | provider-helm's last error for it (the Release's Synced condition), uncut |
| `failures` | failed attempts in a row (the Release's `status.failed`) |
| `since` | when the component entered its phase |
| `recoveries` | how often the platform recovered a stuck release of it at this version |

The controller logs each change of phase once, with the cluster and the
component: `platform component failed` (warn, with the message),
`platform component installing`, `platform component ready`,
`platform component removed`, and `platform components condition changed`
when `PlatformComponentsReady` flips. A pass that changed nothing logs
nothing.

**What recovers by itself.**

- A failed install or upgrade is retried by provider-helm (up to the
  Release's `rollbackLimit`, 100).
- **A stuck Helm release.** When an install, upgrade, rollback or uninstall
  is interrupted or its hooks fail, Helm leaves the release's newest
  revision in `pending-install`, `pending-upgrade`, `pending-rollback` or
  `uninstalling`, and every later attempt fails ("has no deployed
  releases", "another operation (install/upgrade/rollback) is in
  progress"): provider-helm has no option that clears it. Once a
  component's Release has been like that for 10 minutes, and Helm has not
  written that revision for 10 minutes, the controller removes the Helm
  record of that one revision in the workload cluster (the Secret
  `sh.helm.release.v1.<release>.v<revision>`), with the kubeconfig the
  platform holds for the cluster. The revision before it is the newest
  again: the deployed one after a stuck upgrade or rollback, none after a
  stuck first install or uninstall, so the chart is installed anew.
  A `deployed`, `superseded` or `failed` revision is never removed, and
  nothing else in the workload cluster is touched. Each recovery is logged
  (`platform component recovered`) and counted in `recoveries`. No
  permission in the management cluster is added for this: the controller
  already reads the kubeconfig Secret through the tenant namespace's
  binding, and the record is removed in the workload cluster.
- A deleted recorded object of the cluster (docs/controller.md).

**What does not.**

- A release that got stuck again after 3 recoveries at one version: the
  component stays Failed and says so. Look at the chart's hook Jobs and at
  whether the default node pool has room (a hook Job preempted on a full
  node is how Kyverno got stuck in testing).
- With `--recover-stuck-releases=false`, or when the workload cluster
  cannot be reached (no kubeconfig Secret, a kubeconfig that runs a
  command): the component is Failed and its message says which.
- A component the cluster's release does not pin, a chart that cannot be
  pulled, a component that needs IRSA on a cluster without it: those are
  not Helm's state, and need the release, the definition or the cluster
  changed.
- Whether the chart's pods run: a component is Ready when Helm deployed
  its charts.

**Recovering a failed component or upgrade.** One command:

```sh
cluster-vending cluster retry --id <cluster>                          # everything that failed
cluster-vending cluster retry --id <cluster> --component kyverno      # only that one
cluster-vending cluster retry --id <cluster> --upgrade                # only the automatic upgrade
```

It answers per item what happens:

| Action | When | What the controller does on its next pass |
|---|---|---|
| `Retry` | a component whose chart failed (any Helm state) | starts its recovery count over; removes a stuck Helm revision at once (no 10-minute wait, past the 3-recovery limit); annotates its Release (`cloud.cluster-vending.io/retried-at`) so provider-helm tries now; event `ComponentRetried` |
| `Retry` | a Failed or TimedOut automatic upgrade (`status.rollout`) | the upgrade is in progress again from now, with a new `upgrade-timeout`; it no longer counts against the channel's `maxFailed` (a halted channel resumes when no failed cluster is left); event `UpgradeRetried` |
| `Fix` | a component that failed before it had a chart (IRSA off, a requirement missing, not in the release) | nothing: change what its message says; it installs by itself afterwards |
| `Nothing` | it has not failed | nothing |

`cluster get` lists, under the platform components, a `Recovery` line per
failed one, and `component list --details` the same: the exact `cluster
retry` command, or what to fix first. The request is an annotation of the
Cluster (`cloud.cluster-vending.io/retry`), merged with one not yet acted
on and removed once acted on; it changes no etag. A chart that keeps
failing the same way (a hook that never completes, a node pool with no
room) fails again: retrying is for what changed since, or what was
transient (a network error to the workload cluster).

**Bundles.** `istio` has no chart of its own: `includes` lists six
definitions marked `internal: true`, each with its chart, namespace,
version pin and `requires`. The members are installed in `requires` order
(`component.BundleOrder`), each once the one before is Ready, and removed
in reverse; the bundle is Ready when every member is. A release pins every
member of a bundle or none. Tenants never see internal definitions, and
cannot enable one by itself.

**Karpenter and the cluster autoscaler both run on every cluster.** They do
not compete because platform pods select the cluster's default node pool
and tenant pods are kept off it: the cluster autoscaler only grows the
default managed node group, for platform pods, and Karpenter launches nodes
for everything else. Karpenter's NodePool puts none of the default pool's
labels on its nodes. **This split relies on the default node pool being
tainted**: without a taint, a pending tenant pod fits both, and both may
add a node for it.

## One definition per cloud

Every definition is for exactly one cloud: `spec.provider` is `aws`, `gcp`
or `azure` (the cluster's provider). It is immutable, and defaults to
`aws`, the cloud every definition was for before the field existed. Every
definition this build ships is `provider: aws`, the cloud-neutral ones
(`kyverno`, `kyverno-policies`) too: their values and names are written
for EKS, and a component for another cloud is always its own definition.
**No GCP or Azure definition is shipped.**

A variant for another cloud is a separate definition named
`<name>-<provider>`: `kyverno-gcp`, `gpu-operator-gcp`, `istio-gcp`. A
bundle's variant has its own members, named after it
(`istio-gcp-base` for `istio-base`). The AWS definitions keep their
names: renaming one would make clusters uninstall it.

What the provider decides:

| Where | What happens |
|---|---|
| platform installer | a cluster gets the Platform definitions for its cloud. One installed already whose definition is now for another cloud is kept and logged, never uninstalled. |
| tenant installer | a tenant component for another cloud is not installed; its status is `FAILED` with `not available on <provider> clusters: <name> is for <p>` |
| `EnableComponent` | refused (`FAILED_PRECONDITION`) for a definition of another cloud, naming both |
| `component catalog` | a `PROVIDER` column; with `--cluster`, only that cluster's cloud |
| `component list --cluster` | a tenant component for another cloud is not listed as `not enabled` |

The catalog check (`CheckCatalog`) refuses, and marks not Valid:

- a bundle whose member is for another cloud than the bundle;
- a definition that requires one for another cloud;
- two definitions for the same cloud that install the same chart into the
  same namespace: a cluster would get both.

### Adding a GCP variant

1. Copy the AWS definition to `<name>-gcp.yaml`, set `metadata.name:
   <name>-gcp` and `spec.provider: gcp`.
2. Replace what is AWS-specific: IAM roles (`cloudResources.iamRole`), AWS
   placeholders in `platformValues` (`${account.id}`, the IRSA role ARN),
   `conflictsWithAddons` naming EKS add-ons.
3. Point `requires` at the GCP definitions (`kyverno-policies-gcp` requires
   `kyverno-gcp`); a bundle's `includes` at its `-gcp` members.
4. Pin `<name>-gcp` in the releases that offer it: a release pins by
   definition name.
5. Apply it (`kubectl create -f`) or ship it in `pkg/catalog/definitions/`.

## How the catalog reaches the management cluster

- **By the controller (default).** On startup, after it installs the CRDs,
  the controller syncs the shipped definitions (`--install-catalog`, default
  `true`; `pkg/catalog` `Sync`). It labels what it ships
  `cloud.cluster-vending.io/shipped: "true"` and records the hash of the
  spec it shipped in `cloud.cluster-vending.io/shipped-hash`, so it can tell
  an untouched definition from one the platform team edited:

  | On the management cluster | What a new build does |
  |---|---|
  | missing | creates it, labelled, with the hash |
  | shipped, spec still has the recorded hash, build's spec differs | replaces the spec with the build's (whole: fields the new file dropped go), keeps `disabled`, records the new hash |
  | shipped, unedited, same as the build | nothing |
  | shipped, spec changed since (hash differs) | nothing: the platform team's; logged |
  | not labelled, equal to the build's spec | adopts it: adds the label and hash |
  | not labelled, different | nothing: logged with how to take the build's |
  | shipped, unedited, no longer in the build | deletes it (the in-use guard holds it while clusters use it) |
  | shipped, edited, no longer in the build | nothing |
  | the platform team's own (not labelled, not in the build) | nothing |

  `disabled` is not part of the hash: switching a shipped definition off
  does not stop later builds from updating the rest of it. To take a newer
  shipped version of an edited definition, replace it with the file
  (`kubectl replace -f pkg/catalog/definitions/<name>.yaml`; `apply` keeps
  fields the new file dropped) or delete it and restart the controller.
- **By hand.** `--install-catalog=false`, then
  `kubectl create -f pkg/catalog/definitions/` once the CRDs are installed.
  The catalog is not part of any kustomization.

## Disabling and removing a component

**Disable** a component without deleting its definition:

```sh
kubectl patch componentdefinitions.cloud.cluster-vending.io istio --type merge -p '{"spec":{"disabled":true}}'
```

- **Platform component:** no longer installed; every cluster that has it
  removes it on its next pass.
- **Tenant component:** cannot be enabled any more. Components already
  enabled stay until the tenant disables them.
- **What depends on it** is off with it: the members of a disabled bundle,
  and every definition that requires it. The catalog shows each as off,
  with why.
- **Kept across builds:** a new build that updates the shipped definition
  keeps `disabled` as the platform team set it.
- Turn it back on with `"disabled": false`.

**Remove** a definition. Each one carries the
`cloud.cluster-vending.io/definition-in-use` finalizer, and its `InUse`
condition says what holds it:

```sh
kubectl delete componentdefinitions.cloud.cluster-vending.io external-dns
kubectl get componentdefinitions.cloud.cluster-vending.io external-dns \
  -o jsonpath='{.status.conditions[?(@.type=="InUse")].message}'
```

| What holds the delete | What to do |
|---|---|
| a tenant's Component of it | the tenant disables it; until then it stays |
| a cluster with objects of it still in the management cluster (its Helm Releases or IAM Role) | nothing: a definition being deleted is off, so the cluster uninstalls it; the objects go when the uninstall is done, and the delete finishes on its own |
| the same, on a paused cluster | resume the cluster; the condition says who paused it and since when |
| its bundle | delete or disable the bundle first |
| a definition that requires it | delete or change that one first |

What a cluster holds is read from the component's objects themselves, not
from the cluster's status, so a cluster that stops reporting does not hold
a delete for ever. Clusters that can never release do not hold: one that is
being deleted (it removes its components with it), and one that no longer
exists.

The removal procedure for a Platform component:

1. **Disable** it (`"disabled": true`) and wait: every cluster uninstalls
   it on its next pass.
2. **Delete** the definition. The delete finishes once no cluster has its
   objects any more.
3. **Last resort**, when a cluster cannot uninstall (its workload cluster
   is gone, its Helm releases are stuck): annotate the definition with
   exactly `cloud.cluster-vending.io/force-delete: "true"`. The finalizer
   goes, after a warning event and a log line that name every holder it
   ignored.

   ```sh
   kubectl annotate componentdefinitions.cloud.cluster-vending.io external-dns cloud.cluster-vending.io/force-delete=true
   ```

   What it installed stays on those clusters, orphaned. To clean up, delete
   the objects labelled with the component in the tenant's namespace on the
   management cluster (their providers then uninstall the charts and delete
   the role, if they still can):

   ```sh
   kubectl delete releases.helm.m.crossplane.io,roles.iam.aws.m.upbound.io \
     -n tenant-acme -l cluster-vending.io/component=external-dns
   ```

   Any other value of the annotation is ignored, and the `InUse` condition
   says so.

So for a Platform component, deleting is how it is removed from every
cluster: the delete waits until no cluster has it. To stop it everywhere
without deleting the definition, disable it instead.

A shipped definition that is deleted comes back when the controller
restarts (it is in the build); disable it to keep it off.

## Changing the catalog without a build

The controller reads the catalog live, so the platform team can change it
with `kubectl` alone:

- **Add a component:** apply a `ComponentDefinition` (chart, namespace,
  `management`, values, and an IAM role if it needs one), then publish a
  `Release` that pins its version and point `alpha` at it
  ([release-admin](design/release-admin.md)). New clusters install it; a
  cluster already on an older release reports it "not in release" until it
  moves.
- **Disable one:** `spec.disabled: true` (above).
- **Remove one:** delete its definition (above).
- **Take a newer shipped version of a definition you edited:**
  `kubectl replace -f pkg/catalog/definitions/<name>.yaml`, or delete it and
  restart the controller.

What still needs code:

- **AWS resources other than an IAM role.** A component that needs a queue,
  a bucket or a DNS zone cannot describe it in a definition.
- **New placeholders.** `${cluster.name}`, `${iam.roleArn}` and the others
  are a fixed list in `pkg/component/placeholders.go`.
- **Kyverno's excluded namespaces.** A component added at runtime installs
  into a namespace Kyverno does not skip; add it to the `kyverno` and
  `kyverno-policies` definitions by hand (for shipped components a test
  checks this).

## Changing a version

Versions are data, in one file: `pkg/catalog/release.yaml`. It lists the
Kubernetes minors on offer (default, upgrade target, end of support), the
chart version of every component, per-minor Helm values (the cluster
autoscaler's image tag), and the channels a new management cluster gets. To
change a version, edit that file and ship the build: the controller turns it
into the shipped `Release` (named after its content, so a change gives a new
one) and moves `alpha` to it unless an operator took `alpha` over. No
controller code names a version. A test fails when the file and the
definitions disagree about which components exist.

Chart versions are not in the definitions. A `Release` pins them per
Kubernetes minor; the versions below are what each definition was checked
against.

### Kubernetes minors and channels

Each minor in `release.yaml` names the channel whose clusters run it:

| Minor | Channel | Default (cluster on no channel) | Upgrade to |
|---|---|---|---|
| 1.37 | `alpha` | yes | |
| 1.36 | `beta` | | 1.37 |
| 1.35 | `stable` | | 1.36 |

- Each of `alpha`, `beta` and `stable` is on exactly one minor, `alpha`'s
  the newest and `stable`'s the oldest. The file and every published
  `Release` are checked for this (a `Release` may also put no minor on any
  channel, as releases published before channels did). Each
  `ReleaseChannel` shows its minor in `status.kubernetesMinor`
  (`kubectl get releasechannels`, column `KUBERNETES`).
- **Moving a channel is editing one line.** Adding 1.38 as `alpha` moves
  only alpha: put `channel: alpha` on 1.38, `beta` on 1.37, `stable` on
  1.36 when you want those channels to move too. Nothing moves a channel
  except that edit.
- A minor on no channel (say 1.35, once `stable` is on 1.36) stays
  supported: clusters keep running it. Its users are warned to move off it
  (`status.kubernetes.ending`, the `KubernetesEnding` event, and a
  `WARNING:` line in `cluster get`); it is not removed and no cluster is
  upgraded because of it.
- A cluster created on a channel without a version gets its channel's
  minor, and the API refuses a newer one than the channel's
  (`idl/README.md`). A cluster on no channel keeps the version it asks for,
  or EKS's default: `default: true` is not applied to it yet. (Automatic
  upgrades to the channel's minor come in later increments; see
  `docs/design/upgrades.md`.)
- A `Release` offers three to six minors, consecutive.

## The components

| Component | Chart (version checked) | Namespace | Tenant values | CRD chart |
|---|---|---|---|---|
| `aws-load-balancer-controller` | `aws-load-balancer-controller` 3.6.2, `https://aws.github.io/eks-charts` | `kube-system` | none: fixed | none: the chart installs its own CRDs once |
| `cluster-autoscaler` | `cluster-autoscaler` 9.60.0, `https://kubernetes.github.io/autoscaler`; image tag from the release pin | `kube-system` | none: fixed | none |
| `external-secrets` | `external-secrets` 2.15.0 (External Secrets Operator v2.15.0), `https://charts.external-secrets.io` | `external-secrets` | none: fixed | none: the chart installs and upgrades its own CRDs (`installCRDs`) |
| `capsule` | `capsule` 0.15.2, `oci://ghcr.io/projectcapsule/charts` | `capsule-system` | none: fixed | Existing cert-manager required; the chart installs/upgrades its CRDs through its lifecycle hook. [Values and operation](components/capsule.md). |
| `karpenter` | `karpenter` 1.15.1, `oci://public.ecr.aws/karpenter`; no node class or node pool is shipped | `kube-system` | none: fixed | none: the chart installs its own CRDs once |
| `kyverno` | `kyverno` 3.10.0 (Kyverno v1.20), `https://kyverno.github.io/kyverno/` | `kyverno` | none: fixed | none needed (subcharts, upgraded by Helm) |
| `kyverno-policies` | `kyverno-policies` 3.10.0; requires `kyverno` | `kyverno` | yes | none |
| `gpu-operator` | `gpu-operator` v26.7.2, `https://helm.ngc.nvidia.com/nvidia` | `gpu-operator` | yes | none (the chart's pre-upgrade hook applies them) |

`istio`, in ambient mode (no sidecars), is a bundle of five internal
definitions, all Istio's own charts:

| Component | Chart (version checked) | Namespace | Requires |
|---|---|---|---|
| `istio-base` | `base` 1.32.0, `https://blob.istio.io/istio-release/charts` | `istio-system` | |
| `istiod` | `istiod` 1.32.0, profile `ambient` | `istio-system` | `istio-base` |
| `istio-cni` | `cni` 1.32.0, profile `ambient` (DaemonSet) | `istio-system` | `istiod` |
| `ztunnel` | `ztunnel` 1.32.0 (DaemonSet) | `istio-system` | `istio-cni` |
| `istio-ingress-gateway` | `gateway` 1.32.0 (Istio's gateway chart): an Envoy Deployment behind an internal NLB | `istio-ingress` | `istiod`, `aws-load-balancer-controller` |

All five are fixed: no tenant values. None needs a CRD chart: `istio-base`
renders Istio's CRDs as templates, kept on uninstall.

**Tenant values.** Only `gpu-operator` and `kyverno-policies` have a values
file and a schema. Every Platform component runs as the platform set it:
the values a tenant could once tune are in `platformValues`, at the
researched defaults. Tests (`pkg/catalog`) hold that exactly those two
are tenant components and accept overrides.

All three Kubernetes minors the shipped release offers (1.35 to 1.37) are
supported by the versions above. The GPU operator's pin carries the leading
`v` (`v26.7.2`).

## Where the pods run

Platform pods run on the cluster's default node pool. Each definition sets
`${system.nodeSelector}` (the pool's labels) and `${system.tolerations}` (a
toleration per taint of the pool) at its chart's own keys, checked in each
chart's `values.yaml`:

| Component | Keys |
|---|---|
| `aws-load-balancer-controller`, `cluster-autoscaler`, `karpenter`, `istiod` | `nodeSelector`, `tolerations` |
| `kyverno` | `global.nodeSelector`, `global.tolerations` (all controllers and hook Jobs) |
| `external-secrets` | `global.nodeSelector`, `global.tolerations` (the controller, webhook and cert-controller Deployments fall back to them) |
| `capsule` | Root `nodeSelector` / `tolerations` for the controller, `global.jobs.kubectl.nodeSelector` / `tolerations` for lifecycle hooks |
| `istio-ingress-gateway` | `nodeSelector`, `tolerations` of Istio's gateway chart |
| `gpu-operator` | `operator.nodeSelector`, `operator.tolerations` (the operator's Deployment only) |

Not placed, on purpose or for lack of a verified key:
- `istio-cni` and `ztunnel` are DaemonSets that must run on every node;
  their charts' default tolerations already accept every taint.
- The GPU operator's operands keep the chart's `nvidia.com/gpu` toleration:
  a placeholder is a whole value and cannot be added to a list. Its Node
  Feature Discovery master is a subchart whose keys were not checked.
- `kyverno-policies` (hook Jobs) is not placed.

Karpenter keeps its affinity (off the nodes it launches, on a managed node
group) next to the selector.

## CRD charts

Helm installs a chart's `crds/` directory once and never upgrades it. A
definition can name a chart that holds only the CRDs, as templates
(`install.helm.crds`): it is installed and upgraded before the main chart, at
the version the release pins for the component, and the main chart then runs
with `skipCRDs`.

- `karpenter`: no CRD chart. The upstream chart installs the CRDs in its
  `crds/` folder with the first install, and Helm never upgrades them: a
  version that changes them (1.15 changed the NodePool CRD) needs that
  version's CRDs applied to the cluster first. Upstream ships them as the
  `karpenter-crd` chart for that purpose.
- `aws-load-balancer-controller`: no CRD chart. The upstream chart installs
  the CRDs in its `crds/` folder with the first install, and Helm never
  upgrades them: when a new chart version changes them, apply upstream's
  `crds.yaml` for that version to the cluster first (the controller's
  upgrade notes say when).
- `gpu-operator`: NVIDIA ships no CRD chart; `operator.upgradeCRD` (the
  default) runs a pre-upgrade hook that applies them.

Every chart comes from its upstream repository. The platform publishes
no chart of its own, and a test fails if a definition names the platform's
registry.

## What the platform does not install

**Karpenter's node class and node pool.** Karpenter runs, but launches
nothing until an `EC2NodeClass` and a `NodePool` exist: no upstream chart
ships them, so the cluster's users create them. A starting point, with the
cluster's node role and the tags the platform puts on the cluster's subnets
and EKS on its security group (`<cluster>` is the EKS name, the node role
is `<prefix>-node-role`):

```yaml
apiVersion: karpenter.k8s.aws/v1
kind: EC2NodeClass
metadata:
  name: default
spec:
  role: <prefix>-node-role
  amiSelectorTerms:
  - alias: al2023@v20261107        # a pinned EKS AMI release; "latest" replaces nodes on every release
  subnetSelectorTerms:
  - tags: {karpenter.sh/discovery: <cluster>}
  securityGroupSelectorTerms:
  - tags: {aws:eks:cluster-name: <cluster>}
  metadataOptions: {httpEndpoint: enabled, httpPutResponseHopLimit: 1, httpTokens: required}
---
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: default
spec:
  template:
    spec:
      nodeClassRef: {group: karpenter.k8s.aws, kind: EC2NodeClass, name: default}
      expireAfter: 720h
      requirements:
      - {key: kubernetes.io/arch, operator: In, values: [amd64]}
      - {key: kubernetes.io/os, operator: In, values: [linux]}
      - {key: karpenter.sh/capacity-type, operator: In, values: [on-demand]}
      - {key: karpenter.k8s.aws/instance-category, operator: In, values: [c, m, r]}
      - {key: karpenter.k8s.aws/instance-generation, operator: Gt, values: ["2"]}
  limits: {cpu: "100", memory: 400Gi}
  disruption:
    consolidationPolicy: WhenEmptyOrUnderutilized
    consolidateAfter: 1m
```

**The Gateway API CRDs.** Kubernetes publishes them as a manifest, not as a
Helm chart, so the platform does not install them. The mesh (mTLS and L4
policy through ztunnel) and the ingress gateway work without them. Waypoints,
which ambient mode needs for anything at L7, and `Gateway`/`HTTPRoute`
ingress need them: install the version that goes with the Istio release
(v1.6.1 for Istio 1.32):

```sh
kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.6.1/standard-install.yaml
```

## Using External Secrets

The operator (two replicas of the controller and of the cert-controller with
leader election, two of the webhook, a PodDisruptionBudget each) runs
**without any AWS credentials**: the platform gives it no IAM role. A store
names the service account whose IAM role it reads with, so each team's
access is its own role, not the operator's (external-secrets.io, "AWS
Authentication": "The big advantage of this approach is that ESO runs
without any credentials").

What the cluster's users set up:

1. The cluster has `spec.aws.irsa: true` (its IAM OIDC provider; the
   platform's own IAM components need it too).
2. An IAM role that trusts the cluster's OIDC provider for the service
   account below, with a policy on the secrets it may read, for example
   `secretsmanager:GetSecretValue`, `DescribeSecret`, `GetResourcePolicy` and
   `ListSecretVersionIds` on `arn:aws:secretsmanager:<region>:<account>:secret:<prefix>-*`.
3. The service account, the store and an ExternalSecret:

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: secrets-reader
  namespace: team-a
  annotations:
    eks.amazonaws.com/role-arn: arn:aws:iam::111122223333:role/team-a-secrets
---
apiVersion: external-secrets.io/v1
kind: SecretStore
metadata:
  name: aws-secrets-manager
  namespace: team-a
spec:
  provider:
    aws:
      service: SecretsManager        # ParameterStore for SSM Parameter Store
      region: us-west-1
      auth:
        jwt:
          serviceAccountRef:
            name: secrets-reader     # a ClusterSecretStore also names its namespace
---
apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata:
  name: database
  namespace: team-a
spec:
  refreshInterval: 1h0m0s
  secretStoreRef: {name: aws-secrets-manager, kind: SecretStore}
  target: {name: database, creationPolicy: Owner}
  data:
  - secretKey: password
    remoteRef: {key: team-a-database, property: password}
```

- `serviceAccountRef` does not work with EKS Pod Identity: the operator
  cannot act as a service account whose role is bound that way
  (external-secrets.io). Use IRSA for the store's service account.
- A `ClusterSecretStore` lets every namespace read with one role. Nothing
  in the platform restricts who may create one; Kyverno policies can.
- **Versions.** The project's support table lists 2.15 for Kubernetes 1.37
  only; it names one Kubernetes version per release (1.36 for 2.9 to 2.13,
  1.35 for 2.7 and 2.8, all past their end of life). The shipped release
  pins 2.15.0 on 1.35, 1.36 and 1.37; the chart itself accepts Kubernetes
  1.19 and later. Not installed on a real cluster yet.
- **Release size.** Helm keeps a release as a Secret of at most 1 MiB. With
  these values the 2.15.0 release record is 527,971 bytes gzipped (about
  704 KB as stored), measured with a client-side dry run. It fits; the
  chart's `crds.create*` switches drop CRDs if a later version grows past
  the limit.

## Versions that move together, and upgrade order

- **Version groups** (`versionGroup`): `kyverno` and `kyverno-policies` are
  group `kyverno`; `istio-base`, `istiod`, `istio-cni`, `ztunnel` and
  the ingress gateway are group `istio`. A release that pins members of a
  group at different versions on one Kubernetes minor is not Valid.
- **One minor at a time** (`upgradeOneMinorAtATime`): the `istio` group
  members. A channel does not move to a release whose pin skips a
  minor version of such a component relative to the release it offers now
  (1.31 to 1.32 is fine, 1.30 to 1.32 is not).
- **Order**: components are installed and upgraded in `requires` order
  (`release.UpgradeOrder`): `kyverno` before `kyverno-policies`;
  `istio-base`, then `istiod`, `istio-cni`,
  `ztunnel`, and the ingress gateway after `istiod`.

## Values a release sets per Kubernetes minor

A pin may carry `values` (`ComponentPin.values`): Helm values for that
component on that Kubernetes minor. They are merged over the definition's
`platformValues` (the pin wins), locked for tenants the same way, and may
use placeholders. A pin whose values are not an object, or name an unknown
placeholder, makes the release not Valid.

The cluster autoscaler needs it: its minor version must equal the cluster's
Kubernetes minor, the chart hardcodes one image per chart release, and no
chart ships an image for 1.36. The shipped release pins chart 9.60.0 on
every minor with `image.tag` `v1.37.0`, `v1.36.2` and `v1.35.3`; a test
holds that a pin's image minor is the minor it is pinned under.

## Chart versions per Kubernetes minor

`components:` in `pkg/catalog/release.yaml` is every component's chart
version on every minor. A minor's own `components:` overrides it on that
minor only, for a version that does not run there, or one the channel on
that minor keeps while the others move on:

```yaml
kubernetes:
- minor: "1.35"
  channel: stable
  components: {karpenter: "1.14.2"}   # stable's clusters stay on 1.14.2
components:
  karpenter: "1.15.1"                 # every other minor
```

An override must name a component under `components:` and give a version.
A version group stays one version per minor: overriding `istiod` alone
makes the release not Valid (override every `istio-*` member together).
`admin release get --id <release>` lists the versions minor by minor.

## Caveats

**Cluster autoscaler**
- It only resizes existing managed node groups between their minimum and
  maximum size; it never changes instance types.
- The platform sets a node group's desired size at creation only
  (`DesiredSizeAtCreateOnly`), so it does not undo the autoscaler's changes.
- EKS tags a managed node group's Auto Scaling group for auto-discovery
  itself. The IAM policy allows changes only where the tag
  `k8s.io/cluster-autoscaler/<cluster>` has the value `owned`: **verify that
  value on a real Auto Scaling group before release.**
- One node group across several zones can scale up in the wrong zone for a
  pod bound to a zonal volume.
- Its resource requests (100m CPU, 300Mi; 600Mi memory limit) are the
  platform's choice; the chart sets none.

**Karpenter**
- The node class uses a pinned AMI release (`al2023@v20261107`), not
  `al2023@latest`, which would replace every Karpenter node on each AMI
  release. The platform bumps the pin by changing the definition.
- The default NodePool is on-demand only. Spot needs the
  `AWSServiceRoleForEC2Spot` service-linked role, which the single IAM role a
  definition can declare does not cover, and there is no interruption queue.
- Nodes reuse the managed node group's role; no extra access entry is needed.
- Two controller replicas need at least two managed node group nodes.
- Karpenter manages the instance profile through the IAM API, so private
  subnets need outbound internet (NAT).

**AWS Load Balancer Controller**
- The Service mutating webhook is off: a Service opts in with
  `spec.loadBalancerClass: service.k8s.aws/nlb`.
- The Gateway API feature gates need the upstream Gateway API CRDs, which
  nothing in the catalog installs.
- Webhook port 9443 must be reachable from the control plane; custom node
  security groups must allow it.

**Kyverno**
- Disabling `kyverno` deletes every policy and PolicyException: the chart's
  CRDs are not kept on uninstall.
- Kyverno 1.20 no longer serves the old policy kinds (ClusterPolicy and the
  like) and refuses to install or upgrade while any exist.
- The four legacy policy CRDs are not installed (`crds.groups.kyverno`
  `clusterpolicies`, `policies`, `clustercleanuppolicies`,
  `cleanuppolicies` off): tenants cannot create `ClusterPolicy`, `Policy`,
  `ClusterCleanupPolicy` or `CleanupPolicy` objects; `kyverno-policies` uses
  the `policies.kyverno.io` kinds. With them, Kyverno's Helm release record
  is 93% of the 1 MiB a Secret may hold; without, about 57% (measured on chart
  3.10.0). On a cluster that already has them, the next apply removes those
  four CRDs and with them every object of those kinds.
- Kyverno skips `kube-system`, `kube-public`, `kube-node-lease`, `kyverno`
  and every namespace a catalog component installs into (`gateway-system`,
  `gpu-operator`, `istio-ingress`, `istio-system`). A test fails when a
  new component's namespace is missing from the three lists.

**GPU operator**
- For GPU nodes on the EKS-optimized AL2023 NVIDIA AMI only: the driver and
  the container toolkit come from the AMI and are off in the operator.
  Bottlerocket needs a definition of its own (its device plugin must be off
  too). NVIDIA's support matrix does not list the AL2023 AMI; the setup
  follows AWS's documentation.
- GPU nodes come from a node group or a Karpenter NodePool with GPU instance
  types, tainted `nvidia.com/gpu`.
- Do not run a standalone Node Feature Discovery or NVIDIA device plugin
  next to it; a definition cannot declare a conflict with another component.
- An upgrade has not been tested on a real cluster.

**Istio (ambient)**
- A tenant adds a namespace to the mesh with the label
  `istio.io/dataplane-mode=ambient`; no pod restart is needed. Sidecar
  injection is not supported.
- Waypoints (L7 routing and authorization) are the tenant's: a `Gateway` of
  class `istio-waypoint` in their namespace.
- Tenants reach the ingress with an `HTTPRoute` (port 80) or a
  `ListenerSet` (their own hostnames and certificates) that names the
  Gateway `ingress` in `istio-ingress`. It is internal only; an
  internet-facing ingress needs another definition with
  `loadBalancer.scheme: internet-facing` and another `name`.
- An Istio upgrade restarts ztunnel on each node (one node at a time), which
  briefly interrupts ambient traffic on that node. istio-cni has no canary
  upgrade either: while it restarts on a node, new pods there wait.
- When pods use their own security groups (VPC CNI `ENABLE_POD_ENI`), the
  `vpc-cni` add-on must run with `POD_SECURITY_GROUP_ENFORCING_MODE=standard`
  or kubelet probes to ambient pods fail. A definition cannot require an
  add-on setting, so nothing checks this.
- `istio-system` runs privileged DaemonSets. A definition cannot set
  namespace labels, so if Pod Security levels are ever enforced, the
  namespace needs `pod-security.kubernetes.io/enforce=privileged`.
- A tenant's own `Gateway` of class `istio` also gets a LoadBalancer
  Service, without the internal annotations; nothing stops that yet.

**Autoscaling.** A component that uses a HorizontalPodAutoscaler (istiod,
the ingress gateway) needs the `metrics-server` EKS add-on; a definition cannot require an
add-on.

None of the components has been installed on a real cluster through the
platform yet: the installer (the component controller) is not written.
