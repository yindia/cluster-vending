# Cluster violations and violation events

The **Violations** tab in cluster details shows **Current violations** and **Violation events** together.
Both are read-only and scoped to the selected tenant and cluster. Search and
filters operate on the loaded snapshot; refreshing the UI fetches that snapshot,
not an immediate workload scan.

## Data semantics

Violations are failing, warning or evaluation-error results reported by a policy
engine. A failed background result does not prove an admission request was denied.
Unknown enforcement, resource references and timestamps remain unknown. Passing
and skipped results are not violations.

Violation events are the admission decisions still retained by Kubernetes. They
expire according to the workload cluster's event retention, and an engine can
omit emission. These records are not a durable audit log. Kubernetes may aggregate
repeated occurrences into one event with a count. Report findings and event
occurrences must not be added together as a unique violation total.

Missing APIs, denied reads, bounded collection and uncollected snapshots produce
coverage notices. An empty or partial snapshot is not proof of compliance. The
collection timestamp identifies when the workload data was observed.

## Architecture

The controller uses the cluster's existing workload credentials to read policy
reports and policy-related events. It records a bounded snapshot in the management
Cluster status. The API reads that stored snapshot through the existing
Kubernetes-backed store; no database, workload mutation or cloud SDK call is added
to request handling.

`ClusterService.ListClusterViolations` and
`ClusterService.ListClusterViolationEvents` require a tenant ID and cluster ID.
Tenant viewers can read their own clusters; cross-tenant reads are rejected.
Both support namespace/engine filtering and bounded pagination. Pagination tokens
belong to the snapshot and scope that issued them; a changed snapshot requires
restarting the list instead of mixing generations.

## Collection limits and coverage

The collector runs on a five-minute schedule and respects existing pause/freeze
gates. It only reads the workload cluster, so it runs in every phase that has
one: Running, Updating, Failed and Imported clusters whose kubeconfig exists,
not while the cluster is being created or deleted. It reads WG PolicyReports, OpenReports, discovered
Gatekeeper constraints and reason-filtered core Kubernetes Events. Native
ValidatingAdmissionPolicy findings require a report producer; native API-server
audit logs are not collected.

A collection has a 45-second deadline, 10-second request timeouts, at most 48 list
requests, and at most four pages of 100 objects per source. Gatekeeper discovery
is capped at 32 kinds. The snapshot retains up to 200 findings and 200 events,
with an aggregate serialized limit of 128 KiB. Reaching a limit is reported as
incomplete/truncated coverage. Individual field lengths are bounded as well.

Events older than one hour are excluded both during collection and API reads;
they may disappear earlier because Kubernetes retention is independently
configured. The collector replaces the event snapshot instead of accumulating
history. A snapshot older than ten minutes, or a non-empty snapshot with no
collection time, is flagged `summary.stale` by the API (with a notice, subject
to the 32-notice cap). Stale is distinct from `incomplete`: it means collection
stopped, not that sources were unreadable, and the UI shows a separate
"Snapshot is stale" banner. Page tokens hash each scope field with a length
prefix, so filters containing NUL cannot collide.

## Deployment

Deploy the updated Cluster CRD schema along with the controller, API and UI.
The schema includes `status.policySnapshot`; an older schema can prune this field.
Restarting only the API does not start collection. The controller must be running
with access to the workload cluster, and an eligible cluster receives its first
snapshot on reconciliation. Until then, the UI shows collection as unavailable.

## Reference implementation

The Go implementation in the local `policy` / p9s project informed the distinction
between report findings and retained admission events, report resource references,
Kyverno's policy-side event deduplication, and explicit incomplete-data notices.
Its `video` directory was excluded. The integration uses this platform's existing
controller and API lifecycle rather than importing p9s's terminal UI or starting
per-request informer caches.

## Checks

Parser/controller tests use fake Kubernetes clients. RPC tests cover tenant scope,
input validation and pagination. Browser fixtures exercise data, unavailable
snapshots, coverage notices, search, explicit refresh and narrow layouts without
writing workload resources.
