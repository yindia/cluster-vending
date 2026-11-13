# Deprecated API versions in workload clusters

Before a cluster moves to a new Kubernetes minor, the objects in it that
were written with an API version the new minor removes have to be
rewritten: once the API version is gone they cannot be applied again. The
platform finds those objects and shows them through the API. It is what
`pluto detect-all-in-cluster` finds, and nothing else of Pluto.

## What is detected

An object is found when its **original manifest** is kept in the cluster
and names an API version that is deprecated or removed at the **target
version**: the Kubernetes minor the cluster moves to next. That is the
`upgradeTo` of the cluster's minor in the release its channel offers, or,
when the release does not move that minor, the next minor.

Two sources keep a manifest:

| Source | Where | Shown as |
|---|---|---|
| `kubectl apply` | the `kubectl.kubernetes.io/last-applied-configuration` annotation of a live object | `Manifest` |
| Helm v3 | the manifest of the newest deployed revision of every release (the `sh.helm.release.v1.*` Secrets) | `HelmRelease`, with `<namespace>/<release>` |

A finding is **removed** (the API version is gone at the target version)
or only **deprecated**, and carries the release that removes it and the
API version to use instead.

### What is not seen

- **Objects without a kept manifest.** Created with `kubectl create`,
  server-side apply, a client library or an operator: nothing in the
  cluster says which API version they were written with. EKS upgrade
  insights (which read the audit log) see calls of those clients; the
  platform does not read them yet.
- **Helm releases stored in ConfigMaps** (the non-default storage driver).
- **Events, Leases, Endpoints, EndpointSlices and ReplicaSets** as live
  objects: controllers write them, in volume. A Helm release that declares
  one is still found.
- A cluster-scoped object of a Helm release is shown with the release's
  namespace.

## The rules

The rules are Pluto's data file, read as it is:
`pkg/deprecation/data/versions.yaml`, from
[FairwindsOps/pluto](https://github.com/FairwindsOps/pluto) v5.24.5
(Apache-2.0). Nothing of Pluto is linked into the binary: the file is
vendored and matched by `pkg/deprecation`.

- Rules of component `k8s` are judged against the cluster's target version.
- Rules of other components (`cert-manager`, `istio`) are judged against
  the versions the data file names for them, as Pluto does by default.
- The newest Kubernetes removal the data knows is in **1.36**. For 1.37 it
  names none; `deprecation.NoKnownRemovals` records that, and a test fails
  when a release offers a minor that has neither removals in the data nor
  an entry there.

Refresh the rules when Pluto releases:

```sh
make deprecation-rules PLUTO_VERSION=v5.25.0
# set DataSource in pkg/deprecation/deprecation.go, then
go test ./pkg/deprecation/ ./pkg/controller/
```

## The scan

The controller scans. The API server never reads a workload cluster.

- **Which clusters:** every cluster that is `Running` or `Imported` and
  not being deleted, one at a time.
- **When:** every six hours, and at once when the target version changes.
  A scan that failed is tried again after 15 minutes.
- **How it reaches the cluster:** with the kubeconfig Secret the cluster's
  ClusterAuth writes (`status.kubeconfigSecret`, key `kubeconfig`). The
  controller may `get` Secrets in tenant namespaces only: each tenant
  namespace binds it to the `cluster-vending-kubeconfig-reader` role
  ([controller](controller.md)). Where that binding is missing the scan
  fails with a message naming it, and is tried again.
- **Read-only:** discovery, then metadata-only lists of the kinds a rule
  names, and the Secrets labelled `owner=helm,status=deployed`.
- **Bounded:** 250 objects a page (20 for Helm Secrets), 30 seconds a
  call, two minutes a scan. A kind that cannot be listed is passed over.
- **A scan that fails** (no kubeconfig yet, an expired one, an
  unreachable cluster) is recorded, with what the last scan that worked
  found. It never fails the cluster's own reconcile.

### Where the result is

On the Cluster object:

```yaml
status:
  deprecations:
    targetVersion: "1.37"
    scannedAt: "2026-11-13T04:00:00Z"
    attemptedAt: "2026-11-13T04:00:00Z"
    total: 3
    removed: 1
    findings:          # removed first; at most 200, total counts all
    - namespace: web
      kind: Ingress
      name: site
      apiVersion: extensions/v1beta1
      component: k8s
      deprecatedIn: v1.14.0
      removedIn: v1.22.0
      replacementAPI: networking.k8s.io/v1
      source: Manifest
      removed: true
  conditions:
  - type: DeprecationScan   # True: Scanned, DeprecatedAPIsInUse or RemovedAPIsInUse; False: ScanFailed
```

## API and CLI

`GetCluster` and `ListClusters` carry the summary in
`status.deprecations` (target version, times, counts, the error of a
failed scan) and never the list.

`ClusterService.ListClusterDeprecations` returns the findings, removed
first, paged, from the platform's store:

| Field | |
|---|---|
| `tenant_id`, `cluster_id` | the cluster |
| `page_size`, `page_token` | 50 a page by default, at most 200 |
| `only_removed` | only what is gone at the target version |

```sh
cluster-vending cluster deprecations <cluster-id> --tenant acme
cluster-vending cluster deprecations <cluster-id> --tenant acme --only-removed
cluster-vending cluster deprecations <cluster-id> --tenant acme -o json
```

```text
Kubernetes 1.37, scanned 2026-11-13T04:00:00Z: 3 found, 1 removed
NAMESPACE  KIND     NAME  API VERSION         STATE    REMOVED IN  REPLACEMENT           SOURCE
web        Ingress  site  extensions/v1beta1  removed  v1.22.0     networking.k8s.io/v1  manifest
```

## Upgrades

`deprecation.BlocksUpgrade(findings, target)` is true when a finding is
removed at the target. No upgrade flow calls it yet
([rollouts](design/rollouts.md), section 8): automatic upgrades will hold
on it, manual ones will warn.
