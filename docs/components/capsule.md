# Capsule platform component

The shipped definition is [capsule.yaml](../../pkg/catalog/definitions/capsule.yaml).
Its chart/app version is pinned to **0.15.2** in
[release.yaml](../../pkg/catalog/release.yaml), on all offered Kubernetes minors.
Research uses the released chart tarball, its values/schema/templates, and the
[official installation guide](https://projectcapsule.dev/docs/operating/setup/installation/).
The moving main-branch values and documentation can describe newer fields.

## Installation and rollout

Capsule is `management: Platform`, installed into `capsule-system` only when
the cluster's `spec.naas` is `true`. Omitted or `false` leaves Capsule off.
The existing platform-component reconcile installs it after a cluster is running;
tenants select NaaS in cluster create/update rather than creating a Component.
The existing release,
tier, freeze, takeover and upgrade gates still apply. Shipping this catalog creates
a new shipped release; an existing cluster receives the addition when its channel
and rollout move it to that release, not by editing its older release in place.

Cluster YAML uses the same field as the API and UI:

```yaml
spec:
  naas: true
```

An API update sets `cluster.spec.naas` and includes `spec.naas` in the update mask,
including when setting it to `false`. Turning it off removes the platform-owned
Capsule Helm installation through normal desired-state reconciliation, leaving
unrelated components and the existing cert-manager alone. It disables Capsule
admission enforcement; upstream CRDs and tenant data may remain. Review the
destructive plan before disabling an installation with tenants.
Freezes delay both installation and removal until the resource is allowed through.
Release moves omit Capsule entirely while NaaS is off, so its shared release pin
does not block clusters that do not use it.
If a platform release move is already in progress, the requested toggle waits
for that move to finish before changing its component membership. This preserves
ordered upgrade progress and avoids skipping or reverting components. A halted
move still requires its normal fix/retry; toggling NaaS does not bypass it.

**Use the cluster's existing cert-manager.** It must run in each workload cluster,
with its CRDs, webhook and CA injector ready before Capsule is installed. A
cert-manager running only in the management cluster cannot issue workload-cluster
certificates. This definition neither installs nor takes ownership of cert-manager.
It leaves `certManager.generateCertificates: true`, and turns off Capsule's
alternative built-in TLS generator. The chart creates a namespaced self-signed
Issuer and Certificate; cert-manager issues and renews the webhook Secret.
There is no dependency on a catalog definition named cert-manager.
The cert-manager path has no post-install certificate-wait Job. Verify the
Certificate becomes Ready, the webhook Service has ready endpoints, and admission
and CRD-conversion CA bundles are populated. Existing platform status reports
Helm deployment, not a separate Capsule certificate-readiness condition.

The chart's CRD lifecycle hook uses server-side apply on install/upgrade.
Do not disable hooks or switch to `crds.inline` without designing CRD upgrades.
Both the controller and hook Jobs select and tolerate the default system node pool.
Two controller replicas use leader election, preferred node anti-affinity and a
PDB retaining one pod. Resource requests are 100m CPU / 128Mi memory, with a 512Mi
memory limit; tune from observed workload, not an assumed tenant count.

Strict RBAC is enabled instead of a cluster-admin binding. In this exact chart,
strict mode still includes broad read/delete/patch permissions needed for generic
namespace cleanup; it does not isolate the controller to tenant namespaces.
Custom owner roles or resource replication may need additional permissions.
Admission hooks retain upstream failure policies, with trusted system callers and
nodes excluded from admission matching. The Node webhook stays off. Kyverno's
platform namespace exclusions include `capsule-system`.

This step installs the operator. It does **not** create Capsule Tenant objects,
map platform Tenant CRs into Capsule Tenants, configure IAM/OIDC owner identities,
or deploy capsule-proxy. The default Capsule identity group remains
`projectcapsule.dev`; cluster authentication must map intended users into it.
The platform's `cloud.cluster-vending.io` Tenant and Capsule's
`capsule.clastix.io` Tenant are distinct resources.

## Complete Helm values reference

Retrieve **every value with upstream comments and defaults for the pinned version**:

```sh
helm show values oci://ghcr.io/projectcapsule/charts/capsule --version 0.15.2
helm pull oci://ghcr.io/projectcapsule/charts/capsule --version 0.15.2 --untar
# capsule/values.yaml, capsule/values.schema.json, capsule/templates/
```

The released artifact is also available from the official
[chart repository](https://projectcapsule.github.io/charts/capsule-0.15.2.tgz).
The table below covers all top-level value families, including optional features
left at upstream defaults. Maps accepting arbitrary Kubernetes objects are open
configuration surfaces; they do not have a finite list of possible leaf values.
Platform values are locked to the platform team, not exposed as tenant Helm inputs.

| Values | Purpose and integration decision |
|---|---|
| `global.openshift`, `openshift` | SCC/security context and etcd-encryption integration; off for this EKS definition. |
| `global.jobs` | Post-install, pre-delete and post-delete hooks; kubectl image, labels, annotations, security, resources, placement, priority, TTL and retry budget. Preserve CRD/cleanup hooks and configure system placement. |
| `crds` | Install, inline/exclusive mode, configuration/RBAC/diagnostics creation, labels and annotations. Hook-based lifecycle stays enabled. The chart spells the annotations field `annnotations`. |
| `tls`, `certManager` | Existing Secret, built-in TLS reconciliation versus cert-manager, additional certificate SANs. Use existing cert-manager. |
| `proxy` | Optional capsule-proxy subchart and its own values; disabled. |
| `rbac` | Optional tenant-facing roles for TenantResources, ResourcePoolClaims and CustomQuotas; defaults retained. |
| `manager.rbac` | Role creation, strict/minimal mode, bindable or existing roles, extra namespaced/cluster rules. Strict enabled; `minimal` is deprecated. |
| `manager` deployment/image/network settings | Deployment/DaemonSet strategies, image, hostNetwork, hostPID, hostUsers and webhook port; EKS default VPC CNI settings retained. Custom CNIs may require host networking and firewall review. |
| `manager.options` | Configuration CR creation/name/metadata; workers, QPS/burst, cache timings, leader election; users/administrators/ignored groups; owner promotion, namespace prefixes/protection, node metadata, role names, impersonation and tracing. Do not invent identity mappings during installation. |
| `manager.apiPriorityAndFairness` | Optional FlowSchema/PriorityLevel configuration, precedence and API version; off until cluster API fairness policy is chosen. |
| `manager.extraArgs`, `env`, probes, resources, volumes, volumeMounts, securityContext | Controller flags, environment, health checks and runtime configuration; fixed resource budget in the definition. These keys are under `manager`. |
| `imagePullSecrets`, `podLabels`, `podAnnotations`, `customLabels`, `customAnnotations` | Pod/global metadata and private image credentials. |
| `priorityClassName`, `podSecurityContext`, `securityContext` | Controller priority and pod/container security; upstream non-root hardening retained. |
| `nodeSelector`, `tolerations`, `replicaCount`, `ports`, `affinity`, `topologySpreadConstraints` | Controller placement, availability and additional ports; two replicas with preferred host spreading. |
| `jobs` | Deprecated override of `global.jobs.kubectl`; do not use for new configuration. |
| `serviceAccount` | Creation, name, annotations and image-pull secrets; no IAM role required. |
| `extraManifests` | Additional templated Kubernetes resources; supplies the PDB because this chart has no native PDB value. |
| `monitoring` | Diagnostic/application dashboards, Grafana Operator objects, ServiceMonitor and PrometheusRule configuration. Off by default to avoid requiring monitoring CRDs. |
| `conversions.service` | CRD conversion service URL/name/namespace/port/CA overrides; defaults retained. |
| `webhooks` | Mutating/validating timeouts, metadata, service overrides, global and per-type match conditions, exclusive mode and individual hooks. Keep enforcement enabled. |

Individual `webhooks.hooks` cover `customquotas`, `globalcustomquotas`,
`globalresourcequotas`, `calculations`, `rulestatus`, `metadata`, `generic`,
`resourcepools.pools`, `resourcepools.claims`, `customresources`, `namespaces`,
`cordoning`, `gateways`, `ingresses`, `devices`, `pods`,
`persistentvolumeclaims`, `tenants`, `owners`, `config`, `managed`,
`replicationDefaults`, `replications`, `services`, `nodes` and
`serviceaccounts`. Each exposes the applicable enabled flag, failure/match/
reinvocation policies, selectors, conditions, rules and hook options.
`serviceaccounts.references.failurePolicy` separately controls deletion
reference checks. `namespaceOwnerReference`, `defaults` and
`tenantResourceObjects` are deprecated aliases.

List values replace upstream lists: take care when setting match conditions or
RBAC roles. Preserve chart defaults that protect managed/replicated resources;
use global conditions for trusted callers instead of replacing per-hook lists.
