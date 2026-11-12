# Cluster components

Status: design, not implemented. Nothing in this document exists in code
except where a file or identifier is named.

Each section marks its statements as **Decided** (by the project owner, not
reopened here), **Proposed** (this document's recommendation) or **Open**
(needs an answer; collected in section 14).

How versions reach clusters over time is a separate design:
[rollouts.md](rollouts.md).

Terms used below:

- **Component.** Software the platform installs into a tenant's cluster on
  request: Karpenter, Kyverno, Istio and so on.
- **EKS add-on.** A component AWS installs and upgrades through the EKS API.
  The platform already manages these (`spec.aws.addons`).
- **Catalog.** The list of components the platform offers, kept by the
  platform operator.
- **Values.** The configuration of one installation, in the form a Helm
  chart or an EKS add-on accepts.
- **Management cluster.** The cluster the platform itself runs in. A
  tenant's cluster is a *workload cluster*.

How an operator locks component versions in releases and channels: [release-admin.md](release-admin.md).

How a tenant enables a component and overrides its values: [component-usage.md](component-usage.md).

## 1. Summary

A component is its own resource, owned by one cluster. A tenant enables it
with an API call, optionally with override values, and disables it by
deleting it. The platform operator keeps a catalog entry per component that
says which versions go with which Kubernetes version, what the component
depends on, and which values a tenant may override. How a component is
installed into the workload cluster is decided: Helm charts through the
Crossplane Helm provider (section 9).

## 2. What is decided

**Decided.**

- Components are opt-in. A new cluster has none. The first set named by the
  owner: Karpenter, GPU operator, Dragonfly, Istio, AWS load balancer
  controller, Kyverno, Kyverno policies, node-local DNS cache. More follow.
- Components are a separate resource and a separate API, not fields on the
  cluster.
- A component belongs to one cluster. The ownership chain is account,
  network, cluster, component.
- Users pass override configuration per component.
- Every supported Kubernetes version has its own list of component versions;
  a cluster on an older Kubernetes version gets the versions listed for it.
  The platform supports the last three Kubernetes minors
  ([rollouts.md](rollouts.md), sections 2 and 5).
- How a component is installed: **the Crossplane Helm provider** (decided
  2026-11-13). Anything available as an EKS add-on stays an add-on in the
  cluster spec, not a component ([component-usage.md](component-usage.md)).
  Argo CD and Flux were considered; see section 9 for when to revisit.
- EKS add-on versions come from the AWS API, not from platform
  configuration. Only the platform's own components take their versions
  from the per-Kubernetes-version map in a release.
- An owner is not deleted while it has dependents (decided for accounts and
  networks; applied here to clusters and their components).

## 3. What exists today

| Fact | Where |
|---|---|
| The platform installs five EKS add-ons on every cluster and accepts more through the cluster spec. | `crossplaneaws.DefaultAddons`, `spec.aws.addons`, `AWSAddon` in `api/v1alpha1` |
| Add-ons are created only once the EKS cluster is ACTIVE, and their versions are held back during a Kubernetes upgrade until the control plane reports the target. | `crossplaneaws.ClusterSet`, `Reconciler.GateUpgrade` |
| An add-on can be given an IAM role through a pod identity association. | `PodIdentityAssociation` objects rendered with the add-ons |
| The Crossplane Helm provider is installed in the management cluster with the AWS providers, and its `Release` kind is activated. Nothing renders a `Release` yet. | `crossplane.HelmProvider` (v1.5.0), `pkg/crossplane/aws/install.go`, `activation.go` |
| Every cluster has a `ClusterAuth` object that writes a kubeconfig for the workload cluster into a Secret in the tenant namespace. | `clusterBuilder.clusterAuth` |
| A forced cluster deletion already expects Helm releases: `DeleteClusterRequest.force` "releases" them without uninstalling (management policies without Delete) when the cluster's API is unreachable. | `idl/cloud/v1/cluster_service.proto`, `forceRelease` in `pkg/reconcile/cluster/cleanup.go` |

So two of the three install mechanisms have groundwork: EKS add-ons work
today, and the Helm provider is installed and anticipated by the delete
path. Argo CD has none.

## 4. Goals and non-goals

Goals:

- A tenant can see what is offered, enable a component on a cluster, change
  its values, see whether it is healthy, and disable it.
- The operator can add a component that needs no cloud resources without a
  code change.
- A component's failure is visible on the component and does not make the
  cluster itself report failed.
- Versions follow the cluster's Kubernetes version and upgrade mode.

Non-goals:

- Installing arbitrary tenant charts. The catalog is the operator's.
- Managing what a component then manages (Karpenter's node pools, Istio's
  gateways, individual Kyverno policies beyond the offered policy sets).
- Replacing the five default EKS add-ons; they stay on the cluster spec.

## 5. The Component resource

**Proposed.** A namespaced kind `Component` in `cloud.cluster-vending.io`,
in the tenant namespace, with a controller owner reference to its `Cluster`.

```yaml
apiVersion: cloud.cluster-vending.io/v1alpha1
kind: Component
metadata:
  name: prod-karpenter            # <cluster>-<component>
  namespace: tenant-acme
spec:
  clusterRef: prod                # required, immutable
  component: karpenter            # catalog name; required, immutable
  version: ""                     # empty: from the release, for the cluster's Kubernetes version
  values:                         # the tenant's overrides only
    controller:
      resources:
        requests: {cpu: 500m}
status:
  phase: Ready                    # Pending, Installing, Ready, Degraded, Failed, Deleting
  conditions:
    - {type: Ready, status: "True"}
    - {type: Synced, status: "True"}
    - {type: DependenciesMet, status: "True"}
  observedGeneration: 3
  installedVersion: 1.8.1
  mechanism: Helm                 # how it was installed (section 8)
  effectiveValues: {}             # what was applied after merging (section 7)
  inventory:                      # the objects the platform has for it
    - {apiVersion: iam.aws.m.upbound.io/v1beta1, kind: Role, name: prod-1a2b3c4d-karpenter, externalName: prod-1a2b3c4d-karpenter}
    - {apiVersion: helm.m.crossplane.io/v1beta1, kind: Release, name: prod-1a2b3c4d-karpenter}
```

| Field | Meaning |
|---|---|
| `spec.clusterRef` | The cluster the component is installed into. The controller sets the owner reference. |
| `spec.component` | The catalog entry. |
| `spec.version` | Empty means "the version the release lists for this cluster's Kubernetes version". A value pins it; on an automatic cluster a pin is refused (section 9). |
| `spec.values` | The tenant's overrides. Platform defaults are not copied here. |
| `status.installedVersion` | What is running. |
| `status.effectiveValues` | The merged values, with protected keys shown and secrets never included. |
| `status.inventory` | Same shape as on `Cluster`: the objects the platform created for it. |

**Proposed contract.** A `ComponentService` beside `ClusterService`, and a
`Component` message mirroring the kind:

| RPC | Does |
|---|---|
| `ListCatalog` | The components offered, with the versions for a given Kubernetes version and the schema of their values. |
| `EnableComponent` | Creates a component on a cluster. Fails if a dependency is missing or the cluster is unsupported. |
| `UpdateComponent` | Changes version (manual clusters) or values. `validate_only` returns the merged values and what would change. |
| `DisableComponent` | Deletes it. Refused while another component depends on it. |
| `GetComponent`, `ListComponents` | The stored view, for one component or for a cluster. |
| `GetComponentStatus` | The live status, as for networks and clusters. |

CLI: `cluster component catalog`, `enable`, `update`, `disable`, `get`,
`list`, `status`, with `--values file.yaml`.

Like every handler, these write desired state only; the component's
reconciler installs.

## 6. The catalog entry

The catalog this build ships, with each component's chart version, CRD
chart and caveats, is in [catalog.md](../catalog.md); version groups and the
upgrade order are in component-usage.md, section 7.1.

**Proposed.** A cluster-scoped kind `ComponentDefinition`, written by the
operator with `kubectl` or from Git, like `Release`.

```yaml
apiVersion: cloud.cluster-vending.io/v1alpha1
kind: ComponentDefinition
metadata:
  name: kyverno-policies
spec:
  description: Baseline Kyverno policy set.
  class: Catalog                  # Catalog, or Builtin (needs renderer code)
  source:                         # for the Helm and Argo CD mechanisms
    chart: {repository: https://kyverno.github.io/kyverno, name: kyverno-policies}
  eksAddon: ""                    # the add-on name, when one exists
  namespace: kyverno              # where it is installed in the workload cluster
  requires: [kyverno]             # other components that must be Ready first
  defaults:                       # platform default values, under the release's
    podSecurityStandard: baseline
  valuesSchema: {}                # JSON Schema of what a tenant may override
  protectedKeys:                  # paths a tenant may not set
    - clusterName
    - serviceAccount.annotations
  cloud:                          # prerequisites the platform creates (Builtin only)
    podIdentity: false
```

| Field | Meaning |
|---|---|
| `class` | `Catalog`: installable from this entry alone. `Builtin`: also needs cloud resources rendered by platform code, so the entry is only usable with a platform build that knows the component. |
| `source`, `eksAddon` | Where it comes from, per install mechanism. |
| `requires` | Dependencies. Enable is refused until they are Ready; disable is refused while a dependant exists. |
| `defaults` | Values the platform sets for everyone. A release can override them per Kubernetes minor. |
| `valuesSchema` | What a tenant may pass. Values outside it are refused at the API. |
| `protectedKeys` | Values the platform owns (cluster name, role ARNs, anything tied to cloud resources it created). |
| `cloud` | What must exist in the tenant's account first. |

Versions are not in the definition. They are in the release, per Kubernetes
minor (`Release.spec.kubernetes[].components`), which is what gives an older
Kubernetes version its own, older, component versions. A component that is
not listed for a minor is not offered on clusters of that minor.

**A consequence of two decisions, to confirm.** A component delivered as an
EKS add-on has two possible sources for its version. The rule this document
takes from the decisions:

- If the component is installed as an EKS add-on, its version comes from
  AWS: the default AWS reports for the cluster's Kubernetes version, looked
  up the way add-on versions are today (rollouts.md, section 5). The release
  does not list a version for it; it lists the component's name for each
  minor it is offered on, so the catalog still says where it is available.
- Otherwise its version comes from the release's map for the cluster's
  Kubernetes minor.

So the same component can be versioned by AWS in one installation mechanism
and by the platform in another, and "an older Kubernetes version gets the
older component version" is then AWS's compatibility list, not the
operator's. This is Open question 9.

## 7. Values: merging and validation

**Decided:** users pass overrides. **Proposed:** the rest.

Merge order, later wins:

1. The chart's or add-on's own defaults.
2. The platform defaults: the definition's `defaults`, then the release's
   for the cluster's Kubernetes minor.
3. Values the platform computes (cluster name, region, role ARNs, queue
   names). These are the protected keys.
4. The tenant's `spec.values`, for every key that is not protected.

Rules:

- A tenant value on a protected key is refused when the request is made,
  with the key named. It is not silently dropped.
- Tenant values are validated against `valuesSchema` at the API. A
  definition without a schema accepts no overrides.
- Maps merge key by key; lists replace as a whole, as Helm does.
- `spec.values` holds only what the tenant set, so a later change of platform
  defaults reaches the component. `status.effectiveValues` shows the result.
- Values are not a place for secrets. A component that needs one refers to a
  Secret in the workload cluster by name.
- A size limit applies to `spec.values` (proposed: 64 KiB).

## 8. Two classes of component

**Proposed.**

| Class | What it needs | How it is added |
|---|---|---|
| Catalog | Only what is installed inside the workload cluster. | A `ComponentDefinition` and a version in a release. No code. |
| Builtin | Cloud resources in the tenant's account as well (IAM roles, queues, instance profiles, tags on subnets). | Renderer code that produces those objects, shipped in a platform build, plus the definition. |

**Update.** A component that only needs an IAM role is no longer Builtin:
the definition declares the role (`cloudResources.iamRole`: IRSA by
default, or Pod Identity; a policy with placeholders and managed policy
ARNs), and the platform renders it with its own trust policy, no code per
component (component-usage.md, sections 5.1 and 5.2). Builtin, meaning code,
is left for what a role cannot cover: Karpenter's interruption queue and
EventBridge rules, a later preset.

Classification of the components named so far. "To confirm" marks what was
not verified against the component's current documentation.

| Component | Class | Cloud resources it needs | EKS add-on available | Depends on |
|---|---|---|---|---|
| Karpenter | Builtin | IAM role for the controller, a role and instance profile for the nodes it launches, an SQS queue and EventBridge rules for interruption handling, discovery tags on subnets and security groups | To confirm | none |
| AWS load balancer controller | Catalog with an IAM role (`cloudResources.iamRole`) | IAM role with the controller's policy; subnet role tags (the platform already tags subnets for load balancers) | To confirm | none |
| GPU operator | Catalog | None in the account. Needs GPU instance types in a node group or from Karpenter; the node image must suit it. | No (to confirm) | GPU nodes; to confirm whether Karpenter should be a declared dependency |
| Dragonfly | Catalog (to confirm) | None known; to confirm whether it is configured with an object store that needs an IAM role | No (to confirm) | none |
| Istio | Catalog | None for the mesh itself. An ingress gateway that is exposed needs a load balancer. | No (to confirm) | AWS load balancer controller, when a gateway is exposed (to confirm as a hard dependency) |
| Kyverno | Catalog | None | No (to confirm) | none |
| Kyverno policies | Catalog | None | No | Kyverno |
| Node-local DNS cache | Catalog | None. It changes how nodes resolve DNS, which interacts with the CoreDNS add-on and kubelet settings; to confirm what node configuration it needs on EKS. | No (to confirm) | none |

Whether a component is available as an EKS add-on changes over time and by
region; the catalog entry records it (`eksAddon`), and the table above should
be checked against `aws eks describe-addon-versions` before it is relied on.

## 9. Install mechanisms

**Decided (2026-11-13): the Crossplane Helm provider.** It is already
installed by the platform, and its releases are ordinary managed resources,
so ordering (the sequence engine), the inventory, deletion order and status
work as for every other resource. Its known weak spots have workarounds the
component controller applies: a large `rollbackLimit` on every release
(provider-helm #193: a failed release is otherwise not retried), and
readiness judged by the controller in the workload cluster rather than only
from the release's status (provider-helm #329). Revisit Argo CD if operators
need a UI across clusters or the releases outgrow one provider pod; Flux, run
in each workload cluster, if clusters must keep reconciling while the
management cluster is down. The tenant API does not name the mechanism, so a
change later only affects the controller. The comparison as it was made:

| | EKS add-on | Crossplane Helm / Kubernetes provider | Argo CD |
|---|---|---|---|
| What must run in the management cluster | Nothing new. The EKS provider is installed. | The Helm provider, installed today. The Kubernetes provider would be added for raw manifests (policy sets). | Argo CD itself: several controllers, its own storage of state, its own access control. Not installed today. |
| How it reaches the workload cluster | Through the AWS API; the workload cluster's API is not contacted. | Through the workload cluster's API, with the kubeconfig Secret the cluster's `ClusterAuth` writes. A provider configuration per cluster is needed. | Through the workload cluster's API, with a cluster registered in Argo CD (a Secret per cluster). |
| How values are passed | `configurationValues` on the add-on, limited to what AWS's schema for that add-on allows. | Helm values on the `Release` object, unrestricted. | Helm values or a Git path on an `Application`. |
| How status and health come back | The add-on's status from EKS, as a Crossplane object's conditions. Already read by the platform. | The `Release` object's conditions: installed or failed. Not the health of the workloads inside. | Sync and health status per application, including workload health. The richest of the three. |
| Fit with the sequence engine and ownership | Full: it is one more object in a set, with "after the cluster is ACTIVE" already expressed. | Full: `Release` objects are Crossplane objects, so they go in a set with their IAM roles ahead of them, are recorded in the inventory, and are owned by the component. | Partial: the platform would create `Application` objects and read their status, but Argo CD, not the engine, orders and applies what is inside. Two systems decide when something is done. |
| Deletion | Deleting the add-on object removes it. | Deleting the `Release` uninstalls the chart, which needs the workload cluster's API. The forced path that skips the uninstall already exists (`forceRelease`). | Deleting the `Application` with its finalizer prunes the resources, which also needs the workload cluster's API. A forced path would have to be written. |
| Cost | Lowest. Fewest choices: only what AWS offers, AWS's versions, AWS's value schema. | Low: the provider is there; per-cluster provider configuration and RBAC for the kubeconfig are to be built. | Highest: a second control plane to run, secure and upgrade. |
| Covers | Few of the named components (to confirm which). | All of them. | All of them. |

**Recommendation** (not a decision): use the EKS add-on where one exists and
its value schema is enough; use the Crossplane Helm provider for the rest.
The reasons are that the provider is already installed, the delete path
already anticipates it, and it keeps one system, the sequence engine,
responsible for order and completion. Argo CD becomes worth its cost if
tenants are to bring their own applications, which is a non-goal here.

Whatever is chosen, the mechanism is recorded per component
(`status.mechanism`), because moving one component from Helm to an add-on
later is an uninstall and an install, not an upgrade.

## 10. How a component is reconciled

**Proposed.** The same shape as networks and clusters: a pure renderer
produces a `sequence.Set`, and the engine applies it.

For a Builtin component the set is, in order:

1. Cloud resources in the tenant's account (IAM role and policy, queue,
   instance profile), as provider objects.
2. The pod identity association that gives the component's service account
   its role. It waits for the EKS cluster to be ACTIVE, as add-on pod
   identities do today.
3. The install object (add-on, or Helm `Release`), built with the values
   that only exist after step 1 (role ARN, queue name). This is the engine's
   late-value mechanism (`Item.Build`).

For a Catalog component the set is the install object alone.

Conditions that hold a component back, shown in its status:

- The cluster is not Ready, or is being upgraded.
- A component in `requires` is not Ready.
- The cluster's Kubernetes version has no version listed for the component.

## 11. Rollouts, upgrade mode, support window, tiers

Following [rollouts.md](rollouts.md):

| Situation | What the component gets |
|---|---|
| Manual cluster, component enabled without a version | The version the newest completed release lists for the cluster's Kubernetes minor, fixed at that moment. It changes only when the tenant updates the component or the cluster's Kubernetes version. |
| Manual cluster, version named | That version, if the catalog offers it for the cluster's minor. |
| Automatic cluster | The version of the cluster's release. Releases move it, in the cluster's (channel, tier) stage. Naming a version is refused. |
| Any cluster, component installed as an EKS add-on | The version AWS reports for the cluster's Kubernetes version, not the release's (section 6). |
| The tenant upgrades Kubernetes on a manual cluster | Components move to the versions listed for the new minor, after the control plane reports it, the same gating add-ons have today. |
| The cluster is out of the support window | Enabling a new component is refused (recommended; Open question 4 in rollouts.md). Components already installed keep the last versions listed for their minor. |

Within one cluster and one release the order is: control plane, then node
groups and add-ons, then components, with `requires` ordering the components
among themselves. A component that fails during a release makes its cluster
count as failed for the stage, because the release is not applied there.

The tier and channel are the cluster's. A component has none of its own.

## 12. Deletion

**Decided:** no cascade; an owner is not deleted while it has dependents.

**Proposed.**

- Disabling a component deletes its set in reverse order: the install object
  first, then the pod identity association, then the cloud resources.
- A component is not deleted while another component requires it.
- A cluster is not deleted while it has components. `DeleteCluster` answers
  with a precondition failure naming them; the tenant disables them first.
  A CLI flag can do both in order for convenience, but it is two steps.
- Uninstalling needs the workload cluster's API. When it is unreachable, the
  existing forced deletion applies: `DeleteClusterRequest.force` is
  documented as releasing Helm releases without uninstalling them, and warns
  that what those charts created in the cloud account (load balancers,
  volumes) may be left behind. With components, a forced cluster deletion
  would also force its components. The cloud resources the *platform*
  created for a component (roles, queues) are still deleted, because that
  does not need the workload cluster.
- What a component created through the workload cluster (load balancers from
  the load balancer controller, nodes from Karpenter) is the component's to
  remove during a normal uninstall. For Karpenter the nodes must be gone
  before its role is; the set's reverse order gives that only if the
  uninstall waits for them. This is listed under Open.

## 13. Failure cases

**The install fails.** The component is `Failed` with the mechanism's
message. The cluster stays Ready. The reconciler retries; a change of values
is a new attempt.

**A dependency is disabled.** Refused while the dependant exists.

**Values pass the schema but break the chart.** The install fails as above.
`UpdateComponent` with `validate_only` shows the merged values first; it
cannot prove a chart accepts them.

**The cluster is upgraded and a component has no version for the new
minor.** The update of the cluster's Kubernetes version is refused, naming
the component, so the tenant disables it or waits for a release that lists
it.

**The workload cluster's API is unreachable.** Add-on components are
unaffected. Helm and Argo CD components report not synced; nothing is
uninstalled. Deletion needs the forced path.

**An imported cluster already runs a component.** The import does not see
what is installed inside the cluster. Enabling the component would install
over an existing one. **Proposed:** not supported at first; enabling on a
taken-over cluster warns that an existing installation is not detected.
Adopting an existing Helm release is a later step.

**The operator removes a definition that is in use.** Refused while any
`Component` refers to it, the same no-cascade rule.

## 14. Staged plan

Before any of this: the controller reconciles clusters, and the per-minor
version lists exist (rollouts.md plan, step 3).

| Step | Delivers |
|---|---|
| 1. `ComponentDefinition` and `Component` kinds, the contract messages and `ComponentService`, write-only handlers, the catalog listing | Tenants can see the catalog and record what they want. Nothing is installed. |
| 2. Decide the mechanism (section 9). If Helm: per-cluster provider configuration from the `ClusterAuth` Secret | The platform can reach a workload cluster's API. |
| 3. The first Catalog component end to end (Kyverno, then Kyverno policies for `requires`) | Enable, values, status, disable, dependency order. |
| 4. Values schema, protected keys, `validate_only` | Overrides are safe to offer. |
| 5. The first Builtin component (AWS load balancer controller: one role, no queue) | Cloud resources, pod identity and late values in one set. |
| 6. Karpenter | The largest Builtin: node role, instance profile, queue, and deletion that waits for its nodes. |
| 7. The remaining named components | Catalog entries, plus what each needs confirmed in section 8. |
| 8. Cluster deletion refuses while components exist; forced path covers components | The no-cascade rule holds for the new level. |
| 9. Component versions move with releases on automatic clusters | Joins rollouts.md plan, step 7. |

## 15. Open questions

Each is explained in plain terms, with a recommendation.

1. **Which install mechanism.** The owner's rule is: an EKS add-on where one
   exists, otherwise the Crossplane Helm provider or Argo CD. The choice
   between the last two is open. *Recommendation:* the Crossplane Helm
   provider. It is already installed, forced deletion already expects it,
   and it keeps one system in charge of order. Section 9 has the comparison.

2. **Raw manifests.** Some things are not charts (a policy set can be plain
   manifests). The Helm provider installs charts only. *Recommendation:*
   offer policy sets as charts where the project publishes one, and add the
   Crossplane Kubernetes provider only when a component cannot be a chart.

3. **May a tenant pin a component version on a manual cluster to one the
   release does not list?** *Recommendation:* no. The version must be one
   the catalog offers for the cluster's Kubernetes minor, in the newest
   release or an earlier one that is still completed. Anything else is
   untested by the platform.

4. **Deleting a cluster that has components.** The no-cascade rule says the
   delete is refused. That makes "delete this cluster" several calls.
   *Recommendation:* keep the rule in the API and let the CLI offer one
   command that disables the components and then deletes the cluster,
   showing each step.

5. **Karpenter's nodes at uninstall.** Karpenter launches nodes that the
   platform does not record. If Karpenter is removed while they run, they
   are left without a controller and its IAM role cannot be deleted cleanly.
   *Recommendation:* disabling Karpenter first deletes its node pools and
   waits until its nodes are gone, with a timeout and a forced variant.

6. **Do the default EKS add-ons become components?** They are on the cluster
   spec today and every cluster has them. *Recommendation:* leave them where
   they are. They are not opt-in, and a cluster does not work without them.

7. **Who may write the catalog.** *Recommendation:* the platform operator
   only, as with releases; tenants read it through `ListCatalog`.

8. **Existing installations on imported clusters.** *Recommendation:* out of
   scope for the first version; enabling warns. See section 13.

9. **Version source of a component that is an EKS add-on.** Add-on versions
   come from AWS and component versions from the release; a component
   installed as an add-on falls under both. This document applies "installed
   as an EKS add-on: AWS decides; otherwise: the release decides"
   (section 6). The effect is that the operator cannot choose or hold back
   the version of such a component. *Recommendation:* confirm the rule, and
   for a component where the operator must control the version, install it
   with Helm even though an add-on exists.
