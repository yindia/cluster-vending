# cloud.v1 API contract

`idl/cloud/v1` is the contract of the platform: the Protobuf messages and
Connect services through which tenants' cloud accounts, networks and
Kubernetes clusters are described, created, imported and deleted. It is one
provider-neutral contract with a provider block for what has no cross-cloud
equivalent. Only AWS is implemented.

It is read by:

- API clients, over Connect, gRPC or gRPC-Web (JSON or binary);
- the CLI (`pkg/cli`), whose manifests are the messages as JSON or YAML;
- the server (`pkg/server`), which validates requests against it and stores
  specs and statuses as its JSON form;
- the code that turns a spec into Crossplane managed resources
  (`pkg/network`, `pkg/cluster`, `pkg/crossplane/aws`);
- the controller's kinds (`api/v1alpha1`), which mirror it.

The server translates the contract into Crossplane managed resources of
`crossplane-contrib/provider-upjet-aws` v2.9.0 (namespaced kinds,
`*.aws.m.upbound.io`). The GCP and Azure columns in this document and in the
proto comments are the intended mapping for providers that do not exist yet.

## Layout

| File | Holds |
|---|---|
| `tenant_service.proto` | `TenantService`; `CloudAccount`, `AwsAccount`, `CloudAccountStatus`, `CloudAccountState` |
| `network_service.proto` | `NetworkService` and its request and response messages |
| `network.proto` | `Network`, `NetworkSpec`, `NetworkLayout`, `Subnet`, `NetworkFlowLogs`, `NetworkStatus`, `NetworkResources`; `ClusterNetwork` (a cluster's reference to a network) |
| `cluster_service.proto` | `ClusterService` and its request and response messages; `AwsImportCluster` |
| `cluster.proto` | `Cluster`, `ClusterSpec` with its CEL rules, `ReleaseChannel`, `ClusterTier`, `ClusterState`, `ClusterStatus`, `ClusterEndpoint`, `ClusterUpgrade`, `ClusterUpgradePhase`, `ClusterKubernetes` |
| `control_plane.proto` | `ControlPlaneSpec`, `SecretsEncryption`, `EndpointAccess` |
| `node_pool.proto` | `NodePoolSpec`, `ScalingConfig`, `Taint`, `CapacityType`, `TaintEffect` |
| `access.proto` | `AccessSpec`, `AccessEntry`, `AccessPolicy` and their enums |
| `aws.proto` | The AWS provider block: `AwsClusterSpec`, add-ons, control plane log group, launch template, AMI types, `AwsClusterStatus`; `AwsImportNetwork` |
| `import.proto` | Import plans: `ImportStatus`, `ImportedResource`, `ImportPhase`, `Disposition` |
| `plan.proto` | Update plans: `Plan`, `PlannedChange`, `PlannedAction` |
| `managed_resource.proto` | `ManagedResource`, `ManagedResourceState`, `ManagedResourceEvent` |
| `conditions.proto` | `Condition`, `ConditionStatus` |
| `options.proto` | The `(cloud.v1.crossplane)` enum value option |
| `admin_service.proto` | `AdminService` (operators only) |
| `component_service.proto` | `ComponentService` and its request and response messages |
| `component.proto` | `Component`, `ComponentStatus`, `ComponentPhase`; `ComponentDefinition` (what tenants see of a catalog entry) |

## Services and RPCs

Every RPC of every service is implemented; none returns `UNIMPLEMENTED` as
a whole. `ComponentService` stores a tenant's components and does not
install them yet (see below). What individual fields and values are refused is listed under
[Declared but not implemented](#declared-but-not-implemented).

All tenant-facing RPCs carry `tenant_id` (a DNS label of at most 56
characters, so the namespace `tenant-<tenant_id>` fits 63). Where the
server authenticates its callers (`auth.mode` optional or required, see
[docs/authentication.md](../docs/authentication.md)), the caller's OIDC
bearer token must give it a role for that tenant: a viewer may call the
RPCs marked `NO_SIDE_EFFECTS`, a tenant admin all of the tenant's, a
platform admin everything, including `AdminService`. A call the role does
not allow is `PERMISSION_DENIED`; one without an accepted token is
`UNAUTHENTICATED`. With `auth.mode: none` (the default) the server takes
`tenant_id` as given.

### TenantService

| RPC | What it does |
|---|---|
| `BootstrapTenant` | Registers one cloud account of the tenant (creating the tenant if needed), stores it, and creates the tenant namespace and the account's `ProviderConfig` in the management cluster. Idempotent per account. |
| `GetCloudAccount` | Returns one account by its bootstrap ID (`CloudAccount.id`). |
| `ListCloudAccounts` | Lists the tenant's accounts, ordered by provider then account ID. Not paginated. |
| `GetAccountOnboarding` | Returns the CloudFormation template of the platform's role in an AWS account (`reader`, `updater` or `provisioner`, default `provisioner`), the stack's name and its `ManagementRoleArn` parameter, and a console quick-create link when the server has `cloudformation-template-url`. No AWS call; `FAILED_PRECONDITION` without `aws-management-role-arn`. See [docs/onboarding.md](../docs/onboarding.md). |
| `WhoAmI` | Returns the caller as the server sees it: subject, username, issuer, the token's roles and the grants the server's rules give. Open to every accepted caller; anonymous where the server does not authenticate. |

### NetworkService

| RPC | What it does |
|---|---|
| `CreateNetwork` | Stores a network with its defaults resolved. The controller creates its managed resources. |
| `ImportNetwork` | Stores the import of an existing VPC (`IMPORTED`, import phase `DISCOVERING`) without looking at the cloud. The controller looks the VPC up once, read-only, derives the spec, stores the plan (`status.import`) and creates Observe-only managed resources for it. |
| `TakeOverNetwork` | Makes the platform the owner of an `IMPORTED` network. |
| `GetNetwork`, `ListNetworks` | Return the stored network: its spec and the status last recorded. They do not read the management cluster; `GetNetwork` with `live` reads its status now from it. |
| `ListNetworkEvents` | Returns what the controller reported about the network, newest first: the Kubernetes events of its object, kept for about an hour ([docs/events.md](../docs/events.md)). `status.recent_events` of `GetNetwork` keeps the last ten. |
| `WatchNetwork` | Streams the network and the changes of its managed resources until it is deleted or the client cancels. |
| `UpdateNetwork` | Changes the fields named in `update_mask`. |
| `DeleteNetwork` | Marks the network `DELETING` and deletes its resources in order; refused while clusters use it. |

### ClusterService

| RPC | What it does |
|---|---|
| `CreateCluster` | Stores a cluster with its defaults, without looking at the cloud. The controller checks the name against EKS, resolves the Kubernetes and add-on versions (and writes them to the spec), then creates the managed resources. |
| `ImportCluster` | Stores the import of an existing EKS cluster (`IMPORTED`, import phase `DISCOVERING`) without looking at the cloud. The controller looks the cluster up once, read-only, derives the spec, stores the plan (`status.import`) and creates managed resources that only observe it. |
| `TakeOverCluster` | Makes the platform the owner of an `IMPORTED` cluster. |
| `GetCluster`, `ListClusters` | Return the stored cluster: its spec and the status last recorded; `GetCluster` with `live` reads its status now from the management cluster. |
| `ListClusterEvents` | Returns what the controller reported about the cluster, newest first: the Kubernetes events of its object, kept for about an hour; `only_warnings` for the failures. `status.recent_events` of `GetCluster` keeps the last ten. |
| `ListClusterDeprecations` | Returns the objects of the cluster written with API versions that are deprecated or removed at the Kubernetes version it moves to next: what the controller's last scan found, read from the store. `GetCluster` carries only the counts (`status.deprecations`). |
| `ListClusterRecommendations` | Returns the CPU and memory requests (and memory limits) the platform recommends for the cluster's containers, computed from the usage the controller recorded over a week (it samples every five minutes, read-only), with the current values and the difference. `GetCluster` carries only the summary (`status.recommendations`). |
| `WatchCluster` | Streams the cluster and the changes of its managed resources until it is deleted or the client cancels. |
| `UpdateCluster` | Changes the fields named in `update_mask`. With `validate_only` it changes nothing and returns a `Plan`; with `plan_id` it applies only if that plan still holds. |
| `DeleteCluster` | Marks the cluster `DELETING` and deletes its resources. `force` also releases Helm releases without uninstalling them. |

### ComponentService

Catalog components (Karpenter, Kyverno, ...) enabled on a tenant's
clusters, installed by the platform with Helm and versioned by its
releases (docs/design/component-usage.md). Components are never EKS
add-ons; those are `spec.aws.addons`.

**Served, not installed yet.** The handlers (`pkg/server/component`) check
and store. The components the platform manages are installed with their
cluster by the cluster reconcile and reported in the cluster's status; a
component a tenant enables is stored `PENDING` and nothing installs it yet.
The CLI is `cluster-vending component` ([docs/components.md](../docs/components.md)).

| RPC | What it does |
|---|---|
| `ListComponentDefinitions` | Lists the catalog: every component, the platform's included. Each has `management` (`PLATFORM`: installed with every cluster, not a tenant's to enable, change or disable; `TENANT`) and `configurable` (a tenant may set values). A bundle is one entry; its members are not listed. For a tenant's component it also returns `requires`, `conflicts_with_addons`, the values schema, `platform_values` (locked) and the `user_values` file; a platform component's values are not returned. With `cluster_id`, `versions` holds what that cluster's release gives the component on its Kubernetes minor. How a component is installed is never returned. |
| `GetComponentValues` | Returns a catalog entry's `user_values` file as written (YAML with comments), for the tenant to edit and send as `Component.values`. `FAILED_PRECONDITION` for a platform component and for one that takes no values. Keys left as in the file are not stored, so later changes to the file still reach the tenant. |
| `EnableComponent` | Checks the request and stores the component on a cluster, `PENDING` (at most one of each per cluster; `request_id` makes a retry return the first result). The values are pruned to the tenant's edits first; values that touch a `platform_values` path, or a path the cluster's release sets for the component, are refused with every path named. `validate_only` answers with what would be stored (`values`) and what would run (`status.effective_values`), and stores nothing. `PERMISSION_DENIED` for a platform component or a bundle's member. |
| `UpdateComponent` | Replaces `values` as a whole, with the checks of an enable; `component.etag`, when set, must be current (`ABORTED`). The same values again change nothing. |
| `DisableComponent` | Removes the component from its cluster; refused while another enabled component requires it. Nothing is installed for it yet, so it is removed from the store at once and returned as `DELETING`; `force` has nothing to do. |
| `GetComponent` | Returns one stored component. `version` is what the cluster's release gives it. |
| `ListComponents` | The tenant's components, paged. With `cluster_id`: everything on that cluster, by name and unpaged: the platform's components as the cluster's status reports them (`management` `PLATFORM`, no `id`), the tenant's, and the catalog's tenant components that are not enabled (no `id`, no phase, `status.message` "not enabled"). |

**Versions are not a tenant's.** `Component.version` is output only. A
request that sets it, or names the `version` path in `update_mask`, fails
with `INVALID_ARGUMENT`: the version is the one the cluster's release gives
the component.

A cluster cannot be deleted while it has components.

### AdminService

| RPC | What it does |
|---|---|
| `PauseReconcile` | Pauses a tenant, one of its networks or clusters, or a release channel's clusters (`channel`, with `tier` for one tier): everything (the rows get the pause, their managed resources `crossplane.io/paused`), or with `only_upgrades` their automatic upgrades only. `until` ends it by itself. |
| `ResumeReconcile` | Ends the pause of the same target and kind; other pauses of a cluster stay. |
| `ListPauses` | Every pause in effect: target, everything or upgrades only, who, since, until, why, how many clusters. |
| `ListReleases`, `GetRelease` | The releases, newest first; one release per Kubernetes minor: channel, default, upgrade target, end of support, ending, components with versions and locked values. Read-only. |
| `ListReleaseChannels`, `GetReleaseChannel` | alpha, beta, stable: promotion, the release and minor each offers, rollout policy and the tier its rollout is at; one channel adds the releases it offered and its conditions. |
| `GetRollout` | One channel's rollout planned now with the controller's rules (`pkg/rollout`), cluster by cluster (filter by tier, paged). |

### Stored status and live status

| Call | Source | Status returned |
|---|---|---|
| `GetNetwork`, `ListNetworks`, `GetCluster`, `ListClusters`, `WatchNetwork`, `WatchCluster` | the platform's store | the status last recorded: by the controller on every reconcile while the resource is being created, changed or deleted, or is imported. A converged resource keeps the status it converged on. |
| `GetNetwork`, `GetCluster` with `live` | the management cluster, now | live: the conditions of every managed resource, cloud IDs, the EKS endpoint and versions |

A stored change shows in `Get*` at once (`DELETING`, `UPDATING`). What the
cloud did since the last pass of the loop shows in the status calls first.
The watch RPCs read the stored resource every five seconds and send what
changed. The CLI's `--wait` polls the status call.

Mutations are asynchronous: a call returns once the desired state is
stored. The API server checks a request against the contract, the store and
the management cluster; it never looks at the cloud and applies nothing.
What needs the cloud happens in the controller afterwards, and what it
finds wrong is the resource's status, not the answer of the call: `FAILED`
with the reason in `status.message` (a VPC or EKS cluster to import that
does not exist or cannot be read, an EKS cluster name that is taken, a
version EKS does not offer, a provider that is not installed), or the
state it had with a "retrying" message when the cloud did not answer. A
resource has converged when `status.observed_generation` equals
`generation` and `status.state` is `READY` (network) or `RUNNING`
(cluster).

## Resources

### Identity, shared by `Network` and `Cluster`

| Field | Meaning |
|---|---|
| `tenant_id` | Output only: from the request. |
| `id` | Output only: a time-ordered UUID assigned by the server on create. Every other call refers to the resource by it. |
| `name` | Chosen by the client: a lowercase DNS label of at most 40 characters. Immutable. A network's name is not unique. A cluster's name is the EKS cluster name and is unique per account and region. |
| `display_name` | Free text, at most 128 characters. Mutable. |
| `labels` | At most 40 pairs. They are also the cloud tags of every resource (AWS tag character rules; keys starting with `aws:` are refused by the server). Mutable: an update re-tags. |
| `generation` | Output only: incremented by the server when the spec or labels change. |
| `etag` | Output only: changes when what was asked for changes (spec, labels, display name, a delete), not when the controller records its progress (state, generations, status). Sent back on update, delete or takeover, it must match. |
| `create_time`, `update_time` | Output only. |

### CloudAccount (`tenant_service.proto`)

A tenant has any number of accounts. An account must be bootstrapped before
a network is created or imported in it.

| Field | Notes |
|---|---|
| `id` | Output only: the bootstrap ID. `NetworkSpec.bootstrap_id` and `ImportNetworkRequest.bootstrap_id` refer to it. |
| `account_name` | Informational, at most 128 characters. Bootstrapping the same account again updates it. |
| `aws.account_id` | Required: the 12-digit account ID. Unique within the tenant; it identifies the account on a repeated bootstrap. |
| `aws.role` | Required: the name of the IAM role the platform assumes in the account. The tenant creates it, outside this API: `cluster-vending account create` creates the CloudFormation stack of `cluster-vending-<role>` from `GetAccountOnboarding` before onboarding the account. |
| `status.state` | Always `READY` once bootstrap returned. |
| `status.namespace` | `tenant-<tenant_id>`. |
| `status.provider_config_name` | Generated on the first bootstrap, then fixed (for example `aws-0a1b2c3d`). |
| `status.identity` | The role ARN the `ProviderConfig` assumes. |
| `status.conditions` | `Bootstrapped`, and `AccessVerified` with status `UNKNOWN`: the role is not assumed at bootstrap. |
| `status.managed_resources` | The namespace and `ProviderConfig`, and the objects of the account's networks and clusters. |

Bootstrap creates the namespace and one `ProviderConfig`
(`aws.m.upbound.io`) that uses the provider pods' own identity (IRSA) and an
`assumeRoleChain` into the tenant role. No static credentials are stored.
The Crossplane provider packages (`provider-aws-ec2`, `-eks`, `-iam`,
`-cloudwatchlogs`, `-kms`) are installed by the controller at startup, not by
bootstrap; without the AWS `ProviderConfig` kind bootstrap fails with
`FAILED_PRECONDITION`.

### Network (`network.proto`)

Spec:

| Field | Required | Default | After creation |
|---|---|---|---|
| `spec.region` | yes | - | immutable |
| `spec.bootstrap_id` | yes | - | immutable |
| `spec.layout` | yes (may be empty: `{}`) | see below | immutable |
| `spec.layout.cidr_block` | no | `10.0.0.0/16` | immutable |
| `spec.layout.subnets` | no | one `PUBLIC` and one `PRIVATE` subnet per zone, carved from `cidr_block` (needs /22 or larger) | immutable |
| `spec.layout.availability_zone_count` | no; not with zonal subnets | 3: the first zone IDs of the region in `pkg/constant/aws` | immutable |
| `spec.layout.nat_gateway_mode` | no | `SINGLE` | immutable |
| `spec.flow_logs.enabled` | no | off | can be turned on and off |
| `spec.flow_logs.traffic_type` | no | `ALL` | fixed while flow logs are on |
| `spec.flow_logs.retention_days` | no | 30 | mutable |
| `spec.flow_logs.aggregation_interval_seconds` | no | 600 | fixed while flow logs are on |

The stored spec holds the resolved layout (explicit subnets) and the
resolved flow log settings, so later renders do not depend on defaults. The
request as sent is stored too.

`UpdateNetwork` accepts these `update_mask` paths: `display_name`, `labels`,
`spec.flow_logs`. Any other path is `INVALID_ARGUMENT`. An `IMPORTED`
network cannot be updated until it is taken over; a taken-over network
accepts `display_name` and `labels`, and refuses `spec.flow_logs` with
`FAILED_PRECONDITION` (it keeps the flow logs it was imported with).

Layout rules the server checks for AWS, beyond the schema: subnets inside
`cidr_block` and not overlapping, each /16 to /28; zone IDs of the region;
`PRIVATE` subnets in at least two zones; at least one `PUBLIC` subnet; with
`PER_ZONE` a `PUBLIC` subnet in every zone that has a `PRIVATE` one.
`REGIONAL` NAT is refused with `UNIMPLEMENTED`.

Status (`NetworkStatus`): `state`, `message`, `observed_generation`,
`conditions` (`Ready`, and `Synced` when a resource failed), `resources`
(VPC ID, subnet IDs, NAT gateway IDs, egress IPs), `cluster_ids`, `import`
(the import plan, kept after the takeover), `managed_resources`.

A network is imported with `ImportNetworkRequest`: `name`, `region`,
`bootstrap_id`, and `aws.vpc_id` with `aws.subnet_ids` (required: the
subnets the platform uses). What is modelled, carried over and left alone
is listed in the comment of `AwsImportNetwork` and under
[Imports](#imports).

### Cluster (`cluster.proto` and the files it imports)

Common spec:

| Field | Required | Default | After creation |
|---|---|---|---|
| `spec.region` | yes | - | immutable |
| `spec.network.network_id` | yes | - | immutable |
| `spec.network.zone_ids` | no | every zone of the network with a private subnet (at least two) | immutable |
| `spec.network.pod_cidr` | must be empty on AWS | - | - |
| `spec.network.service_cidr` | no | left to EKS | immutable |
| `spec.control_plane.kubernetes_version` | no | the EKS default version, resolved at creation and stored | can only move up, one minor at a time |
| `spec.control_plane.endpoint_access.public_access_cidrs` | at least one while the public endpoint is on | none | mutable |
| `spec.control_plane.endpoint_access.endpoint_private_access` | no | true | mutable |
| `spec.control_plane.endpoint_access.endpoint_public_access` | no | true | mutable |
| `spec.control_plane.secrets_encryption.enabled` | no | off | can be turned on, never off |
| `spec.control_plane.secrets_encryption.kms_key_arn` | no | the platform creates a key | can be set on a cluster without encryption; then fixed |
| `spec.default_node_pool.instance_types` | yes | - | mutable: replaces the node group |
| `spec.default_node_pool.capacity_type` | no | `ON_DEMAND` | mutable: replaces the node group |
| `spec.default_node_pool.scaling_config.min_size`, `max_size` | yes | - | mutable |
| `spec.default_node_pool.scaling_config.desired_size` | no | `max(min_size, 1)` | create-only; the autoscaler owns the count |
| `spec.default_node_pool.disk_size_gb` | no; not with a launch template | the provider default | mutable: replaces the node group |
| `spec.default_node_pool.labels`, `taints` | no | none | mutable |
| `spec.access.admin_principals` | no | none | mutable |
| `spec.access.access_entries` | no | none; an entry's `type` defaults to `STANDARD`, a policy's `scope` to `CLUSTER` | mutable |
| `spec.access.oidc` | no; with it, either `entra` (`tenant_id` and `client_id`, both GUIDs: Microsoft Entra ID, the platform fills in the issuer and the claims) or `name`, `issuer_url` (HTTPS) and `client_id` | none: IAM only | mutable: a change removes the association and makes a new one (EKS takes several minutes for each); see [docs/oidc.md](../docs/oidc.md) |
| `spec.release_channel` | no | stays unset | yes; changes no cloud resource |
| `spec.tier` | no | `CLUSTER_TIER_0` | yes; changes no cloud resource |
| `spec.upgrade_mode` | no | `UPGRADE_MODE_MANUAL` | yes; changes no cloud resource |
| `spec.maintenance_window` | no; with it, `start` (`HH:MM`, UTC) and `duration` (`1h` to `24h`, like `4h`, `90m`, `1h30m`); `days` (`WEEKDAY_MONDAY` to `WEEKDAY_SUNDAY`, each once) is optional | none: an upgrade may start at any time | yes; changes no cloud resource |

AWS block (`spec.aws`, required, may be empty: `aws: {}`):

| Field | Default | After creation |
|---|---|---|
| `access_config.authentication_mode` | `API` | declared mutable, refused today (`UNIMPLEMENTED`) |
| `enabled_cluster_log_types` | none | mutable |
| `upgrade_policy.support_type` | left to EKS | mutable |
| `addons` | `vpc-cni`, `coredns`, `kube-proxy`, `eks-pod-identity-agent`, `aws-ebs-csi-driver`; an empty `version` is the one EKS recommends for the Kubernetes version | mutable (the whole list) |
| `control_plane_log_group.enabled` | off: EKS creates the group itself when logging is on | mutable |
| `control_plane_log_group.retention_days` | 90 | mutable |
| `irsa` | on for a new cluster (the platform components' IAM roles use it; `false` is refused while they do); an import keeps its own | mutable: turning it off deletes the IAM OIDC provider |
| `additional_security_group_ids` | none | immutable |
| `default_node_pool.ami_type` | `AL2023_X86_64_STANDARD`; `CUSTOM` with `launch_template.image_id` | mutable: replaces the node group |
| `default_node_pool.launch_template` | none | mutable: setting, removing or switching it replaces the node group; a new version of the same template rolls out in place |

A launch template is either an existing one (`id` or `name`, with a numeric
`version`) or one the platform renders (`security_group_ids`, and for a
custom AMI `image_id` with `user_data`).

`UpdateCluster` accepts these `update_mask` paths:

```
display_name
labels
spec.control_plane.kubernetes_version
spec.control_plane.endpoint_access.public_access_cidrs
spec.control_plane.endpoint_access.endpoint_private_access
spec.control_plane.endpoint_access.endpoint_public_access
spec.control_plane.secrets_encryption
spec.default_node_pool.scaling_config.min_size
spec.default_node_pool.scaling_config.max_size
spec.default_node_pool.instance_types
spec.default_node_pool.capacity_type
spec.default_node_pool.disk_size_gb
spec.default_node_pool.labels
spec.default_node_pool.taints
spec.access.admin_principals
spec.access.access_entries
spec.access.oidc
spec.aws.addons
spec.aws.enabled_cluster_log_types
spec.aws.upgrade_policy.support_type
spec.aws.irsa
spec.aws.control_plane_log_group
spec.aws.default_node_pool.launch_template
spec.aws.default_node_pool.ami_type
spec.upgrade_mode
spec.release_channel
spec.tier
spec.maintenance_window
```

`pkg/server/cluster/mutate.go` (`specPaths`) is the source of this list. Any
other path is `INVALID_ARGUMENT`, except
`spec.aws.access_config.authentication_mode`, which is `UNIMPLEMENTED`.
The merged cluster is validated against the contract again. An `IMPORTED`
cluster cannot be updated until it is taken over.

Release channel and tier: `spec.release_channel` (`ALPHA`, `BETA`,
`STABLE`) and `spec.tier` (`CLUSTER_TIER_0`, `_1`, `_2`; tier 0 is the most
sensitive) say when a cluster receives a platform release: the channels in
the order `ALPHA`, `BETA`, `STABLE`, and within a channel tier 2 first, then
tier 1, then tier 0. `spec.upgrade_mode` says whether releases move the
cluster at all: `UPGRADE_MODE_MANUAL` (the default) means the tenant chooses
the versions and channels do not apply; `UPGRADE_MODE_AUTOMATIC` means the
cluster follows its channel (an unset channel is `STABLE`) by tier. All
three are accepted on create and update, stored and returned. Nothing acts
on them yet and they render no Crossplane field: an update of only these
paths changes no cloud resource, counts as applied at once and leaves the
cluster's state as it is. An unset tier is stored as `CLUSTER_TIER_0` and an
unset mode as `UPGRADE_MODE_MANUAL`, on create and on update; an unset
channel stays unset. An imported cluster has all three unset until it is
taken over.

Kubernetes version on a channel: each channel offers one Kubernetes minor
(the release it offers puts one minor on each of alpha, beta and stable;
`pkg/catalog/release.yaml`). A cluster created on a channel without
`spec.control_plane.kubernetes_version` runs the channel's minor. Asking for
a newer minor than the channel's is `INVALID_ARGUMENT`, on create and on
update, and says what to choose instead (an older minor, or the channel that
offers it); an older minor the release offers is allowed, one it does not
offer is refused. Moving a cluster to a channel whose minor is older than the
one it runs is allowed: it is never downgraded, it waits for the channel. A
cluster on no channel is not checked: its version is the client's, or EKS's
default. When the release channels cannot be read, the create or update is
`UNAVAILABLE`, never accepted unchecked.

`status.kubernetes` says what the cluster's channel offers: `channel_minor`,
`available` (the next minor, one at a time, while the cluster is behind),
and `ending`: the cluster's minor is still supported but on no channel any
more, so the cluster should move to a newer one (event `KubernetesEnding`).
`message` says it in a sentence; `cluster get` prints it, the ending warning
as `WARNING:`.

Maintenance window: `spec.maintenance_window` is when an automatic upgrade
of the cluster may start, every week in UTC: on each of `days` (every day
when empty), from `start` for `duration`. A window may run past midnight:
`days: [WEEKDAY_SATURDAY]`, `start: "23:00"`, `duration: "4h"` is open from
Saturday 23:00 to Sunday 03:00. Only an upgrade's start must fall inside the
window; an upgrade that started runs to its end, since stopping EKS halfway
is worse than finishing late. An update with the path and no window removes
it. Like the release fields, it renders nothing and nothing acts on it yet.

```yaml
spec:
  maintenanceWindow:
    days: [WEEKDAY_SATURDAY, WEEKDAY_SUNDAY]
    start: "02:00"
    duration: 4h
```

On the `Cluster` kind the days are spelled `Monday` to `Sunday`.

Status (`ClusterStatus`): `state`, `message`, `observed_generation`,
`conditions` (`Ready`, and `Synced` when a resource failed), `endpoint`
(URL and CA), `current_kubernetes_version`, `oidc_issuer_url`, `network`
(the network's cloud IDs), `aws` (cluster ARN, cluster security group,
platform version, node group ARN, add-ons with the version each runs),
`import`, `managed_resources`, `kubeconfig_secret`, and `upgrade` while a
Kubernetes upgrade is in progress.

`status.upgrade` (`ClusterUpgrade`) is set from the moment
`spec.control_plane.kubernetes_version` differs from what the control plane
runs until the node group and the add-ons have followed, and unset
otherwise. `state` stays `UPDATING` meanwhile.

| Field | Meaning |
|---|---|
| `from_version` | The version the upgrade started from: what the control plane ran. |
| `to_version` | The version being moved to: `spec.control_plane.kubernetes_version`. |
| `phase` | `CLUSTER_UPGRADE_PHASE_CONTROL_PLANE`: the control plane still reports `from_version`; the EKS cluster is applied at `to_version`, the node group at `from_version` and the add-ons at the versions they run. `CLUSTER_UPGRADE_PHASE_NODES_AND_ADDONS`: the control plane runs `to_version`; the node group and the add-ons are applied at it and are rolling out. |
| `start_time` | When the upgrade was first seen in progress. |
| `message` | What the step waits for; when the cloud refuses or fails the step, the provider's error for the resource concerned. The upgrade is retried, not given up. |
| `node_groups` | In `NODES_AND_ADDONS`: each node group with the version it runs (as the provider last saw it), the version it moves to, and its state (`UPDATING`, `READY`, `FAILED`). |
| `addons_behind` | The add-ons not on their new version yet; empty once all are. |

The upgrade is done only when the control plane and every node group *report* `to_version` and the add-ons are settled, not when their Ready conditions are True: a resource the cloud is moving to a new version is `MANAGED_RESOURCE_STATE_UPDATING` ("updating: 1.36 → 1.37") in `managed_resources` while Ready stays True. That covers the EKS cluster (`version`, or EKS status `UPDATING`), each node group, each add-on (`addon_version`), and a platform component's Helm release while Helm applies a new chart version or values. The provider looks at AWS at its poll interval (10 minutes by default), so a resource shows as updating until its next look; the controller does not force an earlier one.

A cluster is imported with `ImportClusterRequest`: `name` (the EKS cluster
name), `network_id` (a network of the tenant, imported or owned, that holds
the cluster's subnets) and `aws.default_node_group_name` (the node group
that becomes `spec.default_node_pool`).

## States and status

### Network states (`NetworkState`)

| State | Meaning |
|---|---|
| `PROVISIONING` | Being created, or taken over and converging. |
| `IMPORTED` | Imported read-only; waiting for `TakeOverNetwork`. |
| `READY` | Every resource is ready and the latest generation is applied and seen by the providers. |
| `UPDATING` | A change is being applied. |
| `DELETING` | `DeleteNetwork` ran; resources are going away tier by tier. |
| `FAILED` | A resource failed or is missing, or the import plan is blocked. |

### Cluster states (`ClusterState`)

| State | Meaning |
|---|---|
| `PROVISIONING` | Accepted; resources are being created. |
| `RUNNING` | Every resource is ready and the latest generation has converged. |
| `UPDATING` | A change is being rolled out. Version-skew errors during a Kubernetes upgrade count as `UPDATING`. |
| `DELETING` | Teardown in progress; `status.message` names what it waits for. |
| `FAILED` | A resource failed; see `message` and `conditions`. Also an import whose plan is blocked. |
| `IMPORTED` | Imported read-only; waiting for `TakeOverCluster`. |

### Managed resource states (`ManagedResourceState`)

| State | Meaning |
|---|---|
| `CREATING` | Being created or updated; also a resource failing for less than two minutes after its creation. |
| `WAITING` | Waits for a resource it references. |
| `READY` | Exists and matches its spec. |
| `FAILED` | The provider cannot reconcile it; `reason` and `message` say why. |
| `DELETING` | Deletion requested. |
| `MISSING` | Recorded, but not in the management cluster. |
| `UPDATING` | Exists and works, and the cloud moves it to the version its spec asks for (a Kubernetes upgrade of the EKS cluster, a node group or an add-on, or Helm applying a new chart version or values); `message` says from what to what. |

A `ManagedResource` also carries the Crossplane `Ready` and `Synced`
conditions, the cloud ID (`external_name`), `generation` and
`observed_generation` (the provider has reconciled the latest change when
they are equal), and for zonal resources `zone_id` and `role`.

### Import phases and dispositions (`import.proto`)

| Phase | Meaning |
|---|---|
| `PLANNED` | Imported; the plan is ready and nothing changed. |
| `BLOCKED` | The plan has `BLOCKING` lines; nothing was imported. |
| `CONVERGING` | Taken over; the plan is being applied. |
| `CONVERGED` | Taken over; the cloud matches the spec. |

| Disposition | Meaning |
|---|---|
| `MATCHES` | Imported; already what the spec says. |
| `WILL_UPDATE` | Imported; the takeover changes it (`detail` has the difference). `requires_replacement` marks changes that delete and recreate the resource. |
| `WILL_CREATE` | Missing; created at the takeover. |
| `NOT_IMPORTED` | Found next to the imported resources but outside the contract. It needs another owner. A takeover with such lines needs `acknowledge_not_imported`. |
| `BLOCKING` | Cannot be expressed in the contract. |
| `CARRIED_OVER` | Imported and taken over as it is although no spec field describes it: owned and deleted with the network or cluster, not editable through the API. |

### Planned actions (`plan.proto`)

| Action | Meaning |
|---|---|
| `CREATE` | The resource does not exist yet and is created. |
| `UPDATE_IN_PLACE` | The field changes without interruption. |
| `DISRUPTIVE` | The field changes in place, and workloads are affected while it rolls out. |
| `DELETE` | The resource is deleted. |
| `REJECTED_IMMUTABLE` | The cloud cannot change the field; the plan is not `applicable`. |

A `PlannedChange` also has `requires_replacement`, a `unified_diff`, and
`waits_for`: set when the change is made in a later step, with what it waits
for (for example node groups during a Kubernetes upgrade).

## Conventions

1. **Common first.** A concept that exists in all three clouds is a field of
   the common spec, with a neutral name; its comment lists the Crossplane
   path per provider. The `provider` oneof (`aws`; `gcp` = 11 and `azure` =
   12 are kept free) carries only what has no equivalent, and its field
   names mirror the provider's `spec.forProvider`.
2. **Additive only.** Fields are added with the next free number; nothing is
   renumbered or retyped. A removed field has its number and name
   `reserved` (`ClusterSpec` 5 `tags` and 7 `existing_cluster`,
   `NetworkSpec` 3 `tags` and 10 `aws`). `existing_cluster` was an import
   mode that was never implemented; clusters import with `ImportCluster`
   and `TakeOverCluster`. A binary request that still sends field 7 is
   refused with `UNIMPLEMENTED` rather than creating a new cluster; the
   JSON codec drops unknown fields, so a JSON request that sends it is
   treated as a plain create.
   `buf breaking` runs with `WIRE_JSON`, which protects binary and JSON
   clients and allows deleting a reserved field.
3. **New booleans are `optional`** when unset must differ from false
   (`irsa`, `endpoint_private_access`, `flow_logs.enabled`). An optional
   feature that is unset leaves the rendered resources as they were before
   it existed.
4. **Enums** have an `_UNSPECIFIED` zero value, which means "the server
   default" where the comment says so. Enum fields are `defined_only`.
5. **Enums carry their Crossplane spelling.** A value that ends up in a
   Crossplane string field declares it with
   `[(cloud.v1.crossplane) = {aws: "..."}]` (`options.proto`). The server
   reads the option and never derives a value from the proto name.
   `test/idl/crossplane_contract_test.go` lists the enums that must declare
   it and checks spellings.
6. **Validation** is `buf.validate` on fields and CEL rules on messages:
   provider constraints on common fields are rules on `ClusterSpec` with IDs
   prefixed by provider (`aws.region`, `aws.zone_ids`, ...), and
   cross-field rules sit on the message they concern
   (`scaling_config.bounds`, `launch_template.existing_or_rendered`,
   `network_layout.*`). The server runs `protovalidate` on every request.
   What needs data the schema does not have (subnets inside the network,
   zones of a region, EKS versions) is checked by the server.
7. **Update masks.** `UpdateNetworkRequest` and `UpdateClusterRequest` skip
   field rules on the resource (`IGNORE_ALWAYS`): only masked paths are
   copied onto the stored resource and the merged result is validated. In
   JSON the mask is a comma-separated string of lowerCamelCase paths, for
   example `"spec.controlPlane.kubernetesVersion,labels"`.
8. **Etags.** An `etag` sent with an update, delete or takeover must match
   the stored one, or the call fails with `ABORTED`. Without one the call
   applies to the current state. The etag is the user's version: the
   controller applying the resource and recording it Ready or Running
   does not change it, so an etag from a get stays good until someone
   changes what the resource asks for.
9. **Request IDs.** `request_id` (a UUID) on create and import makes a retry
   return the resource of the first call.
10. **Pagination.** `page_size` defaults to 50 and is capped at 200.
    `next_page_token` is the ID of the last resource returned; lists are
    ordered by ID, oldest first.
11. **Zones are zone IDs** (`use1-az1`), never zone names: a zone name maps
    to a different physical zone in each AWS account.

## Errors

The server answers with these Connect codes. Code outside the handlers
returns faults (`pkg/fault`), which `service.ToConnect` and the `Faults`
interceptor map to the same codes.

| Code | Used for |
|---|---|
| `INVALID_ARGUMENT` | A request that fails the contract's validation; an `update_mask` path that cannot be updated or an empty mask; a layout, region, version or add-on the cloud does not accept; a label key starting with `aws:`. |
| `FAILED_PRECONDITION` | The state does not allow the call: the account is not bootstrapped; the network is not ready, or is used by clusters on delete; the resource is being deleted or is imported read-only; a takeover when the phase is not `PLANNED` or `NOT_IMPORTED` lines are not acknowledged; encryption turned off; a stale or inapplicable plan. |
| `NOT_FOUND` | The network, cluster or account does not exist in the tenant. |
| `ALREADY_EXISTS` | A cluster of the platform with that name in the account and region; a VPC that a network already stands for (also a blocked or failed import); a reused `request_id`. |
| `ABORTED` | The `etag` does not match, or the resource changed while the call ran. Get it again and retry. |
| `UNAVAILABLE` | The management cluster cannot be reached; or, where the server authenticates, the token's issuer cannot be reached to verify it. |
| `UNAUTHENTICATED` | Where the server authenticates: no bearer token, or one that is not accepted (expired, another issuer or audience, a bad signature). HTTP 401 with `WWW-Authenticate: Bearer`. |
| `PERMISSION_DENIED` | Where the server authenticates: the caller's roles do not allow the call for the tenant the request names. Also a platform component a tenant tries to change. |
| `UNIMPLEMENTED` | Valid in the contract and not built: a provider other than AWS, `REGIONAL` NAT, updating the authentication mode. Also a binary `CreateCluster` that still carries the removed `ClusterSpec` field 7 (`existing_cluster`). |
| `INTERNAL` | The platform failed. The message carries no detail; the cause is in the server log. |

No call fails because of the cloud. These are a resource's status after the
controller looked (`status.state` `FAILED`, the text in `status.message`,
and a condition `Reconciled` with status `FALSE`), where they used to be the
codes above:

| Was | Now the status of |
|---|---|
| `NOT_FOUND`, `INVALID_ARGUMENT`, `FAILED_PRECONDITION` of `ImportNetwork` and `ImportCluster` (the VPC or cluster does not exist, its ID is malformed, the account's credentials cannot read it) | the imported network or cluster: `FAILED` with the reason. Not found and malformed end the import (`BLOCKED`, not looked up again: delete and import again); unreadable keeps it `DISCOVERING` and is retried |
| `ALREADY_EXISTS` of `CreateCluster` for an EKS cluster made outside the platform | the cluster: "account ... already has an EKS cluster named ..." |
| `INVALID_ARGUMENT` of `CreateCluster` and `UpdateCluster` for a Kubernetes or add-on version EKS does not offer | the cluster; nothing of it is created or changed |
| `FAILED_PRECONDITION` for a provider package that is not installed | the network or cluster |
| `UNAVAILABLE` for an AWS lookup that did not answer | not `FAILED`: the message reads "retrying: ..." and the controller tries again |

## Field mapping

Paths are relative to the Crossplane resource. The node pool is always a
separate resource: AWS `NodeGroup`.

| Common field | AWS | GCP (planned) | Azure (planned) |
|---|---|---|---|
| `spec.region` | `region` | `location` | `location` |
| `control_plane.kubernetes_version` | `Cluster.version`, `NodeGroup.version` | `Cluster.minMasterVersion`, `NodePool.version` | `kubernetesVersion`, `orchestratorVersion` |
| `control_plane.secrets_encryption.enabled` | `kms` `Key` + `Alias`; `Cluster.encryptionConfig` | Cloud KMS key; `Cluster.databaseEncryption` | Key Vault key; `keyManagementService` |
| `control_plane.secrets_encryption.kms_key_arn` | an existing key: `Cluster.encryptionConfig` only | - | - |
| `endpoint_access.endpoint_private_access` / `endpoint_public_access` | `Cluster.vpcConfig.endpointPrivateAccess` / `endpointPublicAccess` | - | - |
| `endpoint_access.public_access_cidrs` | `vpcConfig.publicAccessCidrs` | `masterAuthorizedNetworksConfig.cidrBlocks[].cidrBlock` | `apiServerAccessProfile.authorizedIpRanges` |
| `NetworkSpec.layout.cidr_block` | `VPC.cidrBlock` | carved into `Subnetwork.ipCidrRange` | `VirtualNetwork.addressSpace` |
| layout `subnets[].cidr_block` | `Subnet.cidrBlock` | `Subnetwork.ipCidrRange` | `Subnet.addressPrefixes` |
| layout `subnets[].role` | `PUBLIC` / `PRIVATE` | `PRIVATE` only | `PRIVATE` only |
| layout `subnets[].zone_id` | `Subnet.availabilityZoneId` (required) | empty (regional) | empty (regional) |
| layout `nat_gateway_mode` | `NATGateway` + `EIP`, one or one per zone | `Router` + `RouterNAT` + `Address` | `NATGateway` + `PublicIP` |
| `NetworkSpec.flow_logs` | `FlowLog` on the VPC, cloudwatchlogs `Group`, iam `Role` + `Policy` + `RolePolicyAttachment` | - | - |
| `network.network_id` | the network's private `Subnet`s: `vpcConfig.subnetIds`, `NodeGroup.subnetIds` | `Cluster.subnetwork` | `vnetSubnetId` |
| `network.zone_ids` | private subnets of those zones only | `Cluster.nodeLocations` | pool `zones` |
| `network.service_cidr` | `kubernetesNetworkConfig.serviceIpv4Cidr` | secondary range "services" | `networkProfile.serviceCidr` |
| pool `instance_types` | `instanceTypes` | `nodeConfig.machineType` | `vmSize` |
| pool `capacity_type` | `capacityType` | `nodeConfig.spot` | `priority` |
| pool `scaling_config.{min,max}_size` | `scalingConfig.{min,max}Size` | `autoscaling.total{Min,Max}NodeCount` | `{min,max}Count` |
| pool `scaling_config.desired_size` | `scalingConfig.desiredSize`, sent at creation only | `initialNodeCount` | `nodeCount` |
| pool `disk_size_gb` | `diskSize` | `nodeConfig.diskSizeGb` | `osDiskSizeGb` |
| pool `labels`, `taints` | `labels`, `taint[]` | `nodeConfig.labels`, `nodeConfig.taint[]` | `nodeLabels`, `nodeTaints` |
| `labels` (as cloud tags) | `tags` | `resourceLabels` | `tags` |
| `spec.access.admin_principals` | `AccessEntry` + `AccessPolicyAssociation` (AmazonEKSClusterAdminPolicy) | cluster-admin `ClusterRoleBinding` | `adminGroupObjectIds` |
| `spec.access.access_entries` | `AccessEntry` + one `AccessPolicyAssociation` per policy | - | - |
| `spec.access.oidc` | `IdentityProviderConfig` (`oidc`), created once the `Cluster` is ACTIVE; its external name (the EKS association) is `<name>-<hash8 of its settings>`; `entra` is rendered as issuer `https://login.microsoftonline.com/<tenant_id>/v2.0`, user name claim `oid` and groups claim `groups`, both prefixed `entra:` | - | - |
| `spec.aws.control_plane_log_group` | cloudwatchlogs `Group` `/aws/eks/<name>/cluster`, created before the `Cluster` | - | - |
| `spec.aws.irsa` | iam `OpenIDConnectProvider` of the cluster's issuer | - | - |
| `spec.aws.additional_security_group_ids` | `Cluster.vpcConfig.securityGroupIds` | - | - |
| `spec.aws.default_node_pool.launch_template` | `NodeGroup.launchTemplate`: an existing template, or a rendered ec2 `LaunchTemplate` | - | - |
| `spec.release_channel`, `spec.tier`, `spec.upgrade_mode` | none: stored and returned only | - | - |
| `status.network.egress_ips` | `EIP.atProvider.publicIp` | `Address.atProvider.address` | `PublicIP.atProvider.ipAddress` |

Created for every AWS cluster without a spec field: the cluster and node
IAM roles with their policies, the five default add-ons, the EBS CSI
driver's role and pod identity association, the `ClusterAuth` object behind
`status.kubeconfig_secret`, and `Usage` objects that order deletion.

Names are derived by the server so they fit the provider limits:

| Object | Name |
|---|---|
| Namespace | `tenant-<tenant_id>` |
| Managed resources of a network or cluster | `<name>-<hash8>-<suffix>`, `hash8` being the first 8 hex characters of `sha256(tenant_id/id)` |
| EKS cluster | `Cluster.name` |
| Default node group | `<name>-<hash8>-default`; a replacement is `<name>-<hash8>-default-<hash6>` |
| Imported resources | their cloud names, unchanged |

## Imports

`ImportStatus` is the plan of a network import and of a cluster import. The
rules, for both:

- **Nothing changes at import.** The call stores the import and returns
  (`DISCOVERING`). The controller looks the resources up once, read-only, as
  the tenant role of the account, because Crossplane has no lookup kinds,
  and stores the plan (`PLANNED` or `BLOCKED`) before it applies anything. Network objects are created with `managementPolicies: [Observe]`,
  cluster objects with `[Observe, LateInitialize]`, each with the cloud ID
  as `crossplane.io/external-name`. The lookup does not run again.
- **Tags are kept.** Every existing tag survives the takeover; the platform
  adds its ownership tags and the labels on top. A label with the key of an
  existing tag replaces its value while the label is there; when the label
  goes, the tag the import had is back.
- **Takeover applies the import again, as owned.** What the import found is
  kept with the network or cluster; the takeover renders the same objects
  with full management (without Delete for a resource others share) and
  applies them. Nothing is looked up in AWS again. A network or cluster
  imported before that was kept reports `FAILED_PRECONDITION` in its status
  and must be imported again.
- **Closure.** What the contract models becomes part of the spec; what an
  imported resource needs and the contract does not model is
  `CARRIED_OVER`; what only stands next to it is `NOT_IMPORTED` and needs
  another owner before the previous tool is retired.
- **Order.** A cluster's network must be imported before the cluster is
  imported, and taken over before the cluster is taken over.

| Import | Carried over | Not imported |
|---|---|---|
| Network | flow logs on the VPC that deliver to CloudWatch Logs, with their log group and delivery role (one shared with other VPCs is owned without `Delete`); the default network ACL (never deleted); custom network ACLs of imported subnets, with their rules; routes of specific CIDRs to the internet gateway | the main route table, other routes (peering, Transit Gateway, VPN), the default security group, flow logs to S3 or Firehose, network ACLs of other subnets, unused NAT gateways, the VPC's other subnets |
| Cluster | managed policies on the cluster and node roles beyond the platform's; the IAM OIDC provider of the cluster's issuer; the log group `/aws/eks/<name>/cluster`; the KMS key in `encryptionConfig` (owned without `Delete`); OIDC identity provider configs; add-ons the contract has no name for; pod identity associations that are not an add-on's; the control plane's additional security groups with their rules | every node group but the one named as the default; access entries EKS manages; the default node group's launch template and security groups |

`BLOCKING` for a network: secondary VPC CIDRs, IPv6 or dual-stack, private
egress through something other than a NAT gateway, a VPC CIDR outside /16
to /24, private subnets in a single zone. `BLOCKING` for a cluster: a field
EKS cannot change that differs from what the platform renders.

The platform does not clean up inside the workload cluster: `LoadBalancer`
Services and PersistentVolumeClaims must be removed before `DeleteCluster`,
or their load balancers and volumes stay in the account.

External names the provider expects (also needed for Observe-only objects):

| Kind | external-name |
|---|---|
| VPC, Subnet, InternetGateway, EIP, NATGateway, RouteTable, NetworkACL, DefaultNetworkACL, SecurityGroup, FlowLog, LaunchTemplate | the cloud ID (`vpc-…`, `subnet-…`, `igw-…`, `eipalloc-…`, `nat-…`, `rtb-…`, `acl-…`, `sg-…`, `fl-…`, `lt-…`) |
| SecurityGroupIngressRule, SecurityGroupEgressRule | `sgr-…` |
| NetworkACLRule | `nacl-<crc32 of "<acl-id>-<rule number>-<egress>-<protocol>-">` |
| Route | `r-<rtb-id><crc32 of the destination CIDR>` |
| RouteTableAssociation | `rtbassoc-…` |
| Group (cloudwatchlogs) | the log group name |
| Role | the role name |
| RolePolicyAttachment | `<role name>/<policy arn>` |
| OpenIDConnectProvider | the provider ARN |
| Key (kms) | the key ID |
| Alias (kms) | `alias/<name>` |
| Cluster (eks) | the cluster name |
| NodeGroup | the node group name |
| Addon | `<cluster>:<add-on>` |
| AccessEntry | `<cluster>:<principal arn>` |
| AccessPolicyAssociation | `<cluster>#<principal arn>#<policy arn>` |
| PodIdentityAssociation | the association ID (`a-…`) |
| IdentityProviderConfig | the config name |

## Working on the contract

| Task | Command |
|---|---|
| Generate Go code | `make generate` (`buf dep update`, `buf generate`) |
| Lint and check formatting | `make buf-lint` (`buf format --diff --exit-code`, `buf lint` with `STANDARD`); part of `make check` |
| Format in place | `make buf-format` |
| Check for breaking changes | `make breaking` (`buf breaking` against `.git#branch=main`; set `AGAINST` for another baseline). Not part of `make check`. |
| Contract tests | `go test ./test/idl/...`; part of `make check` |

Generated code goes to `pkg/gen/go/cloud/v1` (messages) and
`pkg/gen/go/cloud/v1/cloudv1connect` (services) and is committed.
`buf.gen.yaml` uses remote plugins on the Buf Schema Registry, pinned to
the runtime versions in `go.mod` (`protocolbuffers/go` with
`google.golang.org/protobuf`, `connectrpc/go` with
`connectrpc.com/connect/v2`); bump both together.

Adding a field:

1. Add it to the message with the next free number. Use `optional` for a
   boolean whose unset state must differ from false.
2. Write the comment: what it means, its default, whether it can change
   after creation, and the Crossplane path it maps to.
3. Add validation: `buf.validate` on the field, a CEL rule on the message
   for cross-field constraints, `defined_only` for enums. An enum value that
   becomes a Crossplane string gets `(cloud.v1.crossplane)`, and the enum is
   added to the list in `test/idl/crossplane_contract_test.go`.
4. `make generate`, and commit the generated code with the proto.
5. Add a contract test under `test/idl`: valid values pass, invalid ones
   are rejected.
6. Handle it in the server: resolve its default (`pkg/network`,
   `pkg/cluster`), map it to the render plan, and, if it can change, add
   its path to the update handler (`pkg/server/*/mutate.go`). A field that
   is unset must leave the rendered resources byte-identical.
7. Update this readme: the resource table, the update paths and the field
   mapping.
8. `make check`, and `make breaking` before merging.

Adding a provider: add `cloud/v1/<provider>.proto` with its spec and status
messages holding only what the common spec lacks; add the arm to the
`provider` oneofs (11 = gcp, 12 = azure); fill in the provider's
`crossplane` value on the common enums and extend the contract test; add
`<provider>.*` CEL rules for its constraints on common fields; add its
region and zone catalog next to `pkg/constant/aws`.

## Declared but not implemented

| In the contract | What the server does |
|---|---|
| Updating `spec.aws.access_config.authentication_mode` | `UNIMPLEMENTED`. It can be set at creation. |
| `NAT_GATEWAY_MODE_REGIONAL` | `UNIMPLEMENTED` for AWS. |
| Providers other than AWS (`gcp`, `azure`) | No message exists; only `aws` can be set. Accounts, networks and clusters of another provider are refused. |
| `ClusterNetwork.pod_cidr` | Must be empty on AWS (CEL rule `aws.no_pod_cidr`). |
| `spec.release_channel`, `spec.tier`, `spec.upgrade_mode` | Stored, returned and updatable. The channel sets and limits the Kubernetes version (above); automatic upgrades do not act on them yet. |
| `CLOUD_ACCOUNT_STATE_BOOTSTRAPPING`, `CLOUD_ACCOUNT_STATE_FAILED` | Never set, as their comments say: an account's state is `READY` once bootstrap returned. The tenant role is not assumed at bootstrap; a role that cannot be assumed shows on the managed resources of networks and clusters. |
| `ComponentService` | Served: a tenant's component is checked and stored, and stays `PENDING`. Nothing installs it yet; only the components the platform manages are installed, with their cluster. `Component.version` and the `version` mask path are refused: a tenant does not choose a version. `DisableComponent.force` has nothing to do. |
| Caller authorization of `tenant_id` | Not implemented, as the service comments say: the server trusts the `tenant_id` of the request. |
| `NetworkStatus.carried_over`, `ClusterStatus.carried_over` | Never filled, as their comments say. Carried-over resources appear in `status.import.resources` with disposition `CARRIED_OVER` and in `status.managed_resources`. |
