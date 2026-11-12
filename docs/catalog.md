# The component catalog

The components this build ships, as `ComponentDefinition` files in
`config/catalog/` (one per definition). How tenants enable theirs is in
[design/component-usage.md](design/component-usage.md).

## Who controls a component

| `management` | Components | How they reach a cluster |
|---|---|---|
| `Platform` | `aws-load-balancer-controller`, `cluster-autoscaler`, `karpenter`, `kyverno`, `istio` (a bundle of six charts) | Installed with every cluster, as part of creating it, with fixed values. There is no `Component` object for them, they are not in `ListComponents`, and the Component API refuses their names (`PERMISSION_DENIED`, "installed with the cluster"). |
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
deleted. Nothing is installed on imported clusters. The details are in
[design/component-usage.md](design/component-usage.md), section 7.2.

See what a cluster has:

```sh
kubectl get cluster prod-eks -n tenant-acme \
  -o jsonpath='{range .status.platformComponents[*]}{.name}{"\t"}{.version}{"\t"}{.phase}{"\t"}{.message}{"\n"}{end}'
```

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

## How the catalog reaches the management cluster

- **By the controller (default).** On startup, after it installs the CRDs,
  the controller creates every shipped definition that does not exist
  (`--install-catalog`, default `true`; `pkg/catalog`). It never changes an
  existing definition, so the platform team's edits survive an upgrade of the
  controller. To take a newer shipped version of a definition, delete it and
  restart the controller, or apply the file.
- **By hand.** `--install-catalog=false`, then
  `kubectl apply -k config/catalog` once the CRDs are installed.

Chart versions are not in the definitions. A `Release` pins them per
Kubernetes minor; the versions below are what each definition was checked
against.

## The components

| Component | Chart (version checked) | Namespace | Tenant values | CRD chart |
|---|---|---|---|---|
| `aws-load-balancer-controller` | `aws-load-balancer-controller` 3.6.2, `https://aws.github.io/eks-charts` | `kube-system` | none: fixed | `aws-load-balancer-controller-crds` (platform, `charts/`) |
| `cluster-autoscaler` | `cluster-autoscaler` 9.60.0, `https://kubernetes.github.io/autoscaler`; image tag from the release pin | `kube-system` | none: fixed | none |
| `karpenter` | `karpenter` 1.15.1, `oci://public.ecr.aws/karpenter`; companion `karpenter-nodeclass` 0.1.0 (platform, `charts/`) | `kube-system` | none: fixed | `karpenter-crd` (upstream) |
| `kyverno` | `kyverno` 3.10.0 (Kyverno v1.20), `https://kyverno.github.io/kyverno/` | `kyverno` | none: fixed | none needed (subcharts, upgraded by Helm) |
| `kyverno-policies` | `kyverno-policies` 3.10.0; requires `kyverno` | `kyverno` | yes | none |
| `gpu-operator` | `gpu-operator` v26.7.2, `https://helm.ngc.nvidia.com/nvidia` | `gpu-operator` | yes | none (the chart's pre-upgrade hook applies them) |

`istio`, in ambient mode (no sidecars), is a bundle of six internal
definitions:

| Component | Chart (version checked) | Namespace | Requires |
|---|---|---|---|
| `gateway-api-crds` | `gateway-api-crds` 1.6.1 (platform, `charts/`): Gateway API v1.6.1, standard channel | `gateway-system` | |
| `istio-base` | `base` 1.32.0, `https://blob.istio.io/istio-release/charts` | `istio-system` | |
| `istiod` | `istiod` 1.32.0, profile `ambient` | `istio-system` | `istio-base`, `gateway-api-crds` |
| `istio-cni` | `cni` 1.32.0, profile `ambient` (DaemonSet) | `istio-system` | `istiod` |
| `ztunnel` | `ztunnel` 1.32.0 (DaemonSet) | `istio-system` | `istio-cni` |
| `istio-ingress-gateway` | `istio-ingress-gateway` 0.1.0 (platform, `charts/`): a Gateway API `Gateway` of class `istio` behind an internal NLB | `istio-ingress` | `istiod`, `aws-load-balancer-controller` |

All six are fixed: no tenant values. None needs a CRD chart: `istio-base`
and `gateway-api-crds` render their CRDs as templates, kept on uninstall.

**Tenant values.** Only `gpu-operator` and `kyverno-policies` have a values
file and a schema. Every Platform component runs as the platform set it:
the values a tenant could once tune are in `platformValues`, at the
researched defaults. Tests (`config/catalog`) hold that exactly those two
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
| `istio-ingress-gateway` | `nodeSelector`, `tolerations` of the platform chart, which puts them on the Deployment istiod generates |
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

- `karpenter`: the upstream `karpenter-crd` chart. Karpenter 1.15 changes the
  NodePool CRD, so an upgrade from 1.14 needs it. A cluster whose CRDs were
  first installed from the main chart's `crds/` needs Helm's ownership labels
  and annotations on them before `karpenter-crd` can adopt them (Karpenter
  troubleshooting: "Helm Error When Installing the karpenter-crd Chart"); new
  installs are not affected.
- `aws-load-balancer-controller`: `charts/aws-load-balancer-controller-crds`,
  the upstream chart's `crds/crds.yaml` and `crds/gateway-crds.yaml` (3.6.2)
  unchanged, with `helm.sh/resource-policy: keep`. Its version follows the
  upstream chart: bump both together, copying the new files.
- `gpu-operator`: NVIDIA ships no CRD chart; `operator.upgradeCRD` (the
  default) runs a pre-upgrade hook that applies them.

The four platform charts (`karpenter-nodeclass`,
`aws-load-balancer-controller-crds`, `gateway-api-crds`,
`istio-ingress-gateway`) have their source in `charts/` and must
be published to `oci://ghcr.io/yindia/cluster-vending/charts`; nothing
publishes them yet. Until they are, on every cluster:

- `karpenter` installs, but its companion `karpenter-nodeclass` fails (no
  EC2NodeClass or NodePool: Karpenter launches nothing), so `karpenter`
  reports `Failed`;
- `aws-load-balancer-controller` stays `Failed` at its CRD chart
  (`aws-load-balancer-controller-crds`), and `istio-ingress-gateway`,
  which requires it, waits;
- `gateway-api-crds` stays `Failed`, and `istiod`, `istio-cni`, `ztunnel`
  and `istio-ingress-gateway`, which come after it, wait;
- `cluster-autoscaler`, `kyverno` and `istio-base` install: their charts
  are upstream.

## Versions that move together, and upgrade order

- **Version groups** (`versionGroup`): `kyverno` and `kyverno-policies` are
  group `kyverno`; `istio-base`, `istiod`, `istio-cni` and `ztunnel` are
  group `istio`. A release that pins members of a group at different
  versions on one Kubernetes minor is not Valid. The two platform charts of
  Istio have their own versions and are outside the group.
- **One minor at a time** (`upgradeOneMinorAtATime`): the four `istio`
  group members. A channel does not move to a release whose pin skips a
  minor version of such a component relative to the release it offers now
  (1.31 to 1.32 is fine, 1.30 to 1.32 is not).
- **Order**: components are installed and upgraded in `requires` order
  (`release.UpgradeOrder`): `kyverno` before `kyverno-policies`;
  `gateway-api-crds` and `istio-base`, then `istiod`, `istio-cni`,
  `ztunnel`, and the ingress gateway after `istiod`.
- **Istio 1.32 goes with Gateway API v1.6.1.** Pin `gateway-api-crds` at
  1.6.1 in the same release as Istio 1.32; no check enforces the pairing.

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
- If the Gateway API CRDs already exist on a cluster, `gateway-api-crds`
  fails with Helm's ownership error until they are adopted or removed.
- A tenant's own `Gateway` of class `istio` also gets a LoadBalancer
  Service, without the internal annotations; nothing stops that yet.

**Autoscaling.** A component that uses a HorizontalPodAutoscaler (istiod,
the ingress gateway) needs the `metrics-server` EKS add-on; a definition cannot require an
add-on.

None of the components has been installed on a real cluster through the
platform yet: the installer (the component controller) is not written.
