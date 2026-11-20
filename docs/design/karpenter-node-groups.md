# Node layout: one system node group, Karpenter for users

Status: controller side built (see "Status: built" at the end); decided design. This replaces the "platform ships no NodePool"
rule in `docs/catalog.md` and the unbuilt preset in
`docs/design/component-usage.md` §6.4–6.5. An earlier revision of this
design had a second, Karpenter-managed `platform` node group; there is one
platform node group now, `system` (see "Migration of existing clusters").

## Decisions

| Question | Decision |
|---|---|
| Platform node group | **One**, `system`: the default EKS managed node group. It runs Karpenter, CoreDNS, every platform component, the tenant's components and the EKS add-on controllers. There is no Karpenter pool for the platform and no autoscaler for the group. |
| Its size | Fixed: min = max = desired = `nodeGroups.system.size` (default 2, at least 2: Karpenter runs two replicas with anti-affinity). Set by the platform admin only; a change resizes existing clusters in place. |
| Its machines | `nodeGroups.system.instanceType` (default `t3.medium`), `capacityType`, `diskSizeGiB`: new clusters only. A cluster created before keeps the instance type, capacity type, AMI and disk of its stored spec: EKS cannot change them in place, the node group would be replaced. |
| Its placement | Label `node.cluster-vending.io/pool=system`, taint `node.cluster-vending.io/pool=system:NoSchedule`. Users cannot change its taints, labels or scaling (`failed_precondition`). |
| User node groups | `ClusterSpec.node_groups`: Karpenter NodePools, each with its own EC2NodeClass. Users add, change and remove them. Karpenter serves them only. |
| Delivery | The controller server-side applies the user groups' EC2NodeClasses and NodePools into the workload cluster through the kubeconfig it already uses (teardown, scans), on every pass. A platform object that was edited or deleted is put back. No chart. A cluster with no user node groups gets nothing applied. |
| cluster-autoscaler | Removed from the platform set. Existing clusters have it uninstalled through the normal component removal. |
| Existing clusters | Move in place: the default group's taints, labels and size change (no replacement: instance types and AMI stay), platform components get the system placement, and the old `platform` NodePool and EC2NodeClass are removed once nothing needs them (see "Migration"). Imported clusters are untouched until taken over; a takeover with `skip_platform_components` keeps its adopted group as it is. |
| User fields | Instance types and families, architectures, capacity types (on-demand, Spot), labels, taints, CPU and memory limits. |

## Placement

| Where | Label (nodeSelector) | Taint | What runs |
|---|---|---|---|
| default managed node group (`system`) | `node.cluster-vending.io/pool=system` | `node.cluster-vending.io/pool=system:NoSchedule` | Karpenter, CoreDNS, the platform components, the tenant's components, the EKS add-on controllers (the EBS CSI controller, and any placed add-on) |
| user NodePool `<name>` | `node.cluster-vending.io/pool=<name>` plus the user's labels | the user's taints | the user's workloads |

DaemonSets (vpc-cni, kube-proxy, pod identity agent, istio-cni, ztunnel)
tolerate every taint and run everywhere. The platform's pods requesting more
than a `system` node holds (all of them, with their DaemonSets, are on 2
nodes) are a sizing question, not a placement one: see "Sizing" below.

Placeholders (`pkg/component/placeholders.go`): `${system.nodeSelector}` /
`${system.tolerations}` are the default managed node group's. `${platform.nodeSelector}`
/ `${platform.tolerations}` are aliases of them, kept so the catalog's values do
not churn; either name gives the same value, from live state.

## Install order

1. EKS add-ons, with every other cluster resource: the cluster is RUNNING
   (and the default managed node group Ready).
2. The platform components, in their definitions' `sequence` (and `requires`)
   order, through the Helm gate (one operation at a time,
   [helm-reliability.md](helm-reliability.md)): Karpenter first (sequence 0).
   Nothing is held for a node group: they run on the system group, which
   exists before the cluster is RUNNING.
3. The tenant components (they already wait for every platform component to
   be Ready).
4. Once Karpenter is Ready (its platform component phase READY: chart
   deployed, so its CRDs exist) and its IAM is, the controller applies the
   user node groups, if there are any.

**Sequence in the definition.** `ComponentDefinition.spec.sequence`
(int32, default 100): platform components install in ascending sequence;
one starts only once every platform component of a lower sequence is
deployed and Ready (the same mechanism as `requires`, which still applies
within and across sequences). Karpenter is `0`. Removal runs in reverse
sequence.

## Contract (`cloud.v1`, additive)

```proto
// ClusterSpec
repeated NodeGroupSpec node_groups = 16;   // user Karpenter node groups, max 20

message NodeGroupSpec {
  string name = 1;                         // DNS label, unique; not "platform", "default", "system" (reserved)
  repeated string instance_types = 2;      // e.g. m7i.large; empty: any of instance_families
  repeated string instance_families = 3;   // e.g. m7i, c7g
  repeated Architecture architectures = 4; // AMD64, ARM64; empty: AMD64
  repeated CapacityType capacity_types = 5;// ON_DEMAND, SPOT; empty: ON_DEMAND
  map<string, string> labels = 6;
  repeated Taint taints = 7;
  NodeGroupLimits limits = 8;              // optional
}
message NodeGroupLimits { optional int32 cpu = 1; optional int32 memory_gib = 2; }

// ClusterStatus
repeated NodeGroupStatus node_groups = 52; // `system` first (platform_managed), then the user's
message NodeGroupStatus {
  string name = 1; bool platform_managed = 2;
  NodeGroupPhase phase = 3;                // PENDING, READY, FAILED, DELETING
  string message = 4; int32 nodes = 5;
}
```

- `spec.node_groups` is one update-mask path (the list is compared whole).
- `default_node_pool` stays (field 4, no longer `required`): when omitted the
  platform fills it; a request that changes its taints, labels or scaling is
  refused (`failed_precondition: the default node group is managed by the
  platform`). Instance types, capacity type and AMI stay as they are for
  existing clusters. The size is the platform admin's setting
  (`nodeGroups.system.size`), not the spec's: the render takes the current
  setting, whatever the stored spec says.
- CRD `ClusterSpec.NodeGroups` / `ClusterStatus.NodeGroups` mirror it;
  `pkg/convert` converts both ways.

## AWS

- **Node role** `<prefix>-karpenter-node` (worker, CNI, ECR read, SSM core)
  and an explicit `EC2_LINUX` access entry for it, so node access does not
  hang on the managed group. Karpenter's controller policy gets
  `iam:PassRole` on it; Karpenter creates the instance profile from
  `EC2NodeClass.spec.role`.
- **Interruption queue** `<prefix>-karpenter` (SQS, SSE) with EventBridge
  rules for Spot interruption, rebalance, instance state change and AWS
  Health; `settings.interruptionQueue` set; the SQS statement added back to
  the controller policy.
- **Spot service-linked role** `AWSServiceRoleForEC2Spot`: account-level,
  added to the account's provisioner CloudFormation template (created if
  missing).
- **EC2NodeClass** (user groups only): `role: <prefix>-karpenter-node`; subnets and security
  groups by `karpenter.sh/discovery=<cluster>` or, where another cluster of
  the network holds that tag, `kubernetes.io/cluster/<cluster>`; AMI
  `alias: al2023@<pin>` from the release's pin for the cluster's Kubernetes
  minor.

## Upgrades

The Kubernetes upgrade's "node groups" step also moves the user groups'
EC2NodeClasses' AMI pin to the target minor's; Karpenter drift replaces
their nodes. The step is settled when every NodeClaim runs the target
version (read through the workload kubeconfig; only a cluster that has user
node groups is read). The system group upgrades through the existing managed
node group path (the node group's `version`, with the rest of the step).

## Status and deletion

Each pass reads the user NodePools' `Ready` condition and node counts into
`status.node_groups`. The first entry is `system` (`platform_managed`): its
phase is the readiness of the EKS `NodeGroup` managed resource (the newest,
while a replaced one is retired), its node count that of the nodes labelled
`eks.amazonaws.com/nodegroup=<the default group's name>`, read by the same
paginated node reads. A user node group removed from the spec is deleted from
the workload cluster (Karpenter drains its nodes). Cluster deletion runs
in the reverse of the install order above, each stage gone before the next:
load balancers, tenant components, platform components by descending
sequence (above 0), then the node groups (the NodePools, the NodeClaims
Karpenter has left, the EC2NodeClasses, read with
bounded pages and a deadline per pass; a wait ends after the teardown
timeout), then Karpenter itself (its chart, its IAM, the interruption queue,
rules and targets, the node role and its access entry), then the EKS
add-ons, then the rest (the system group among it) ([../architecture.md](../architecture.md),
"Deletion"; [../controller.md](../controller.md)). Karpenter stays until
its nodes are gone, so it can terminate them and remove the instance
profiles.

## Status: built

Controller side, as designed:

- `ComponentDefinition.spec.sequence` (default 100; Karpenter 0). The plan
  orders by sequence; a component's first Release waits for the last Release
  of every lower-sequence component (Ready and deployed) and keeps it with a
  Usage. Requiring a higher sequence is a catalog error.
- No component is held for a node group: they follow `sequence` and
  `requires` and the Helm gate alone.
- `pkg/reconcile/cluster/nodegroups*.go` applies and reads the node groups
  (`Deps.NodeGroupClients`, `management.WorkloadClients`), after
  `installPlatform` and only once Karpenter is READY; the IAM readiness wait
  gates only the user groups. Status reads are
  bounded and never fail a pass; the last good phase is kept on errors.
- The AMI pin is `nodeAmiPin` per minor in `pkg/catalog/release.yaml` and
  `Release.spec.content.kubernetes[]` (unset: `al2023@latest`). The upgrade
  moves it with the node groups step; the step also waits for every
  Karpenter node to run the target kubelet minor.
- `component.IAMOutputs.ClusterPrefix` is passed; a taken-over cluster
  (without `skip_platform_components`) renders Karpenter's queue and node
  role.

Deviations and deferred:

- Security groups are selected with `aws:eks:cluster-name=<cluster>` (the tag
  EKS sets itself, see `TagKarpenterDiscovery`), not the discovery tag;
  subnets follow the design.
- Not covered without a real cluster: the SSA of Karpenter's kinds against
  a real API server (tests use a fake client), Karpenter's drift replacing
  nodes on an AMI pin change, and the `Ready` condition's real wording.
- The default managed node group's instance type for a new cluster is
  `t3.medium` (`nodeGroups.system.instanceType`; machines are t3, x86 and
  burstable: sustained CPU above the baseline is throttled, or charged in
  unlimited mode). A cluster created before keeps the instance type of its
  stored spec (no node group replacement). See "Sizing" for what 2 nodes
  hold.
- The system group's YAML is not in `ListClusterNodeGroupManifests`: the
  contract's `NodeGroupManifest` has the two Karpenter fields only. The
  response has a `system` entry (`platform_managed`) without YAML. An
  additive `string node_group_yaml` carrying the rendered EKS `NodeGroup`
  (`spec.forProvider`) is the way to show it; it needs `make generate`.
- A release published before `nodeAmiPin` falls back to the pin this build
  ships for the minor (`pkg/catalog/release.yaml`), never to `latest`; only
  when that is missing too the class pins `latest` and the platform group's
  status says so.

### Sizing

The system group holds Karpenter (2 replicas, 250m CPU / 512 MiB requested
each, limits 1 CPU / 1 GiB), CoreDNS, every platform component and the add-on
controllers, with the DaemonSets on each node. On 2 x `t3.medium` (2 vCPU,
4 GiB, 17 pods per node; allocatable 1.93 CPU / 3.2 GiB per node by the EKS
AL2023 reservations, so 3.86 CPU / 6.3 GiB / 34 pods) the requests sum to 2.4
CPU (63%), 4.3 GiB (66%) and 30 pods (88%). The pod count is the tight
limit. Only Karpenter keeps two replicas among the platform components
(Capsule keeps 2 too); the rest run one, without a PodDisruptionBudget so a
drain can proceed. Requests are explicit in the definitions and summed by
`pkg/catalog/system_group_sizing_test.go`, which fails above 85% of the
default group's CPU or memory allocatable or above its pod capacity: size
`nodeGroups.system` up (`t3.large`, or more nodes) before adding replicas or
components.

### Migration of existing clusters

A cluster created before this build keeps working while it moves. The
placement placeholders resolve from **live state**, never from what the
platform intends to roll out:

- `${system.*}` is what the default managed node group carries now: its
  labels and taints as the provider reports them (`status.atProvider` of the
  live `NodeGroup`). On an existing cluster that is the old placement until
  the node group really has `node.cluster-vending.io/pool=system` and its
  taint; then the new one. Where the node group is not reported yet, a
  cluster whose components started keeps its stored default node pool, a new
  one gets the fixed system placement.
- `${platform.*}` is an alias of `${system.*}`: the same live value.
- A component already installed keeps running on its current placement and
  moves with one Helm upgrade (the Helm gate orders them) when the target
  becomes live. Components that have not started on a new cluster follow
  `sequence` and `requires` only.
- **The old `platform` NodePool and EC2NodeClass** of a cluster built before
  the system group took over (labelled `managed-by=platform`) are removed by
  the applier's normal removal path, but only in this order, and until it
  holds they are kept exactly as they are (not applied again, not changed):
  1. the system node group has the system label and taint, is ACTIVE at
     the admin's size and that many of its nodes are Ready;
  2. every platform component is READY with its Helm release settled
     (nothing written in this pass, the gate's `unchanged`), every tenant
     component is READY, and no pod but a DaemonSet's or a static one is
     left on the old pool's nodes (a paginated pod read), so nothing still
     selects only the old pool;
  3. then the NodePool `platform` is deleted (Karpenter drains and
     terminates its nodes), the EC2NodeClass `platform` once the NodePool and
     its nodes are gone.
  The system group's status message says where it is: "moving components off
  the old platform pool: N left (...)", "removing the old platform pool: ...".
  There is no `platform` row in `status.node_groups`.
- The user groups' EC2NodeClasses and NodePools are applied only when the Karpenter node
  role, its four policy attachments and its `EC2_LINUX` access entry are
  Ready and the Karpenter controller policy as applied (the live `Policy`,
  Ready) allows `iam:PassRole` on that role; until then the groups report
  PENDING "waiting for Karpenter's IAM (...)" (a group that had a phase
  keeps it).
- The EventBridge rules and targets set `eventBusName: default` (the
  provider cannot read them without it). On existing objects that is an
  add-only change (unset means default), never a rejected immutable one.

## Instance allowlist and manifests

- The layout is the `nodeGroups` section of the config file, the same shape
  for `serve` and `controller` (`config.NodeGroups`, one struct, validated
  at startup with clear errors), so the server's checks and the controller's
  render cannot drift:

  ```yaml
  nodeGroups:
    system:                      # the default EKS managed node group
      instanceType: t3.medium    # new clusters only
      size: 2                    # desired = min = max; at least 2
      capacityType: ON_DEMAND    # new clusters only
      diskSizeGiB: 0             # new clusters only; 0: EKS's default
    karpenter:                   # what users may create
      instanceFamilies: [t3]
      instanceTypes: []
      defaults:                  # for a user group that leaves them unset
        architectures: [AMD64]
        capacityTypes: [ON_DEMAND]
  ```

  Each value is also a flag and `CV_*` variable (`system-node-group-size`,
  `system-node-group-instance-type`, `system-node-group-capacity-type`,
  `system-node-group-disk-size-gib`, `node-group-instance-families`,
  `node-group-instance-types`, `node-group-default-architectures`,
  `node-group-default-capacity-types`; lists comma-separated); they win over
  the file. The two allowlist names are the ones that shipped before the
  section and still work as flat file keys. An empty list or `none` restricts
  nothing. Validation: size at least 2, an instance type like `t3.medium`,
  families like `t3`, `ON_DEMAND`/`SPOT`, `AMD64`/`ARM64`.
- The server refuses (InvalidArgument) a new or changed user group whose
  families are outside the family list, whose types are in neither list nor
  of an allowed family, or, when a family list is set, that names neither (the
  default categories would sidestep it). Unchanged groups are not rechecked,
  so narrowing the list never blocks an unrelated update. The system group
  is not subject to it. The controller gives a user group that sets no
  architecture or capacity type the `defaults`.
- `GetNodeGroupOptions` returns the allowlist, architectures and capacity
  types (read-only); the UI suggests and pre-checks with it.
- `ListClusterNodeGroupManifests`: `system` first (`platform_managed`, no
  YAML, see above), then each user group, rendered with
  `reconcilecluster.Reconciler.NodeGroupObjects`, the function the applier's
  input comes from, as `sigs.k8s.io/yaml`; none for an imported cluster that
  was not taken over. The release pin is read from the recorded
  `status.platform_release`.

## Node group detail page and node list

- Each node group has a page, `/clusters/<id>/node-groups/<name>` (the Node
  groups tab links `system`, the default managed group, then the user
  groups). It shows the group's phase, the platform-managed mark, the
  group's settings, View YAML, and a table of its nodes (name, status, instance
  type, zone, capacity, kubelet version, age, NodeClaim) with a status filter and
  a name search, paged by the server at 100 nodes.
- The nodes come from `ListNodeGroupNodes`, which reads the group's shard of the
  `nodes` result in the cluster's results bucket, never the workload cluster
  ([cluster-results-s3.md](cluster-results-s3.md): Scale, and "Sharding and the
  `nodes` kind"). The applier collects the rows during the node and NodeClaim reads
  it already makes; the Cluster keeps counts only. Before the first collection, with
  no bucket or when it cannot be read, the page says so and keeps the group's
  settings and counts.
- A user group named `index` has no node list (the name is the index's object).
