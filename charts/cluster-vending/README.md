# cluster-vending Helm chart

Runs the platform in a management cluster from one values file:

| Workload | Command | Port | Probes | Service account | AWS |
|---|---|---|---|---|---|
| API server | `cluster-vending serve` | 8080 (Connect, gRPC, gRPC-Web, `/metrics`) | `/healthz` (liveness, startup), `/readyz` | `<fullname>-server` | optional IRSA: its own server role (`aws.serverRoleArn`), to read results buckets |
| controller | `cluster-vending controller` | 8081 (probes), 8080 metrics (`controller.metrics`, on) | `/healthz`, `/readyz` on 8081 | `<fullname>-controller` | IRSA: the management role (`aws.managementRoleArn`) |
| live reader | `cluster-vending live` | 8090 (internal), 8091 `/metrics` only (`live.metrics`, on) | `/healthz`, `/readyz` | `cluster-vending-live` (fixed) | none |
| console | `node server.js` (ui/) | 3000 | TCP (liveness), `/favicon.ico` (readiness) | `<fullname>-ui`, no token | none |

`<fullname>` is the release name when it contains `cluster-vending`, else
`<release>-cluster-vending`. Install with the release name `cluster-vending`
and the object names are those of `config/` (`cluster-vending-server`, ...).
The kustomize manifests in `config/` are the reference this chart follows;
`config/helm_chart_test.go` holds the two together.

## Prerequisites

- Kubernetes 1.29+ and Helm 3.8+ (or Helm 4).
- **Crossplane**, installed first. The controller installs the Crossplane AWS
  providers, provider-helm and the activation policy on start, not Crossplane
  itself. As `hack/start.sh` does:

  ```sh
  helm repo add crossplane-stable https://charts.crossplane.io/stable
  helm upgrade --install crossplane crossplane-stable/crossplane \
    -n crossplane-system --create-namespace --version 2.4.3 \
    --set provider.defaultActivations=null --wait
  ```

- In production, IRSA (or Pod Identity) for the controller's service account,
  with the management role.

## Install

The API's login is required: the chart refuses to render without
`platform.auth.mode: required` (start from [ci/auth-values.yaml](ci/auth-values.yaml)
and put your issuer and audience in it):

```sh
helm install cluster-vending charts/cluster-vending \
  -n cluster-vending-system --create-namespace \
  -f my-auth-values.yaml \
  --set aws.managementRoleArn=arn:aws:iam::111122223333:role/crossplane-provider-aws \
  --set controller.serviceAccount.annotations."eks\.amazonaws\.com/role-arn"=arn:aws:iam::111122223333:role/crossplane-provider-aws
```

**Login is not optional.** The render fails when:

- `server.enabled` and `platform.auth.mode` is not `required`: every caller of the
  API would be anonymous and could call every RPC for every tenant (freeze the
  platform, delete any cluster, read account external IDs);
- `ui.ingress.enabled` and `ui.auth.enabled` is false: the Ingress would publish the
  console, and through it the API, without login.

`allowInsecureNoAuth: true` lifts both checks. It is for **local/dev clusters only**
(`ci/kind-values.yaml` sets it); NOTES.txt then prints a warning of what is open.
It is a chart value only and never reaches the settings file.

The management role template
(`pkg/cloudformation/templates/cluster-vending-management-role.template.yaml`) trusts
the service accounts `cluster-vending-controller` and `cluster-vending-server` in
`cluster-vending-system` by default: exactly what this install creates. With another
release name or namespace, set `controller.serviceAccount.name` and
`server.serviceAccount.name`, or the template's parameters, to match.

One release per management cluster. The controller binds fixed names in every
tenant namespace: the ClusterRoles `cluster-vending-kubeconfig-reader` and
`cluster-vending-usage-history-writer`, and the live reader's service account
`cluster-vending-live` in the controller's own namespace. Two releases would
share them.

**CRDs.** The chart ships none. The controller installs and updates the
`cloud.cluster-vending.io` CRDs on every start, from the copy embedded in its
binary, so they always match the running version (Helm never upgrades a
chart's `crds/`). The API server waits up to 5 minutes for them (its startup
probe allows that), so it becomes ready shortly after the controller. With
`controller.enabled=false`, a controller must run elsewhere. `helm uninstall`
leaves the CRDs and every tenant's objects in place.

## Port-forward

The console's browser code calls only its own origin: `/api/rpc/<service>/<method>`.
The console's server side forwards each call to `CV_API_BASE_URL`, the API's
in-cluster Service (`http://<fullname>-server.<namespace>.svc:8080`, or
`ui.apiBaseUrl`). One port-forward is therefore enough for the console:

```sh
kubectl -n cluster-vending-system port-forward svc/cluster-vending-ui 3000:3000
# http://localhost:3000
```

The CLI talks to the API directly:

```sh
kubectl -n cluster-vending-system port-forward svc/cluster-vending-server 8080:8080
bin/cluster-vending --server http://localhost:8080 tenant list
```

A port-forward enters the pod's network namespace (the kubelet connects to the
pod's loopback), so the network policies do not block it: the API server's
policy, on by default, leaves this path open.

The console's RPC route refuses a request whose `Origin` is not its own. With
login off, it accepts only a loopback host the browser addressed (`localhost`,
`*.localhost`, `127.0.0.1`, `[::1]`, as with a port-forward), or exactly
`CV_PUBLIC_ORIGIN`. Any other host is refused, which stops DNS rebinding: a page
whose own name points at 127.0.0.1 is not loopback. With `ui.ingress.enabled` and
login off, the chart sets `CV_PUBLIC_ORIGIN` from the first Ingress host (https when
`tls` is set); set it through `ui.extraEnv` for anything else. Forwarded headers
are never consulted. Requests marked `Sec-Fetch-Site: cross-site` are always
refused. With login on, the
route accepts the `AUTH_URL` origin only.

With web login on (`ui.auth.enabled`), the console refuses plain-HTTP URLs in
production (`AUTH_URL` and the issuer must be HTTPS). Use the Ingress with TLS
for login. Port-forward works for a console with login off.

## Values

The full list with comments is [values.yaml](values.yaml), and
[values.schema.json](values.schema.json) checks it. The main ones:

| Value | Default | Meaning |
|---|---|---|
| `image.repository`, `image.tag` | `ghcr.io/yindia/cluster-vending`, appVersion | The Go binary (`make docker-build`) for serve, controller and live |
| `ui.image.repository`, `ui.image.tag` | `ghcr.io/yindia/cluster-vending-ui`, appVersion | The console (`make docker-build-ui`) |
| `server.enabled`, `controller.enabled`, `live.enabled`, `ui.enabled` | `true` | Each workload on or off |
| `aws.managementRoleArn` | empty, **required** with the controller | The role the Crossplane AWS providers run as. Also printed by the server's `account create`. |
| `aws.serverRoleArn`, `aws.region`, `aws.sourceRoleArn` | empty | The server's own role, the SDK region, a role assumed first |
| `platform` | `config/node-groups/platform.yaml` | The shared settings file (below) |
| `log.level`, `log.format` | `info`, `json` | All three Go processes |
| `server.replicas` | `1` | More replicas serve the API |
| `controller.replicas`, `controller.leaderElect` | `1`, `true` | More than one replica needs leader election (the chart refuses otherwise) |
| `controller.installProviders`, `controller.installCatalog` | `true` | `--install-providers`, `--install-catalog` |
| `controller.metrics.enabled` | `true` | Metrics on :8080 and a `<fullname>-controller-metrics` Service |
| `live.metrics.enabled`, `.port` | `true`, `8091` | The live reader's `/metrics` on a port of its own (`CV_LIVE_METRICS_ADDR`), so a scraper never reaches its internal endpoint |
| `metrics.serviceMonitor.enabled`, `.labels`, `.interval` | off | Prometheus Operator ServiceMonitors for the API server, the controller and the live reader |
| `metrics.prometheusRule.enabled`, `.labels` | off | The platform's alerts (`files/prometheus-rules.yaml`, a copy of `config/prometheus/rules.yaml`), each with a `runbook_url` |
| `networkPolicy.metricsFrom` | namespaces labelled `metrics=enabled` | Who reaches the live reader's metrics port, and the controller's with `networkPolicy.controllerMetrics` |
| `live.*` | | Always one replica, strategy `Recreate`; no `replicas` value |
| `ui.apiBaseUrl` | the server Service | `CV_API_BASE_URL`, the console's upstream |
| `ui.auth.enabled`, `.url`, `.issuer`, `.clientId`, `.existingSecret` | off | Web login. The Secret holds `KEYCLOAK_CLIENT_SECRET` and `AUTH_SECRET`. |
| `ui.ingress.enabled` | `false` | An Ingress for the console |
| `<workload>.serviceAccount.annotations` | none | IRSA. The live reader's may not carry a role (the chart refuses one). |
| `<workload>.resources`, `.nodeSelector`, `.tolerations`, `.affinity`, `.podSecurityContext`, `.securityContext`, probes | as `config/` | Every container runs non-root with a read-only root filesystem and no capabilities |
| `networkPolicy.live` | `true` | The live reader accepts the API server's pods only, as `config/live` |
| `allowInsecureNoAuth` | `false` | **Local/dev only.** Render without `platform.auth.mode: required`, and a console Ingress without `ui.auth` |
| `networkPolicy.server.enabled`, `.from` | **on**, none | The API accepts the console's pods and the listed peers only. List an ingress controller, in-cluster CLI users or Prometheus (it scrapes `/metrics` on 8080) in `from`. |
| `networkPolicy.ui.enabled`, `.from` | off | The console accepts the listed peers (an ingress controller) |
| `rbac.userRoles` | `true` | The helper ClusterRoles for people (cluster, network, cloud account admin/editor/viewer, release admin/viewer) |

### The shared settings file

`platform` is rendered into one ConfigMap, `<fullname>-platform`, mounted at
`/etc/cluster-vending/platform.yaml` into the API server and the controller
(`--config`), so they cannot disagree. Both pods carry a checksum of it and
restart when it changes. `aws.*` values are added to it as flat keys
(`aws-management-role-arn`, ...). Keys are the commands' flag names plus the
`auth`, `naas`, `nodeGroups` and `providers` sections ([docs/architecture.md, section
5](../../docs/architecture.md#5-configuration)). `platform.providers` tunes the
Crossplane providers (replicas, poll, reconcile rate, resources; defaults and
per-provider overrides); the controller applies it on every start
([provider runtime](../../docs/architecture.md#provider-runtime)). Controller
throughput settings are flat keys too: `scan-workers` (4), `kube-qps` (50),
`kube-burst` (100). The live reader reads none of it.

Never put a secret in `platform`: Secrets are referenced by name, e.g.
`provider-credentials-secret: crossplane-system/aws-local-credentials`.

Authentication of the API is `platform.auth`, and it must be `mode: required`
(see Install). Mode `none`, where every caller is anonymous and may call
everything, renders only with `allowInsecureNoAuth`.

## RBAC

The rules are those of `config/`, read from copies in `files/rbac` (`make
helm-sync` refreshes them; `config/helm_chart_test.go` fails when they drift):

- controller: `manager-role` (generated) and `platform-role`, a leader election
  Role in the release namespace, and the unbound `cluster-vending-kubeconfig-reader`
  and `cluster-vending-usage-history-writer` it binds in tenant namespaces.
- API server: `server-role`. No Secret access and no workload-cluster
  credentials.
- live reader: `live-role` (Clusters and ClusterAuths, read only). Its kubeconfig
  Secrets come through the controller's binding in each tenant namespace.

## Local test on kind

On a separate kind cluster, never the development one:

```sh
make kind-up KIND_CLUSTER=cv-helm          # kind + Crossplane
make docker-build docker-build-ui TAG=dev
make kind-load helm-install-kind TAG=dev   # HELM_KIND_CLUSTER=cv-helm
```

`ci/kind-values.yaml` sets a placeholder management role and
`allowInsecureNoAuth: true` (the API runs without login, for this local test
only). Kind has no IRSA: the
controller installs the CRDs, providers and catalog, and its AWS calls fail
until credentials are given (docs/local-development.md). The API, the live
reader and the console work without them.
