# Events

The controller reports what happens to cloud accounts, networks, clusters,
releases and channels as Kubernetes events of the object it happens to, and
keeps the last ten with a network's or a cluster's status.

An event is emitted when something **changes**: a state is reached, a step
finishes, something fails or fails for another reason. A pass that finds
things as they were emits nothing, however often the controller looks.

## Where they show

| Where | What | Kept |
|---|---|---|
| `kubectl describe` / `kubectl get events` on the object in the management cluster | every event, as an `events.k8s.io` Event | about an hour (Kubernetes' own retention) |
| `cluster-vending cluster events <id>` / `network events <id>`, the `ListClusterEvents` and `ListNetworkEvents` RPCs | the same events, read through the API, newest first; `--only-warnings` | as above |
| `status.recentEvents` of the Network and Cluster (`get`, `GetCluster`, `GetNetwork`) | the last ten, oldest first, with a count when the same thing happened again | with the object |

Events of a network or a cluster are in the tenant's namespace; those of
the cluster-scoped Release and ReleaseChannel are in `default`. Each names
the controller that reported it (`cluster-vending-controller`).

```sh
cluster-vending cluster events 01a1d2f6-12a9-7505-b7b2-8b8acfcab306 --tenant acme
cluster-vending cluster events 01a1d2f6-12a9-7505-b7b2-8b8acfcab306 --tenant acme --only-warnings -o json
kubectl describe clusters.cloud.cluster-vending.io -n tenant-acme prod-eks
```

```
TIME                  COUNT  TYPE     REASON                   MESSAGE
2026-11-13T05:10:41Z  1      Warning  PlatformComponentFailed  kyverno 3.10.0: chart kyverno: create failed (chart kyverno, Helm state failed, failed 2 times): ...
2026-11-13T05:02:10Z  1      Normal   Running                  generation 1 is applied and all 51 resources are ready
2026-11-13T04:58:03Z  1      Normal   AddonsReady              all 11 add-ons are ready
2026-11-13T04:51:30Z  1      Normal   ControlPlaneActive       the EKS cluster is ACTIVE on Kubernetes 1.37
```

## Reasons

`Normal` is something that went as expected; `Warning` is a failure or
something that needs a look. The reason is the same word for every event of
its kind; the message has the details.

| Object | Reason | Type | When |
|---|---|---|---|
| Network, Cluster | `Provisioning` | Normal | its resources are being created |
| Cluster | `ControlPlaneActive` | Normal | the EKS cluster is ACTIVE |
| Cluster | `NodeGroupReady` | Normal | the default node group is ready |
| Cluster | `AddonsReady` | Normal | every add-on is ready |
| Cluster | `Running` | Normal | every resource is ready and the generation is applied |
| Network | `Ready` | Normal | every resource is ready and the generation is applied |
| Network, Cluster | `Updating` | Normal | a new generation, or a renderer change, is being applied |
| Network, Cluster | `Failed` | Warning | a resource failed or vanished, or the pass could not apply the change; the message names it. Said again only when it is about another resource |
| Network, Cluster | `Repaired` | Normal | an object deleted from the management cluster was created again |
| Network, Cluster | `Paused` | Normal | an operator paused its resources (`admin pause`): nothing changes in the cloud |
| Network, Cluster | `Resumed` | Normal | the pause ended |
| Network, Cluster | `Deleting` | Normal | deletion started, waits for something else than before, or got stuck |
| Network, Cluster | `Deleted` | Normal | everything is gone and the object is removed |
| Network, Cluster | `ImportDiscovered` | Normal | an import found the existing resources; observed, not owned |
| Network, Cluster | `ImportBlocked` | Warning | the import found something it cannot take over |
| Network, Cluster | `TakenOver` | Normal | the platform owns the imported resources now |
| Cluster | `UpgradeStarted` | Normal | a Kubernetes upgrade began (the control plane first) |
| Cluster | `UpgradeNodes` | Normal | the control plane runs the new version; the node groups, then the add-ons, follow |
| Cluster | `UpgradeFinished` | Normal | control plane, node groups and add-ons run the new version |
| Cluster | `PlatformComponentInstalling` | Normal | a platform component's charts are being installed |
| Cluster | `PlatformComponentReady` | Normal | a platform component is installed |
| Cluster | `PlatformComponentFailed` | Warning | a platform component failed: its chart, Helm's state, the failures and the provider's last error |
| Cluster | `PlatformComponentRemoved` | Normal | a platform component is no longer installed |
| Cluster | `StuckReleaseRecovered` | Warning | a stuck Helm release's record was removed so it installs again |
| Cluster | `RecoveryLimitReached` | Warning | a stuck Helm release was recovered as often as allowed (3 times) |
| Cluster | `PlatformComponentsReady` | Normal | every platform component is installed |
| Cluster | `DeprecatedAPIs` | Warning | the scan found API versions the next Kubernetes version removes |
| Cluster | `DeprecationScanFailed` | Warning | the scan for deprecated API versions failed |
| Cluster | `DeprecationScanClean` | Normal | a scan that had findings, or had failed, finds nothing removed now |
| Cluster | `KubernetesEnding` | Warning | the cluster's Kubernetes minor is on no release channel any more: it is ending (`status.kubernetes`) |
| CloudAccount | `Bootstrapped` | Normal | the tenant namespace and the account's ProviderConfig exist |
| CloudAccount | `ProviderConfigFailed` | Warning | the account's ProviderConfig or credentials copy could not be applied |
| CloudAccount | `AccessVerified` | Normal | the tenant role works in every region the account uses (the access check, docs/controller.md) |
| CloudAccount | `AccessDenied` | Warning | a region or call of the access check was denied; the message says which and why |
| CloudAccount | `Deleted` | Normal | the account's ProviderConfig is gone and the account is removed |
| Release | `ReleaseValid` | Normal | a release passed its checks and can be offered |
| Release | `ReleaseInvalid` | Warning | a release fails a check; the message says which |
| ReleaseChannel | `ChannelOffers` | Normal | a channel offers another release than before |
| ReleaseChannel | `ChannelCannotMove` | Warning | a channel cannot offer the release it should; the message says why |
| ReleaseChannel | `RolloutTier` | Normal | the rollout of the channel's Kubernetes minor moved to another tier |
| ReleaseChannel | `RolloutHalted` | Warning | more upgrades failed in a tier than `spec.policy.maxFailed` allows; nothing more starts |
| Cluster, ReleaseChannel | `AutomaticUpgradeStarted` | Normal | the rollout set the cluster's next Kubernetes minor, e.g. "automatic upgrade 1.36 → 1.37 (channel alpha, tier 2)" (`--automatic-upgrades`) |
| Cluster | `AutomaticUpgradeSucceeded` | Normal | an automatic upgrade finished: the cluster runs the new minor (`status.rollout`) |
| Cluster | `AutomaticUpgradeFailed` | Warning | an automatic upgrade failed, or did not finish within `--upgrade-timeout`; it counts against the tier's `maxFailed` |

The constants are in `pkg/reconcile/events.go`.

## How it works

- **What is an event is decided from the status.** When a pass records a
  network's or a cluster's status, it compares it with the one recorded
  before (`statusEvents` in `pkg/reconcile/cluster` and
  `pkg/reconcile/network`): the differences are the events. So an event is
  emitted once the status that shows it is stored, and a restart of the
  controller does not repeat it. A step that knows more than the status
  shows (a repair) says so itself.
- **The reconcilers know no event recorder.** They report through a small
  interface (`reconcile.Events`); the controller gives them the manager's
  recorder (`pkg/controller/events.go`), which writes `events.k8s.io`
  Events with the object's UID, so `kubectl describe` lists them.
- **Repeats are folded, not flooded.** A failure whose provider text
  changes on every retry is one event, as is a deletion whose count of
  resources goes down. In `status.recentEvents` the same reason and message
  again bumps the time and the count of the newest row.
- **The API server only reads them.** `ListClusterEvents` and
  `ListNetworkEvents` list the Events of the tenant's namespace through the
  store and keep those about the object; the server's role may `get` and
  `list` events for that.

## Permissions

| Who | On `events` (`events.k8s.io`) |
|---|---|
| controller (`config/rbac/role.yaml`) | `create`, `patch` |
| API server (`config/server/base/role.yaml`) | `get`, `list` (and `create`, `patch` for its own leader-election events) |

Through the API, the events RPCs are reads of a tenant: with
authentication on, any role for the tenant may call them
([authentication.md](authentication.md)).

## Limits

- Kubernetes drops events after about an hour; only `status.recentEvents`
  outlives that, and it keeps ten.
- A CloudAccount, a Release and a ReleaseChannel have no `recentEvents`
  and no events RPC: use `kubectl describe`.
- An event that is emitted but whose status write is lost to a conflict
  with an API call is not repeated: the next pass sees the status as
  recorded by that call.
- The last event of a deleted object (`Deleted`) refers to it by name
  only: the object is gone by then.
