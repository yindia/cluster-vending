# Cluster topology

The cluster detail page's topology reads `ClusterStatus.architecture` through the
existing generated Connect API. Kubernetes remains the source of truth; no AWS
SDK discovery, infrastructure writes, or database is introduced.

## Server projection

`pkg/reconcile/cluster/architecture.go` reads allowlisted, recorded managed
resources within the cluster's tenant and ownership scope. The AWS adapter in
`architecture_aws.go` projects control planes, node groups, add-ons, Helm releases,
subnets, security groups and launch templates. It preserves explicit configured
and observed relationships, including disagreements. It never infers links from
resource names, desired add-on lists, or matching labels alone.

Relations include `has_node_group`, `has_addon`, `has_component`, `uses_subnet`,
`uses_security_group` and `uses_launch_template`. A Helm component link requires
an explicit Release → ProviderConfig → ClusterAuth → Cluster binding. The
connection plumbing is used as evidence; Secret payloads, credentials, Helm
values and authentication objects are not exposed by this projection.

External subnet, security-group and launch-template identifiers may appear as
`reference_only` facts. They have no independently verified readiness or inferred
location. Missing references remain missing; the UI must not invent resources.
Older snapshots may omit architecture until a live read or controller refresh.

## Presentation semantics

The default architecture shows only the EKS control plane, managed node groups,
and actual worker facts when available. Add-ons, Helm releases and Kubernetes
internal components remain in the source inventory but never appear in this view.
Network is off by default: its nodes, edges and layout space are absent.

Show Network performs a tenant-scoped live network read and reuses the network
architecture graph, nested containers, route relationships, icons and router.
The control plane stays outside customer subnets. Each node group appears once,
with explicit subnet associations even when it spans Availability Zones. A worker
is placed inside a subnet only when its recorded relationship identifies exactly
one subnet. Reference-only workers and fabricated capacity are never rendered.

The measured layout is recalculated for the visible graph when the toggle changes.
Independent resource identities and route entries remain inspectable, including
Elastic IP badges attached to NAT gateways. Selecting routing resources highlights
their recorded paths; a dependency edge is not proof of reachability.

## Available metadata

The current API does not supply a complete worker inventory or per-node-group live
capacity. Missing counts and worker properties are shown as Not reported.
Default-pool configuration is shown only when one actual node group can be matched
unambiguously to the recorded EKS ARN. Initial desired size is labeled as a
create-time setting, never as the live autoscaler target. Configuration is never
copied onto unrelated node groups.

## Validation

Model fixtures cover empty, multiple-group, cross-AZ, shared-subnet, actual worker,
partial-resource and network-toggle cases. Browser fixtures exercise default and
network modes, shared and AZ-local NAT, scope rejection, inspection, light/dark
fullscreen and mobile layouts. Geometry assertions check painted nodes and edges,
card intersections, containment and duplicate identities.
