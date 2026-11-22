# PROP-02: Cluster data: live reads for nodes, violations and events; stored results for deprecations and recommendations

| | |
|---|---|
| Status | Proposed |
| Author | `<author>` |
| Date | 2026-11-20 |
| Reviewers | `<reviewers>` |
| Related | [architecture.md](../architecture.md): 1. Overview (Results in S3), 3. How it works (Results in S3, Scanners, Node groups configuration), 7. Known limits; [PROP-01](PROP-01-admin-configuration.md) (format); [PROP-03](PROP-03-component-placement.md) (component placement) |

## 1. Summary

The console shows four kinds of workload data: node rows and counts, policy violations, policy and
admission events, deprecated API findings and resource recommendations. Today the controller collects all
of them on passes that run every 5 minutes to 6 hours, writes them to S3, and the API reads them back.
Nodes get no resource data at all.

This revision splits the kinds by what they need:

- **Live only, no S3 copy:** node rows, counts and resources (per node and per group), policy violations,
  and policy and admission events from the last hour. Kubernetes keeps events about an hour, so a live
  read covers the window a stored copy would. A read-only **live reader** (a second Deployment of the same
  binary) reads the workload cluster on request and returns sorted, paginated pages from a short-lived
  materialized view. The API server holds no workload credentials.
- **Stored in S3, unchanged mechanism:** deprecation findings (a scan costing up to 2 minutes and Helm
  Secret reads) and recommendations (a week of usage history). The API keeps read-only S3 access for
  these two kinds only.
- **Cluster status summary:** the controller writes a small, bounded summary on each pass (node counts,
  per-group totals, violation counts by severity, last-hour event counts, and the existing deprecation and
  recommendation summaries). Fleet dashboards, the upgrade readiness gates and the unreachable case read it.

The API adds `sort_by`, `sort_order` and `live_at` on the live list RPCs, and a `GetNodeResources` RPC
for group totals. It adds no `source` field: the live kinds are always live, and the stored kinds are
always stored. Existing deprecation and recommendation requests and tokens do not change.

Targets: 100 clusters, most of them small, a tail of 1,000-node and 10,000-pod clusters. No cluster of
that size has been load tested ([architecture.md, Known limits](../architecture.md#7-known-limits)).
The numbers in section 6 are estimates from the code's own limits and must be measured in phase 3.

## 2. How it works today

Checked against `main`. Paths are the code that does each step. This is the baseline the migration
starts from.

### 2.1 Per kind

| Kind | Collected by, and when | Stored where | API read path | Paging and sort | Freshness | Limits in code |
|---|---|---|---|---|---|---|
| Node rows and counts | Controller, the node pass of every cluster reconcile (`pkg/reconcile/cluster/nodegroups.go`, `readNodes`). Runs on change, and every 10 minutes when settled (`resyncDelay`). Rows written at most once a minute (`nodeRowsInterval`, `pkg/reconcile/cluster/node_rows.go`). | Rows: S3 `latest/nodes/<group>.json` and `latest/nodes/index.json` (the only sharded kind, `results.Nodes`). Counts: `status.nodeGroups[].nodes`. | `ListNodeGroupNodes` (`pkg/server/cluster/node_group_nodes.go`) reads the index, then one shard. | Name order only. Page 100 by default, 500 at most. Filter Ready or NotReady; name contains. | Pass cadence (up to 10 minutes settled). | Node list pages of 500 (`nodeListLimit`). |
| Node allocatable, requested | **Not collected.** `nodeRowOf` keeps labels, kubelet version, provider ID, conditions, taints count and creation time. | None | None | None | None | Pods are listed only to count pods on the legacy pool (`podsOnOldPool`); the count is not kept. |
| Deprecated APIs | Controller `DeprecationReconciler` every 6 hours, and 15 minutes after a failed scan. Metadata-only lists, 250 per page; Helm release Secrets 20 per page; 2-minute deadline (`pkg/deprecation/scan.go`). | Full findings: S3 `latest/deprecations.json`, history 30 days. Summary on `status.deprecations` (at most 200 findings on the CR). | `ListClusterDeprecations`: the bucket object when the summary names it (token bound to the ETag), otherwise the CR copy (plain decimal offset). | Removed first, then scan order. Page 50 by default, 200 at most. `only_removed`. | Up to 6 hours old. | Only objects whose manifest is kept are seen: `last-applied` annotations and Helm release Secrets. |
| Resource recommendations | Controller `RecommendationReconciler` every 5 minutes (`pkg/reconcile/recommendations`, `pkg/recommend`). Reads workloads, `metrics.k8s.io` pod usage and VPA objects. Up to 20,000 objects per kind. | Usage: ConfigMap `<cluster>-usage-history` in the tenant namespace, one bucket per day, 7 days, 2,000 containers at most. Recommendations: S3 `latest/recommendations.json` (a single object, not sharded), history at most hourly (`historyGate`). Summary on `status.recommendations`. | `ListClusterRecommendations`: the bucket object when the summary names it. Otherwise computed from the usage ConfigMap through the management-cluster store. | Namespace filter; `only_changes`. Page 50 by default, 200 at most. | Up to 5 minutes old. | The estimate needs at least a day of usage. Advice only. |
| Policy violations | Controller `PolicyReconciler` every 5 minutes, a full collection written to S3 (`pkg/policy/reader.go`). Request budget 1,000, 2-minute deadline, list pages of 500, up to 50,000 rows, at most 32 Gatekeeper kinds. | S3 `latest/violations.json` and `history/violations/`. Bounded copy (200 rows, 128 KiB) on `status.policySnapshot` (`pkg/controller/policy_controller.go`). | `ListClusterViolations`: the bucket object, else the CR copy. | Collection order. No sort option. Page 50 by default, 200 at most. Namespace and engine filters. | Up to 5 minutes old; flagged stale after 10 minutes. | Reports are engine observations, not proof of denial. |
| Violation events | The same pass. Events from all namespaces with reasons `PolicyViolation`, `FailedAdmission`, `WarningAdmission`, `DryrunViolation`, observed within the last hour (`EventTTL`). | S3 `latest/violation-events.json` and `history/violation-events/<RFC3339>.json`. History expires after 30 days. Bounded copy on the CR. | `ListClusterViolationEvents`. Applies the one-hour cut at read time. | Newest first, set at collection. No sort option. | Up to 5 minutes old. | Kubernetes keeps events for its own event TTL (one hour by default). |

### 2.2 What the code does not do

- **No resources.** Nothing in `pkg/`, `idl/` or `ui/` holds allocatable, capacity, requested or
  pod-count values for a node or a group.
- **No workload access from the API.** The API reads the management cluster and the results bucket
  (`ReadOnlySessionPolicy`). Workload kubeconfigs are read only by the controller.
- **No server-side sort, and no byte bound on the read cache.** `results.NewCache(0)` keeps 16 objects,
  so a violations object can hold up to 256 MiB. `collectPages` (`ui/src/lib/api/pagination.ts`) loads
  every page in the browser.
- **Violation events are kept for 30 days.** The collector trims to one hour, but every changed pass
  writes a history object, and the bucket keeps those for 30 days (`ResultsHistoryDays`).

## 3. Problems at scale

| # | Problem | Where it comes from | At 1,000 nodes and 10,000 pods | Addressed by |
|---|---|---|---|---|
| P1 | Staleness: the node view is as old as the last pass. | Node pass runs on change and every 10 minutes when settled. | Up to 10 minutes old for a settled cluster. | Live nodes (5.7). |
| P2 | Missing data: no resources per node or per group. | `nodeRowOf` keeps no resource fields. | "How full is this group?" has no answer. | Live resources (5.3), group totals in the summary (5.2). |
| P3 | Event history is 30 days, not one hour. | Each changed pass writes `history/violation-events/…`. | Up to 288 history objects a day per cluster. | Events are live only (5.4). No history object. |
| P4 | Event objects can exceed the object bound. | 16 MiB per object, about 400 bytes per row. | About 40,000 event rows fill the bound. | No S3 object for events. Live view cap of 50,000 rows (5.9). |
| P5 | S3 write rate. | Nodes, violations and events each write latest and history. | About 10,300 PUTs an hour across 100 clusters. | Only deprecations and recommendations are written (6.4). |
| P6 | Controller load. | Pods in the node pass for group totals. | At most about 4,600 more LIST requests an hour across the fleet (6.3). | Kept, for the summary, at most every 5 minutes per cluster. Accepted (open question 2). |
| P7 | API memory. | 16 cached objects, no byte bound. | Up to 256 MiB of decoded results per API replica. | 64 MiB byte bound for the two stored kinds (6.5). |
| P8 | Browser load. | `collectPages` follows every page. | 50,000 violations are 250 requests before anything is shown. | Server paging for every list (5.10). |
| P9 | No sort. | No `sort_by` on any RPC. | Users cannot see the most-requested nodes first across pages. | `sort_by` on live views (5.7), in-memory sort for the stored kinds. |

## 4. Goals and non-goals

### Goals

1. Node rows, counts and resources (per node and per group), violations, and last-hour policy events,
   read live from the workload cluster. Events are limited to one hour by rule, not by storage.
2. A bounded Cluster status summary, written on each pass, for fleet dashboards, rollout gates and the
   unreachable case.
3. Deprecations and recommendations keep their S3 mechanism, read by the API with a role limited to
   their prefixes.
4. Every live read is bounded (rows, requests, time, concurrency, per cluster, tenant and fleet, with a
   cooldown). A failed live read returns UNAVAILABLE with the reason, and the console shows the summary.
5. The API server holds no workload credentials. The API surface is additive (`sort_by`, `sort_order`,
   `live_at`, `GetNodeResources`) with no `source` field.

### Non-goals

- Live deprecation scans and live recommendation estimates. Both stay stored (decided; see 8).
- Stored history of violations or events. Live reads show the current state and the last hour only.
- Informers or watch caches for every cluster (section 5.7 explains why).
- Per-pod views, per-cluster agents, changes to what the scanners detect, and making the Cluster object's
  platform events (`ListClusterEvents`) live.

## 5. Design

### 5.1 What is stored and what is live

| Kind | Stored in S3 | Read by | Summary on Cluster status | Sort (live or in memory) |
|---|---|---|---|---|
| Node rows, counts, resources | No | Live reader | Counts and totals per group (requests only, no usage) | `NAME`, `AGE`, `READINESS`, CPU and memory requested percent, CPU and memory used percent, `POD_COUNT` |
| Node totals per group and cluster | No | Live reader (`GetNodeResources`) | Yes (same totals) | Not paged |
| Policy violations | No | Live reader | Counts by severity (15-minute counts pass) | `NAMESPACE`, `ENGINE`, `SEVERITY` |
| Policy and admission events, last hour | No | Live reader | Count by reason (15-minute counts pass) | `TIME`, `COUNT` |
| Deprecation findings | Yes: `latest/deprecations.json`, history 30 days | API, read-only role | Summary (existing) | `REMOVED_FIRST`, `NAMESPACE`, `KIND` |
| Recommendations | Yes: `latest/recommendations.json`, history hourly 30 days; usage ConfigMap in the management cluster | API, read-only role | Summary (existing) | `NAMESPACE`, `CPU_CHANGE`, `MEMORY_CHANGE` |
| Platform events of the Cluster | No (management cluster) | API | `recentEvents` (existing) | Newest first |

The live kinds have no `source` option. The stored kinds have no `source` option either. A caller
reads what the kind is.

### 5.2 Cluster status summary

The controller writes this on its passes. It uses the same reads the passes already make, plus the pod
list the node pass gains (5.3). The summary is the only copy of these numbers the platform keeps.

| Field on Cluster status | Content | Written by | Bound |
|---|---|---|---|
| `conditions[Reachable]` | True or False, reason (`CLUSTER_UNREACHABLE`, `CREDENTIALS_REJECTED`, or empty), `lastSuccessfulAt` | Each pass, either kind | Fixed |
| `nodes` | Total, ready, not ready, `nodesCollectedAt` | Node pass | Fixed |
| `nodeGroups[]` (extended) | Per group: name, nodes, ready, allocatable CPU (millicores), memory (bytes), pods; requested CPU, memory; pod count; pods without requests | Node pass | 16 entries; further groups are summed into one `other` entry |
| `violations` | Total, counts by severity (up to 8 values, rest in `other`), `policyCollectedAt` | Policy pass | Fixed |
| `violationEvents` | Total in the last hour, count by the four reasons, `policyCollectedAt` | Policy pass | Fixed |
| `deprecations`, `recommendations` | Existing summaries | Their reconcilers | Unchanged |
| `policySnapshot` | **Removed** (the bounded copy of violations) | — | — |
| `recentEvents` | Unchanged | Unchanged | Last 10 |

Rules:

- Written once per pass. A pass that changes no count refreshes only the `…CollectedAt` time, at most every
  10 minutes. A pass that cannot reach the cluster sets `Reachable` to False with the reason and keeps the
  counts (it does not zero them); the `…CollectedAt` times show their age.
- Bounded by the field list above. A test marshals a 1,000-node summary with 16 groups and checks it stays
  under 8 KiB (typical size about 2 KiB, estimate).
- **The policy counts cost reads, not writes.** To count violations by severity the policy pass still lists
  every PolicyReport (and the other report kinds) of the cluster. Removing the S3 copy removes the writes, not
  these reads, and the live reader reads the same reports again when someone views them. The counts pass
  therefore moves from every 5 minutes to every **15 minutes**: the counts feed dashboards and a summary, not a
  table. The console marks the policy counts "old" after 30 minutes (two missed counts passes); the gates below read only the node part of the summary. On a large cluster that cuts the policy pass's
  reads to a third. The event counts are read on the same pass.
- **The upgrade readiness gates read the summary**, never the live reader, so an upgrade does not depend on the
  live reader. The automatic Kubernetes upgrade's hold (`UpgradeHold`, `pkg/reconcile/cluster/upgrade_ready.go`)
  and the `NOT_READY` gate of `pkg/upgrade/gates.go` (`Check`) gain one rule: `Reachable` False, or a
  `nodesCollectedAt` older than **15 minutes**, makes the gate wait with the message "the platform cannot read
  the cluster" or "the cluster's summary is N minutes old". The gate waits; it does not pass on old data and it
  does not fail the upgrade. The console marks the same summary "old" after 15 minutes.

### 5.3 Node resources (live)

**Row.** `NodeGroupNode` gains these fields (additive; numbers are the next free field numbers):

| Field | Source | Unit |
|---|---|---|
| `allocatable_cpu_millis`, `allocatable_memory_bytes`, `allocatable_pods` | `status.allocatable` of the Node | millicores, bytes, count |
| `requested_cpu_millis`, `requested_memory_bytes` | Sum over the node's pods, scheduler definition below | millicores, bytes |
| `pod_count` | Pods bound to the node that are not Succeeded or Failed | count |
| `pods_without_requests` | Of those pods, the ones with a container that sets no CPU or memory request | count |
| `used_cpu_millis`, `used_memory_bytes` (optional, live only) | `NodeMetrics` of `metrics.k8s.io` for the node | millicores, bytes |

**Available and used.** "Available" is allocatable minus requested: what the scheduler can still place. Usage
is shown next to it, as a separate number, so a group that is fully requested but idle (or the reverse) is
visible. Usage comes from one list of `nodes.metrics.k8s.io` per build (one object per node, a few hundred
bytes each). When metrics-server is not installed, or the list fails, the usage fields are absent and the
response carries the notice "node usage unavailable: metrics.k8s.io is not served". Usage is never on the
status summary: the controller does not read metrics for it, and the summary stays requests only.

**Requested, per pod.** The scheduler's definition: effective request = max(sum of container
requests, max of init container requests) + `spec.overhead`, per resource. Only pods with
`spec.nodeName` set and phase not Succeeded or Failed count. A missing request counts as zero.

**One definition.** The projection and aggregation live in one package, called by the controller's pass
(summary) and the live build (rows), so the two cannot disagree.

**Collection.** The live build lists nodes, then pods (`limit` 500, `continue`), then NodeClaims, then
node metrics (optional, above). Pods
are projected to the fields above as each page arrives and never kept whole. For a 10,000-pod cluster
that is 20 pages per build. Where the client supports it, the list uses the protobuf content type
(estimate: about a third of the JSON size; to be measured).

**Totals.** The build computes totals per group and for the cluster: nodes, ready nodes, sums of
allocatable and requested CPU, memory and pods, `pods_without_requests`, and used CPU and memory when the
metrics were read. `GetNodeResources` returns them. The summary carries the same totals from the controller's
pass, without usage.

### 5.4 Violation events: one hour, from the source

Policy and admission events come from the Kubernetes Events API, with the four reasons
(`PolicyViolation`, `FailedAdmission`, `WarningAdmission`, `DryrunViolation`). One list per reason, with
`reason` as a server-side field selector.

1. **Window at build.** The build keeps events observed within one hour before the build started, and not
   later than one minute after it (the existing bounds in `Collect`).
2. **Window at serve.** A view can live up to 5 minutes after its build (5.7), so each response also drops
   events older than one hour at request time. A response never includes an event more than one hour old.
3. **No stored copy.** No S3 object is written for events, and no lifecycle rule is needed.
4. **Source bound.** Kubernetes removes events after its event TTL (one hour by default). The rule above
   does not depend on it (open question 5).

The controller's policy pass counts last-hour events by reason for the summary, and does not keep the rows.

### 5.5 Who reads the workload cluster

| Option | Credentials it needs | Latency in the request path | Blast radius if the API is compromised | Operations | Verdict |
|---|---|---|---|---|---|
| **A. API server reads live** | Workload kubeconfig for every cluster, plus Secret read in tenant namespaces | Each request waits on a workload cluster | All clusters' read access, from an internet-facing process | Lowest: no new process | Rejected. The API becomes a holder of workload credentials. |
| **B. Controller serves live** | Same as today's controller: AWS and management write access | Requests share the reconcile workers | A request bug runs in the process that writes AWS and Crossplane | Low | Rejected. Mixes user traffic with write-capable reconcilers. |
| **C. Per-cluster agent** | Local, in-cluster | Lowest (local watch) | Limited to that cluster | High: 100 installs, versions, RBAC | Deferred. Revisit only if section 6 shows the live reader cannot meet its targets. |
| **D. Live reader (separate Deployment, same binary)** | Its own read-only identity per cluster (7.1): a kubeconfig written by a second ClusterAuth for the account's reader role, mapped by an EKS access entry to a read-only group. It reads that Secret in tenant namespaces; it holds no AWS credentials. | Each live request waits on the reader, not on the API | Read-only, no write to any system, reachable only from the API | One more Deployment; per cluster one more ClusterAuth, access entry and ClusterRoleBinding | **Recommended.** |

**Why D.** The API stays what it is: it holds no workload credentials. The live reader holds them, has no
write path to AWS, Crossplane or the workload cluster, and has its own failure domain, so console bursts do
not slow reconciliation. It is a subcommand of the same binary (`cluster-vending live`) with its own
ServiceAccount, Deployment and internal Service. It takes only tenant and cluster IDs, and resolves the rest
from the Cluster object. The controller keeps reading each pass, so a workload cluster sees two readers.

**Replicas.** Version 1 runs **one** live reader replica (Deployment strategy `Recreate`, so two never run at
once). Views, singleflight, cooldowns and the per-cluster, per-tenant and fleet limits of 5.9 are that
process's memory. A restart loses the views: a caller holding a token gets `INVALID_ARGUMENT` "restart the
list" and the console restarts at page one. The views are a cache of a few seconds, so this is the whole cost
of a restart. Two replicas behind a plain Service would split a list across replicas (page 2 on a replica
without the view) and double every limit, so they are not run.

**Scale-out (later, only if the phase 3 load test shows one replica cannot meet its targets).** The API routes
each live request by a hash of (tenant ID, cluster ID) to one replica (a headless Service and rendezvous
hashing over its endpoints). A cluster's views, singleflight and cooldown then live on one replica. The tenant
and fleet limits are divided by the replica count. A replica change moves some clusters to another replica,
whose callers restart their lists once.

### 5.6 What the Kubernetes API allows

| Constraint | Consequence for live reads |
|---|---|
| A LIST with `limit` returns a `continue` token. The chain reads one consistent snapshot, the one of the first page. | Pages of one list are consistent with each other. Lists of different kinds are not consistent with each other. |
| A `continue` token expires once the snapshot is compacted (`410 Gone`, reason Expired). The apiserver default compaction interval is 5 minutes. | A build must finish well inside that. A build that gets a 410 restarts once, then fails with `CLUSTER_UNREACHABLE` and the reason "list expired". |
| Field selectors are limited. Pods: `spec.nodeName`, `status.phase`, namespace and name. Nodes: `metadata.name`, `spec.unschedulable`. Events: `involvedObject.*`, `reason`, `type`, `source`. | Pods are filtered by node in memory. Events are filtered by `reason` on the server, one call per reason. |
| There is no server-side sort. | Sorting is done in memory after the whole list is read. Only the views in 5.7 give a sorted, paginated list. |
| Priority and Fairness estimates the cost of a LIST from the number of objects. A large LIST takes more seats. | Concurrency per cluster is capped (5.9). The live identity can get its own FlowSchema (open question 4). |

### 5.7 Sorted, paginated live data: three options

A sorted, paginated list over data the server cannot sort needs one of three approaches. Costed for 100
clusters:

| Option | How | Memory | Workload API cost | Verdict |
|---|---|---|---|---|
| **(a) Materialized view per request key** | Build the rows once per (tenant, cluster, kind, filter): stream the list pages, project each item to a small row, aggregate, sort, keep the rows briefly. Tokens refer to the view. | Only the rows: a node about 300 bytes (1,000 nodes = 0.3 MB); an event about 300 bytes (50,000 = 15 MB, the cap). Raw objects are never held. | About 30 requests per build for a large cluster, at most 4 builds a minute while viewed (about 2 requests a second). Nothing while nobody looks. | **Chosen.** Cost follows the reads, not the fleet size. |
| **(b) Informers per cluster** | Long-lived LIST and WATCH per kind per cluster, with an in-memory index. | Full objects: pods about 3 KB each, so 30 MB for a 10,000-pod cluster; about 40 MB per large cluster; about 670 MB for 100 clusters. | About 400 long-lived watch streams and a constant event flow, whether or not anyone looks. | Deferred. Consider for clusters with steady console traffic, measured in phase 3. |
| **(c) Native `continue` order only** | Use the native order. | None | One LIST per page | Rejected as the only option: sorting is required. Name order stays the default and needs no sort step. |

**The materialized view, in detail.**

1. **Key.** (tenant ID, cluster ID, kind, filter hash). The sort order is not part of the key, so one
   build serves several sort orders.
2. **Reuse.** A request whose key has a view built within the last 15 seconds is served from it.
   The response carries `live_at`, the time the build started.
3. **Build.** The build streams list pages (500 each), projects each item to its row, and aggregates per
   node as it goes. It reads nodes, then pods, then NodeClaims, so a pod whose node is missing from the
   node list is counted in `notices`, not dropped.
4. **Sort.** Rows are sorted in memory with the requested key, and the node ID or name as tie-breaker.
   The view is then immutable.
5. **Token.** `live:<view-id>:<offset>`. The view ID is 128 random bits. The view stays alive for 60
   seconds after its last use, and at most 5 minutes after it was built. A token for a gone view returns
   `INVALID_ARGUMENT` "restart the list".
6. **Caps.** See 5.9. A build over a cap stops and the request gets UNAVAILABLE with the reason.

**Cost at the target scale (estimates).**

| Live build | Small (20 nodes, 300 pods) | Medium (200 nodes, 2,000 pods) | Large (1,000 nodes, 10,000 pods) |
|---|---|---|---|
| Node pages (500) | 1 | 1 | 2 |
| Pod pages (500) | 1 | 4 | 20 |
| NodeClaim pages | 1 | 1 | 2 |
| Warning event pages, last hour (3,000 events assumed for large) | 1 | 2 | 6 |
| Requests per build | about 4 | about 8 | about 30 |
| Bytes read (JSON; protobuf about a third, estimate) | about 2 MB | about 12 MB | about 65 MB (about 20 MB protobuf) |
| Peak memory of the build | under 1 MB | about 1 MB | about 3 MB (one page, plus the rows) |
| Rows kept in the view | 20 nodes | 200 nodes | 1,000 nodes |

### 5.8 API shape

Additive changes to `idl/cloud/v1`. Field numbers are chosen when the proto is edited. No existing field
changes meaning.

**Enum.** `SortOrder { SORT_ORDER_UNSPECIFIED = 0; SORT_ORDER_ASC = 1; SORT_ORDER_DESC = 2; }`. No
`ResultSource` enum: the live kinds are always live and the stored kinds are always stored.

**Live list RPCs** (`ListNodeGroupNodes`, `ListClusterViolations`, `ListClusterViolationEvents`):

- Request: `sort_by` (per-kind enum, default first in the table below), `sort_order` (default per
  kind), `page_token` (`max_len` from 8 digits to 512; live tokens are the string in 5.7).
- Response: `live_at` (time the build started), `complete` (every row was read within the caps), and
  `notices`. The response has no snapshot time, since nothing is stored.
- Errors: UNAVAILABLE with a reason in the error details (5.9). `INVALID_ARGUMENT` for an expired or
  foreign view token.
- Tokens from the stored node and violations RPCs stop working at cutover (phase 4). They return
  `INVALID_ARGUMENT` "restart the list". Clients restart the list.

**`GetNodeResources`** (live): takes `tenant_id` and `cluster_id`. Returns totals per group and for the
cluster, as in 5.3. The same build as the node rows (5.7), so one build serves both.

**Stored list RPCs** (`ListClusterDeprecations`, `ListClusterRecommendations`): unchanged in request and
response, except for the new `sort_by` and `sort_order` (phase 5). The object is read once per ETag,
kept in the cache (bounded in bytes, 5.9) and sorted in memory. Their snapshot tokens keep their form.

**Sort keys per kind:**

| Kind | `sort_by` values (default first) | Served from |
|---|---|---|
| Nodes | `NAME`, `AGE`, `READINESS` (not Ready first), `CPU_REQUESTED_PERCENT`, `MEMORY_REQUESTED_PERCENT`, `CPU_USED_PERCENT`, `MEMORY_USED_PERCENT` (rows without usage last), `POD_COUNT` | Live |
| Node totals (`GetNodeResources`) | Not paged; one row per group, plus the cluster total | Live |
| Violations | `NAMESPACE` (default), `ENGINE`, `SEVERITY` | Live |
| Violation events | `TIME` (default, newest first), `COUNT` | Live |
| Deprecations | `REMOVED_FIRST` (default), `NAMESPACE`, `KIND` | Stored |
| Recommendations | `NAMESPACE` (default), `CPU_CHANGE`, `MEMORY_CHANGE` | Stored |

**Cluster summary.** Returned with the Cluster read the console already makes (`status`, section 5.2).
A fleet view reads it for many clusters without calling the live reader.

### 5.9 Caps, budgets and unreachable behaviour

| Control | Value |
|---|---|
| Rows per response | 500 maximum |
| Rows per view | Nodes 5,000; events 50,000; violations 50,000 (today's collector bound, about 20 MB at 400 bytes a row). Pods are not kept: only per-node aggregates. |
| Build deadline | Nodes and pods 30 seconds; events and violations 15 seconds |
| Requests per build | 200 pages |
| Per cluster | One build at a time; at most 4 builds a minute; at most 10 requests a second (client-side limiter, burst 20) |
| Per tenant | At most 10 builds a minute across the tenant's clusters (section 7.2) |
| Fleet | At most 8 builds in flight. A build waits 2 seconds for a slot, then is refused as `BUSY`. |
| Failure cooldown | 30 seconds after a failure: the same error is returned without contacting the cluster. 2 minutes after 3 consecutive failures. |
| Replicas | One in version 1 (5.5). Every limit in this table is that process's. |
| Memory | Live views: 256 MiB in total in the live reader, least recently used evicted first. Stored results cache: 64 MiB by bytes (replaces the 16-object count bound). |
| Page size | 100 nodes by default; 50 for the others. Same as today. |

**Unreachable.** A build is "unreachable" when one of these happens before the first page:

- the Cluster object is missing (`CLUSTER_UNREACHABLE`, reason "no cluster");
- the live kubeconfig Secret is missing or its ClusterAuth is not Ready (`NO_LIVE_IDENTITY`, 7.1; no
  cooldown, since no request reached the cluster);
- connect, DNS or TLS fails, or the deadline passes with no page (`CLUSTER_UNREACHABLE`);
- the apiserver answers 401 or 403 (`CREDENTIALS_REJECTED`, shown to the platform team with the rotation
  hint; the tenant sees "the platform cannot read this cluster").

A failure starts the cooldown. During the cooldown the live RPCs return UNAVAILABLE at once.

**What the caller gets.** The live RPC returns UNAVAILABLE with the reason and no rows. It never falls
back to an older copy: there is no older copy to fall back to. The console shows the cluster summary from
the Cluster status, with the times `nodesCollectedAt` and `policyCollectedAt` (5.2) and a notice:
"The workload cluster cannot be reached right now (<reason>). Showing the counts from the last successful
controller pass, <time>." The `Reachable` condition is shown next to it.

### 5.10 Consistency and the console

- A live view is consistent within each kind, not across kinds. Notices say when pod requests refer to
  nodes not in the view. Changing the sort while paging refuses the old token; the console shows "The list
  changed; restarting" and goes to page one.
- Every table shows its time: "Live as of 12:04:31 UTC" with Refresh (starts a new build), or "Collected
  4 min ago" for the stored kinds. When the live read is unavailable, the table shows the notice (5.9) and
  the summary counts, and no rows.
- Every list pages on the server (phase 5). `collectPages` is removed from those pages.

### 5.11 Deprecations and recommendations (stored, unchanged mechanism)

Scanners, schedules, S3 keys and 30-day history stay as they are. The API's S3 role is narrowed to
`latest/deprecations.json`, `history/deprecations/`, `latest/recommendations.json` and
`history/recommendations/`: no other read, no write. The results cache is bounded by bytes (64 MiB).

### 5.12 Migration: what is removed

Removed in phase 6, after the live path has run for a release (section 9):

| Item | Where |
|---|---|
| Writers for `latest/nodes/<group>.json`, `latest/nodes/index.json`, `history/nodes/` | `pkg/reconcile/cluster/node_rows.go`, `pkg/reconcile/resultsink` (`StageSharded`) |
| Node-row throttle (`nodeRowsInterval`, `w.last`) | `pkg/reconcile/cluster/node_rows.go` |
| Sharding and index code: `results.Nodes` is the only sharded kind (`Sharded()` returns true only for it), so `PutSharded`, `StageSharded`, `ShardLatestKey`, `IndexKey`, `ValidShard`, `ResultIndexShard` and the index digest go as well | `pkg/results/kind.go`, `pkg/results/index.go`, `pkg/results/writer.go`, their tests |
| Writers for `latest/violations.json`, `history/violations/`, `latest/violation-events.json`, `history/violation-events/` | `pkg/controller/policy_controller.go`, `pkg/policy/reader.go` (write path), `pkg/results/writer.go` |
| Violations history gate (`historyGate` stays, used by recommendations only) | No change to `pkg/controller/recommendation_controller.go`; the violations path that would have used it is removed |
| Readers: the bucket paths of `ListNodeGroupNodes`, `ListClusterViolations`, `ListClusterViolationEvents` | `pkg/server/cluster/node_group_nodes.go`, `violations.go`, `stored.go` |
| `status.policySnapshot` and the `ClusterPolicySnapshot` CR copy | `api/v1alpha1/cluster_types.go` (`PolicySnapshot`), `pkg/controller/policy_controller.go` |
| `ResultRef`s for the three kinds on Cluster status | `api/v1alpha1` |
| S3 objects under those prefixes | One sweep: delete the prefixes, then remove their lifecycle rules once the sweep is confirmed empty (`pkg/crossplane/aws/results_bucket.go`) |
| API S3 role for the removed prefixes | `ReadOnlySessionPolicy` narrowed to the four prefixes in 5.11 |
| UI pages: node pages, violations pages and events pages switch to the live API; `collectPages` removed from those pages | `ui/src/lib/api/pagination.ts` callers |

Kept: the results package for `Deprecations` and `Recommendations`, `historyGate`, and the bytes-bounded cache.

## 6. Scale model

### 6.1 Assumptions

Estimates from the code's own limits, labelled as such. Each must be measured in phase 3.

| Item | Assumed size |
|---|---|
| Node row, with section 5.3 fields (live response) | 570 bytes JSON |
| Pod object, full JSON (read by the live build or the controller pass) | about 5 KB; protobuf about a third (estimate) |
| Policy or event row (live) | about 400 bytes |
| Deprecation finding | about 300 bytes |
| Recommendation row | about 500 bytes |
| Cluster status summary (5.2) | about 2 KB typical, under 8 KiB at 16 groups |
| Warning events per hour, large cluster | 3,000 (assumed; a worst case of 50,000 is in section 3) |

### 6.2 Per cluster

| Object | Small (20 nodes, 300 pods) | Medium (200 nodes, 2,000 pods) | Large (1,000 nodes, 10,000 pods) |
|---|---|---|---|
| Stored: deprecation findings (latest) | 20: 6 KB | 200: 60 KB | 1,000: 300 KB |
| Stored: recommended containers (latest, cap 2,000) | 50: 25 KB | 400: 200 KB | 2,000: 1 MB |
| Cluster status summary | about 2 KB | about 2 KB | about 3 KB (16 groups max) |
| Controller pass requests, nodes and pods (every 10 min, settled) | about 4 | about 7 | about 26 |
| Live build requests (5.7) | about 4 | about 8 | about 30 |

### 6.3 Fleet (100 clusters: 60 small, 30 medium, 10 large)

| Quantity | Value |
|---|---|
| Nodes | 17,200 (1,200 + 6,000 + 10,000) |
| Pods | 178,000 (18,000 + 60,000 + 100,000), aggregated, not stored |
| Stored latest bytes, deprecations and recommendations | about 23 MB (60 × 31 KB + 30 × 260 KB + 10 × 1.3 MB) |
| Stored history bytes, 30 days, recommendations (upper bound) | about 13 GB (72,000 objects × about 175 KB average, estimate) |
| Extra controller LIST requests an hour for pods | at most about 4,600 (2,400 large + 1,440 medium + 720 small: pod pages x 12 an hour, the summary's 5-minute bound). Accepted (open question 2). |
| Controller policy counts pass | Every 15 minutes instead of 5: a third of today's report reads. Each live view of violations adds one full read of the reports. |
| Live read requests, if every large cluster is viewed at once | Capped by 8 builds in flight: at most about 16 requests a second across the fleet |

### 6.4 S3 writes and history

Counted per cluster-hour, upper bound. The rate of PUTs is what changes.

| Kind | Today (PUTs per cluster-hour) | After (PUTs per cluster-hour) | Fleet of 100, after |
|---|---|---|---|
| Nodes (shards and index) | about 42 | 0 | 0 (4,200 today) |
| Violations (latest and hourly history) | 24 (12 latest + 12 history) | 0 | 0 (2,400 today) |
| Violation events (latest and history) | 24 (12 + 12) | 0 | 0 (2,400 today) |
| Recommendations (latest and hourly history) | 13 | 13 | 1,300 |
| Deprecations (latest and history, every 6 hours) | 0.33 | 0.33 | 33 |
| **Total** | about 103 | about 13.3 | about 1,330 an hour (about 10,300 today) |

History objects over 30 days, fleet of 100, upper bound:

| Prefix | Today | After |
|---|---|---|
| `history/violation-events/` | 864,000 (288 per day × 30 × 100) | 0 |
| `history/violations/` | 864,000 | 0 |
| `history/recommendations/` | 72,000 | 72,000 |
| `history/nodes/` | about 108,000 | 0 |
| `history/deprecations/` | 12,000 | 12,000 |
| Total | about 1.9 million | 84,000 (-96 %) |

Request cost is small at these rates. The gain is the object count and the storage with no reader.

### 6.5 API process and live reader

| Resource | Today | After |
|---|---|---|
| API results cache, decoded | Up to 256 MiB (16 objects × 16 MiB) | 64 MiB by bytes (two stored kinds only) |
| Live views, per live reader | Not applicable | 256 MiB by bytes. A large cluster's node view is 0.3 MB; its events view is up to 15 MB. |
| Transient memory per live build | Not applicable | One page (about 2.5 MB JSON for 500 pods) plus rows. Eight builds in flight: about 20 MB peak. |

## 7. Security

### 7.1 Identities

| Identity | Holds | Does not hold |
|---|---|---|
| API server ServiceAccount (management cluster) | Reads and writes its CRs (as today). Reads the results bucket for the two stored kinds through the narrowed role (5.11). Calls the live reader. | Any workload kubeconfig, any Secret read in tenant namespaces, any workload credential, any read of the removed S3 prefixes |
| Controller ServiceAccount (management cluster) | As today. Writes the status summary and the two stored kinds. | Write access to the removed S3 prefixes is unused after phase 6 and is removed with them |
| Live reader ServiceAccount (management cluster) | Get on Cluster CRs; get on kubeconfig Secrets in tenant namespaces through the `cluster-vending-kubeconfig-reader` binding (a second binding for this ServiceAccount) | Write to any CR, AWS, Crossplane |
| Live reader identity in each workload cluster (new) | The account's reader IAM role, mapped by an EKS access entry to the Kubernetes group `cluster-vending:live-reader`, bound to the `cluster-vending-live-reader` ClusterRole: get and list on nodes, pods, events, NodeClaims (`karpenter.sh`), PolicyReports and ClusterPolicyReports (`wgpolicyk8s.io`), OpenReports and ClusterReports (`openreports.io`), Gatekeeper constraints, and `nodes` of `metrics.k8s.io` | Secrets, ConfigMaps, VPAs, pod metrics, `pods/exec`, `pods/log`, any write verb, watch in v1, any EKS access policy |
| Controller identity in the workload cluster (today) | Its kubeconfig. Keeps the reads it has (including the deprecation scan's Helm release Secrets and the recommendation reads). | Unchanged by this proposal (open question 6) |

**How the live identity is made.** ClusterAuth (`eks.aws.m.upbound.io`) does not create Kubernetes
identities: it writes a kubeconfig whose token is a short-lived EKS token for the AWS identity of its
ProviderConfig, and refreshes it every `refreshPeriod`. Today's ClusterAuth (`clusterAuth`,
`pkg/crossplane/aws/cluster.go`) uses the account's tenant role, which the cluster's access entry gives the
cluster admin policy. The live identity is therefore a second AWS identity with its own access entry, not a
ServiceAccount:

| Object | Where | Content |
|---|---|---|
| IAM role | The tenant account: the existing **reader role** (`cluster-vending-reader-role.template.yaml`) | Unchanged. Its trust already allows the management role (and the server principal); its policy has `eks:Describe*`, which covers the `eks:DescribeCluster` the token needs. No template change and no `TemplateVersion` bump, so tenants re-apply nothing. |
| CloudAccount field | Management cluster | `spec.aws.readerRole` (new, optional). Default `arn:aws:iam::<account>:role/cluster-vending-reader`, the stack's default `RoleName`. The platform does not record the reader role today. |
| ProviderConfig `<account>-live` | Management cluster | Assumes the reader role from the management role, with the account's external ID. |
| ClusterAuth `<prefix>-live-auth` | Management cluster, tenant namespace | The `<account>-live` ProviderConfig; writes the Secret `<prefix>-live-kubeconfig`; the same `refreshPeriod` as the existing ClusterAuth. |
| AccessEntry `<prefix>-live` | The EKS cluster | `principalArn` the reader role, type `STANDARD`, `kubernetesGroups: [cluster-vending:live-reader]`, **no** AccessPolicyAssociation. |
| ClusterRole `cluster-vending-live-reader` and ClusterRoleBinding to the group `cluster-vending:live-reader` | The workload cluster | Applied by the controller with its own kubeconfig, after the cluster is `ACTIVE`, like the other workload objects it applies. |

The live reader reads only the Secret `<prefix>-live-kubeconfig`. It holds no AWS credentials: the token is
minted by the Crossplane provider. Rotation is ClusterAuth's refresh. An account without the reader stack, or
whose reader role cannot be assumed, has a ClusterAuth that is not Ready; its clusters get no live identity.

**Imported clusters.** The same four objects are added when a cluster is imported: an access entry is
additive and changes no existing access. A cluster whose authentication mode is `CONFIG_MAP` (no access
entries) gets no live identity; the platform does not change its authentication mode.

**No live identity.** When the live Secret is missing or its ClusterAuth is not Ready, the live RPCs return
UNAVAILABLE with reason `NO_LIVE_IDENTITY` ("the platform has no read-only identity for this cluster"), and the
console shows the summary, as for an unreachable cluster. The controller's kubeconfig is never used as a
fallback.

Narrowing the live ClusterRole to what the projection needs (no VPAs, no pod metrics, no Secrets) is the
reason recommendations stay with the controller. The live identity needs nothing for them.

### 7.2 Tenant scoping

- The API checks the tenant and the cluster as it does today (`s.get(tenant, cluster)`) before it calls
  the live reader.
- The live reader takes only tenant ID and cluster ID. It never takes a kubeconfig, a server URL or a
  Secret name from a request. It resolves them from the Cluster object, and refuses a Cluster whose tenant
  label differs from the request.
- View keys include the tenant ID. Two tenants never share a view, even if IDs collide in a bug.
- Per-tenant fairness: a tenant may start at most 10 live builds a minute across its clusters.

### 7.3 What is returned

- Only the projected fields in 5.3 and 5.8. Never annotations (`last-applied-configuration` in particular),
  environment variables, command lines, volume or secret references, image names, or pod specs.
- Node provider IDs (instance IDs) are returned today in `NodeGroupNode.provider_id`. They stay; they are
  a field of the tenant's own nodes, and the console shows them.
- Violation messages come from the engines. They can contain resource names. They are returned as today.
- The status summary carries counts and totals only. No names of violations or events.

### 7.4 Audit and limits

- Each live build is logged with tenant ID, cluster ID, kind, duration, rows, pages, outcome and reason (no
  row contents). Metrics: builds by outcome, duration, cooldowns, busy refusals, view bytes.
- Network: the live reader's Service accepts only the API server's pods (NetworkPolicy); egress is limited to
  the management cluster's API and the workload endpoints.

### 7.5 Unauthenticated mode

`auth.mode: none` in the API lets the caller set any tenant ID
([architecture.md, Known limits](../architecture.md#7-known-limits)). With auth off, any caller could read
any cluster's live data. So `serve` refuses live reads in that mode: the live RPCs return
`FAILED_PRECONDITION` "live reads need authentication", and the console shows the summary with that notice.
The config file can allow them with `live.allowWithoutAuth: true`. Only `hack/config.local.yaml` sets it, for
local development; the deploy bases (`config/server/base`) never do, and `serve` logs a warning at start when
it is set.

## 8. Alternatives considered

| Alternative | Why not |
|---|---|
| **S3 snapshots for everything** (the previous revision of this proposal) | Nodes, violations and events in S3 cost about 10,300 PUTs an hour across the fleet and 1.9 million history objects, and still show data up to 10 minutes old, with no server sort. Events do not need the bucket: Kubernetes keeps them for about an hour, and a live read covers that window. Decided against by the user. |
| **Snapshot with a live fallback for nodes, violations and events** (AUTO) | Keeps the S3 copies alive only to hide an outage behind stale rows. The console shows the summary and a notice instead. |
| **Everything live, results stored in the workload cluster** (for example a ConfigMap or CRD per kind) | Rejected for deprecations and recommendations, per the user's decision. Deprecation scans would run per request or per cluster; recommendations need a week of usage kept in each workload cluster, which is lost when a cluster is rebuilt; and the scan needs Secret reads or write access in each workload cluster. The management-side S3 store keeps both across cluster rebuilds. |
| **Live deprecations and recommendations** | Deprecation scans take up to 2 minutes and read Helm release Secrets, so they are not run per request. Recommendations need the week of usage. Both stay stored (decided). |
| **Live reads from the API or the controller, or a per-cluster agent** (options A, B, C in 5.5) | Workload credentials in the API, write-capable reconcilers serving users, or a component in 100 clusters. Rejected or deferred in 5.5. |
| **Informers per cluster** (5.7 (b)) | About 40 MB per large cluster, about 670 MB and about 400 watches for the fleet, mostly for clusters nobody views. Deferred to measured hot clusters. |
| **Name order only** | Sorting was required. Name order stays the default. |

## 9. Rollout plan

Each phase ships alone and leaves the system consistent. Stored kinds are unchanged until phase 6. The
S3 writers for the live kinds run alongside the live path through phase 5, so the two can be compared and
rolled back.

**Status (2026-11-21).** Phases 0, 1 and 2 are implemented on `main` (commit `2ce6048`). Phase 2b is not: the
summary is on the Cluster object but not yet in the API. The 15-minute counts pass and the gate rule of 5.2
are not implemented yet; they ship with phase 2b.

### Phase 0: Stop violation event history (interim) — implemented

- Write `violation-events` with `results.WithoutHistory()`. Write `violations` history at most hourly, with
  the existing gate. Delete the existing `history/violation-events/` objects once, at rollout.
- **Accepted when:** no `history/violation-events/` object is written after the release.
- **Tests:** the write options; an S3 fake that counts PUTs per pass.

### Phase 1: API shape — implemented

- Proto additions of 5.8 (`sort_by`, `sort_order`, `live_at`, `complete`, `GetNodeResources`, page token
  length). The live RPCs return `UNIMPLEMENTED` until phase 3. The stored RPCs are unchanged.
- **Accepted when:** existing deprecation and recommendation requests and tokens are answered as before (a
  golden test), and a live RPC returns `UNIMPLEMENTED`, not a crash.
- **Tests:** wire compatibility (old client, new server; new client, old server); token scope tests.

### Phase 2: Cluster status summary — implemented

- The controller writes `conditions[Reachable]`, `nodes`, the extended `nodeGroups[]` (with resource totals and
  the pod list in the node pass), `violations` and `violationEvents` (5.2). The pass counts events and violations
  in memory and stores no rows.
- **Accepted when:** on a fixture of 3 nodes and 40 pods, the group totals equal `kubectl describe node` sums
  (requests, init containers, overhead, terminal pods excluded). A cluster with no pods reports zero requests.
  A 1,000-node summary with 16 groups marshals under 8 KiB. An unreachable fixture sets `Reachable` False and
  keeps the counts.
- **Tests:** the requested-resources function on tables (init containers, overhead, terminal phases, missing
  requests); a fake API with paginated pods; the summary bound; the status write rule (time-only refresh at most
  every 10 minutes).

### Phase 2b: Summary in the API

- `idl/cloud/v1`: on `Cluster`, the `nodes` counts (total, ready, not ready, `nodes_collected_at`), the
  `violations` counts (total, by severity, `policy_collected_at`), the `violation_events` counts (total, by
  reason), and the `Reachable` condition (status, reason, `last_successful_at`). On `NodeGroupStatus`: allocatable
  and requested CPU and memory, allocatable pods, pod count, pods without requests. Additive fields only.
- `pkg/convert`: the Cluster status summary to these fields; absent when the summary is absent (a cluster no
  pass has read yet), never zero.
- CLI: `cluster get` prints the counts, the per-group requested and allocatable totals, and Reachable with its
  reason. UI: the cluster overview shows the same, with `…CollectedAt` ages and the "old" marking of 5.2.
- The policy counts pass moves to 15 minutes, and the upgrade readiness gates get the rule of 5.2 (Reachable
  False or a summary older than 15 minutes waits).
- **Accepted when:** `cluster get` and the overview show the counts of a fixture cluster; a cluster without a
  summary shows "not read yet", not zeros; an unreachable fixture shows Reachable False with its reason and the
  last counts; an upgrade on a cluster whose summary is 16 minutes old waits with the gate message.
- **Tests:** convert tests (summary present, absent, unreachable); a CLI print golden; a UI component test for the
  overview's three states; gate tests at 14 and 16 minutes and with Reachable False; the policy pass cadence.

### Phase 3: Live reader, nodes and node totals

- The `cluster-vending live` subcommand, the internal Connect endpoint, the Deployment (one replica, `Recreate`,
  5.5), the Service, the NetworkPolicy, the caps, the cooldown, the views.
- The live identity of 7.1, per account and cluster: `spec.aws.readerRole` on the CloudAccount, the
  `<account>-live` ProviderConfig, the `<prefix>-live-auth` ClusterAuth writing `<prefix>-live-kubeconfig`, the
  `<prefix>-live` AccessEntry with the `cluster-vending:live-reader` group and no access policy, and the
  `cluster-vending-live-reader` ClusterRole and ClusterRoleBinding in the workload cluster. The same objects at
  import; none for a `CONFIG_MAP` cluster. Deletion removes them with the cluster's other access objects.
- Node usage from `nodes.metrics.k8s.io` (5.3), optional.
- `serve` refuses live reads with `auth.mode: none` unless `live.allowWithoutAuth` is set (7.5).
- The console's node pages switch to the live API, with the unreachable notice and the summary counts. S3 node
  writers keep running for comparison and rollback.
- **Accepted when:** a 1,000-node, 10,000-pod test cluster returns a sorted, paginated node list in under 3
  seconds at p95 cold, and under 100 milliseconds warm. A cap breach returns its reason. Three failures start
  the cooldown, and no request reaches the cluster during it. The API process holds no kubeconfig. A 403 shows
  `CREDENTIALS_REJECTED`, not `CLUSTER_UNREACHABLE`. A cluster without the live Secret returns `NO_LIVE_IDENTITY`
  and the controller's kubeconfig is not used. A live reader restart makes an open token answer "restart the
  list". With `auth.mode: none` and no `live.allowWithoutAuth`, the live RPCs return `FAILED_PRECONDITION`.
- **Tests:** build tests against a fake API that paginates, returns 410 on a stale token, and times out, one row
  per cap; singleflight; token tests (expiry, scope, sort change, a token from before a restart); render golden
  tests of the live ProviderConfig, ClusterAuth and AccessEntry (group set, no access policy), for a created and an
  imported cluster, and none for a `CONFIG_MAP` cluster; the usage fields present and absent (no metrics API); an
  RBAC test that the live identity cannot read Secrets, ConfigMaps, VPAs or pod metrics and cannot write;
  the auth-mode refusal; a load test at 100 synthetic clusters (the target number for
  [Known limits](../architecture.md#7-known-limits)).

### Phase 4: Live violations and events

- The same build for PolicyReports and admission events, the one-hour cut at build and at serve time (5.4).
  The console's violations and events pages switch to live. Stored token kinds for those RPCs are refused
  from this phase (5.8). S3 writers still run for comparison.
- **Accepted when:** live events older than one hour never appear, including on a view served 5 minutes after
  its build. Live violations match the stored copy for a cluster that has not changed in the same minute.
- **Tests:** build tests as in phase 3, plus reason selectors; the window at 59, 60 and 61 minutes and one minute
  of clock skew; a live-versus-stored comparison of a fixture.

### Phase 5: Console

- Sort controls on live and stored tables. `live_at` and `collected_at` on every table. Server-side paging for
  deprecations and recommendations. `collectPages` removed from every page.
- Optional, if phase 3 measurements call for it: informers for clusters with steady console traffic (5.7 (b)).
- **Accepted when:** a browser test with 50,000 violations loads one page at a time. A sort change restarts the list
  with a message. A Lighthouse run on the node page stays within the targets of the web performance rules.
- **Tests:** component tests for the sort state and the unreachable state; end-to-end paging against the fake server.

### Phase 6: Remove the stored live kinds

- Remove the writers, readers, ResultRefs, `status.policySnapshot`, the node-row throttle, the sharding and index code,
  and the results kinds listed in 5.12. Delete the S3 prefixes for the removed kinds once. Narrow the API's S3 role to
  the four prefixes of 5.11. Remove their lifecycle rules after the sweep is confirmed empty.
- **Accepted when:** no object under the removed prefixes exists; the API's S3 role cannot read any other
  prefix; the summary still updates on a pass; `policySnapshot` is gone from the Cluster object.
- **Tests:** a removal test (no references to the removed kinds in `pkg/` or `ui/`); the S3 role policy test;
  golden tests for the stored deprecation and recommendation responses (unchanged).

## 10. Open questions

All closed (2026-11-22). Kept for the record, with the decision.

1. **Pod request definition.** Decided: the scheduler's definition (init containers and overhead included), as
   built in phase 2. A pod that sets pod-level `spec.resources` (Kubernetes 1.32 and later) counts those instead of
   its containers' sum, as the scheduler does; that is a code follow-up of phase 3.
2. **Pod list in the controller pass.** Decided: accepted, at most once every 5 minutes per cluster, whatever the
   pass rate (`summaryInterval`, `pkg/reconcile/cluster/summary.go`). The node rows keep their own 1-minute
   throttle until phase 6 removes them. A large cluster costs at most about 240 pod LIST requests an hour (20 pages
   x 12), and the fleet at most about 4,600 (6.3).
3. **Live reader as a Deployment.** Decided: accepted (option D), one replica in v1 (5.5).
4. **Flow schema for the live identity.** Decided: the platform provides it. With the live RBAC (phase 3) the
   controller applies, in each workload cluster, a PriorityLevelConfiguration `cluster-vending-live` (limited,
   `nominalConcurrencyShares` 10, queuing on) and a FlowSchema `cluster-vending-live` matching the group
   `cluster-vending:live-reader`. Live LISTs then queue in their own level: they neither take the cluster's
   `global-default` seats nor get starved by them.
5. **Event TTL on EKS.** Decided: EKS runs the apiserver with the default event TTL of one hour and offers no
   setting to change it, so the one-hour window of 5.4 matches what the cluster keeps. The window rule does not rely
   on it either way.
6. **Scan identity.** Deferred to a separate proposal. Not needed here: the controller's identity is unchanged.
7. **Limits.** Decided: the caps of 5.9 are the starting values. The phase 3 load test measures them, and the
   measured values replace them in 5.9 before phase 3 is accepted.
8. **Summary staleness.** Decided: the node part of the summary is "old" after 15 minutes, and the upgrade readiness
   gates wait (not pass, not fail) on a summary that old or on `Reachable` False (5.2). The policy counts are "old"
   after 30 minutes.
9. **Violation history.** Decided: live only, as the user chose ("everything to live"): no stored history of
   violations or events after phase 6. Until then the interim hourly history of phase 0 keeps being written. An audit
   trail of violations, if needed later, is a separate proposal.
10. **Stored history retention and notice wording.** Decided: 30 days for deprecations and recommendations, as
    today, and the notice and the reason split of 5.9 as written.

**Recommendation sort (5.8).** Decided: `CPU_CHANGE` and `MEMORY_CHANGE` order by the size of the change, up or
down, largest first (`sort_order` reverses it); rows without a recommendation come last in either order, then by
namespace and name.
