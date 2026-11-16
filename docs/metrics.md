# Prometheus metrics

The API server serves `/metrics` on its HTTP listener. The controller exposes
`/metrics` when started with `--metrics-addr=:8080` (or `CV_METRICS_ADDR=:8080`).
Its default is `0`, disabled. The endpoints use plain HTTP without RPC
authentication. Scrape them through the management network.

For deployed controllers, enable `../metrics` under `components` in
`config/default/kustomization.yaml`. Enable `../prometheus` under `resources`
to install both ServiceMonitors and example PrometheusRules; this requires
Prometheus Operator CRDs. The server monitor selects the server Service in the
same namespace. Set your Prometheus instance's ServiceMonitor and rule selectors
to include these resources. No manifests are applied automatically.

## API metrics

| Metric | Type | Meaning |
| --- | --- | --- |
| `cluster_vending_rpc_requests_total` | Counter | Completed calls, by `rpc_service`, `rpc_method`, `rpc_type`, `code` |
| `cluster_vending_rpc_duration_seconds` | Histogram | Complete call lifetime, by `rpc_service`, `rpc_method`, `rpc_type` |
| `cluster_vending_rpc_in_flight` | Gauge | Currently executing calls, with the same labels as duration |

Authentication refusals and translated domain faults are included. Successful
RPCs use `code="ok"`; errors use Connect codes such as `invalid_argument`,
`unauthenticated`, `failed_precondition` and `unavailable`. Streaming lifetime
includes the time a stream remains open, if a streaming RPC is registered.
Exclude streams from unary latency SLOs. Request decoding or transport failures
that occur before RPC dispatch are not counted. `/metrics` and probes are not RPCs.

An update RPC completing successfully means desired state was recorded, not that
cloud provisioning finished. Preview calls also count as calls to UpdateCluster
or UpdateNetwork. These metrics do not distinguish preview from apply.

## Controller metrics

Controller-runtime already supplies reconciliation duration, reconciliation
errors, worker utilization and workqueue depth/retries. Keep scraping those;
fleet metrics complement them with the current work and observed results.

The elected leader refreshes the fleet snapshot from its informer cache every
30 seconds. Standbys expose `cluster_vending_fleet_metrics_active=0` and do not
publish fleet snapshots. Refreshes perform no cloud discovery or writes. Scrapes
read an immutable in-memory snapshot and make no Kubernetes/AWS requests.

| Metric prefix `cluster_vending_` omitted | Labels | Meaning |
| --- | --- | --- |
| `fleet_resources` | `kind`, `phase` | Account, network, cluster and tenant Component counts by lifecycle phase |
| `fleet_cluster_configuration` | `channel`, `tier`, `mode` | Cluster distribution, using the current parent network tier |
| `fleet_inventory_resources` | `owner_kind` | Inventory reference counts, including platform component inventory separately |
| `fleet_conditions` | `kind`, `condition`, `status` | Observed Ready/Synced true, false or unknown counts |
| `fleet_generation_pending` | `kind` | Owners whose status has not caught up with the Kubernetes generation |
| `fleet_oldest_provisioning_seconds` | `kind` | Oldest current provisioning age; account bootstrapping is included |
| `fleet_frozen_resources` | `kind` | Owners reporting a true Frozen condition |
| `fleet_upgrade_clusters` | `channel`, `tier`, `phase` | Clusters currently reporting a Kubernetes upgrade phase |
| `fleet_upgrade_oldest_phase_seconds` | `phase` | Longest time spent in each current Kubernetes upgrade phase |
| `fleet_addons_behind` | none | Add-ons reported behind during Kubernetes upgrades |
| `fleet_components` | `scope`, `phase` | Platform and tenant component instances by phase |
| `fleet_release_moves` | `phase` | Cluster platform release moves by observed phase |
| `fleet_code_changes` | `kind`, `phase` | Network/cluster renderer rollout status |
| `freeze_policy_present` | none | Canonical freeze policy exists, including scoped policies |
| `freeze_phase` | `phase` | Canonical confirmation state: pending, freezing, frozen, unfrozen or unknown |
| `freeze_resources` | `state` | Matched and paused managed resources from the last freeze confirmation |
| `fleet_metrics_active` | none | This process is the leader publishing fleet snapshots |
| `fleet_metrics_refresh_success` | none | Latest refresh succeeded |
| `fleet_metrics_last_success_timestamp_seconds` | none | Last successful refresh Unix timestamp; 0 before the first |
| `fleet_metrics_refresh_errors_total` | none | Failed snapshot refresh attempts |
| `fleet_metrics_refresh_duration_seconds` | none | Duration of the latest refresh attempt |

Label values are lowercase finite vocabularies. Names, tenant IDs, UUIDs, cloud
ARNs, arbitrary component names, resource versions, error messages and raw URLs
are never labels. Unrecognized status values map to `unknown`. No version or
release ID labels are emitted. Fleet data is observed status, not a fresh cloud
health check. Inventory counts include Usages and stale/not-yet-created
references; they are not live AWS resource counts. Provisioning ages include
time frozen or waiting on dependencies. Condition counts can describe an older
generation; inspect `fleet_generation_pending` alongside them.

Successful refreshes replace the snapshot atomically and remove disappeared
series. Some aggregate series emit explicit zero values for an empty fleet;
configuration and upgrade combinations without members are absent. A failed
refresh retains the last complete snapshot and marks refresh health failed.
Use freshness and active-publisher filters before alerting on fleet state.
Fleet reads are not a Kubernetes transaction; related owners may have changed
between cached list calls.

## Useful queries

API throughput, aggregated across server replicas:

```promql
sum by (rpc_service, rpc_method, code) (
  rate(cluster_vending_rpc_requests_total[5m])
)
```

Unary p95 lifetime by method:

```promql
histogram_quantile(0.95,
  sum by (le, rpc_service, rpc_method) (
    rate(cluster_vending_rpc_duration_seconds_bucket{rpc_type="unary"}[5m])
  )
)
```

Cluster lifecycle counts from the fresh leader snapshot:

```promql
sum by (phase) (
  cluster_vending_fleet_resources{kind="cluster"}
  and on(job, instance) (cluster_vending_fleet_metrics_active == 1)
  and on(job, instance)
    (time() - cluster_vending_fleet_metrics_last_success_timestamp_seconds < 120)
)
```

Recorded managed-resource inventory:

```promql
sum by (owner_kind) (
  cluster_vending_fleet_inventory_resources
  and on(job, instance) (cluster_vending_fleet_metrics_active == 1)
)
```

Controller bottlenecks (controller-runtime metrics, supplied by the existing registry):

```promql
sum by (name) (workqueue_depth)
sum by (controller) (rate(controller_runtime_reconcile_total{result="error"}[5m]))
histogram_quantile(0.95,
  sum by (le, controller) (rate(controller_runtime_reconcile_time_seconds_bucket[5m]))
)
```

The rules in `config/prometheus/rules.yaml` cover unavailable scrape targets,
RPC server errors, stale fleet snapshots, absent leaders, slow provisioning,
long upgrade phases and freezes that remain unconfirmed. Thresholds are
starting points to tune after measuring the fleet; slow-phase alerts do not
prove progress has stopped. Scope or group queries by your platform/management
cluster label when one Prometheus instance monitors several installations.

Scrape Crossplane and provider metrics separately for provider queue depth,
reconcile errors, CPU/memory, Kubernetes REST request throttling and cloud API
throttling. Platform lifecycle metrics do not measure AWS API call rates.
