# PROP-03: Component placement on a bootstrap group, a platform pool and tenant pools

| | |
|---|---|
| Status | Proposed |
| Author | `<author>` |
| Date | 2026-11-20 |
| Reviewers | `<reviewers>` |
| Related | [architecture.md](../architecture.md): 3. Platform components, Deletion, Node layout, Releases and rollouts, 5. Node groups configuration; [PROP-01](PROP-01-admin-configuration.md): the config file `nodeGroups` section (4.4) and gated catalog changes (4.7) |

## 1. Summary

Today every platform component, tenant component, EKS add-on controller and Karpenter
itself runs on one managed node group, `system` (2 x `t3.medium`, autoscaled to 6 by
cluster-autoscaler). The request is that few components run on the default node group,
and the rest on Karpenter nodes that the platform manages. This proposal splits the
layout into three pools:

| Pool | What runs | Capacity |
|---|---|---|
| **Bootstrap** (`system`, the EKS managed default node group) | Karpenter (2 replicas), CoreDNS (2), the aws-ebs-csi-driver controller (2), and Capsule (2) when the cluster has `naas` | Fixed size, configured: `nodeGroups.system` |
| **Platform** (Karpenter NodePool `platform-pool`, EC2NodeClass `platform-pool`) | Every other platform component, and every tenant component | Grows and shrinks with demand, configured: `nodeGroups.platform` |
| **Tenant** (`nodeGroups.karpenter`, unchanged) | The tenant's own workloads | As today |

DaemonSets (vpc-cni, kube-proxy, the pod identity agent, the EBS node plugin, istio-cni,
ztunnel) run on every node, as today.

Decisions, each argued in section 4:

| Question | Decision |
|---|---|
| Bootstrap set | Karpenter, CoreDNS, aws-ebs-csi-driver controller, and Capsule when `naas` is on (its fail-closed namespace webhooks, 4.11). Nothing else. |
| Bootstrap substrate | The EKS managed node group that the cluster has as its default node group. Default: 2 x `t3.medium` (for testing), fixed size, `instanceTypes` a list. Production sizes are a recommendation in 4.6. No Fargate. |
| cluster-autoscaler | Removed from the default set. The bootstrap group does not need to grow, and Karpenter scales the platform and tenant pools. |
| Platform pool | One NodePool `platform-pool` and one EC2NodeClass `platform-pool`, on-demand, family `t3` by default (testing size, like the bootstrap group; `m7i` and `m6i` are the production recommendation in 4.6), tainted `node.cluster-vending.io/pool=platform-pool:NoSchedule`, `WhenEmpty` consolidation in every class, explicit limits and disruption budgets. |
| Tenant components | Platform pool. Not the tenant's own groups. No per-cluster switch. |
| Helm capacity | A release that waits on pool capacity is classified `capacity`: not counted, retried for up to 2 x its install timeout (4.11). |
| Deadlock | Karpenter is never held for any pool. A platform pool is "Ready" when its objects are Ready, not when it has nodes. Pods create the nodes. |
| Migration | Record each component's placement first (phase 3), so the new build changes nothing on an existing cluster. Create the platform pool. Move one component at a time by changing its recorded placement, admitted by the per-definition gated rollout of PROP-01 phase 5 (a dependency: phase 4 here waits for it), and only when the pool has a Ready node. Shrink the bootstrap group last. |
| Priority | Platform components run with PriorityClass `cluster-vending-platform` (100000), tenant components with `cluster-vending-tenant-component` (10000), so a platform pod preempts tenant component pods when the pool is at its limits (4.7). |
| Quota | Cluster creation and `account verify` check the region's On-Demand Standard vCPU quota against the bootstrap group and the platform pool's floor (4.5). |
| Prefix delegation | New clusters only. Existing clusters opt in by a manual node replacement. No automatic migration (4.9). |
| Configuration | The PROP-01 file: `nodeGroups.system`, `nodeGroups.platform`, `nodeGroups.karpenter`. |

## 2. Problems today

Checked against `main`. File references are to the code as it is.

**2.1 One alias for every pool.** `${system.*}` and `${platform.*}` resolve to the same
managed node group (`placement()` in `pkg/component/placeholders.go`). Karpenter,
cluster-autoscaler, CoreDNS, the EBS controller and every platform and tenant component
share one taint (`node.cluster-vending.io/pool=system:NoSchedule`). A failure of that group
takes down the whole platform control path, DNS included.

**2.2 The group is the scaling ceiling.** Platform components are sized to fit the group
at its minimum: `SystemGroupFillLimit` is 0.85 of allocatable CPU and memory, and the pod
count is limited too (`pkg/catalog/sizing.go`, `pkg/catalog/system_group_sizing_test.go`).
A `t3.medium` has 1930m CPU, 3246 MiB and 17 pods (VPC CNI, prefix delegation off). Platform
components that grow with cluster size (istiod, the ingress gateway, Kyverno, External
Secrets) have no place to grow except this group. The design targets of 1,000 nodes and
10,000 pods per cluster (architecture.md, Known limits) cannot be met on it.

**2.3 History.** The layout went through three shapes:

- `c8199d3`: a tainted `system` group for Karpenter only, a platform NodePool `platform`
  on `t3` for the platform components, cluster-autoscaler removed.
- `cc7dc5a`: the platform NodePool removed. One fixed-size `system` group for every
  platform component. The commit gives the decision, not the reason.
- `3954cb3`: cluster-autoscaler back, scaling `system` only.
- `5a41b37`: "never hold Karpenter for the platform node group". With Karpenter at the
  default sequence, Karpenter waited for the platform node group and the group for
  Karpenter, so every platform component stayed Pending. That is the deadlock this
  proposal has to avoid again.

The reasons for `cc7dc5a` are not in the repository. The inferred reasons, which the
design below treats as the constraints to meet and not as settled facts: the platform
components then depended on Karpenter launching their nodes, Karpenter's own placement had
to be solved separately, and the platform NodePool had to be applied before anything it
needed existed.

**2.4 Add-on placement is hard-wired to the default pool.** `addonPlacements`
(`pkg/crossplane/aws/addon_placement.go`) gives CoreDNS and the EBS controller the labels
and taints of the default pool. A different pool means that code names the pool
explicitly, not the default one.

**2.5 A name collision, avoided.** `legacyPlatformPool = "platform"`, and `legacyPoolGate`
(`pkg/reconcile/cluster/nodegroups.go`), remove a NodePool and EC2NodeClass named
`platform` once nothing needs them. The new pool is named `platform-pool`, which the gate
does not match, so the gate does not change (4.3, phase 1).

**2.6 Gating that already exists and must stay.** The NodePools and EC2NodeClasses of any
pool are applied only once `karpenter` is `Ready` (`reconcileNodeGroups`). The `karpenter`
component is exempt from the hold on other components (`5a41b37`).

**2.7 Helm classes.** Helm failures are transient, chart or release, and release failures
are counted, including "the rollout timed out" (architecture.md, Helm operations). A
platform component whose pods cannot schedule because a pool has no capacity would count
toward `Failed` today. That is wrong for a capacity problem and needs its own class
(4.11).

## 3. Goals and non-goals

### Goals

1. A bootstrap set that is minimal, fixed in size, and named in the code.
2. Platform and tenant components on Karpenter-managed capacity that grows with the cluster.
3. Machine types, counts, capacity types, limits and consolidation in the PROP-01 config file.
4. Existing clusters move without stranding a pod and without moving Karpenter.
5. Refusal at start, with every problem listed, when the bootstrap set does not fit.
6. A failure of one pool is visible and bounded, and does not stop the other pools.

### Non-goals

- Fargate in any form, per-component pools, Spot capacity, or tenant-selected placement of
  platform components. Three pools only; the platform pool is on-demand.
- Changing the DaemonSet set.
- Runtime resizing of the bootstrap instance type. EKS can only change it by replacing the
  group (PROP-01 4.7).
- Automatic prefix delegation on existing clusters (4.9).

## 4. Design

### 4.1 Classification of every shipped component

The principle: **bootstrap is the set that must run before a platform NodePool can launch
its first node, plus the EKS add-on controllers, which EKS creates before any platform node
exists, plus CoreDNS, whose outage would stop name resolution across the cluster whatever
the platform pool does, plus Capsule, whose fail-closed namespace webhooks would otherwise make a
pool outage block every namespace change in the cluster.** Everything else is platform or tenant.

| Workload | Class | Where | Replicas | Reason |
|---|---|---|---|---|
| `karpenter` (sequence 0) | Bootstrap | Bootstrap group | 2 | Launches every platform and tenant node. Must not depend on the pool it provisions. |
| `coredns` (EKS add-on) | Bootstrap | Bootstrap group | 2 | Cluster DNS must not depend on the autoscaled pool. EKS creates it before any platform node. |
| `aws-ebs-csi-driver`, controller | Bootstrap | Bootstrap group | 2 | An EKS add-on. EKS creates it before any platform node, so its placement cannot select a pool that has no nodes yet. Its node DaemonSet is a DaemonSet (below). |
| `cluster-autoscaler` | Removed | none | none | It scales the group the bootstrap set no longer needs to grow. Karpenter scales the rest. Existing clusters remove it through a gated disable (4.9). |
| `aws-load-balancer-controller` | Platform | Platform pool | per chart, one or two | Needed to create load balancers. Existing ones keep serving without it. Deletion stage 1 needs it running (4.12). Its webhooks' `failurePolicy` is checked (phase 0). |
| `external-secrets` | Platform | Platform pool | 1 per controller | Not on any request path. Its outage stops secret refresh, not running pods. |
| `kyverno` (admission, background, cleanup, reports) | Platform | Platform pool | 1 to 3 each | Admission webhooks. Their `failurePolicy` decides whether an outage stops pod creation (4.11). Kept off bootstrap, with that risk explicit. |
| `istio-base` | Platform (no workload) | none | none | CRDs and webhook configuration only. |
| `istiod` | Platform | Platform pool | 2, HPA above | Ambient control plane. Needed for new mesh configuration, not for running pods. |
| `istio-ingress-gateway` | Platform | Platform pool | 2, HPA above | Traffic entry. Its pods are on the pool that scales with the traffic. |
| `istio-cni`, `ztunnel` | DaemonSet | Every node | one per node | Ambient mesh needs them on every node. |
| `capsule` (when `naas`) | Bootstrap | Bootstrap group | 2 | Tenancy controller. Its `namespaces` and `replications` webhooks are `Fail` with no namespace selector (phase 0 results, section 7). On the pool, a pool outage would block every namespace create, update and delete. On the bootstrap group they depend only on the group whose loss is already the one total outage (4.11). No `failurePolicy` is weakened. Its values use `${system.*}`. |
| `kyverno-policies` (tenant) | Tenant, no workload | none | none | Policy objects only. |
| `gpu-operator` (tenant) | Tenant | Platform pool for the operator and its controllers | per chart | The operands (device plugin, DaemonSets) run on the tenant's GPU groups with the chart's own tolerations. |
| Tenant components in general | Tenant | Platform pool | per chart | Their placement is the platform's, not the tenant's (4.7). |
| `vpc-cni`, `kube-proxy`, `eks-pod-identity-agent`, `aws-ebs-csi-driver` node | DaemonSet | Every node | one per node | Node networking, proxy, identity and volume agents. |

Pod count of the bootstrap set: 8 controller replicas (Karpenter 2, CoreDNS 2, EBS
controller 2, Capsule 2 when `naas` is on) plus the DaemonSets on each node (vpc-cni,
kube-proxy, pod identity agent, EBS node, istio-cni, ztunnel: six per node). Two `t3.medium`
nodes give 34 pod slots and need 20 (18 without `naas`). Capsule's hook job (CRD install and
cleanup, 100m and 128 MiB) runs on the bootstrap group too, for the length of an install or
upgrade. The 17-pod limit of each node is the binding one, so the sizing check counts pods,
not only CPU and memory (4.5).

### 4.2 The bootstrap substrate

The bootstrap set runs on the EKS managed node group that the cluster has as its default
node group (`nodeGroups.system` in the config, `system` in the code). The platform offers no
other substrate. Fargate was considered and rejected (section 5, E).

Why the managed group:

- The DaemonSets (vpc-cni, kube-proxy, pod identity agent, EBS node plugin, istio-cni,
  ztunnel) run on it, and the ambient mesh covers its pods.
- CoreDNS and the EBS controller take the add-on placement as today (`addonPlacements`),
  with no pod-level patch.
- IRSA and EKS Pod Identity both work, and the bootstrap workloads already use IRSA.

Shape:

- `instanceTypes` is a list, so a capacity shortage of one type does not block creation.
  Every type in the list has the same architecture and vCPU and memory class (4.4).
- Subnets in every availability zone of the network, so the capacity of one zone does not
  take the group out.
- Fixed size: desired = minimum = maximum = `minSize`. A new cluster (from phase 3) is created
  this way and gets no cluster-autoscaler. An existing cluster keeps cluster-autoscaler, with
  `system.maxSize` as its ceiling, until M3 removes it (4.9); after that `maxSize` is ignored
  with a warning. The group changes size only through M3.
- Node-level limits apply: 17 pods on `t3.medium` without prefix delegation (4.5).
- Upgrade: the managed node group's rolling update, one node at a time, under PDBs (4.10).
- Failure domain: the capacity of one instance type in one zone can run out. The list of
  types and the zone spread are the mitigation.

### 4.3 The platform pool

**EC2NodeClass `platform-pool`** is rendered like the tenant's classes (`KarpenterNodeGroupObjects`,
`pkg/crossplane/aws/karpenter_nodepools.go`): the same node role
(`<prefix>-karpenter-node`), subnets and security groups by discovery tag, the AMI pinned by
the release's `nodeAmiPin` (the existing mechanism, PROP-01 2.2 and architecture.md
"Upgrade"), and the cluster's tags. The class adds no `kubelet` settings unless
`platform.maxPods` is set (4.6).

**A release without `nodeAmiPin`.** PROP-01 decided that such a release falls back to
`al2023@latest`. For the platform pool, `latest` is resolved once, at the pool's first render
for that release, and the resolved AMI is recorded on the cluster status. It moves only with a
release move or a Kubernetes upgrade, never because AWS published a newer AMI. Without this, a
drift replacement of the whole pool would follow every AMI publication.

**NodePool `platform-pool`**, rendered for each cluster:

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: platform-pool
  labels:
    cloud.cluster-vending.io/managed-by: platform
spec:
  template:
    metadata:
      labels:
        node.cluster-vending.io/pool: platform-pool
    spec:
      nodeClassRef: {group: karpenter.k8s.aws, kind: EC2NodeClass, name: platform-pool}
      taints:
        - key: node.cluster-vending.io/pool
          value: platform-pool
          effect: NoSchedule
      requirements:
        - {key: kubernetes.io/arch, operator: In, values: [amd64]}
        - {key: karpenter.sh/capacity-type, operator: In, values: [on-demand]}
        - {key: karpenter.k8s.aws/instance-family, operator: In, values: [t3]}       # config default (testing)
        - {key: karpenter.k8s.aws/instance-size, operator: In, values: [large, xlarge, 2xlarge]}
      expireAfter: 336h
      terminationGracePeriod: 1h
  limits:
    cpu: "128"
    memory: 512Gi
  disruption:
    consolidationPolicy: WhenEmpty
    consolidateAfter: 5m
    budgets:
      - nodes: "1"
```

The pool is named `platform-pool` for two reasons: the legacy gate removes a NodePool named
`platform` (2.5), and the pool's label and taint value match its name. The placeholder
aliases (`${platform.*}`, `${system.*}`) and the config key `nodeGroups.platform` keep their
names; they name the role, not the pool object.

- **Spot is refused** in this version (the offering set is on-demand only).
- **Consolidation** is `WhenEmpty` in every scale class. Consolidating an underutilized node
  moves running platform pods, and a Kyverno or Istio pod restart is a visible event. Stability
  is chosen over cost. Empty nodes are removed; a node that still carries a few pods stays
  until it is empty. The cost is idle capacity, which the phase 0 measurement records per
  class. `WhenEmptyOrUnderutilized` is available in `nodeGroups.platform.consolidationPolicy`.
- **Disruption budget** of one node at a time keeps drift replacements (AMI pin moves) to one
  node in flight. Karpenter creates the replacement before it drains the old node, so a
  failed launch leaves the old node in place.
- **`expireAfter` 336 h** rotates the nodes every two weeks even without a pin change.
- **`terminationGracePeriod` 1 h** bounds a drain that a PodDisruptionBudget holds.
- **Limits** cap the pool so a runaway pending set cannot launch unbounded capacity. They
  are configured (4.4) and checked against the platform's own requests (4.5).

**Placement.** The placeholders change meaning, and two priority placeholders are added. With
Capsule's switch to `${system.*}`, those are the only changes to the catalog:

| Placeholder | Today | After |
|---|---|---|
| `${platform.nodeSelector}`, `${platform.tolerations}` | the managed node group | the platform pool (`platform-pool`) on a new cluster; on an existing one, the component's recorded placement (4.8) |
| `${system.nodeSelector}`, `${system.tolerations}` | the managed node group | the bootstrap group, unchanged |
| `${platform.priorityClassName}` | none | `cluster-vending-platform` |
| `${tenant.priorityClassName}` | none | `cluster-vending-tenant-component` |

The change to `${platform.*}` rewrites the values of every platform definition that uses it
(external-secrets, aws-load-balancer-controller, istiod, istio-ingress-gateway, kyverno,
gpu-operator). Capsule moves from `${platform.*}` to `${system.*}` (4.1). For an existing
cluster `${platform.*}` renders from the component's recorded placement (4.8), so the new
meaning reaches it only when that record changes, through the gated rollout of PROP-01 4.7
(4.9). `${system.*}` keeps its meaning.

**Priority.** The platform creates two PriorityClasses on every cluster it places components
on, with the pool: `cluster-vending-platform` (value 100000) and
`cluster-vending-tenant-component` (value 10000), neither global default. Platform
definitions set `priorityClassName: ${platform.priorityClassName}`, and the platform sets
`${tenant.priorityClassName}` on tenant components where the chart has the key. When the pool
is at its `limits`, a pending platform pod preempts tenant component pods, so a heavy tenant
component cannot keep istiod or Kyverno from scaling. The bootstrap components keep the
`system-cluster-critical` or `system-node-critical` class their charts and add-ons set
(Capsule already sets `system-cluster-critical`). Tenant workloads on tenant pools are not
affected: preemption acts within the nodes a pod can schedule on.

**Who places what, in code.** The bootstrap add-ons (CoreDNS, EBS controller) keep
`addonPlacements` but select the bootstrap group's labels explicitly, not `b.p.NodePool`,
which is the managed group of the cluster. The platform pool's labels are never given to an
EKS add-on: the add-on would wait for a pool with no nodes (2.4).

**Order of a new cluster, with the wait for each step:**

1. EKS control plane, the bootstrap managed node group, and the EKS add-ons: vpc-cni,
   kube-proxy, pod identity agent, CoreDNS, EBS CSI. Waits: the cluster is `ACTIVE`, as today.
2. Karpenter (sequence 0) on the bootstrap group. Waits: nothing from the platform pool.
   Ready means the chart is deployed, as today.
3. The Karpenter node role, its access entry and interruption queue (as today), then the
   EC2NodeClass and NodePool `platform-pool`, and the tenant pools. Waits: Karpenter Ready.
   Ready of the pool means its EC2NodeClass and NodePool report `Ready`. It does not mean a
   node exists.
4. The two PriorityClasses, then platform components by sequence (`requires` order within a
   sequence). Each component placed on the pool waits for the platform pool to be Ready;
   Capsule, on the bootstrap group, does not. Its pods make Karpenter launch nodes. The Helm install waits
   for the pods to become Ready, as today, and a capacity wait is classified as in 4.11.
5. Tenant components, as today: after the platform components.

**Avoiding the deadlock of 2.3.** The cycle that stalled before needs a wait for nodes in
the step that creates them. This design has no such wait:

- Karpenter depends on nothing in the platform pool.
- The platform pool's readiness is its objects, not its nodes, so it cannot wait for a
  component.
- The platform components wait for the pool's objects, which wait for Karpenter, which waits
  for nothing.
- A component whose pods do not schedule yet is a capacity wait, not a failure. The Helm
  capacity rule of 4.11 classifies it, and it is not counted.
- The `karpenter` exemption from holds (`5a41b37`) stays, and a test pins it for the new
  pool.

### 4.4 Configuration

The keys are added to the PROP-01 file. The `karpenter` section is unchanged.

```yaml
nodeGroups:
  system:                          # bootstrap group (the managed default node group): Karpenter, CoreDNS, EBS CSI controller
    instanceTypes: [t3.medium]     # default for testing; production sizes in 4.6. A list; same architecture and vCPU/memory class; new clusters only
    minSize: 2                     # fixed size: desired = min = max; at least 2 (Karpenter's two replicas)
    capacityType: ON_DEMAND        # new clusters only
    diskSizeGiB: 0                 # new clusters only; 0 is EKS's default
    # maxSize: 6                   # deprecated: only cluster-autoscaler's ceiling on existing clusters until M3; then ignored with a warning
  platform:                        # the Karpenter NodePool "platform-pool": platform and tenant components
    instanceFamilies: [t3]         # default for testing; production: [m7i, m6i] (4.6)
    instanceSizes: [large, xlarge, 2xlarge]
    architectures: [AMD64]         # AMD64 or ARM64; one per pool
    capacityTypes: [ON_DEMAND]     # SPOT is refused in this version
    limits:
      cpu: 128
      memory: 512Gi
    consolidationPolicy: WhenEmpty # WhenEmpty | WhenEmptyOrUnderutilized; WhenEmpty in every scale class
    consolidateAfter: 5m
    expireAfter: 336h
    terminationGracePeriod: 1h
    disruptionBudget: 1            # nodes disrupted at once
    maxPods: 0                     # 0: Karpenter's default for the shape; else the kubelet's maxPods
  karpenter:                       # tenant node groups, unchanged
    instanceFamilies: [t3]
    instanceTypes: []
    defaults:
      architectures: [AMD64]
      capacityTypes: [ON_DEMAND]
```

| Key | Flag | Environment | Default |
|---|---|---|---|
| `system.instanceTypes` | `--system-instance-types` | `CV_SYSTEM_INSTANCE_TYPES` | `t3.medium` |
| `system.minSize` | `--system-node-group-min-size` (existing) | `CV_SYSTEM_NODE_GROUP_MIN_SIZE` (existing) | `2` |
| `platform.instanceFamilies` | `--platform-instance-families` | `CV_PLATFORM_INSTANCE_FAMILIES` | `t3` |
| `platform.instanceSizes` | `--platform-instance-sizes` | `CV_PLATFORM_INSTANCE_SIZES` | `large,xlarge,2xlarge` |
| `platform.limits.cpu` / `.memory` | `--platform-limit-cpu` / `--platform-limit-memory` | `CV_PLATFORM_LIMIT_CPU` / `CV_PLATFORM_LIMIT_MEMORY` | `128` / `512Gi` |
| `platform.consolidationPolicy` | `--platform-consolidation-policy` | `CV_PLATFORM_CONSOLIDATION_POLICY` | `WhenEmpty` |
| `platform.disruptionBudget` | `--platform-disruption-budget` | `CV_PLATFORM_DISRUPTION_BUDGET` | `1` |

Startup fails, listing every problem, when: `minSize` is below 2; `instanceTypes` is empty,
or mixes architectures or vCPU and memory classes (the sizing uses the smallest); a family or
size does not match the shape of the name; `capacityTypes` contains `SPOT`; `limits` are not
positive; `maxPods` is above 110 while the cluster's VPC CNI prefix delegation is off (4.6).
A `system.maxSize` or a `CV_SYSTEM_NODE_GROUP_MAX_SIZE` still set logs a warning, as the
PROP-01 variables do. Until M3 has removed cluster-autoscaler from an existing cluster, the
value is still that cluster's autoscaler ceiling; new clusters and migrated ones ignore it.

### 4.5 Validation and sizing

Three layers, as in PROP-01 4.5.

**Offline (`controller --check`), with the rendered catalog and the config file:**

- **Bootstrap sizing.** `SizingTable` restricted to the bootstrap set (karpenter, coredns,
  the EBS controller, capsule when `naas` can be on, and the DaemonSets) against 85 % of `minSize` nodes of the smallest
  instance type in `instanceTypes`. It counts CPU, memory and pods. The pod count is
  `minSize x maxPodsPerNode`, where `maxPodsPerNode` is 17 for `t3.medium` (and the sizing
  test's table), 29 for `m6i.large`, and 110 when prefix delegation is on. A bootstrap set
  that does not fit is refused, with the numbers.
- **Platform sizing.** The requests of the enabled platform and tenant components at their
  minimum replicas must fit `limits` with headroom, and each request must fit the largest
  shape the pool allows (`instanceSizes`). A request larger than every shape can never
  schedule and is refused.
- **Catalog placement.** Every platform definition has `${platform.*}` or `${system.*}`
  values, never a literal `nodeSelector` (checked by the placeholder rules). `karpenter`,
  CoreDNS and `capsule` use `${system.*}`. Nothing else uses `${system.*}`. Every platform
  definition on the pool sets `priorityClassName: ${platform.priorityClassName}`.
- **Requires.** A bootstrap component requires only bootstrap components, so the pool is
  never a dependency of the bootstrap set.

**Runtime (controller, on the live objects):** the pool's objects report `Ready`; the
bootstrap group has `minSize` nodes Ready (the existing `readNodes`); the pool's offering
set is read into its status, and an empty set is reported.

**Account preflight (cluster creation and `account verify`).** The platform's nodes run in the
tenant's account, so the region's EC2 quota "Running On-Demand Standard (A, C, D, H, I, M, R,
T, Z) instances" (`L-1216C47A`, in vCPUs, read through Service Quotas) must hold the cluster's
floor: `minSize` x the vCPUs of the smallest type in `system.instanceTypes`, plus two nodes of
the platform pool's smallest shape (the smallest `instanceSizes` entry of the smallest family).
The check subtracts nothing for running instances; it is a floor, not a forecast. Below the
floor, creation is refused with the numbers (quota, floor, and how it was computed), and
`account verify` reports the same as a failed check. With the defaults that floor is
2 x 2 (`t3.medium`) + 2 x 2 (`t3.large`) = 8 vCPUs. Quota that cannot be read (no
`servicequotas:GetServiceQuota`) is reported as a warning, not a refusal.

### 4.6 Scale classes

The platform pool grows with the cluster. The bootstrap set does not: it is the same
size at 10 nodes and at 1,000 nodes, apart from CoreDNS and Karpenter replicas.

**The config default is the testing size for every class:** 2 x `t3.medium` for the bootstrap
group, and family `t3` (`large` to `2xlarge`) for the platform pool. The recommended production
sizes are in the table below as guidance: an operator sets them in the file for the class that
applies. The class values are starting values, not measured yet; the
phase 5 load test replaces them.

| | Small (up to 50 nodes, 1,000 pods) | Medium (up to 250 nodes, 3,000 pods) | Large (up to 1,000 nodes, 10,000 pods) |
|---|---|---|---|
| Bootstrap, config default (testing) | 2 x `t3.medium` | 2 x `t3.medium` | 2 x `t3.medium` |
| Bootstrap, recommended for production | 2 x `t3.medium` | 2 x `m6i.large` | 3 x `m6i.large` |
| Pod slots, bootstrap, recommended size, default CNI | 34 | 58 | 87 |
| Platform limits | cpu 32, memory 128Gi | cpu 128, memory 512Gi | cpu 256, memory 1Ti |
| Platform families and sizes, config default (testing) | `t3`; `large` to `2xlarge` | same | same |
| Platform families and sizes, recommended for production | `m7i`, `m6i`; `large` to `2xlarge` | same; `xlarge` to `4xlarge` | same; `xlarge` to `8xlarge` |
| Platform consolidation | `WhenEmpty` | `WhenEmpty` | `WhenEmpty` |
| Karpenter | 2 | 2 | 2 |
| CoreDNS | 2 | 3 | 4 (its requests and cache size to be measured) |
| istiod, ingress gateway | 2, 2 | 2 to 4 HPA, 2 to 4 HPA | 3 to 6 HPA, 3 to 6 HPA |
| Kyverno admission | 1 | 2 | 3 |
| Prefix delegation (VPC CNI) | off | off | on by default at creation (`maxPods` 110 on the platform pool) |

Notes on the classes:

- **Bootstrap size.** The sizing check (4.5) decides whether the config default fits a class.
  If it refuses the default for a class, the recommended row is the size to set. Three nodes at
  the large class keep Karpenter's two replicas on different nodes with room for a drain.
- **Cost.** Moving platform components off the shared group adds platform capacity: the
  steady state is the nodes the platform's pods need, with a floor of about two nodes
  because the components spread across them. At 100 clusters that is on the order of 200
  additional nodes next to today's shared group, paid in the tenants' accounts. `WhenEmpty`
  (4.3) adds idle capacity on top. The phase 0 measurement records both per class. The cost is
  the price of isolation and of a fleet that grows. The testing default (`t3`) keeps the floor
  of a small cluster near two `t3.large`; the production families are an operator's choice per
  class. The account preflight (4.5) refuses a cluster whose region quota cannot hold the floor.
- **Pod density.** Without prefix delegation a `t3.medium` holds 17 pods, and the large class
  cannot hold its platform pods on the bootstrap group. Prefix delegation is the VPC CNI add-on
  setting (`ENABLE_PREFIX_DELEGATION`). It is set at cluster creation, and the large class
  enables it by default. An existing cluster can enable it only through the manual node
  replacement of 4.9. `maxPods` above 110 is refused.

### 4.7 Tenant components

**Decision: tenant components run on the platform pool.** Not on a tenant's own Karpenter
groups. There is no per-cluster switch to keep one on the tenant's group.

Reasons:

- Tenant component values are literal. A tenant does not choose a placement, so the platform
  must own it, and the platform pool is the only pool it owns.
- A tenant's node groups belong to the tenant's workloads, with their own taints, limits and
  budgets. Putting platform-wide controllers there would make a tenant's pool a dependency
  of the platform.
- The gpu-operator's operands need GPU nodes. They stay on the tenant's GPU groups, with the
  chart's own tolerations. Only the operator's controllers move to the platform pool.

The platform adds no switch. A `nodeSelector` and `tolerations` that a tenant sets in its own
component values are passed through as today; the platform does not read them as a placement
choice.

**Sharing the pool.** Tenant components and platform components share the pool's `limits`. A
tenant component runs at `cluster-vending-tenant-component` (10000), below the platform's
`cluster-vending-platform` (100000) (4.3, Priority). When the pool is at its limits, a pending
platform pod preempts tenant component pods; the reverse never happens. The platform sizing
check (4.5) counts platform components only against `limits`, so the platform always has its
own minimum within them.

### 4.8 Live state and the rule for existing clusters

The rule of `livePlacement` (`pkg/reconcile/cluster/platform_placement.go`) stays. A
component's placement is what its pods run on now, not what the catalog intends. Today
`livePlacement` resolves `${system.*}` and `${platform.*}` alike, from the managed group's live
labels and taints. That is safe only while both aliases name the same group. Before
`${platform.*}` changes meaning, each component needs its own record, so the record ships first,
in phase 3, before any placement changes:

- **The record.** `status.platformPlacement`, a map from component name to pool (`system` or
  `platform-pool`).
- **First pass of the new build.** A cluster that has any platform component installed records
  each installed component as `system`: read from its live Deployment's or DaemonSet's
  `nodeSelector` on the workload cluster, and `system` when that cannot be read or matches
  neither pool. A component not installed yet on such a cluster is recorded `system` too, so
  that cluster keeps one layout until it migrates (4.9). Only a cluster with no platform
  component installed yet, a new cluster, records `platform-pool` for every component placed
  there.
- **Rendering.** `${platform.*}` renders per component from that record: `system` gives the
  values `livePlacement` gives today, `platform-pool` gives the pool's. `${system.*}` is
  unchanged. So deploying phase 3 changes no rendered value on an existing cluster, and no
  Helm release is upgraded by the deploy.
- **A record with no entry.** A component without a record (written before the record existed,
  or added later by a catalog change) renders as `system` on a cluster that has any `system`
  entry, and as `platform-pool` otherwise.
- **Changing it.** Only a move (4.9, M2) changes a component's record, and only when its Helm
  upgrade has settled (its pods are Ready on the target pool). A failed move leaves the record
  where it was.

### 4.9 Migration of existing clusters

Today every cluster has the managed `system` group and no platform pool. After phase 3, each
existing cluster has every component recorded `system` (4.8), so nothing has moved yet.

**Dependency.** M2 and M3 need the gated catalog changes of PROP-01 phase 5, in the
per-definition form: a fingerprint per definition, so one definition's change is admitted on
its own, one definition at a time, in a given order. A single fingerprint per cluster cannot
express "move external-secrets now and istiod later". Phase 4 here does not start before
PROP-01 phase 5 ships. The steps:

- **M1. Create the pool.** Once `karpenter` is Ready on an existing cluster, the controller
  applies the EC2NodeClass and NodePool `platform-pool`, as it does for tenant pools. No pod
  is placed on it yet, so nothing moves. Pool Ready is reported on the cluster.
- **M2. Move components, one at a time.** A move is a change of the component's recorded
  placement (4.8) from `system` to `platform-pool`, which changes its rendered values. It is
  admitted per component by the per-definition gated rollout of PROP-01 phase 5 (PROP-01 4.7):
  tiers Tier2, Tier1, Tier0, `maxInProgress`, soak, windows, freeze, and halt past `maxFailed`.
  The rollout admits the components in the order below, the next only after the previous one
  has settled on the cluster. A component moves only when the platform pool has at least one
  Ready node, which the controller reads with the node list it already has. The move waits
  while the pool has no Ready node, so the component is never moved onto a pool that cannot
  run it.
  Order, lowest risk first: `external-secrets`, `aws-load-balancer-controller`, the
  gpu-operator controllers (tenant), `kyverno` (one controller at a time, the admission
  controller last), `istio-ingress-gateway`, `istiod`. Istiod moves last because its pods
  serve the mesh configuration. The move of each is one Helm upgrade with `wait: true`.
- **M3. Remove cluster-autoscaler, then resize the bootstrap group.** After M2, a gated
  disable (PROP-01 4.7) uninstalls cluster-autoscaler. The bootstrap group's size changes only
  here, and only through the platform's own drain check:
  1. For each node above `minSize`, the controller cordons it and evicts its pods through the
     eviction API, which honours PodDisruptionBudgets.
  2. A node is drained only when it carries no non-DaemonSet pod outside the bootstrap set,
     and the bootstrap set's PodDisruptionBudgets allow the eviction. Otherwise the shrink
     stops there and reports which pod holds it.
  3. Only when every excess node is empty is the group's desired size reduced to `minSize`.
     The group's minimum and maximum are `minSize` throughout, so the managed group never
     changes size by itself.
  Whether a managed node group's own scale-in respects PodDisruptionBudgets, and which node it
  removes, is verified in phase 0. The design does not rely on either. If phase 0 shows that
  the group can remove a node the platform has not drained, the shrink stays blocked until the
  removal can be pointed at an empty node; the platform never reduces the group while a
  non-empty node is one of the candidates.
  After M3 the group is fixed size and `system.maxSize` is ignored for that cluster, with a
  warning if it is still set.
- **M4. Keep Karpenter, CoreDNS, the EBS controller and Capsule on the bootstrap group.** They
  never move. Their placement is the bootstrap group in every class. Capsule's values change
  from `${platform.*}` to `${system.*}`, which on an existing cluster renders the same values,
  so it has nothing to move.

**Rollback.** There is no automatic rollback, as in PROP-01 and the rollouts. Moving a
component back is the same gated change with the old value, and it works only while the
bootstrap group still has room, which the sizing check guarantees until M3.

**Prefix delegation on existing clusters.** Not a migration step. The platform does not
enable prefix delegation on a cluster that was created without it. An operator who wants it
does so as a documented manual operation: enable the VPC CNI setting, then replace the nodes of
the bootstrap group and the platform pool, one at a time, so that new nodes take the setting.
The cost is that node replacement. The runbook for that operation is written with phase 5.

**Imported clusters.** An imported cluster keeps its default node pool and its placements
(`placedConfiguration`, the `ImportFrom` branch of `placement()`) until it is taken over.
A takeover runs M1 to M4 like an existing cluster. A takeover with `skip_platform_components`
creates no pool and moves nothing.

### 4.10 Upgrade impact

The Kubernetes upgrade's steps (architecture.md, "How a Kubernetes upgrade runs") become:

1. **ControlPlane.** As today.
2. **NodeGroups.** The bootstrap group moves first, one node at a time, with the managed
   group's rolling update. The platform pool's nodes move next, through the EC2NodeClass AMI
   pin (the release's `nodeAmiPin`), one node at a time under the disruption budget. The
   step settles when every node of the bootstrap group and of the platform pool runs the
   target kubelet minor. The tenant groups are unchanged. The step now includes the
   platform pool, which the code does not do today. This change ships in phase 3, with the
   first clusters that have nodes on the pool.
3. **Addons.** As today. CoreDNS runs on the bootstrap group already at the new minor.
4. **PlatformComponents.** The pins move, as today. A pod restart is a rolling update
   inside the pool.
5. **TenantComponents.** As today.

Points that change the timing:

- Drift replacement is one node at a time, so a platform pool of 3 to 6 nodes adds a few
  node cycles to the NodeGroups step. Tenant groups are not affected.
- A platform release (not a Kubernetes minor) changes the pool only when a component's
  placement or values change.

### 4.11 Failure modes and the Helm capacity class

| Failure | What keeps running | What degrades | Signal | Action |
|---|---|---|---|---|
| The platform pool cannot launch a node: no capacity for every type in its families, IAM, quota, subnet IP exhaustion, an AMI pin that does not resolve, or a node that does not register | Every running platform pod. Karpenter does not drain a node until its replacement exists. | New platform pods and new tenant component pods stay Pending. Tenant workloads on their own groups are unaffected. Deletion stage 1 waits for the load balancer controller (4.12). | `PlatformComponentsReady` reason `PlatformPoolNoCapacity` after 10 minutes. Karpenter events. The pool's offering count in its status. | Widen `instanceFamilies` or `instanceSizes`; check quota and subnet IPs. A release that waits on this is a capacity class (below): not counted. |
| Karpenter is down (crash loop, IAM, the controller role) | Bootstrap group, CoreDNS, the EBS controller, every running node. | No node is launched for any pool. Pending pods stay Pending. | Karpenter deployment unavailable; `KarpenterUnavailable` reason on the platform and tenant pools. | The two replicas sit on different bootstrap nodes. Restart or fix the role. Nothing else needs a change. |
| A bootstrap node is lost | The other node. CoreDNS and Karpenter run one replica each, with anti-affinity. | Capacity of bootstrap is halved until the managed group replaces the node. | Node count below `minSize`. | The managed group replaces it. The PDBs stop a drain from taking both replicas. |
| Both bootstrap nodes are lost, or their instance type has no capacity in any zone | Platform and tenant pods on running nodes. | Karpenter and DNS are down. No scaling. With `naas`, Capsule is down too, and its `Fail` webhooks block namespace changes. | DNS and Karpenter alerts. | This is the one total outage of the design. Mitigate with multi-type `instanceTypes` and subnets in every AZ. |
| A Kyverno, istiod, load balancer controller or External Secrets webhook is unavailable (Capsule's run on the bootstrap group: row above) | Running pods. | Pods whose admission goes through a webhook with `failurePolicy: Fail` cannot be created in the namespaces its `namespaceSelector` matches. | Admission errors in events. | The failure policies and selectors are verified in phase 0 (rule below). A `Fail` webhook whose pods run on the platform pool must exclude the bootstrap namespaces. This risk exists today; the move of a component to the pool does not change it. |
| The load balancer controller is down | Existing load balancers. | New Services of type LoadBalancer and new Ingresses are not reconciled. | Pending LB objects. | Its webhook `failurePolicy` and selector are checked in phase 0. |

**Webhook rule.** Every webhook of a platform component is recorded in phase 0 with its
`failurePolicy` and `namespaceSelector`. Any webhook with `failurePolicy: Fail` whose pods run
on the platform pool has a `namespaceSelector` that excludes `kube-system` (where Karpenter,
CoreDNS and the EBS controller run in this catalog; phase 0 found no `karpenter-system`
namespace), so a pool outage cannot block the bootstrap namespaces. The platform sets a
selector of this kind on the chart values where the chart allows it; phase 0 recorded the chart
keys that do (section 7). A component whose `Fail` webhooks cannot be scoped this way and whose
outage would block the cluster as a whole does not run on the pool: Capsule's `namespaces` and
`replications` webhooks are the case, so Capsule is bootstrap (4.1).

**Helm capacity class (decided).** A release is classified `capacity`, not counted, when all
of the following hold at the end of its install or upgrade wait:

- the wait timed out;
- every pod of the release that is not Ready is Pending with reason `Unschedulable`; and
- Karpenter has a NodeClaim launching for that pool, or reports insufficient capacity for it.

A capacity release is retried with the existing backoff. The retries run for up to 2 x the
component's install timeout, counted from the first capacity classification. After that it
becomes a counted release failure, and its message carries the capacity reason. A timeout
that does not meet the rule is a counted failure, as today.

### 4.12 Deletion

The seven stages of `driveDelete` (architecture.md, Deletion) stay in order. Stages 2
(tenant components) and 6 (EKS add-ons) are as today. Changes:

- **Stage 1, load balancers.** The controller that deletes them runs on the platform pool, so
  the stage needs pool capacity. Without it the stage ends at `--teardown-timeout` with
  `TeardownTimedOut`, which names the pool, as today.
- **Stage 3, platform components above sequence 0.** Helm uninstall does not need the pods to
  run, so a pool with no nodes does not block it.
- **Stage 4, node groups.** The NodePool `platform-pool` goes with the tenant pools (label
  `managed-by=platform`). Karpenter terminates the NodeClaims' nodes and must still run.
- **Stage 5, Karpenter**, on the bootstrap group, as today.

## 5. Alternatives considered

| Alternative | Why not |
|---|---|
| **A. Everything on the managed group, autoscaled (today).** | The group is the ceiling (2.2), a single failure domain for all platform components (2.1), and the sizing test forces every component to a minimum replica count. It cannot serve the large class. |
| **B. Everything on Karpenter, Karpenter included.** | Karpenter cannot launch its own nodes without the deadlock of 2.3. And DNS and the EKS add-on controllers would have no fixed capacity when the platform pool fails. The bootstrap set is small, so a fixed managed group costs little and keeps DNS independent of the pool. |
| **C. A dedicated managed group per role.** | Each adds an autoscaling group, an IAM mapping, a separate rolling update, and minutes per node against Karpenter's seconds. Karpenter gives the same separation through NodePools with one upgrade path. |
| **D. Fargate for every platform component.** | DaemonSets do not run on Fargate, so the ambient mesh cannot cover those pods, and each component pays the per-pod price at its request size. Fargate is the wrong model for the platform's largest and most variable workloads. |
| **E. Fargate for the bootstrap set.** | Rejected. DaemonSets such as vpc-cni, kube-proxy, ztunnel and istio-cni do not run on Fargate, which breaks the mesh and the node-level agents. CoreDNS and Karpenter need patches to run there. User decision (2026-11-20). |
| **F. Keep cluster-autoscaler on the bootstrap group.** | The bootstrap set grows only when the set grows, which the sizing check prevents. A scaler with no load to follow is an extra component. Removed. |
| **G. A tenant's own groups for tenant components.** | Tenant values are literal (4.7), so the platform cannot place them safely there, and tenant groups would carry platform dependencies. |

## 6. Rollout plan

Each phase ships alone and leaves the system consistent. Tests are table tests on the fake
client unless noted. The sandbox end-to-end run (the opt-in run from `830afe6`) is the
acceptance test for phases 2 to 4.

### Phase 0: Measure and check the unknowns

- Confirm, in a sandbox account, on the pinned chart and add-on versions:
  - every webhook of Kyverno, istiod, aws-load-balancer-controller, external-secrets and
    capsule: render each pinned chart's `ValidatingWebhookConfiguration` and
    `MutatingWebhookConfiguration`, and record each webhook's `failurePolicy` and
    `namespaceSelector`, and the chart value keys that set a selector (4.11, webhook rule);
  - the managed node group's scale-in: whether it respects PodDisruptionBudgets, and which
    node it removes (4.9, M3).
- Measure the requests of the bootstrap set and of the platform set on a `t3.medium` and an
  `m6i.large`, the platform pool's steady node count per class, and the idle capacity that
  `WhenEmpty` leaves per class.
- **Accepted when**: a written table of the answers is recorded in section 7 (Phase 0
  results), and the class values of 4.6 are confirmed or changed.
- **Tests**: none in code. The sizing table (`SizingTable`) runs over the bootstrap set.

### Phase 1: Configuration and checks, no behaviour change

- The `system.instanceTypes`, `system.minSize`, `platform.*` and the variables of 4.4 in the
  PROP-01 file. The validator of 4.4 and 4.5 (offline and runtime).
- No change to `legacyPoolGate`: the pool is `platform-pool`, which the gate does not match
  (2.5).
- **Accepted when**: an invalid file stops both processes with every problem listed. A
  `system.maxSize` logs a warning. No rendered object changes for an existing cluster.
- **Tests**: table tests for each rule of 4.4 and 4.5; the loader on one file for both
  commands; a `legacyPoolGate` test that `platform-pool` is not removed.

### Phase 2: The platform pool, built and empty

- The EC2NodeClass and NodePool `platform-pool` rendered per cluster, after `karpenter` is
  Ready (4.3). No placement changes yet. Pool readiness on the cluster status. Deletion of the
  pool in stage 4.
- **Accepted when**: a new cluster has the pool Ready and no pod on it; a deletion removes it
  in stage 4 before Karpenter's stage 5.
- **Tests**: rendering golden test of the NodePool and EC2NodeClass; the stage 4 order test;
  the readiness test with no nodes.

### Phase 3: New clusters place components on the pool

In this order within the phase; the first item ships before, or in the same build as, the rest:

- **The placement record first** (4.8): `status.platformPlacement`, filled on the first pass of
  the new build (`system` for every component of a cluster that has any installed,
  `platform-pool` only for a new cluster), and `${platform.*}` rendered from it per component.
- Placeholders split (4.3). New clusters place every platform and tenant component on
  `platform-pool`, except the bootstrap set (Karpenter, CoreDNS, the EBS controller, and
  Capsule, which switches to `${system.*}`). Add-on placement names the bootstrap group
  explicitly (2.4).
- New clusters get no cluster-autoscaler, and their system group is created fixed size
  (desired = min = max = `minSize`, 4.2). Existing clusters keep the autoscaler and
  `system.maxSize` as its ceiling until M3.
- The two PriorityClasses and the priority placeholders (4.3, Priority).
- The NodeGroups step of the Kubernetes upgrade includes the platform pool (4.10).
- The account preflight of the vCPU quota at creation and in `account verify` (4.5).
- The Helm capacity class (4.11). The platform pool's `PlatformPoolNoCapacity` reason.
- Code follow-up for the sizing: `CheckBootstrapSizing` counts Capsule (2 replicas, 50m and
  96 MiB each from `capsule.yaml`, plus its 100m and 128 MiB hook job) when `naas` can be on.
- **Accepted when**:
  - deploying phase 3 over an existing cluster renders identical Helm values for every
    component, and upgrades no Helm release;
  - on a new cluster, only the bootstrap set and the DaemonSets run on bootstrap nodes; every
    other platform and tenant pod is on a node labelled `pool=platform-pool`; no
    cluster-autoscaler is installed and the system group's min, max and desired are equal;
  - a pool that cannot launch leaves the components Pending with the reason, not Failed;
  - with the pool at its limits, a pending platform pod preempts a tenant component pod;
  - a Kubernetes upgrade's NodeGroups step settles only when every bootstrap and
    platform-pool node runs the target kubelet minor;
  - creation in a region whose quota is below the floor is refused with the numbers.
- **Tests**: a deploy-over test: an existing cluster's components render the same values
  before and after the build, with the record filled as `system`; the record rules of 4.8 (an
  installed cluster, a new cluster, a component without an entry, an unreadable
  `nodeSelector`); placement tests per definition (every `${platform.*}` resolves to the pool on
  a new cluster, every `${system.*}` to the bootstrap group); the PriorityClass render and the
  priority placeholder per definition; the NodeGroups step with a platform-pool node on the old
  minor (not settled) and on the new one (settled); the quota preflight below, at and above the
  floor, and with the quota unreadable; a Helm classifier test for the capacity class with
  these cases: a timeout with Unschedulable pods and a launching NodeClaim is `capacity`; the
  same after 2 x the install timeout is counted; a timeout with no NodeClaim and no
  insufficient-capacity report is counted; the Karpenter-not-held test with the pool; a sandbox
  run for the new-cluster layout.

### Phase 4: Existing clusters move

- **Depends on PROP-01 phase 5** (gated catalog changes with a fingerprint per definition,
  admitted one definition at a time in a given order). Not started before it ships.
- M1 to M4 of 4.9: each move a change of the component's recorded placement (4.8), admitted
  by that rollout per component, in the order given. The cluster-autoscaler removal and the
  bootstrap group's drain-checked resize (M3).
- **Accepted when**: on a sandbox cluster built in the old layout, every component moves,
  with no pod left Pending past the Helm timeout, the move halting and recovering as the
  rollout rules say on an injected failure, and the autoscaler gone with the bootstrap group
  at `minSize`. The shrink is refused while an excess node carries a pod outside the bootstrap
  set. Karpenter is never moved.
- **Tests**: planner tests with the tiers of the rollout, a halt on an injected failure, a
  freeze in the middle of a move, an import left as it is, a takeover running M1 to M4, and
  the M3 shrink refused by a non-empty excess node.

### Phase 5: Scale classes, load, and the prefix delegation runbook

- The classes of 4.6 as documented presets. The config default stays 2 x `t3.medium`. Prefix
  delegation at creation for the large class. The manual prefix delegation runbook of 4.9.
- A load test on the sandbox account (250 nodes, then 1,000 nodes and 10,000 pods, within the
  sandbox's quota).
- **Accepted when**: the bootstrap group stays Ready and DNS answers through a Karpenter
  scale-up and a platform pool drift of a full pool; the NodeGroups upgrade step with the
  platform pool settles within the upgrade's stall timeout.
- **Tests**: the load run as an opt-in test with its results recorded; table tests for the
  presets.

## 7. Open questions

**Decisions (2026-11-20)**

1. **Bootstrap substrate.** The EKS managed default node group. No Fargate: the option and
   its configuration, activation, RBAC, pod execution role, pod labels and phases are removed
   (4.2, section 5 E). Former questions 2, 3 and 8 are moot.
2. **Bootstrap default size.** 2 x `t3.medium` for testing. Production sizes are a
   recommendation in the scale-class table (4.6), not the default.
3. **cluster-autoscaler.** Removed from the default set. The bootstrap group is fixed size and
   Karpenter scales everything else.
4. **Webhook failure policies.** A Phase 0 verification task (4.11, webhook rule). The design
   consequence: a `Fail` webhook whose pods run on the platform pool excludes `kube-system` in
   its `namespaceSelector` (Karpenter runs in `kube-system` in this catalog; there is no
   `karpenter-system`). A component whose `Fail` webhooks cannot be scoped that way stays on the
   bootstrap group (decision 11).
5. **Consolidation.** `WhenEmpty` for every scale class, for stability over cost. It is
   configurable in `nodeGroups.platform.consolidationPolicy`. The idle-capacity cost is
   measured in Phase 0 per class (4.3, 4.6).
6. **Pool name.** `platform-pool`, for the NodePool, the EC2NodeClass, the label and the taint
   value. The legacy gate removes `platform`, so the new name avoids that cleanup (2.5).
7. **Managed group scale-in and PDBs.** A Phase 0 verification task. The design does not rely
   on it: the bootstrap group's size changes only through the platform's drain check (4.9, M3),
   and the group is fixed-size otherwise.
8. **Helm capacity class.** The rule is in 4.11: a timed-out release whose pods are Pending as
   Unschedulable, with a launching NodeClaim or an insufficient-capacity report, is `capacity`.
   It is retried for up to 2 x the install timeout, then counted with the capacity message.
9. **Tenant components.** They run on `platform-pool`. There is no per-cluster switch (4.7).
10. **Prefix delegation.** Set at creation, on by default for the large class. Existing clusters
    may enable it only through a documented manual node replacement. There is no automatic
    migration (4.6, 4.9). Spot is out of scope as decided in 4.3.
11. **Capsule on the bootstrap group.** With `naas`, Capsule is part of the bootstrap set. Its
    `namespaces` and `replications` webhooks stay `Fail` with no namespace selector, and depend
    only on the bootstrap group (4.1, 4.11). No `failurePolicy` is weakened. It adds 2 pods to
    the bootstrap group (20 of 34 slots on 2 x `t3.medium`).
12. **Placement record before the split.** `status.platformPlacement` ships first in phase 3,
    filled as `system` on every existing cluster, so the build that changes `${platform.*}`
    renders nothing new on an existing cluster (4.8).
13. **Migration on per-definition gating.** Phase 4 depends on PROP-01 phase 5 with a fingerprint
    per definition, admitted one at a time in the M2 order (4.9).
14. **Priority classes.** `cluster-vending-platform` (100000) for platform components,
    `cluster-vending-tenant-component` (10000) for tenant components; platform pods preempt
    tenant component pods when the pool is at its limits (4.3, 4.7).
15. **Testing defaults for the pool.** The config default of the platform pool is `t3`, `large`
    to `2xlarge`, like the bootstrap group's testing size. `m7i` and `m6i` are the production
    recommendation (4.6).
16. **Quota preflight.** Cluster creation and `account verify` check the region's On-Demand
    Standard vCPU quota (`L-1216C47A`) against the cluster's floor and refuse below it (4.5).
17. **`system.maxSize`.** Kept as cluster-autoscaler's ceiling on existing clusters until M3; new
    clusters are fixed size and ignore it, with a warning when it is set (4.2, 4.4).
18. **AMI of the pool without a release pin.** `latest` is resolved once per release and
    recorded; it moves only with a release move or a Kubernetes upgrade (4.3).
19. **New clusters in phase 3.** No cluster-autoscaler, a fixed-size system group, and the
    platform pool in the NodeGroups upgrade step (4.2, 4.10).

**Phase 0 results (offline part, 2026-11-20).** The five pinned charts were pulled and rendered with
`helm template` (Helm 4.1.0, the charts' own repositories, the platformValues of each
`pkg/catalog/definitions/*.yaml` with the `${...}` placeholders removed): kyverno 3.10.0,
aws-load-balancer-controller 3.6.2, external-secrets 2.15.0, istio `base` and `istiod` 1.32.0,
capsule 0.15.2. Every row below is read from the rendered output or the chart's templates and values.
Nothing is guessed: what a chart does not render is marked, with where the answer is.

Finding that changes the webhook rule of 4.11: **Karpenter runs in `kube-system` in this catalog**
(`karpenter.yaml`, `namespace: kube-system`), not in a `karpenter-system` namespace. A `Fail` webhook
that excludes `kube-system` therefore protects Karpenter and CoreDNS and the EBS controller, and the
rule's second namespace name does not exist.

| Component (chart) | Webhook configuration | Webhook | `failurePolicy` | `namespaceSelector` | Chart keys that set it |
|---|---|---|---|---|---|
| kyverno 3.10.0 | none in the chart: the admission controller creates its configurations at runtime (`autoUpdateWebhooks`) | resource validating/mutating, policy, exception, verify, cleanup, ttl | **not in the chart: needs a cluster** (kind or the sandbox account). The controller decides per policy. | The chart's `config.webhooks.namespaceSelector` applies; the shipped value excludes `kube-system`, `kube-public`, `kube-node-lease`, `kyverno`, `capsule-system`, `external-secrets`, `gpu-operator`, `istio-ingress`, `istio-system` (the Kyverno namespace is added by `config.excludeKyvernoNamespace`) | `config.webhooks.namespaceSelector`, `config.webhooks.objectSelector`, `features.forceFailurePolicyIgnore.enabled` (shipped `false`: resource webhooks stay fail closed), `features.excludeBootstrapResources.enabled` (shipped `true`: Node and CSR out of `Fail` webhooks) |
| aws-load-balancer-controller 3.6.2 | MutatingWebhookConfiguration `aws-load-balancer-webhook` | `mpod.elbv2.k8s.aws` (pods) | `Ignore` | namespaces labelled `elbv2.k8s.aws/pod-readiness-gate-inject=enabled`; object not the controller itself | `webhookNamespaceSelectors` |
| | | `quicid.elbv2.k8s.aws` (pods) | `Fail` | namespaces labelled `elbv2.k8s.aws/quic-server-id-inject=enabled`; object not the controller | `webhookNamespaceSelectors` |
| | | `alb-target-control.namespace.elbv2.k8s.aws` (pods) | `Fail` | namespaces labelled `elbv2.k8s.aws/alb-target-control-agent-injection=enabled`; pods without inject=false | none for the selector |
| | | `alb-target-control.object.elbv2.k8s.aws` (pods) | `Fail` | namespaces **without** that label (kube-system included), but only pods labelled `elbv2.k8s.aws/alb-target-control-agent-inject=true` | none for the selector |
| | | `mtargetgroupbinding.elbv2.k8s.aws` (TargetGroupBinding only) | `Fail` | none (cluster-wide, one CRD kind) | none |
| | ValidatingWebhookConfiguration `aws-load-balancer-webhook` | `vingressclassparams`, `vtargetgroupbinding`, `vingress` | `Fail` | none (Ingress, IngressClassParams, TargetGroupBinding in every namespace) | none (`serviceMutatorWebhookConfig.namespaceSelectors` exists for the Service mutator, which the shipped values do not render) |
| external-secrets 2.15.0 | ValidatingWebhookConfiguration `secretstore-validate` | `validate.secretstore...`, `validate.clustersecretstore...` | `Fail` | none (SecretStore and ClusterSecretStore objects only) | `webhook.failurePolicy` (applies to all three) |
| | ValidatingWebhookConfiguration `externalsecret-validate` | `validate.externalsecret...` | `Fail` | none (ExternalSecret objects only) | `webhook.failurePolicy` |
| istio base 1.32.0 | ValidatingWebhookConfiguration `istiod-default-validator` | `validation.istio.io` | `Ignore` (istiod flips it to `Fail` once ready, `base.validationFailurePolicy` pins it) | none (Istio config resources) | `base.validationFailurePolicy` |
| istiod 1.32.0 | ValidatingWebhookConfiguration `istio-validator-istio-system` | `rev.validation.istio.io` | `Ignore` | none; object label `istio.io/rev=default` | none |
| | MutatingWebhookConfiguration `istio-sidecar-injector` | `rev.namespace.sidecar-injector.istio.io`, `rev.object...`, `namespace.sidecar-injector.istio.io`, `object.sidecar-injector.istio.io` (pods) | `Fail` | opt-in by label: `istio.io/rev=default` or `istio-injection=enabled`; the `object` forms match namespaces with neither label, kube-system included, but only pods labelled `sidecar.istio.io/inject=true` | `sidecarInjectorWebhook.enableNamespacesByDefault`, `neverInjectSelector`, `alwaysInjectSelector` (pod level) |
| capsule 0.15.2 | none rendered as such: the chart puts them in `CapsuleConfiguration.spec.admission` and the controller creates `capsule-dynamic-webhook` (validating and mutating) | 23 validating, 12 mutating | `Fail` for all but 3 (`serviceaccount-references` and `config` validating, `metadata` mutating are `Ignore`) | pods, PVCs, services, ingresses, gateways, service accounts: namespaces labelled `capsule.clastix.io/tenant` (so kube-system is not matched). **Not selected by namespace and `Fail`:** `namespaces` (create, update, delete of every namespace, system serviceaccounts of kube-system, kyverno, capsule-system and cert-manager excluded by a match condition), `replications` (all resources, all scopes), `managed` (by object label), the Capsule CRDs | `webhooks.hooks.<name>.failurePolicy`, `.namespaceSelector`, `.objectSelector`, `.matchConditions`; `webhooks.matchConditions` for all |

What this means for 4.11:

- No `Fail` webhook of the five matches pods of `kube-system` unless the pod opts in by a label, except
  the two pod webhooks of Kyverno (runtime), which the shipped `namespaceSelector` excludes from
  `kube-system`. The webhook rule holds for ALB, External Secrets, istio and Capsule's pod webhooks.
- An outage of the pool does stop: Ingress and TargetGroupBinding writes (ALB), SecretStore and
  ExternalSecret writes (External Secrets), and pod creation in opted-in namespaces (istio, ALB readiness
  gate). Karpenter's own pods and CoreDNS are not in that set. Had Capsule moved to the pool, it would
  also stop every namespace create, update and delete and Capsule's CRDs (`namespaces` and
  `replications`), and pod creation in Capsule tenants; with Capsule on the bootstrap group it does not.
- Capsule's `namespaces` and `replications` webhooks are `Fail` with no namespace selector. Moving
  Capsule to the pool makes a pool outage block namespace changes cluster-wide (the platform's own
  namespaces included), where today it is the same outage of the system group. **Decided:** Capsule
  is not moved. It joins the bootstrap set (4.1, decision 11), and its webhooks keep `Fail`.
- Still to check on a cluster (needs a cluster; kind is enough): Kyverno's runtime webhook
  configurations (names, `failurePolicy` per webhook with the shipped policies, `namespaceSelector` as the
  controller writes it); Capsule's generated `capsule-dynamic-webhook`, which should equal the rendered
  `CapsuleConfiguration`.

| Phase 0 check | Result |
|---|---|
| Webhook `failurePolicy` and `namespaceSelector` of the five charts | Above, from the rendered charts. Kyverno's runtime webhooks: needs a cluster. |
| Managed node group scale-in against PodDisruptionBudgets, and which node it removes | **Needs a sandbox account.** Not answerable offline. The design does not rely on it (4.9, M3). |
| Idle capacity that `WhenEmpty` leaves, per class | **Needs a sandbox account** (a running pool). |
| Platform pool steady node count per class | **Needs a sandbox account.** Offline: the platform set requests 675m CPU and 1,088 MiB at minimum replicas (`pkg/catalog` `CheckPlatformSizing` on the shipped catalog, measured before Capsule moved to the bootstrap set; to be re-run without it), which fits one `t3.large` or `m7i.large`; the floor of two nodes in 4.6 is for spread, not for these requests. |
| Requests of the bootstrap set on a `t3.medium` | Offline, from `CheckBootstrapSizing`: 2 x `t3.medium` allocate 3,860m CPU, 6,492 MiB, 34 pods; the bootstrap set requests 1,750m (45%), 3,172 MiB (49%) and 18 pods (53%). It fits, and the 17-pod limit is the one counted. These numbers are without Capsule. With Capsule on the bootstrap group (2 x 50m and 96 MiB, from `capsule.yaml`), the set is 1,850m (48%), 3,364 MiB (52%) and 20 pods (59%), before its 100m and 128 MiB hook job; `CheckBootstrapSizing` must add Capsule (phase 3). An `m6i.large` (29 pods) was not measured on a node. |

The class values of 4.6 are neither confirmed nor changed: they need the sandbox measurements.

**Open questions.** None remain. The facts this design depends on are the Phase 0 checks in
section 6.
