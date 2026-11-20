# Reliable Helm installs and upgrades (provider-helm, hardened)

Status: built (all eight mechanisms, below); what cannot be proven without
a real workload cluster is listed under "Not proven offline". provider-helm
stays the engine; the controller manages every Helm operation around it so a
release never sits stuck, flaps, or burns revisions.

## What goes wrong today (seen on a live cluster)

| Release | State | Cause |
|---|---|---|
| kyverno | `pending-rollback`, "another operation is in progress" | Helm's release lock left behind by an interrupted operation; nothing clears it. |
| gpu-operator | revision 15 | A pre-upgrade hook Job could not schedule; every poll retried the upgrade, each a new failed revision. |
| capsule | revision 9 | The workload API was briefly unreachable; each failure counted as an upgrade attempt. |

The Release objects set no `wait`, no timeout, no rollback; `deployed` only
meant "manifests written", and `maxHistory` was 20.

## Mechanisms

1. **Wait for real rollout.** Every Release sets `wait: true` and
   `waitTimeout` from the definition's new `install.timeout` (default 10m;
   heavy charts more). Ready means the workloads rolled out, so slow image
   pulls on new nodes are waited for, not failed. `maxHistory: 5`.

2. **Change only when something changed.** The controller computes a
   digest of chart (repository, name, version) and the rendered values and
   stores it on the Release (`cloud.cluster-vending.io/helm-digest`). The
   Release spec is written only when the digest differs. A Kubernetes
   upgrade whose components keep their version and values changes no
   Release: the component steps are reported as skipped ("versions
   unchanged").

3. **One operation per cluster at a time.** Within a cluster, at most one
   Release is installing or upgrading; the next starts when it is settled
   (deployed and Ready, or given up). Order follows `sequence` and
   `requires`.

4. **Back off instead of hammering.** After a failed install or upgrade the
   controller pauses that Release (`crossplane.io/paused`) and unpauses it
   after an exponential backoff (1m, 2m, 4m … capped at 30m, jittered). A
   failed attempt therefore costs one revision per backoff step, not one per
   15-second poll. A spec change (new digest) resets the backoff.

5. **Classify errors.** Transient (workload API unreachable, timeouts,
   throttling, "could not get server version", TLS handshake) → retried
   quietly with backoff, never counted as a failure. Chart errors (pull
   failed, chart not found) → retried with backoff, reported. Release errors
   (hook failed, rollout timeout, invalid values) → counted.

6. **Recover stuck releases** (through the workload kubeconfig, with the
   Helm SDK, operations only — provider-helm still owns the Release):
   - `pending-install` / `pending-upgrade` / `pending-rollback` older than
     the component's timeout plus 2m, with no operation running: roll back
     to the last `deployed` revision; with none (a first install that never
     finished), uninstall so the next attempt installs cleanly.
   - A failed hook Job left behind is deleted before the retry.
   - Each recovery emits an event and is counted on the component.

7. **Give up loudly, not silently.** After 5 counted failures in a row the
   component is `FAILED` with the classified last error, the backoff stays
   at its cap, and a `HelmReleaseFailed` event is emitted once. A new spec
   (digest) or an explicit retry (`cluster retry --component`) starts
   again.

8. **Status.** Each component reports: phase, chart version, Helm state,
   revision, attempts, last error (classified), next retry time, and the
   last recovery.

## Settings

Per definition: `install.timeout`. Controller flags: `--helm-backoff-max`
(default 30m), `--helm-max-failures` (default 5).

## How it is built

| Mechanism | Where |
|---|---|
| 1 Wait for the rollout | `crossplaneaws.PlatformSet` renders `forProvider.wait: true`, `waitTimeout` (the definition's `spec.install.timeout`, default `10m`, allowed 1m to 2h) and `maxHistory: 5`; `rollbackLimit` stays 100 (the backoff, not provider-helm, paces retries). Heavy charts ship `15m` (gpu-operator, istiod, istio-ingress-gateway, kyverno). |
| 2 Digest | `pkg/helmrel.Digest`: sha256 of repository, chart, version, the rendered values, `skipCRDs` and the timeout (a longer timeout must reach the live Release). Annotation `cloud.cluster-vending.io/helm-digest`. `sequence.Item.Gate` skips an object whose live digest equals the rendered one: nothing is written, and the component reports `unchanged`. A Kubernetes upgrade whose component steps find every component unchanged reports the step in `status.upgrade.skipped_steps` ("component versions unchanged"); the gate that holds the upgrade is not touched. |
| 3 One operation at a time | `pkg/reconcile/cluster/helm_gate.go`: one list of the cluster's own Releases (label selector, no fleet scan) per pass, shared by the platform and tenant components of the pass. A Release that must change (new or different digest) is written only when no other Release has an operation in flight and none was written earlier in the pass. In flight means running within its timeout; a Release given up, stuck past its timeout (`stuck`), in its backoff, paused by anyone or going away blocks nobody. The gate follows install order: a Release never waits behind one of a higher sequence (or whose component requires it), and while several must change the lowest sequence (a required one first) goes first, so Karpenter is never held by capsule. It is the Gate of Helm Release items only, never of a component's IAM objects; the rest wait (`waiting for Helm release X to settle`), reported PENDING or INSTALLING, never READY. Order is the set's: `sequence`, then `requires`. A Release that is itself unsettled may take a new digest, which is how a fix ends its backoff. |
| 4 Backoff | `helmrel.Step` (pure, table-tested): after a new failed attempt (the Release's `status.failed` rose, the error changed, or a due retry did not report within its timeout plus 2m) the Release is paused with `crossplane.io/paused` and the platform's marker `cloud.cluster-vending.io/helm-backoff-until`; 1m, 2m, 4m, ... capped at `--helm-backoff-max`, up to a quarter jitter, never past the cap. The pass after the end lifts the pause (and deletes a failed hook Job first). Attempts, next retry and class are on the component status. A pause without the marker (an operator's, a freeze's) is never touched, counted or lifted; a freeze that starts during a backoff pause finds the Release paused already and so does not mark it, and nothing lifts it before the freeze ends because the pass that would lift it does not run on a frozen cluster. A cluster that is imported and not taken over has no Release objects and is never touched. |
| 5 Classes | `helmrel.Classify` on the Release's Synced message: transient (workload API unreachable, TLS handshake, i/o timeout, connection refused, throttling, Helm's lock "another operation ... is in progress", "has no deployed releases"), chart (not found, pull failed), release (anything else: a hook failed, rollout timeout, invalid values). Only release errors count. |
| 6 Recovery | `platform_recover.go` extended: a Release in a pending or uninstalling Helm state for its timeout plus 2m (the fixed 10m for a Release an older build wrote) is rolled back to the last deployed revision (the stuck one is marked failed first), or uninstalled when none was deployed, with the Helm SDK (`helm.sh/helm/v3`) through the workload kubeconfig (`management.helmOps`, behind `reconcile.HelmOperator`; a storage that is only records still gets the stuck record removed). A failed hook Job of the release is deleted first. Counted in `recoveries` (at most 3 per version), `last_recovery`, event `StuckReleaseRecovered`. |
| 7 Give up | After `--helm-max-failures` counted failures in a row: FAILED with the classified last error, retry at the cap, event `HelmReleaseFailed` once, the Release marked `helm-gave-up` (it no longer holds back the cluster's other Releases). A new digest, or `cluster retry --component`, starts over. |
| 8 Status | `PlatformComponent` and `ComponentStatus`: phase, `chart_version`, `helm_state`, `revision`, `digest`, `attempts`, `counted_failures`, `last_error` with `last_error_class`, `next_retry_at`, `last_recovery`(`_at`), `unchanged`. Also on the kind's `status.platformComponents`, in `cluster components` and `component list --details`. |

Behaviour worth knowing:

- A transient error shows INSTALLING with when it is tried again; a chart or
  release error that is being retried shows FAILED with the attempt and the
  next try (`status.platform_components[].next_retry_at`), and the component
  stays FAILED once given up. Chart errors back off and are reported but
  never give up.
- The controller looks at a cluster again when a platform component's
  backoff ends (`PlatformRetryAt`). Tenant components are lifted by whichever
  pass comes next.

## Migration of existing clusters

A cluster upgraded to this build has Releases that are stuck or failing
from before. The gate never lets one of them deadlock the rest: only a
running operation blocks, and install order decides who goes first. The
placement values of the charts follow the live node group, not the rollout
(see `karpenter-node-groups.md`, "Migration of existing clusters"), so a
chart is not moved to a node label that no node has yet; once the target is
live, each component moves with one Helm upgrade, in install order.

## Not proven offline

No real cluster was available, so these rest on reading provider-helm
v1.5.0's CRD and Helm v3.22's code, and on tests with fakes and Helm's
in-memory driver:

- That provider-helm acts on an annotation-only change (the unpause) at
  once, and that `status.failed` rises with every failed attempt. The
  retry-not-reported rule (timeout plus 2m) is the backstop.
- That Helm accepts `rollback` after the stuck revision is marked failed for
  every kind of interruption (the SDK path is tested against Helm's
  in-memory storage, not a live API server).
- The Ready condition's `observedGeneration` is used when provider-helm sets
  it, to tell a Release that has not seen its new spec yet from a settled
  one; where it does not, the one-operation rule can let a second change
  start in the seconds before the first is observed.
