# PROP-07: OpenTelemetry tracing from the console to the cloud

| | |
|---|---|
| Status | Draft (2026-11-26) |
| Author | `<author>` |
| Date | 2026-11-26 |
| Reviewers | `<reviewers>` |
| Related | [architecture.md](../architecture.md): Metrics, Authentication, Results in S3; [PROP-02](PROP-02-cluster-data-live-and-snapshot.md) (live reader); [PROP-04](PROP-04-api-authentication.md); the Prometheus metrics work in progress (Connect interceptor, collectors) |

## 1. Summary

Question (user, 2026-11-26): how can we add OTel tracing from the server to the controller, the Kubernetes
API and external calls, and is tracing hard to add to the controller?

- **Synchronous paths use standard propagation.** These are console → API server, API server → live
  reader, any process → Kubernetes API, and any process → AWS. They need `otelconnect` (it supports
  Connect v2 since v0.12.0), `otelhttp` on the plain handlers, a client-go round-tripper set through
  `rest.Config.Wrap`, and `otelaws` middleware. Each has one or two chokepoints in the code (section 2).
- **The server → controller hop is asynchronous, so it uses a span link, not a parent.** When the API
  server writes a row object (Cluster, Network, CloudAccount, Component, SubTenant, ReleaseHold) for a
  sampled request, it adds one annotation, `cloud.cluster-vending.io/traceparent`, in the same request
  as the change. Only the API server writes it. The controller never does. So the annotation adds no
  writes, no resourceVersion bumps and no reconciles. Each reconcile is its own short trace (a root
  span) with a **link** to that request. Requeues, retries and resyncs link to the same request until
  the next API write replaces the annotation or removes it.
- **A sampler that follows links.** It follows the parent's decision when there is one. A root span that
  links to a sampled span is kept, so every reconcile of a sampled request is kept. Other roots
  (resyncs, watch-driven passes) are sampled at a low ratio.
- **The trace ends where the platform's writes end.** Crossplane providers and provider-helm are separate
  processes that call AWS and Helm out of band. They emit no traces we can join. The platform's last span
  is the server-side apply of the managed resource or Release. It records the kind, namespace, name and
  the resourceVersion the API server returned, so the provider's events and logs for that object can be
  found from the trace.
- **Is the controller hard? No, not mechanically.** controller-runtime has no tracing, but one wrapper
  around each `Reconciler` (15 `Complete(...)` calls) adds it. `ctx` is already passed through
  `pkg/reconcile` consistently. The only places that drop it are the Helm SDK actions (they take no
  context) and the OIDC discovery (on purpose). The real work is choosing what to trace and keeping the
  volume sane. Section 9 has the estimate. The spike in section 6.11 proves the two riskiest pieces.

## 2. How it works today

Checked against `main` (6793d2b). There is no tracing. `go.opentelemetry.io/otel` and `otel/trace` v1.46.0
are in `go.mod` only as indirect dependencies.

| Concern | Where | Today |
|---|---|---|
| API handler | `pkg/server/server.go` `NewHandler` | `connect.NewServer(rpcMetrics.Interceptor, service.Faults, authn.Interceptor(...))`. `/metrics`, `/healthz`, `/readyz` and the CloudFormation template (`cloudformation.Mount`) are on a plain `http.ServeMux`. |
| Console proxy | `ui/src/lib/api/proxy.ts` `proxyRPC` | Forwards only `Authorization`, `Connect-Protocol-Version`, `Connect-Timeout-Ms` and `Accept`. A browser `traceparent` is dropped. There is no `instrumentation.ts` and no OTel package in `ui/package.json`. |
| Server writes | `pkg/database/cloudstore` (`setMeta`/`annotate` in `store.go`, used by `clusters.go`, `networks.go`, `tenants.go`, `components.go`, `retry.go`, `access.go`), plus direct `deps.Kube` writes in `pkg/server/subtenant/{crud,name_claim,parent}.go` and `pkg/server/admin/freeze.go` | Row state is kept in annotations (`AnnotationRow`, `AnnotationInput`, ...) and written in one request with the spec. The **controller uses the same store** for its own writes (`Record`, status, `row-generation`). |
| Controller entry | `pkg/controller/*_controller.go`, `SetupAll` in `controller.go` | 15 controllers, built with `ctrl.NewControllerManagedBy(...).Complete(r)`. The `For` predicate `rowChanged()` includes `AnnotationChangedPredicate`, so **any annotation change triggers a reconcile**. |
| Requeues | `pkg/controller/controller.go` | `pendingDelay` = 20 s while pending, `resyncDelay` = 10 min steady. Provider objects are watched through `batchedOwner`. |
| Context in `pkg/reconcile` | e.g. `cluster/reconcile.go` | `ctx` reaches every store, kube and AWS call. It carries values (`withHelmPass`, `withDeleteStage`, `WithNotes`, ...). `context.WithoutCancel` (`cluster/node_rows.go:114`, `cluster/summary.go:85`, `cloudstore/clusters.go:409`, `live/reader.go:92`) keeps the values, so it keeps the span. There are no goroutines in `pkg/reconcile`. |
| Context dropped | `pkg/management/helm_ops.go` `runBounded` | Helm SDK actions (`rollback.Run`, `uninstall.Run`, `cfg.Releases.Get/Update`) take no context. Their kube calls run with `context.Background()`. |
| Context dropped (on purpose) | `pkg/auth/verify.go:148` | OIDC discovery uses `context.Background()` so that one caller going away cannot fail it for others. |
| Management kube client | `pkg/kube/kube.go` `RESTConfig` | Used by the server and controller managers (`management.Connect`), `direct` clients (`controller/manager/manager.go:165`, `cmd/controller.go:103`) and the live reader (`live/run.go:38`). Informers (list and watch) use the same config. |
| Workload kube clients | `pkg/deprecation/kube.go` `RESTConfig` | Used by `management.WorkloadClients` (`workload_client.go`), `Workloads` (`workload.go`), `Teardowns` (`teardown.go`), `SubTenantWorkloads` (`subtenant_workloads.go:199`), `policy/reader.go`, `reconcile/recommendations`. Two bypass it: `deprecation.NewKubeReader` (`kube.go:43`) and the live reader's `NewDynamicLister` (`live/opener.go:91`) call `clientcmd.RESTConfigFromKubeConfig` directly. A workload client is built **per call** (`client.New`), so its REST mapper discovers the API on first use, inside the reconcile. |
| AWS | `pkg/cloud/aws/aws.go` `New`/`NewFromConfig` | One base `aws.Config` per process. `NewFromConfig` already appends `callTimeout` to `APIOptions`. Every client (`ec2`, `eks`, `iam`, `sts`, `logs`, `s3` in `s3.go`, the access check in `access_check.go`) and the `stscreds` AssumeRole chains (`assume`, `sourceConfig`) are built from copies of it. The controller gets it through `management.NewAWSClient`, the server through `server.NewResultsStore`. |
| Live reader | `pkg/live/handler.go` | `live.NewClient` is a Connect client over `http.DefaultClient`. `NewHandler` is `connect.NewServer()` with no interceptors. |
| Logs | `pkg/logging`, 76 files using `InfoContext`/`ErrorContext` | slog JSON or text. The context is passed, so trace IDs can be added with one handler. |
| Metrics | `pkg/telemetry/rpc.go`, `fleet.go`, the controller-runtime registry | Plain Prometheus text format (`promhttp.HandlerOpts{}`), so no exemplars. |
| Chart | `charts/cluster-vending/templates/{server,controller-deployment,live,networkpolicy}.yaml` | `extraEnv` exists per component. The live reader's NetworkPolicy allows egress only to 53, 443 and 6443. |

## 3. Problems today

1. **No causal view of a request.** "I edited cluster X at 10:02. Why is it still `Updating`?" can only be
   answered from logs of three processes, read by `cluster_id` and time.
2. **The asynchronous hop hides latency.** Nothing shows how long a change waited in the queue, how many
   passes it took, or which pass did the write.
3. **Kubernetes and AWS latency are invisible per call.** Per-reconcile client construction (workload
   discovery) and STS AssumeRole on a cold credentials cache are suspected costs with no measurement.
4. **Metrics have no path to an example.** A slow `cluster_vending_rpc_duration_seconds` bucket cannot be
   followed to one request.

## 4. Goals and non-goals

### Goals

- One trace per API request, from the console (optionally the browser) through the API server, the live
  reader, the Kubernetes API calls and the AWS calls it makes.
- Every reconcile is a trace, linked to the API request that caused the change it works on.
- Sub-spans for the stages of a cluster reconcile that matter: render, apply, observe, Helm gate,
  rollout readiness, deletion stages, node groups, catalog changes.
- Standard configuration: `OTEL_*` environment variables, OTLP export, off by default with no overhead
  when off.
- Bounded cardinality and no secrets or tenant-supplied free text in span names or attributes.
- Trace IDs in logs, and exemplars on the RPC and reconcile histograms.

### Non-goals

- Tracing inside Crossplane providers, provider-helm, the EKS control plane or AWS. We cannot configure
  EKS `kube-apiserver` tracing (no access to its `--tracing-config-file`).
- Metrics or logs through OTLP. Prometheus stays as it is, and so do logs on stdout.
- Running a tracing backend. The chart can deploy a collector; storage (Tempo, Jaeger, X-Ray, a vendor)
  is the operator's choice.
- Tracing the CLI. It could send a `traceparent` later. See open question 5.

## 5. Where the spans come from

```
browser ──traceparent?──▶ Next.js /api proxy ──traceparent──▶ API server (otelconnect)
                                                                ├─ kube get/list/patch (client-go RT)
                                                                ├─ S3 GetObject + STS AssumeRole (otelaws)
                                                                ├─ live reader ──▶ (otelconnect) ─▶ workload kube API
                                                                └─ store write: annotation traceparent=<this span>
                                                                          ┆ (async: informer event, queue)
                                                                          ┆ span link
controller: "reconcile cluster" (root) ◀┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┘
   ├─ store get (kube GET clusters)          ├─ render
   ├─ apply <kind>/<name> rv=… (SSA PATCH)   ├─ observe (kube GETs, AWS Describe*)
   ├─ helm gate ─ rollout readiness (workload kube LISTs)
   └─ status write                                       ⟶ Crossplane provider ⟶ AWS (not traced)
```

## 6. Design

### 6.1 Bootstrap: `pkg/telemetry/tracing.go`

- `telemetry.SetupTracing(ctx, service string) (shutdown func(context.Context) error, error)`, called first
  in `cmd/serve.go`, `cmd/controller.go` (`manager.Run`) and `cmd/live.go`. With
  `OTEL_TRACES_EXPORTER` unset or `none` (the default), or `OTEL_SDK_DISABLED=true`, it installs the
  no-op provider. Instrumented code then costs one interface call per span.
- The exporter is `otlptracegrpc` or `otlptracehttp`, chosen by `OTEL_EXPORTER_OTLP_PROTOCOL`. All other
  settings come from the standard variables (`OTEL_EXPORTER_OTLP_ENDPOINT`, `_HEADERS`, `_TIMEOUT`,
  `OTEL_BSP_*`). There are no platform flags (memory: fewer CLI options).
- The resource has `service.name` (`cluster-vending-server`, `cluster-vending-controller`,
  `cluster-vending-live`, `cluster-vending-console`), `service.version` = `version.Version`,
  `service.instance.id` = pod name, `k8s.namespace.name`, `k8s.pod.name` (downward API), and
  `deployment.environment.name` from `OTEL_RESOURCE_ATTRIBUTES`.
- The propagator is W3C `tracecontext` only. No `baggage`: it would carry caller-chosen values across
  processes.
- The sampler is `LinkedParentBased(ratio)` (6.4), with the ratio from `OTEL_TRACES_SAMPLER_ARG` and
  default 0.1. If `OTEL_TRACES_SAMPLER` is set explicitly, the SDK's sampler is used instead.
- `logging.New` wraps its handler so that records carrying a context with a valid span get `trace_id` and
  `span_id`. The 76 files that already log with `...Context` gain correlation without changes.
- Shutdown flushes with the existing shutdown timeouts (`server.Run`, `live.Run`, manager stop).

### 6.2 API server

- **Connect.** `otelconnect.NewServerInterceptor(otelconnect.WithoutMetrics(),
  otelconnect.WithPropagateResponseHeader())` goes **first** in `connect.NewServer(...)`
  (`server.go:110`). The metrics interceptor, faults and authn then run inside the span. Failed
  authentications are traced too, and the metrics interceptor can read the span for exemplars (6.9).
  `WithoutMetrics` is used because the metrics agent's `telemetry.RPC` already owns RPC metrics.
- **Remote parents.** otelconnect's default treats an incoming `traceparent` as untrusted: the server
  span becomes a **new root with a link** to it. That default is right for the public listener (the CLI,
  in-cluster callers, anything allowed by `networkPolicy.server.from`). It stops a caller from forcing
  sampling or joining our traces. The console is the exception. A request from the console's pod can be
  trusted with `WithTrustRemote()` behind a filter. The server cannot tell the console apart by itself
  (the UI calls over plain HTTP inside the cluster), so it is a setting: `tracing.trustRemoteParent:
  console|none` (open question 1). Until it is decided: link only.
- **Attributes** (otelconnect's semconv set): `rpc.system=connect_rpc`, `rpc.service`, `rpc.method`,
  `rpc.connect_rpc.error_code`, `server.address`. We add, from the authn interceptor once the caller is
  known: `cv.auth.mode`, `enduser.id` = the token's `sub` **only if** open question 4 allows it
  (otherwise nothing). From the handlers: `cv.tenant_id`, `cv.cluster_id`, `cv.network_id` (opaque
  platform IDs, already in every log line). Never request bodies, names chosen by tenants, input JSON
  or tags.
- **Plain HTTP.** Wrap only the non-RPC routes with `otelhttp.NewHandler` (CloudFormation template).
  `/metrics`, `/healthz` and `/readyz` are excluded with `otelhttp.WithFilter`: probes every few seconds
  would be the largest source of spans.
- **The server's own clients.** Its manager (`management.Connect`), its store (`ConnectStore`) and the
  results reader (`NewResultsStore`) get the kube and AWS instrumentation of 6.6 and 6.7.

### 6.3 Console (`ui/`)

- Add `ui/src/instrumentation.ts` (Next.js 16's hook) that registers the Node SDK (`@opentelemetry/sdk-node`,
  OTLP HTTP exporter, `service.name=cluster-vending-console`) only when `OTEL_EXPORTER_OTLP_ENDPOINT` is
  set. Load it dynamically in `register()` for `NEXT_RUNTIME === "nodejs"` only, so the edge bundle and
  the build stay unaffected.
- In `proxyRPC`, wrap the upstream `fetch` in a span `POST <service>/<method>`. The route is already checked
  against the allowlist, so the name is bounded. Inject its context with `propagation.inject` into the
  `headers` object the proxy builds. This is an explicit change in `proxy.ts` (the code builds its own
  `Headers`; nothing is copied by default), and it keeps the header allowlist explicit.
- **Starting in the browser (optional, phase 4).** Accept `traceparent` from the browser as a **link**,
  not a parent, for the same reason as 6.2. A browser span can then be found from the console span. Only
  with a setting `CV_TRACE_BROWSER=true` should the proxy take it as the parent, and even then it
  re-samples: an untrusted `sampled` flag is not followed. Browser instrumentation
  (`@opentelemetry/sdk-trace-web` + fetch instrumentation on the Connect transport) needs an OTLP
  endpoint the browser can reach and CORS. That is a deployment decision, so it is left for later.
- Do not forward `tracestate` or `baggage` from the browser.

### 6.4 Server → controller: the cause annotation and span links

**What is stored.** One annotation, `cloud.cluster-vending.io/traceparent`, holds the W3C `traceparent`
of the server span that made the change. `tracestate` is not stored: it is vendor data, up to 512 bytes,
and no backend needs it for a link. It is written on the objects the API writes:

| Object | Write path |
|---|---|
| CloudAccount, Network, Cluster, Component (rows) | `cloudstore`: every create, update, delete request (a delete through the store is an annotation change), `RequestRetry` (`retry.go`), `RequestAccessCheck` (`access.go`). Merged in `setMeta`/`annotate` (`store.go`), in the same request. |
| SubTenant | `pkg/server/subtenant/crud.go` Create and Patch |
| ReleaseHold (freeze) | `pkg/server/admin/freeze.go` Create/Update |

A direct `Delete` (`subtenant/crud.go:335`, `admin/freeze.go:104`) carries no body. A delete like that gets
no cause: adding a patch before it would be a second write and a second reconcile. The deletion reconcile
is still traced, just without a link.

**Only the API stamps it (no churn).** The store is shared: the controller writes rows through the same
`cloudstore` code with a context that also carries a span (the reconcile's). If the store stamped
whatever span is in the context, every controller write would change the annotation. That fires
`AnnotationChangedPredicate` (in `rowChanged()`) and causes another reconcile, which is a loop. So:

- The server interceptor marks the context: `telemetry.WithOrigin(ctx)`. The controller never does.
- `telemetry.CauseAnnotations(ctx)` returns nil without the mark, so nothing changes. With the mark it
  returns `{traceparent: <value>}` if the request is sampled, or `{traceparent: ""}` (remove) if it is
  not, so a reconcile never links to an older request than the one that made the latest change.
- The store merges that map into the annotations it already writes, so it adds no request. The
  controller's writes never touch the key. An API write already bumps resourceVersion and already
  triggers a reconcile, so the annotation adds none.
- A test in `pkg/database/storetest` checks that a controller-context write keeps the annotation as it
  is, and that an API-context write sets or clears it.

**Link, not child.** Each reconcile starts a **new root span** with a link to the cause. It is not a child
of the request, for these reasons:

1. Reconciles are level-based. One pass acts on everything that changed (possibly several requests,
   plus provider events). One request leads to many passes over hours (20 s requeues while
   `Provisioning`, then 10 min resyncs). As a child, one API trace would grow for hours, with thousands
   of spans. Backends cap trace duration and size, and tail samplers decide after a short wait, so the
   trace would be cut or decided before it is complete.
2. The RPC trace stays small and complete, and shows what the caller waited for.
3. The links are what OTel defines for batch and asynchronous work ("caused by, not part of"). Tempo,
   Jaeger and X-Ray all show them.

Making only the first reconcile after the write a child was considered (section 7). It needs "first"
to be known (`status.observedGeneration < generation`, or a "seen cause" record, which is a write) and it
still makes the trace long (the first pass of a create can last minutes). It is rejected.

**Finding all reconciles of a request.** Links are not indexed everywhere, so the reconcile span also gets
the attribute `cv.cause.trace_id`. TraceQL `{ span.cv.cause.trace_id = "<id>" }`, or a Jaeger tag
search, lists every pass of that request.

**When the link stops.** The annotation stays until the next API write. Without a bound, a resync a week
later would still link to an old edit. The wrapper links only while the change is open:

- Rows: while `row.Generation > row.ObservedGeneration` (or `AppliedGeneration`), or the state is pending
  (`PendingStates()`: Provisioning, Updating, Deleting, Imported), or within 1 h of the write (the
  annotation value is the traceparent alone, so this uses `rowMeta.UpdatedAt`).
- SubTenant and ReleaseHold: while `generation > status.observedGeneration`.

After that, the reconcile is a plain root trace with no link, sampled at the ratio.

**Retries, requeues, resyncs.** All of them reuse the same object, so while the change is open they link
to the same cause. The span records what controller-runtime gives us: `cv.reconcile_id`
(`controller.ReconcileIDFromContext`, also in controller-runtime's log lines), the result
(`cv.requeue_after_ms`), and the error with status `Error`. controller-runtime does not expose a retry
count. The number of passes of a request is the number of linked traces.

**Sampling.** `LinkedParentBased(ratio)` (in the spike, `tracing.go`):
- with a parent: follow it (standard parent-based);
- with no parent but a link to a **sampled** span: sample. Every pass of a sampled request is kept,
  even when the root ratio is 0;
- otherwise: `TraceIDRatioBased(ratio)`.

**Errors.** A head sampler cannot decide on an error before the span exists. To keep all failing
reconciles and requests, run a collector with `tail_sampling` (policies: `status_code: ERROR`, latency
over 5 s, `probabilistic` 10 %) and set the processes to ratio 1. Ratio 1 is fine at our volume (6.9).
Without a collector, errors are only kept at the ratio. Their logs still have the trace ID.

### 6.5 Controller

**The wrapper.** `telemetry.Traced(controller string, cache client.Reader, newObj func() client.Object,
r reconcile.Reconciler) reconcile.Reconciler`. It goes into each of the 15 `Complete(...)` calls, through
a small helper in `pkg/controller` (for example `complete(b, "cluster", &Cluster{}, r)`), so a new
controller cannot forget it. It:
- reads the object from the **informer cache** (`mgr.GetClient()`). This makes no API call: the `For` kind
  is always cached. It then parses the cause;
- starts `reconcile <controller>` with `trace.WithNewRoot()`, the link, and `cv.controller`,
  `k8s.namespace.name`, `cv.tenant_id`/`cv.cluster_id` (labels `cloudstore.LabelTenant`/`LabelID`),
  `k8s.object.generation`, `cv.reconcile_id`;
- ends it with the result and the error.

`span.AddLink` after the reconciler's own `Get` would also work, but the sampler sees only links given at
start. So the link is read from the cache before the span starts.

**Sub-spans worth having** (cluster reconciler first; `pkg/reconcile/cluster/reconcile.go`):

| Span | Where | Attributes |
|---|---|---|
| `freeze sync` | `SyncFreeze` | `cv.frozen` |
| `move on` | `moveOn`, with children `discover`, `resolve`, `apply`, `delete` | `cv.state`, `cv.generation` |
| `render` | the `crossplaneaws` plan build called by apply | `cv.render_version`, object count |
| `apply` | `kube.Apply` / `ApplyFields` (`pkg/kube/apply.go`), the single server-side apply path | `k8s.object.kind`, `k8s.object.name`, `k8s.namespace.name`, the returned `k8s.object.resource_version`, `cv.changed` |
| `observe` | `Deps.Observe` (`reconcile/managed.go`), `viewWith` | records, not ready count |
| `platform components` / `helm gate` | `installPlatform`, `helmGate.load`/`register` (`helm_gate.go`) | components written, waiting, blocked |
| `rollout readiness` | `helmGate.rollout`, `workloads.of` (`helm_rollout.go`) | release, ready, timed out |
| `helm recover` | `helmOps.RollBack`/`Uninstall` (`management/helm_ops.go`) | wraps `runBounded`. It is the only span for the Helm SDK's kube calls (they have no context, so the round-tripper skips them). |
| `node groups` | `reconcileNodeGroups` | groups, changed |
| `tenant components` | `installTenant` | count |
| `delete stage` | `driveDelete` / `delete_stage.go`, `delete_platform.go` | `cv.delete.stage` (the seven stages), reachable |
| `catalog change` / `release move` | `catalog_change.go`, `release_move.go` | definition, from and to fingerprint (hashes, not values) |
| `status write` | `remember` | `cv.state` after |

Other controllers (network, account, subtenant, release, rollout) start with the wrapper span alone. Add
stages later where a trace shows a gap.

**What cached reads mean.** The reconcilers read through `Deps.Kube` = `mgr.GetClient()`, which reads
from the informer cache. A cached `Get`/`List` makes no HTTP call, so the round-tripper records no span,
which is correct: it costs microseconds. To still see them, wrap the cached client with
controller-runtime's `client/interceptor` to count reads as a span **event** or attribute
(`cv.cache.reads`) on the current span, not as spans. Uncached reads (`Direct`, `KubeReader`, Secrets
excluded from the cache in `kube/manager.go`) are real HTTP calls and get spans.

### 6.6 Kubernetes API clients

A round-tripper set with `rest.Config.Wrap(telemetry.KubeTransport(tp, cluster))`, added at the
chokepoints:

| Config | Covers |
|---|---|
| `kube.RESTConfig` (`pkg/kube/kube.go`) | management: server, controller, live reader. Managers, `direct`, stores, informers. |
| `deprecation.RESTConfig` (`pkg/deprecation/kube.go`) | workload: `WorkloadClients`, `Workloads`, `Teardowns`, `SubTenantWorkloads`, policy reader, recommendations, Helm ops (`restGetter` copies it) |
| `deprecation.NewKubeReader`, `live.NewDynamicLister` | the two that call `clientcmd.RESTConfigFromKubeConfig` directly. Change them to go through one helper. |

Rules (proven in the spike):
- **Child-only.** A span is recorded only when the request's context already has a valid span. The
  informers' list and watch requests (their own background context), leader election renewals and the
  Helm SDK's context-less calls make no spans. Otherwise they would be thousands of root traces, and a
  watch would be a span hours long.
- **Watches are never traced**, even under a span (`watch=true`).
- **The name is `kube <verb> <resource>[/<subresource>]`**, built from the path, for example
  `kube patch clusters/status` or `kube list deployments`. Attributes are `k8s.cluster`
  (`management` or `workload`), `k8s.verb`, `k8s.resource`, `http.request.method`,
  `http.response.status_code`, and `server.address` (the API endpoint host). **No URL**: it holds the
  namespace and object names, which are recorded once, deliberately, on the `apply` span.
- **Errors:** status Error on transport errors, 5xx and 429. 404 and 409 are normal control flow in
  this code (`IgnoreNotFound`, `ErrConflict` retries), so they are attributes, not errors.
- The `traceparent` is injected into the request. An upstream `kube-apiserver` with `APIServerTracing`
  (on by default since 1.27) could continue the trace, but **EKS exposes no tracing configuration**, so
  for EKS clusters, the management cluster on EKS included, the trace ends at our client span. A kind
  or self-managed management cluster could export apiserver and etcd spans to the same collector.
- A hand-written round-tripper is used instead of `otelhttp.NewTransport` because otelhttp always sets
  `url.full` and cannot be told to skip calls with no parent. It is about 80 lines.
- A visible effect: workload clients are built per reconcile (`client.New` in `workload_client.go`). Their
  discovery (`kube list discovery`) will show up in every pass. That is a measurement this work brings
  for free, and the likely first optimization (cache clients per kubeconfig Secret resourceVersion).

### 6.7 AWS and other external calls

- **AWS SDK v2.** In `aws.NewFromConfig` (`pkg/cloud/aws/aws.go`), next to `callTimeout`:
  `otelaws.AppendMiddlewares(&base.APIOptions, otelaws.WithTracerProvider(tp))`. Every client, in the
  controller (`management.NewAWSClient`) and in the server (`NewResultsStore`), is built from copies of
  `base`, so this one line covers EC2, EKS, IAM, CloudWatch Logs, S3 and STS. Spans are
  `<Service>.<Operation>` with `rpc.service`, `rpc.method`, `cloud.region`, `aws.request_id` and the
  HTTP status.
- **STS chains.** `stscreds.NewAssumeRoleProvider(sts.NewFromConfig(from), ...)` uses a client built
  from the same base, so AssumeRole calls are traced too. The credentials cache retrieves inside the
  signing step of the call that needs them, with that call's context, so a cold AssumeRole (and the
  source-role hop of `sourceConfig`) shows up **nested under the first EC2/S3 call** that triggered it.
  That is the right place to see it. Concurrent callers waiting on the same refresh show only a longer
  span.
- **Attribute filtering.** otelaws can add DynamoDB and SQS attributes; we use neither. Do not record
  role ARNs or External IDs (`AssumeRole` input) in span attributes. otelaws does not record request
  parameters, and we add none.
- **S3 results reads** (server): traced through the same middleware. Add `cv.cluster_id` on the handler
  span, not the bucket key.
- **OIDC** (`pkg/auth/verify.go`): the `http.Client` gets `otelhttp.NewTransport`. Discovery and JWKS
  refreshes run on `context.Background()` on purpose. Change that to `context.WithoutCancel(reqCtx)`: it
  keeps the "one caller leaving does not cancel it" property and keeps the span, so the first request
  after a restart shows its discovery cost.
- **Registries.** The Go code makes no registry calls. Charts are pulled by provider-helm, packages by
  Crossplane. Nothing to instrument.
- **Crossplane and provider-helm.** They are separate processes with their own reconcile loops. They
  read our managed resources and Releases from their own informers and call AWS or Helm later, possibly
  many times. Their code has no OTel tracing we can enable, and their AWS calls are out of band, so **the
  trace ends at the managed-resource write**. Correlation:
  - the `apply` span records `k8s.object.kind/name/namespace` and the `resource_version` the write
    returned;
  - a later reconcile's `observe` span records, per object that changed condition, `Synced`/`Ready` and
    the object's `resourceVersion`, so "written at rv 812, ready at rv 815" is visible across two
    traces;
  - the providers' Kubernetes Events and logs name the same object. A runbook query: from a trace,
    `kubectl get events --field-selector involvedObject.name=<name>`.
  - We do **not** stamp the reconcile's traceparent on managed resources: it would change their
    annotations (and the render hash comparison) on every pass that writes, for no reader.

### 6.8 Live reader

The server → live hop is a synchronous Connect call, so normal propagation applies:
- `live.NewClient` (`pkg/live/handler.go`) gets `connect.NewClient(..., otelconnect.NewClientInterceptor())`,
  and its `http.Client` stays as it is (the interceptor injects the header).
- `live.NewHandler` gets `otelconnect.NewServerInterceptor(otelconnect.WithTrustRemote())`. The only
  caller is the API server: the NetworkPolicy admits only its pods (PROP-02 7.4).
- `Reader.build` runs under `context.WithoutCancel(ctx)` and is shared by concurrent callers. The span
  belongs to the first caller. Later callers that join get a span event `joined build`, with no span of
  their own.
- The workload calls go through `NewDynamicLister`, wrapped as in 6.6.
- **The chart's live NetworkPolicy must allow egress to the collector** (4317/4318) when tracing is on.

### 6.9 Export, volume, cardinality, exemplars

**Volume estimate** (100 clusters, PROP scale decisions). Steady state is one cluster pass per 10 min per
cluster, about 0.17 passes/s, with about 50–150 spans per pass (kube, AWS, stages): roughly 10–25
spans/s. A provisioning cluster passes every 20 s, about 3 passes/min each. Ten clusters provisioning at
once give about 75 spans/s more. The API adds little. At ratio 1 this is under 10 MB/min of OTLP, which a
single collector handles easily. Overhead in the processes: a span is about 1–2 µs and a few hundred
bytes, against reconciles that take 100 ms to seconds. Negligible. The batch processor bounds memory
(`OTEL_BSP_MAX_QUEUE_SIZE`, default 2048, dropped when full).

**Cardinality and PII rules** (in `pkg/telemetry` doc and a test that checks attribute keys):
- Span **names** are bounded: RPC procedure names (registered schema only, as in `telemetry.RPC`), stage
  names, `kube <verb> <resource>`, `<Service>.<Operation>`. Never an ID or a name.
- **Allowed** attributes: opaque platform IDs (tenant, cluster, network, account row IDs), Kubernetes
  object kind, namespace, name and resourceVersion on `apply`/`observe`, generations, states, phases,
  error codes, counts, durations.
- **Forbidden**: request or response bodies, `AnnotationInput`, spec values, tags, descriptions,
  kubeconfigs, tokens, `Authorization`, role ARNs and External IDs, S3 keys, error messages from AWS
  that echo input. Error messages are recorded only through `span.RecordError` with our own wrapped
  errors. Open question 3 asks whether those are safe.
- Tenant-chosen names (cluster names) appear only where the object name is the correlation key (`apply`,
  `observe`), never in span names.

**Exemplars.** The metrics agent's `cluster_vending_rpc_duration_seconds` can attach the trace ID with
`ObserveWithExemplar(v, prometheus.Labels{"trace_id": ...})` when the span is sampled. This works
because the otelconnect interceptor runs first (6.2). Exposition must be OpenMetrics: the server's
`promhttp.HandlerOpts{EnableOpenMetrics: true}` (`server.go:120`), the controller's
`metricsserver.Options.HandlerOpts` (controller-runtime 0.25 accepts option funcs), and the live
reader's handler. Prometheus needs `--enable-feature=exemplar-storage`. The metrics work in progress
adds `cluster_vending_cluster_provisioning_seconds`, `_upgrade_duration_seconds` and
`_component_install_seconds` (`pkg/telemetry/controller.go`). They measure long, multi-pass durations,
so an exemplar would point at only the final pass. Link exemplars only from per-pass histograms. Exemplars can also go on the
live reader's build histogram (`live/metrics.go`). They cannot go on controller-runtime's own
`controller_runtime_reconcile_time_seconds` (internal to controller-runtime), so the wrapper should
observe a `cluster_vending_reconcile_duration_seconds{controller}` histogram with exemplars. Coordinate
with the metrics agent so that one histogram exists, not two. Fleet gauges get no exemplars.

### 6.10 Chart

- `tracing:` values block, off by default:
  ```yaml
  tracing:
    enabled: false
    endpoint: ""            # e.g. http://otel-collector.observability:4317 ; empty + collector.enabled = the bundled one
    protocol: grpc          # grpc | http/protobuf
    samplerRatio: "0.1"     # 1 when a tail-sampling collector decides
    headersSecret: ""       # Secret with OTEL_EXPORTER_OTLP_HEADERS (vendor API keys); never in values
    collector:
      enabled: false        # a small otel-collector Deployment with tail_sampling (errors, >5 s, 10 %)
  ```
- When enabled, each Deployment (server, controller, live, ui) gets `OTEL_TRACES_EXPORTER=otlp`,
  `OTEL_EXPORTER_OTLP_ENDPOINT`, `_PROTOCOL`, `OTEL_TRACES_SAMPLER_ARG`, `OTEL_SERVICE_NAME`, and
  `OTEL_RESOURCE_ATTRIBUTES=k8s.pod.name=$(POD_NAME),k8s.namespace.name=$(POD_NAMESPACE)` from the
  downward API. `extraEnv` still overrides.
- The live NetworkPolicy gains an egress rule to the endpoint's port when tracing is enabled.
- The collector is optional. Most installs point at an existing collector or agent (daemonset); the
  bundled one exists for `kind` and demos, and for tail sampling when none exists.

### 6.11 The spike (scratchpad, not in the repo)

A throwaway module at `/private/tmp/claude-502/-Users-evalsocket-workspace-oss-cluster-vending/b874d434-321b-420a-95e6-04c6d408b166/scratchpad/otel-spike`
(otel v1.47.0, sdk tracetest in-memory exporter, client-go v0.37.2, controller-runtime v0.25.2 fake
client). `go test -race -v ./...` passes all three tests:

1. **`TestKubeTransportTracesOnlyUnderASpan`**: a real client-go clientset built with `rest.Config.Wrap`
   against an `httptest` API server. A `Get` with a background context and a `Watch` under a span
   record **0 spans** and send no `traceparent`. A `Get` under a reconcile span records `kube get
   configmaps` as its **child**, the fake API server receives the matching `traceparent`, and no
   attribute holds the object name or namespace.
2. **`TestCauseIsStampedOnlyForAPIRequests`**: a context with a span but no origin mark (the controller)
   returns no annotation change. An API context stamps the traceparent. An unsampled API request
   clears it.
3. **`TestReconcileLinksToTheCause`**: an RPC span stamps the annotation on an object. The wrapped
   reconciler, reading the object from a (fake) cache, starts **root** spans in **their own trace** with
   **one link** to the RPC span, for both the first pass and a requeue. With the root ratio at **0**, both
   linked reconciles **and** their workload kube call are still sampled, and a reconcile of an object no
   request touched is dropped.

Not proven: otelconnect on this repo's interceptor chain, otelaws nesting of AssumeRole, the Next.js
instrumentation, collector tail sampling. These are standard library behaviour and are checked in
phase 1.

## 7. Alternatives considered

| Option | For | Against | Verdict |
|---|---|---|---|
| **Reconcile as a child of the request** | One trace shows everything. | Traces hours long with thousands of spans, cut by backends and tail samplers. A pass that serves several requests can have only one parent. | Rejected. |
| **Child for the first reconcile only, links after** | The first pass is in the request's trace. | "First" needs state (`observedGeneration` or a written "seen" marker, which is a write and a reconcile). A create's first pass can last minutes, so the problem returns. | Rejected. Links plus `cv.cause.trace_id` give the same search. |
| **Stamp the cause in the status, written by the controller** | The server would not need to change. | It is the controller's write, so the resourceVersion bumps, and it is the wrong process (it does not know the request). | Rejected. |
| **Keep the cause in the controller's memory (by resourceVersion)** | No annotation. | Lost on restart and leader change. The server's span context never reaches the controller anyway. | Rejected. |
| **Use the Kubernetes audit log to correlate** | No code. | EKS audit logs are in CloudWatch, minutes late, and carry no trace ID (no `traceparent` capture). | Not a replacement. Useful for humans. |
| **otelhttp for client-go** | Off the shelf. | Records `url.full` (names, namespaces), and creates roots for informer traffic unless filtered by a custom filter anyway. | Rejected for kube, used for OIDC and plain handlers. |
| **Instrument at `client.Client` (controller-runtime interceptor) only** | Typed: kind and name for free. | Misses client-go's retries, discovery, Helm and dynamic clients, and adds spans for cached reads. | Used for cache-read counts only. |
| **Vendor agent (auto-instrumentation via eBPF)** | No code. | No async link, no stages, no control over attributes. | Rejected. |

## 8. Rollout plan

### Phase 1: SDK, server, live, kube, AWS (synchronous paths)
`pkg/telemetry/tracing.go` (provider, sampler, resource, slog correlation), otelconnect on the API server
and both ends of the live hop, the kube round-tripper at the four config points, otelaws in
`NewFromConfig`, OIDC transport and `WithoutCancel`. Off by default. Verified on `kind` with a local
collector and Jaeger.

### Phase 2: Asynchronous link and the controller
`WithOrigin` in the server, `CauseAnnotations` merged in `cloudstore` and the SubTenant and ReleaseHold
writes, the storetest checks (controller writes keep it), the `Traced` wrapper on all 15 controllers,
`LinkedParentBased`, and the cluster sub-spans of 6.5. Update architecture.md (a "Tracing" section next to
"Metrics").

### Phase 3: Chart, exemplars, collector
The `tracing:` values, NetworkPolicy egress, the optional collector with tail sampling, OpenMetrics
exposition and exemplars on the RPC and reconcile histograms, in step with the metrics work.

### Phase 4: Console and browser
`instrumentation.ts`, the proxy span and header injection. Optionally, browser tracing as a link, behind
`CV_TRACE_BROWSER`.

## 9. Difficulty

Rough effort for one engineer who knows the code. Tests included.

| Part | Effort | Risk | Notes |
|---|---|---|---|
| SDK bootstrap + log correlation | 1 d | Low | Standard. Env-only configuration. |
| API server (otelconnect, otelhttp, interceptor order) | 0.5–1 d | Low | otelconnect v0.12 supports connect v2. Decide trust (open question 1). |
| Console proxy + Next instrumentation | 1 d (+1–2 d browser) | Low–medium | The ESM/edge split of `instrumentation.ts`. Browser export needs CORS and a public endpoint. |
| **Server → controller propagation** | 2–3 d | **Medium-high** | The shared store: the origin mark must be the only trigger (a mistake makes a reconcile loop through `AnnotationChangedPredicate`). The direct SubTenant and freeze writes. The "change open" bound per kind. Needs the storetest guard. |
| **Controller wrapper + sub-spans** | 1 d wrapper, 2–3 d stages | Medium | Mechanical. The risk is volume and choosing spans that answer questions, and the sampler (links must be given at start). |
| **client-go round-tripper** | 1 d | **Medium** | Must be child-only and skip watches, or informers flood the exporter. Two configs bypass `deprecation.RESTConfig`. Helm calls stay dark. |
| AWS (otelaws) | 0.5 d | Low | One line in `NewFromConfig`. Check that AssumeRole nests and that no ARN or External ID is recorded. |
| Live reader | 0.5 d | Low | Two interceptors. The shared build span. |
| Chart (+ optional collector) | 1–1.5 d | Low | Env, downward API, NetworkPolicy egress for live, collector config. |
| Exemplars | 0.5–1 d | Low | Depends on the metrics agent's histograms and OpenMetrics exposition. |
| **Total** | **about 11–15 days** | | Phases 1–2 (the useful core) are about 8–10 days. |

**Is it hard in the controller?** No. The mechanics are small: controller-runtime has no tracing, but a
wrapper around each `Reconciler` gives every pass a span. `ctx` is already passed through all of
`pkg/reconcile`, with no goroutines and only `WithoutCancel` (which keeps spans). So sub-spans are
`tracer.Start(ctx, ...)` at the stage functions. Three things take real care: the asynchronous hop
(links, the cause annotation, and not causing churn through the shared store); keeping the volume and
cardinality sane (child-only kube spans, a sampler that follows links); and accepting the gaps that
cannot be closed (Helm SDK calls, Crossplane providers, the EKS control plane).

## 10. Open questions

1. **Trusting the console's `traceparent`.** Should the server take it as the parent (one trace from the
   browser to the store write) or as a link? Trusting it needs a way to tell the console apart (a
   separate listener or port only the UI's NetworkPolicy reaches, or mTLS).
2. **The bound on links.** Is "until observed, while pending, or 1 h" right, or should a create link for
   its whole provisioning (often 15–25 min, already covered by "pending")?
3. **Error messages in spans.** Our wrapped errors sometimes include AWS messages that echo input (names,
   CIDRs). Record them as they are (operators see the same in logs), or record only the error class?
4. **Caller identity.** Record the OIDC `sub` (`enduser.id`) on server spans? It is useful for audits, but
   it is personal data in a trace backend with a different retention and access than logs.
5. **CLI.** Should the CLI start traces (`--trace` printing the trace ID)? It would be treated as an
   untrusted link like any external caller.
6. **Collector ownership.** Ship the optional collector in this chart, or only document pointing at an
   existing one?
7. **Workload client caching.** Phase 1 will measure the per-reconcile discovery cost. If it is large,
   caching clients per kubeconfig Secret resourceVersion is a separate change.
