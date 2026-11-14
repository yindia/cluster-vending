# Resource recommendations

The platform recommends the CPU and memory requests (and memory limits) of
the containers in a workload cluster, from the usage it records over a
week. It installs nothing in the cluster: it reads what the cluster already
reports.

```sh
cluster-vending cluster recommendations <cluster-id> --tenant acme [--namespace shop] [--only-changes] [-o table|json]
```

```
Sampled 2026-11-14T08:00:00Z: 41 containers of 30 workloads, 7 days kept; metrics-server: yes; VPA: no
NAMESPACE  WORKLOAD             CONTAINER  CPU REQ                MEM REQ              MEM LIMIT       SOURCE          DATA
shop       deployment/web       app        1000m -> 120m (-88%)  1Gi -> 230Mi (-77%)  2Gi -> 276Mi    usage-estimate  6d
shop       statefulset/db       pg         -> 240m                -> 345Mi             - -> 414Mi      usage-estimate  6d
ops        daemonset/agent      agent      -                      -                    -               no-data         -
```

## What is read

Every five minutes the controller reads, with the kubeconfig the platform
holds for the cluster, and read-only:

- **The workloads**: Deployments, StatefulSets and DaemonSets, with the
  requests and limits of their pod templates, and the pods' owners.
- **Usage**: what the metrics API (`metrics.k8s.io`, served by
  metrics-server) reports each pod's containers use now. A workload's usage
  is the most any of its pods uses. Without metrics-server nothing is
  recorded, and there are no estimates.
- **The cluster's own Vertical Pod Autoscalers**, when the tenant runs VPA
  (`autoscaling.k8s.io/v1`): their recommendations are used as they are.

Paused clusters, clusters that are not running, and imported clusters
before their takeover (no kubeconfig yet) are not sampled. A sample that
fails is recorded on the cluster (`status.recommendations.error`) and tried
again after 15 minutes; it never affects the cluster itself.
`--recommendations=false` on the controller turns sampling off.

## Where it is kept

- **The recorded usage**: a ConfigMap `<cluster>-usage-history` in the
  tenant namespace of the management cluster, owned by the Cluster object,
  so it is deleted with the cluster. Deleting it loses the history: the week
  starts again.
- **Per container**: its current requests and limits, the VPA
  recommendation if any, when usage was first and last recorded, the sample
  count, and one bucket per UTC day (count, max and sum of CPU and memory),
  at most 7.
- **Bounds**: at most 2,000 containers per cluster (past that the largest
  consumers are kept and the summary says how many were left out), stored
  gzipped under 900 KiB. A full week of 2,000 containers takes about 100 KiB.
- **The summary** (`status.recommendations`, also in `GetCluster`): when it
  was sampled, how many workloads and containers, whether metrics-server and
  VPA were found, and what stands in the way.

The API server computes the recommendations from the ConfigMap when asked
(`ListClusterRecommendations`); it never reads the workload cluster.

## How a recommendation is made

| Source | When | Recommended request | Recommended memory limit |
|---|---|---|---|
| `vpa` | the cluster's VPA recommends for the container | VPA's target | VPA's upper bound |
| `usage-estimate` | at least a day of recorded usage | the peak over the week + 15%, rounded up (CPU to 10m, memory to 1Mi) | the recommended memory request + 20% |
| `insufficient-data` | less than a day of usage | none | none |
| `no-data` | no usage and no VPA | none | none |

- **Floor:** never below 10m of CPU or 16Mi of memory.
- **No CPU limit is recommended.** CPU over a limit only throttles, and the
  request already guarantees the container its share.
- **Difference:** recommended minus current request, with the percentage of
  the current request. `--only-changes` keeps the containers where it is a
  tenth or more, or where the workload sets no request.
- **Platform workloads:** rows in the namespaces of the platform's
  components (and `kube-system`) are marked `platform` in the API.

## Limits

- **metrics-server is needed** for estimates (it is an EKS add-on the
  platform can install). With neither metrics-server nor VPA, the summary
  says nothing can be recommended.
- **Time:** the first numbers come after a day; a week is needed before they
  can be relied on, and a week that missed the busy days misses their peaks.
- **Samples every five minutes** miss short spikes between them.
- **Horizontal Pod Autoscalers:** a workload scaled on CPU or memory by an
  HPA spreads its load over its replicas, so per-pod usage is lower than the
  total; changing its requests also changes when the HPA scales.
- **Jobs and bare pods** are not recorded: only Deployments, StatefulSets
  and DaemonSets.
- **Nothing is applied.** The recommendations are advice; the tenant changes
  their own workloads.
