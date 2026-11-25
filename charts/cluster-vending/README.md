# cluster-vending Helm chart

Runs the platform in a management cluster from one values file:

| Workload | Command | Port | Probes | Service account | AWS |
|---|---|---|---|---|---|
| API server | `cluster-vending serve` | 8080 (Connect, gRPC, gRPC-Web, `/metrics`) | `/healthz` (liveness, startup), `/readyz` | `<fullname>-server` | optional IRSA: its own server role (`aws.serverRoleArn`), to read results buckets |
| controller | `cluster-vending controller` | 8081 (probes), 8080 metrics if enabled | `/healthz`, `/readyz` on 8081 | `<fullname>-controller` | IRSA: the management role (`aws.managementRoleArn`) |
| live reader | `cluster-vending live` | 8090 (internal) | `/healthz`, `/readyz` | `cluster-vending-live` (fixed) | none |
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

```sh
helm install cluster-vending charts/cluster-vending \
  -n cluster-vending-system --create-namespace \
  --set aws.managementRoleArn=arn:aws:iam::111122223333:role/crossplane-provider-aws \
  --set controller.serviceAccount.annotations."eks\.amazonaws\.com/role-arn"=arn:aws:iam::111122223333:role/crossplane-provider-aws
```

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

A port-forward enters the pod's network namespace, so the network policies do
not block it.

The console's RPC route refuses a request whose `Origin` is not its own. Next's
standalone server derives "its own" from its listen address (`http://0.0.0.0:3000`),
not from the host the browser used, so with login off every browser call would be
refused behind a port-forward or an Ingress. The image's entrypoint
(`ui/docker/serve.mjs`) sits in front of Next on port 3000 and runs Next on
loopback. When a request's `Origin` names the host it was sent to (a same-origin
call), the entrypoint rewrites `Origin` to Next's internal origin. Any other
`Origin` passes through unchanged and is still refused. With login on, nothing is
rewritten: the route checks against `AUTH_URL`.

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
| `controller.metrics.enabled` | `false` | Metrics on :8080 and a `<fullname>-controller-metrics` Service |
| `live.*` | | Always one replica, strategy `Recreate`; no `replicas` value |
| `ui.apiBaseUrl` | the server Service | `CV_API_BASE_URL`, the console's upstream |
| `ui.auth.enabled`, `.url`, `.issuer`, `.clientId`, `.existingSecret` | off | Web login. The Secret holds `KEYCLOAK_CLIENT_SECRET` and `AUTH_SECRET`. |
| `ui.ingress.enabled` | `false` | An Ingress for the console |
| `<workload>.serviceAccount.annotations` | none | IRSA. The live reader's may not carry a role (the chart refuses one). |
| `<workload>.resources`, `.nodeSelector`, `.tolerations`, `.affinity`, `.podSecurityContext`, `.securityContext`, probes | as `config/` | Every container runs non-root with a read-only root filesystem and no capabilities |
| `networkPolicy.live` | `true` | The live reader accepts the API server's pods only, as `config/live` |
| `networkPolicy.server.enabled`, `.from` | off | The API accepts the console's pods and the listed peers |
| `networkPolicy.ui.enabled`, `.from` | off | The console accepts the listed peers (an ingress controller) |
| `rbac.userRoles` | `true` | The helper ClusterRoles for people (cluster, network, cloud account admin/editor/viewer, release admin/viewer) |

### The shared settings file

`platform` is rendered into one ConfigMap, `<fullname>-platform`, mounted at
`/etc/cluster-vending/platform.yaml` into the API server and the controller
(`--config`), so they cannot disagree. Both pods carry a checksum of it and
restart when it changes. `aws.*` values are added to it as flat keys
(`aws-management-role-arn`, ...). Keys are the commands' flag names plus the
`auth`, `naas` and `nodeGroups` sections ([docs/architecture.md, section
5](../../docs/architecture.md#5-configuration)). The live reader reads none
of it.

Never put a secret in `platform`: Secrets are referenced by name, e.g.
`provider-credentials-secret: crossplane-system/aws-local-credentials`.

Authentication of the API is `platform.auth`. Without it the API runs with
mode `none`: every caller is anonymous and may call everything. NOTES.txt
warns about that.

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

`ci/kind-values.yaml` sets a placeholder management role. Kind has no IRSA: the
controller installs the CRDs, providers and catalog, and its AWS calls fail
until credentials are given (docs/local-development.md). The API, the live
reader and the console work without them.
