# Karpenter-only node groups

Status: controller side built (see "Status: built" at the end); decided design. This replaces the "platform ships no NodePool"
rule in `docs/catalog.md` and the unbuilt preset in
`docs/design/component-usage.md` §6.4–6.5.

## Decisions

| Question | Decision |
|---|---|
| Workload nodes | Karpenter only. No EKS managed node group except the default one. |
| Default managed node group | One per cluster, platform-managed (`t3.medium` for a new cluster): fixed size (2 nodes, Karpenter runs two replicas), tainted and labelled so only Karpenter and CoreDNS run there. Users cannot change its taints, labels or scaling. |
| Platform node group | One Karpenter `NodePool` + `EC2NodeClass` named `platform`, owned by the platform. Platform components, tenant components and EKS add-on controllers run there. Users cannot create, change or delete it. |
| User node groups | `ClusterSpec.node_groups`: further Karpenter NodePools, each with its own EC2NodeClass. Users add, change and remove them. |
| Delivery | The controller server-side applies the EC2NodeClasses and NodePools into the workload cluster through the kubeconfig it already uses (teardown, scans), on every pass. A platform object that was edited or deleted is put back. No chart. |
| cluster-autoscaler | Removed from the platform set. Existing clusters have it uninstalled through the normal component removal. |
| Existing clusters | Move to the new layout: the default group's taints, labels and size change in place (no replacement: instance types and AMI stay), platform components get the new placement and reschedule onto the platform pool. Imported clusters are untouched until taken over; a takeover with `skip_platform_components` keeps its adopted group as it is. |
| User fields | Instance types and families, architectures, capacity types (on-demand, Spot), labels, taints, CPU and memory limits. |

## Placement

| Where | Label (nodeSelector) | Taint | What runs |
|---|---|---|---|
| default managed node group | `node.cluster-vending.io/pool=system` | `node.cluster-vending.io/pool=system:NoSchedule` | Karpenter, CoreDNS |
| `platform` NodePool | `node.cluster-vending.io/pool=platform` | `node.cluster-vending.io/pool=platform:NoSchedule` | platform components, tenant components, EKS add-on controllers (ebs-csi controller, and any placed add-on) |
| user NodePool `<name>` | `node.cluster-vending.io/pool=<name>` plus the user's labels | the user's taints | the user's workloads |

DaemonSets (vpc-cni, kube-proxy, pod identity agent, istio-cni, ztunnel)
tolerate every taint and run everywhere.

Placeholders (`pkg/component/placeholders.go`): `${system.nodeSelector}` /
`${system.tolerations}` keep meaning "the default managed node group" and are
used by Karpenter only. New `${platform.nodeSelector}` /
`${platform.tolerations}` are the platform pool; every other platform and
tenant component's catalog values switch to them.

## Install order

1. EKS add-ons, with every other cluster resource: the cluster is RUNNING
   (and the default managed node group Ready).
2. Karpenter's chart: the first platform component.
3. Once Karpenter is Ready (its platform component phase READY: chart
   deployed, so its CRDs exist), the controller creates the platform node
   group (`EC2NodeClass` + `NodePool` `platform`), then the user node groups.
   Nothing of the node groups is applied before that.
4. Once the platform node group is Ready (`NodePool` `Ready` condition), the
   other platform components install, in their definitions' sequence.
5. Then the tenant components (they already wait for every platform
   component to be Ready).

**Sequence in the definition.** `ComponentDefinition.spec.sequence`
(int32, default 100): platform components install in ascending sequence;
one starts only once every platform component of a lower sequence is
deployed and Ready (the same mechanism as `requires`, which still applies
within and across sequences). Karpenter is `0`, the only sequence that
runs before the platform node group: every platform component with a
sequence above `0` is also held back, with a status saying so ("waiting for
the platform node group to be Ready"), until the platform node group is
Ready. Removal runs in reverse sequence.

## Contract (`cloud.v1`, additive)

```proto
// ClusterSpec
repeated NodeGroupSpec node_groups = 16;   // user Karpenter node groups, max 20

message NodeGroupSpec {
  string name = 1;                         // DNS label, unique; not "platform", "default", "system"
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
repeated NodeGroupStatus node_groups = 52; // the platform group first, then the user's
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
  existing clusters.
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
- **EC2NodeClass**: `role: <prefix>-karpenter-node`; subnets and security
  groups by `karpenter.sh/discovery=<cluster>` or, where another cluster of
  the network holds that tag, `kubernetes.io/cluster/<cluster>`; AMI
  `alias: al2023@<pin>` from the release's pin for the cluster's Kubernetes
  minor.

## Upgrades

The Kubernetes upgrade's "node groups" step also moves the EC2NodeClasses'
AMI pin to the target minor's; Karpenter drift replaces the nodes. The step
is settled when every NodeClaim runs the target version (read through the
workload kubeconfig).

## Status and deletion

Each pass reads the NodePools' `Ready` condition and node counts into
`status.node_groups`. A user node group removed from the spec is deleted from
the workload cluster (Karpenter drains its nodes). Cluster deletion runs
in the reverse of the install order above, each stage gone before the next:
load balancers, tenant components, platform components by descending
sequence (above 0), then the node groups (user NodePools, the platform
NodePool, the NodeClaims Karpenter has left, the EC2NodeClasses, read with
bounded pages and a deadline per pass; a wait ends after the teardown
timeout), then Karpenter itself (its chart, its IAM, the interruption queue,
rules and targets, the node role and its access entry), then the EKS
add-ons, then the rest ([../architecture.md](../architecture.md),
"Deletion"; [../controller.md](../controller.md)). Karpenter stays until
its nodes are gone, so it can terminate them and remove the instance
profiles.

## Status: built

Controller side, as designed:

- `ComponentDefinition.spec.sequence` (default 100; Karpenter 0). The plan
  orders by sequence; a component's first Release waits for the last Release
  of every lower-sequence component (Ready and deployed) and keeps it with a
  Usage. Requiring a higher sequence is a catalog error.
- Components above sequence 0 are PENDING ("waiting for the platform node
  group to be Ready") until `status.node_groups` has the platform group
  READY. Only when the catalog has Karpenter, and only before another
  component was started: a wavering node group status never holds back what
  is installed.
- `pkg/reconcile/cluster/nodegroups*.go` applies and reads the node groups
  (`Deps.NodeGroupClients`, `management.WorkloadClients`), after
  `installPlatform` and only once Karpenter is READY. Status reads are
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
- The platform NodePool is small burstable x86: family `t3`, sizes `medium`,
  `large`, `xlarge`, amd64, on-demand (`crossplaneaws.PlatformInstanceFamilies`
  and `PlatformInstanceSizes`, one place to change). It is the platform's own
  choice and ignores the admin allowlist, so an allowlist with only Graviton
  families no longer leaves it with nothing to launch. A node of the pool
  has 2 vCPU and 4 GiB (t3.medium) at the least: components with larger
  requests are placed on the larger sizes. t3 is burstable: sustained CPU
  above the baseline is throttled (or charged, in unlimited mode).
- The default managed node group's instance type for a new cluster is
  `t3.medium` (`DefaultSystemInstanceType`; Karpenter's 1 CPU / 1Gi request
  per replica and CoreDNS fit). A cluster created before keeps the
  instance type of its stored spec (no node group replacement).
- A release published before `nodeAmiPin` falls back to the pin this build
  ships for the minor (`pkg/catalog/release.yaml`), never to `latest`; only
  when that is missing too the class pins `latest` and the platform group's
  status says so.

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
- `${platform.*}` is the platform pool only once `status.node_groups` shows
  the platform group READY; until then it is the same as `${system.*}`.
- A component already installed keeps running on its current placement and
  moves with one Helm upgrade (the Helm gate orders them) when the target
  becomes live. Components that have not started on a new cluster still
  wait for the platform node group (the hold above).
- The EC2NodeClasses and NodePools are applied only when the Karpenter node
  role, its four policy attachments and its `EC2_LINUX` access entry are
  Ready and the Karpenter controller policy as applied (the live `Policy`,
  Ready) allows `iam:PassRole` on that role; until then the groups report
  PENDING "waiting for Karpenter's IAM (...)" (a group that had a phase
  keeps it).
- The EventBridge rules and targets set `eventBusName: default` (the
  provider cannot read them without it). On existing objects that is an
  add-only change (unset means default), never a rejected immutable one.

## Instance allowlist and manifests

- `node-group-instance-families` and `node-group-instance-types` (flag,
  `CV_NODE_GROUP_INSTANCE_*`, config file as comma-separated values) are set
  the same on `serve` and `controller`. Empty restricts nothing. The server
  refuses (InvalidArgument) a new or changed user group whose families are
  outside the family list, whose types are in neither list nor of an
  allowed family, or, when a family list is set, that names neither (the
  default categories would sidestep it). Unchanged groups are not rechecked,
  so narrowing the list never blocks an unrelated update. The platform
  NodePool is not subject to it (see above); user groups with neither
  families nor types get the default categories.
- `GetNodeGroupOptions` returns the allowlist, architectures and capacity
  types (read-only); the UI suggests and pre-checks with it.
- `ListClusterNodeGroupManifests` renders the objects with
  `reconcilecluster.Reconciler.NodeGroupObjects`, the function the applier's
  input comes from, as `sigs.k8s.io/yaml`; none for an imported cluster that
  was not taken over. The release pin is read from the recorded
  `status.platform_release`.

## Node group detail page and node list

- Each node group has a page, `/clusters/<id>/node-groups/<name>` (the Node
  groups tab links the platform group, the user groups and the default managed
  group, `system`). It shows the group's phase, the platform-managed mark, the
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
